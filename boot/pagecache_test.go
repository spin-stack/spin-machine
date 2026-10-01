// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/spin-machine/boot"
)

// TestPageCache asks four questions of the page caches a machine has - the host's, which its disk
// is read through, and the guest's, which is the machine's own memory - that nothing else here
// measures:
//
//  1. What a boot costs cold: the first machine on a host that has just started, or whose cache
//     something else has taken, reads the base image, the kernel and QEMU from the disk. The
//     release's files are dropped from the host's cache (POSIX_FADV_DONTNEED, file by file - never
//     drop_caches, which would take every other runner's cache on the host too) before each cold
//     boot, and a warm boot follows each; interleaved, p50/p95, and how much of the base image a
//     cold boot read. A third boot is prefetched: evicted, and then only the pages a boot reads
//     read back, as a host could as it starts.
//
//  2. What the guest's disk holds twice: a workspace that writes and reads its disk has those
//     blocks in its own page cache and, through QEMU's writeback cache, in the host's as well -
//     memory the host pays for a copy only the guest reads. The overlay's residency in the host's
//     cache, read with mincore, after the guest wrote and read a file, with the disk as it is and
//     with --disk-direct-over-backing (O_DIRECT for the overlay, the base still cached). And what
//     the copy is worth: the file read again once the guest dropped it, and the first 128 MB of
//     it split into 8 KiB files and read back in a shuffled order - a build's sources, which the
//     host's copy serves from memory and O_DIRECT sends to the disk one small read at a time.
//     Between the two: the overlay with 128k subclusters, and the machine in a scope whose
//     MemoryHigh bounds the host's copy. The overlay's allocated size and fstrim's own report say
//     whether the guest's discard reaches the file.
//
//  3. Whether the guest's page cache comes back: free page reporting returns what the guest
//     frees, and page cache is not free. QEMU's resident memory at each step of the same boot -
//     idle, the file written, the guest's cache full of it, the guest dropping its cache, the file
//     removed - says what a workspace that read a lot costs the host until it lets go, and when it
//     does.
//
//  4. Whether a guest can let go without losing what it works with: memory.reclaim, which a
//     guest's own software could write when the machine is idle, asked for a file read once
//     while a working set read twice is cached beside it - what comes back to the host, and
//     what reading the working set again costs, with free page reporting at its default order and
//     at lower ones. (reclaimProbe)
//
//     SPIN_PAGE_CACHE=1   run at all
//     SPIN_PAGE_CACHE_ONLY=<regexp>  only the subtests whose name matches: cold boot, caches, reclaim,
//     reclaimers (which needs a kernel with DAMON; see reclaimersProbe), cow
//     REPS=<n>            boots per variant (default 5)
//     FILE_MB=<n>         the file the guest writes and reads (default 512, a quarter of the
//     guest's memory: the overlay is made 2 GiB and the guest's root grown into it)
//
// Needs /dev/kvm, a built release and sudo (the guest's workload is a unit written into its
// overlay, as bench's variants are, and the scopes are systemd's).
func TestPageCache(t *testing.T) {
	if os.Getenv("SPIN_PAGE_CACHE") == "" {
		t.Skip("set SPIN_PAGE_CACHE=1: this boots VMs cold and warm and fills their caches")
	}
	if !canSudo() {
		t.Skip("the guest's workload is written into its overlay through qemu-nbd, which needs sudo")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 5)
	fileMB := envInt(t, "FILE_MB", 512)

	only, err := regexp.Compile(os.Getenv("SPIN_PAGE_CACHE_ONLY"))
	if err != nil {
		t.Fatalf("SPIN_PAGE_CACHE_ONLY: %v", err)
	}
	for _, sub := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"cold boot", func(t *testing.T) { coldBoots(t, out, reps) }},
		{"caches", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, cachesProbe) }},
		{"reclaim", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, reclaimProbe) }},
		{"reclaimers", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, reclaimersProbe) }},
		{"cow", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, cowProbe) }},
	} {
		if only.MatchString(sub.name) {
			t.Run(sub.name, sub.run)
		}
	}
}

// coldBoots boots the baseline cold, warm, and prefetched - the tree evicted and then only what a
// boot reads read back, which a host could do as it starts - interleaved, and logs all three and
// what a cold boot read.
func coldBoots(t *testing.T, out string, reps int) {
	v := labelled("baseline", without())
	base := filepath.Join(out, "image", "rootfs.qcow2")
	samples := map[string]map[boot.Phase][]time.Duration{"cold": {}, "warm": {}, "prefetched": {}}
	var read, prefetchMS, prefetchMB []float64
	if err := evictTree(out); err != nil {
		t.Fatal(err)
	}
	if run := bootOnce(t, out, v); !run.Reached(boot.Usable) {
		t.Fatalf("the boot that finds what a boot reads reached no login prompt:\n%s", tail(run.Output, 800))
	}
	set, err := bootSet(out)
	if err != nil {
		t.Fatal(err)
	}
	for range reps {
		for _, label := range []string{"cold", "warm", "prefetched"} {
			switch label {
			case "cold":
				if err := evictTree(out); err != nil {
					t.Fatal(err)
				}
			case "prefetched":
				if err := evictTree(out); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				mb, err := prefetch(set)
				if err != nil {
					t.Fatal(err)
				}
				prefetchMS = append(prefetchMS, float64(time.Since(start).Milliseconds()))
				prefetchMB = append(prefetchMB, mb)
			}
			run := bootOnce(t, out, v)
			if !run.Reached(boot.Usable) {
				t.Fatalf("%s boot reached no login prompt:\n%s", label, tail(run.Output, 800))
			}
			for _, p := range []boot.Phase{boot.Firmware, boot.Kernel, boot.PID1, boot.Usable} {
				if d, ok := run.At[p]; ok {
					samples[label][p] = append(samples[label][p], d)
				}
			}
			if label == "cold" {
				mb, _, err := resident(base)
				if err != nil {
					t.Fatal(err)
				}
				read = append(read, mb)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nmilliseconds from the moment before QEMU is exec'd, p50/p95 over %d boots each, interleaved;\n", reps)
	fmt.Fprintf(&b, "cold: the release's files dropped from the host's page cache before the boot;\n")
	fmt.Fprintf(&b, "prefetched: dropped, and then the pages a boot reads read back before it\n\n")
	fmt.Fprintf(&b, "%-10s %10s %10s %10s %10s\n", "", "FIRMWARE", "KERNEL", "PID1", "USABLE")
	for _, l := range []string{"cold", "warm", "prefetched"} {
		fmt.Fprintf(&b, "%-10s", l)
		for _, p := range []boot.Phase{boot.Firmware, boot.Kernel, boot.PID1, boot.Usable} {
			fmt.Fprintf(&b, " %10s", cell(samples[l][p]))
		}
		fmt.Fprintln(&b)
	}
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	p50, _ := boot.Percentile(read, 0.5)
	fmt.Fprintf(&b, "\na cold boot read %.0f MB of the base image's %.0f MB (p50 of the resident pages after it)\n",
		p50, float64(fi.Size())/(1<<20))
	ms, _ := boot.Percentile(prefetchMS, 0.5)
	mb, _ := boot.Percentile(prefetchMB, 0.5)
	fmt.Fprintf(&b, "the prefetch read %.0f MB of %d files in %.0f ms (p50)\n", mb, len(set), ms)
	t.Log(b.String())
}

// cacheProbe is a workload the guest runs once it is up, and the steps it announces, in order.
type cacheProbe struct {
	title    string
	steps    []string
	script   string
	variants []cacheVariant
	// trims is a script that ends in fstrim after removing what it wrote, so QEMU must have
	// counted discards.
	trims bool
}

// cacheVariant is one way of starting the machine a probe runs in.
type cacheVariant struct {
	label string
	// flags are spin-machine's, cmdline is added to the guest's kernel command line, and
	// overlayOpts are qemu-img's -o for the overlay.
	flags       []string
	cmdline     string
	overlayOpts string
	// memoryHighMB, when set, starts the machine in a transient scope with that MemoryHigh: the
	// guest's RAM and the host's page cache for its disk are charged to it, so the host's copy
	// of the overlay is bounded per machine instead of by the host's free memory.
	memoryHighMB int
	// reclaimer names what gives the guest's cache back in reclaimersProbe.
	reclaimer string
}

var (
	writeback = cacheVariant{label: "writeback"}
	direct    = cacheVariant{label: "direct overlay", flags: []string{"--disk-direct-over-backing"}}
)

// The guest's side of each probe: a unit that runs once the machine is up, takes each step and
// says so on the console with what the guest sees - its page cache and free memory, in kB - and
// how long the step's I/O took, then waits for the host to measure before the next. The I/O is
// timed by /proc/uptime, which only goes forward: the wall clock is chrony's to step as the
// machine comes up, and on the lab runner it stepped back under a write, which timed it at
// -183 ms. Each script is a format: %[1]d is the file's size in MB.
const cacheCommon = `#!/bin/sh
f=/var/tmp/pagecache.bin
d=/var/tmp/pagecache.d
say() { echo "PAGECACHE $1 $(awk '/^Cached:/{c=$2} /^MemFree:/{m=$2} END{print c, m}' /proc/meminfo) $2" > /dev/ttyS0; sleep 4; }
ms() { awk '{ printf "%%d", $1 * 1000 }' /proc/uptime; }
small() { find $d -type f | shuf | xargs cat > /dev/null; }
sync; echo 3 > /proc/sys/vm/drop_caches
say 0 0
t=$(ms); dd if=/dev/zero of=$f bs=1M count=%[1]d conv=fsync status=none || { echo "PAGECACHE-FAILED $(df -m / | tail -1)" > /dev/ttyS0; exit 1; }
`

// cachesProbe is what the guest's disk holds twice, and what the host's copy is worth. Each read
// starts with the guest's cache dropped, so what it reads comes through the disk: from the
// host's page cache or, with the overlay O_DIRECT, the device.
var cachesProbe = cacheProbe{
	title: "written, read and removed",
	trims: true,
	steps: []string{
		"idle",
		"file written",
		"guest cache full",
		"8 KiB files written",
		"8 KiB files read, shuffled",
		"guest dropped its cache",
		"file removed",
	},
	script: cacheCommon + `say 1 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches
t=$(ms); cat $f > /dev/null; say 2 $(( $(ms) - t ))
mkdir -p $d
t=$(ms); head -c 128M $f | split -b 8K -a 5 - $d/ && sync; say 3 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches
t=$(ms); small; say 4 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches; sleep 6; say 5 0
rm -rf $f $d
# fstrim skips extents ext4 has freed but not yet committed: right after the rm it trimmed only
# the base's free space and none of the file (run 36812647428). sync commits them first.
sync
echo "PAGECACHE-NOTE $(fstrim -v / 2>&1); root $(findmnt -no OPTIONS /); vda discard_max_bytes $(cat /sys/block/vda/queue/discard_max_bytes) granularity $(cat /sys/block/vda/queue/discard_granularity) write_zeroes_max_bytes $(cat /sys/block/vda/queue/write_zeroes_max_bytes)" > /dev/ttyS0
sync; sleep 6; say 6 0
echo PAGECACHE-DONE > /dev/ttyS0
`,
	// The memory.high variants bound what the machine holds: its RAM as the guest touched it
	// (~900 MB at the fullest step) and the host's cache of its disk. 1280 leaves the cache about
	// 350 MB, 1536 about 600.
	variants: []cacheVariant{
		writeback,
		direct,
		{label: "writeback, 128k subclusters", overlayOpts: "extended_l2=on,cluster_size=128k"},
		{label: "writeback, memory.high 1280 MB", memoryHighMB: 1280},
		{label: "writeback, memory.high 1536 MB", memoryHighMB: 1536},
	},
}

// reclaimProbe is whether proactive reclaim gives the host back what a guest read once and keeps
// what it works with: the 8 KiB files read twice - the working set - and then the file read
// once, the guest's cache holding both. memory.reclaim on the root cgroup asks for the file's
// size back, which an LRU would take from the older working set and the multi-gen LRU should
// take from the file, read once; the working set read again says which it took. drop_caches
// after it is everything back, and the working set's read from the disk what that costs.
var reclaimProbe = cacheProbe{
	title: "kept warm and reclaimed",
	steps: []string{
		"idle",
		"cache full, working set read",
		"memory.reclaim the file's MB",
		"working set read",
		"guest dropped its cache",
		"a minute later",
		"working set read",
		"768 MB touched",
	},
	script: cacheCommon + `mkdir -p $d; head -c 128M $f | split -b 8K -a 5 - $d/; sync
[ -w /sys/fs/cgroup/memory.reclaim ] || { echo "PAGECACHE-FAILED no /sys/fs/cgroup/memory.reclaim" > /dev/ttyS0; exit 1; }
echo 3 > /proc/sys/vm/drop_caches
small; t=$(ms); small; w=$(( $(ms) - t )); cat $f > /dev/null; say 1 $w
# memory.reclaim says EAGAIN when it took less than asked for, which is an answer, not a failure.
t=$(ms); echo %[1]dM > /sys/fs/cgroup/memory.reclaim 2>/dev/null; w=$(( $(ms) - t )); sleep 6; say 2 $w
t=$(ms); small; say 3 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches; sleep 6; say 4 0
# Free page reporting hands back a share of what is free each pass, and only blocks of its order:
# whether what was freed but not yet reported (run 36879800428: 344 MB of guest RAM resident with
# 1883 MB free) goes in time, or stays.
sleep 60; say 5 0
t=$(ms); small; say 6 $(( $(ms) - t ))
# What a workspace pays to use memory again once it was given back: 768 MB written to a tmpfs,
# each page of it faulted in on the host - in huge pages where the host still has them whole.
rm -rf $f $d
t=$(ms); dd if=/dev/zero of=/dev/shm/touch bs=1M count=768 status=none; say 7 $(( $(ms) - t ))
rm -f /dev/shm/touch
echo PAGECACHE-DONE > /dev/ttyS0
`,
	// QEMU's memory stayed near 400 MB after the guest freed everything, against 208 MB idle:
	// free page reporting returns free blocks of page_reporting_order and above, 9 (2 MiB) by
	// default, so whether the rest is free memory in smaller blocks is what a lower order says.
	variants: []cacheVariant{
		writeback,
		{label: "writeback, reporting order 3", cmdline: "page_reporting.page_reporting_order=3"},
		{label: "writeback, reporting order 0", cmdline: "page_reporting.page_reporting_order=0"},
	},
}

// reclaimersProbe is what each way of giving the guest's cache back does to a working set that
// is in use: the working set read every 5 s for 40 s while a file read once sits beside it, and
// the reclaimer under test working - memory.reclaim at the end, DAMON_RECLAIM throughout (pages
// idle 10 s and more, anonymous ones skipped, its watermarks opened so free memory does not stop
// it), or MGLRU aged at the start and its older generations evicted at the end, which sorts the
// working set's accessed folios out of them first. Then QEMU's memory, and the working set read
// once more. DAMON and the lru_gen file need a kernel with CONFIG_DAMON_RECLAIM and debugfs:
// the release's has neither DAMON, so this runs on the lab's kernel_run, variant instead.
var reclaimersProbe = cacheProbe{
	title: "read once beside a working set in use for 40 s, and reclaimed",
	steps: []string{
		"idle",
		"cache full, working set read",
		"40 s in use, reclaimed",
		"working set read",
	},
	script: cacheCommon + `mkdir -p $d; head -c 128M $f | split -b 8K -a 5 - $d/; sync
r=$(cat /etc/pagecache-reclaimer)
lg=/sys/kernel/debug/lru_gen
# The root memcg's generations on node 0, one "gen:anon/file" per generation, in pages.
# The cache is charged to the memcg of whoever read it - the probe's own service here - and an
# lru_gen command acts on one memcg, not its children (run 36816203944: root held 12 MB of the
# 673 MB cached). So every memcg is aged and evicted, and what is reported is node 0's file pages
# summed over all of them.
memcgs() { awk '$1 == "memcg" { print $2 }' $lg; }
maxgen() { awk -v id="$1" '$1 == "memcg" { m = $2 == id } m && $1 == "node" { n = $2 == 0 } m && n && $1 ~ /^[0-9]+$/ { g = $1 } END { print g }' $lg; }
gens() { awk '$1 == "node" { n = $2 == 0 } n && $1 ~ /^[0-9]+$/ { f += $4 } END { printf "%%d file pages", f }' $lg; echo " in $(memcgs | wc -l) memcgs"; }
dr=/sys/module/damon_reclaim/parameters
fail() { echo "PAGECACHE-FAILED $*" > /dev/ttyS0; exit 1; }
echo 3 > /proc/sys/vm/drop_caches
small; t=$(ms); small; w=$(( $(ms) - t )); cat $f > /dev/null; say 1 $w
case $r in
damon)
	[ -d $dr ] || fail "no $dr"
	echo 10000000 > $dr/min_age; echo Y > $dr/skip_anon
	echo 1000 > $dr/wmarks_high; echo 999 > $dr/wmarks_mid; echo 0 > $dr/wmarks_low
	echo Y > $dr/enabled ;;
lru_gen)
	# The image masks sys-kernel-debug.mount, so debugfs is not mounted unless asked for.
	mountpoint -q /sys/kernel/debug || mount -t debugfs debugfs /sys/kernel/debug || fail "mounting debugfs"
	[ -w $lg ] || fail "no $lg"
	for id in $(memcgs); do m=$(maxgen $id); [ -n "$m" ] && echo "+ $id 0 $m" > $lg 2>/dev/null; done
	echo "PAGECACHE-NOTE lru_gen after aging: $(gens)" > /dev/ttyS0 ;;
esac
for i in 1 2 3 4 5 6 7 8; do small; sleep 5; done
case $r in
memory.reclaim) echo %[1]dM > /sys/fs/cgroup/memory.reclaim 2>/dev/null ;;
lru_gen)
	echo "PAGECACHE-NOTE lru_gen before evicting: $(gens)" > /dev/ttyS0
	# Every generation but the two youngest, which MGLRU does not evict; swappiness 0, file
	# pages only; nr_to_reclaim in pages, past the file's size. A memcg with too few
	# generations refuses, which is not a failure of the probe.
	no=0
	for id in $(memcgs); do m=$(maxgen $id); [ -n "$m" ] && { echo "- $id 0 $(( m - 2 )) 0 1000000" > $lg 2>/dev/null || no=$(( no + 1 )); }; done
	echo "PAGECACHE-NOTE lru_gen after evicting: $(gens), $no memcgs refused" > /dev/ttyS0 ;;
damon) echo "PAGECACHE-NOTE damon_reclaim $(grep -H . $dr/nr_reclaimed_regions $dr/bytes_reclaimed_regions 2>/dev/null | tr '\n' ' ')" > /dev/ttyS0 ;;
esac
sleep 6; say 2 0
t=$(ms); small; say 3 $(( $(ms) - t ))
rm -rf $f $d
echo PAGECACHE-DONE > /dev/ttyS0
`,
	variants: []cacheVariant{
		{label: "nothing", reclaimer: "none"},
		{label: "memory.reclaim", reclaimer: "memory.reclaim"},
		{label: "DAMON_RECLAIM, 10 s idle", reclaimer: "damon"},
		{label: "lru_gen aged and evicted", reclaimer: "lru_gen"},
	},
}

// cowProbe is what a first write into data the base holds costs: 2000 random 4 KiB overwrites,
// O_DIRECT in the guest, into the largest file under /usr, each one a cluster the overlay does not
// have yet - qcow2 copies the rest of the cluster from the base first, 64 KiB by default and 4 KiB
// with 128k subclusters - and then the same offsets again, which the overlay now has: the
// difference is the copy. The research's seventh experiment.
var cowProbe = cacheProbe{
	title: "untouched; 2000 random 4 KiB overwrites of base data",
	steps: []string{
		"idle",
		"first overwrites",
		"same offsets again",
	},
	script: `#!/bin/sh
say() { echo "PAGECACHE $1 $(awk '/^Cached:/{c=$2} /^MemFree:/{m=$2} END{print c, m}' /proc/meminfo) $2" > /dev/ttyS0; sleep 4; }
ms() { awk '{ printf "%%d", $1 * 1000 }' /proc/uptime; }
b=$(find /usr -xdev -type f -size +20M -printf '%%s %%p\n' | sort -n | tail -1 | cut -d' ' -f2)
sz=$(stat -c %%s "$b")
awk -v n=2000 -v s="$sz" 'BEGIN { srand(1); for (i = 0; i < n; i++) print int(rand() * (s / 4096 - 1)) }' > /var/tmp/offsets
echo "PAGECACHE-NOTE overwriting $b, $(( sz >> 20 )) MB; the file read %[1]d MB is not used" > /dev/ttyS0
sync; echo 3 > /proc/sys/vm/drop_caches
say 0 0
over() { while read -r o; do dd if=/dev/urandom of="$b" bs=4k count=1 seek="$o" conv=notrunc oflag=direct status=none; done < /var/tmp/offsets; sync; }
t=$(ms); over; say 1 $(( $(ms) - t ))
t=$(ms); over; say 2 $(( $(ms) - t ))
echo PAGECACHE-DONE > /dev/ttyS0
`,
	variants: []cacheVariant{
		writeback,
		{label: "writeback, 128k subclusters", overlayOpts: "extended_l2=on,cluster_size=128k"},
		direct,
	},
}

const cacheUnit = `[Unit]
Description=page cache probe
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/bin/sh /usr/local/sbin/pagecache.sh
`

var cacheLine = regexp.MustCompile(`PAGECACHE (\d) (\d+) (\d+) (\d+)`)

// cacheStep is what one step of the workload measured: the guest's cache and free memory, the
// I/O's time, and on the host, QEMU's resident memory, the overlay's and the base image's pages in
// the host's page cache and the overlay's allocated size - all in MB but the time. ramRSS is the
// part of QEMU's resident memory that is the guest's RAM, the rest being QEMU's own: after the
// guest frees everything QEMU stayed ~100 MB above where it booted (run 36803912357), and which
// side that is says whether it is the guest's to give back.
type cacheStep struct {
	guestCache, guestFree, ms        float64
	qemuRSS, overlayCache, baseCache float64
	overlayAlloc, ramRSS             float64
	// qemuCPU is the CPU QEMU had used by then, in ms, all its threads and the guest's vCPUs
	// among them: what giving memory back costs is how much more it climbs. ramHuge is how much
	// of the guest's RAM the host still backs with huge pages, in MB.
	qemuCPU, ramHuge float64
}

// cacheRun is one boot's steps, and what it said beside them: the guest's PAGECACHE-NOTE lines
// and, in a scope, the scope's memory.events.
type cacheRun struct {
	steps []cacheStep
	notes []string
}

// cacheSteps boots probe's workload in each of its variants, reps times each, interleaved, and
// logs each step's p50 and the first boot's notes.
func cacheSteps(t *testing.T, out string, reps, fileMB int, probe cacheProbe) {
	got := map[string][]cacheRun{}
	for range reps {
		for _, v := range probe.variants {
			got[v.label] = append(got[v.label], cacheBoot(t, out, v, fileMB, probe))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\na %d MB file %s in a 2048 MB guest, p50 over %d boots; MB but the I/O's ms.\n", fileMB, probe.title, reps)
	fmt.Fprintf(&b, "host: QEMU's resident memory, the overlay's and the base image's pages in the host's page cache, the overlay's allocated size\n\n")
	for _, v := range probe.variants {
		fmt.Fprintf(&b, "%s\n%-30s %8s %8s %8s %9s %9s %9s %9s %9s %9s %9s\n", v.label, "STEP", "I/O ms", "G.CACHE", "G.FREE", "QEMU RSS", "OVERLAY", "BASE", "ALLOC", "GUEST RAM", "RAM HUGE", "QEMU CPU")
		for i, name := range probe.steps {
			var col [10][]float64
			for _, run := range got[v.label] {
				s := run.steps[i]
				for j, x := range []float64{s.ms, s.guestCache, s.guestFree, s.qemuRSS, s.overlayCache, s.baseCache, s.overlayAlloc, s.ramRSS, s.ramHuge, s.qemuCPU} {
					col[j] = append(col[j], x)
				}
			}
			fmt.Fprintf(&b, "%-30s", name)
			for j, c := range col {
				p, _ := boot.Percentile(c, 0.5)
				w := 8
				if j >= 3 {
					w = 9
				}
				fmt.Fprintf(&b, " %*.0f", w, p)
			}
			fmt.Fprintln(&b)
		}
		for _, n := range got[v.label][0].notes {
			fmt.Fprintf(&b, "  %s\n", n)
		}
		fmt.Fprintln(&b)
	}
	t.Log(b.String())
}

// cacheBoot runs probe's workload in one machine started as v says, and measures each of its
// steps on the host as the guest announces it.
func cacheBoot(t *testing.T, out string, v cacheVariant, fileMB int, probe cacheProbe) cacheRun {
	t.Helper()
	dir := t.TempDir()
	// On tmpfs the overlay is memory whatever the cache mode, and O_DIRECT a no-op: the two
	// variants would measure the same thing.
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	if st.Type == unix.TMPFS_MAGIC {
		t.Fatalf("%s is on tmpfs: set TMPDIR to a directory on a disk", dir)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	base := filepath.Join(out, "image", "rootfs.qcow2")
	// The base image's root has little room: the overlay is larger, and the root grown into it
	// while the overlay is mounted, so the file is the size asked for. The base is not touched.
	create := []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base}
	if v.overlayOpts != "" {
		create = append(create, "-o", v.overlayOpts)
	}
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), append(create, overlay, "2G")...)
	edit := without()
	edit.setup = `resize2fs "$(findmnt -no SOURCE "$MNT")" >/dev/null`
	edit.files["/usr/local/sbin/pagecache.sh"] = fmt.Sprintf(probe.script, fileMB)
	edit.files["/etc/systemd/system/pagecache.service"] = cacheUnit
	edit.files["/etc/pagecache-reclaimer"] = v.reclaimer
	edit.links = map[string]string{"/etc/systemd/system/multi-user.target.wants/pagecache.service": "../pagecache.service"}
	editOverlay(t, overlay, edit)

	// Not under dir: a socket's path has 108 bytes, and the lab runner's TMPDIR alone takes 80 of
	// them, which failed every boot of run 36808388189 before QEMU said a word.
	sockDir, err := os.MkdirTemp("/tmp", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) }) // a socket QEMU made, nothing else
	qmpSock := filepath.Join(sockDir, "qmp.sock")
	machine := append([]string{filepath.Join(out, "bin", "spin-machine"), "boot", "--release", out,
		"--disk", overlay, "--memory", "2048", "--cpus", "2", "--console", "file:/dev/stdout", "--qmp", qmpSock,
		"--append", strings.TrimSpace("init=/sbin/init " + v.cmdline)}, v.flags...)
	// A scope is systemd's, so the machine is started through sudo, as root - which changes
	// nothing the probe measures - and found again by its cgroup; without one, spin-machine
	// execs QEMU in place and its pid is QEMU's.
	var scope string
	if v.memoryHighMB > 0 {
		scope = fmt.Sprintf("spin-pagecache-%d-%d", os.Getpid(), time.Now().UnixNano())
		machine = append([]string{"sudo", "systemd-run", "--quiet", "--scope", "--unit", scope,
			"-p", fmt.Sprintf("MemoryHigh=%dM", v.memoryHighMB), "--"}, machine...)
	}
	cmd := exec.Command(machine[0], machine[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	kill := func() {
		if scope != "" {
			_ = exec.Command("sudo", "systemctl", "kill", "--signal=KILL", scope+".scope").Run()
		}
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	timer := time.AfterFunc(cmp.Or(time.Duration(envInt(t, "CACHE_TIMEOUT_S", 240))*time.Second, 4*time.Minute), kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	pid := cmd.Process.Pid
	cgroup := ""
	if scope != "" {
		cgroup = "/sys/fs/cgroup/system.slice/" + scope + ".scope"
		if pid, err = scopeQEMU(cgroup); err != nil {
			t.Fatal(err)
		}
	}
	run := cacheRun{steps: make([]cacheStep, len(probe.steps))}
	seen := 0
	var console strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		console.WriteString(line + "\n")
		if strings.Contains(line, "PAGECACHE-DONE") {
			break
		}
		if strings.Contains(line, "PAGECACHE-FAILED") {
			t.Fatalf("the guest's workload failed: %s", line)
		}
		if _, note, ok := strings.Cut(line, "PAGECACHE-NOTE "); ok {
			run.notes = append(run.notes, note)
			continue
		}
		m := cacheLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		s := &run.steps[i]
		s.guestCache, s.guestFree, s.ms = kb(m[2]), kb(m[3]), num(m[4])
		if s.qemuRSS, err = rss(pid); err != nil {
			t.Fatal(err)
		}
		if s.qemuCPU, err = cpuMS(pid); err != nil {
			t.Fatal(err)
		}
		// A scope's QEMU is root's, and its smaps are not this test's to read.
		if scope == "" {
			if s.ramRSS, s.ramHuge, err = guestRAM(pid, 2048); err != nil {
				t.Fatal(err)
			}
		}
		if s.overlayCache, _, err = resident(overlay); err != nil {
			t.Fatal(err)
		}
		if s.baseCache, _, err = resident(base); err != nil {
			t.Fatal(err)
		}
		var ost unix.Stat_t
		if err := unix.Stat(overlay, &ost); err != nil {
			t.Fatal(err)
		}
		s.overlayAlloc = float64(ost.Blocks*512) / (1 << 20)
		seen++
	}
	if seen != len(run.steps) {
		t.Fatalf("the guest reported %d of %d steps; console tail:\n%s", seen, len(run.steps), tail([]byte(console.String()), 1500))
	}
	// A scope's QEMU is root's, and so is its socket.
	if scope == "" {
		run.notes = append(run.notes, discardNote(t, out, qmpSock, overlay, probe.trims))
	}
	if cgroup != "" {
		raw, err := os.ReadFile(cgroup + "/memory.events")
		if err != nil {
			t.Fatal(err)
		}
		run.notes = append(run.notes, "memory.events: "+strings.Join(strings.Fields(string(raw)), " "))
	}
	return run
}

// discardNote is where the guest's discards went: how many QEMU's block layer took from the
// guest (query-blockstats' unmap counters), and what the overlay's clusters are now by qemu-img
// map - data, zeroes, or nothing allocated. trimmed is a guest that ran fstrim just before.
func discardNote(t *testing.T, out, socket, overlay string, trimmed bool) string {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dialling QMP: %v", err)
	}
	defer func() { _ = conn.Close() }() // a diagnostic connection
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	q := &qmp{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
	var greeting struct{ QMP *struct{} }
	if err := q.dec.Decode(&greeting); err != nil {
		t.Fatalf("reading the QMP greeting: %v", err)
	}
	q.do(t, "qmp_capabilities", nil)
	var stats []struct {
		Stats struct {
			UnmapOperations int64 `json:"unmap_operations"`
			UnmapBytes      int64 `json:"unmap_bytes"`
			FailedUnmap     int64 `json:"failed_unmap_operations"`
			InvalidUnmap    int64 `json:"invalid_unmap_operations"`
			WrOperations    int64 `json:"wr_operations"`
			WrBytes         int64 `json:"wr_bytes"`
		}
	}
	if err := json.Unmarshal(q.do(t, "query-blockstats", nil), &stats); err != nil {
		t.Fatalf("reading query-blockstats: %v", err)
	}
	var unmap string
	var unmapOps int64
	for _, s := range stats {
		unmapOps += s.Stats.UnmapOperations
		unmap += fmt.Sprintf(" %d ops %d MB (%d failed, %d invalid), beside %d writes %d MB", s.Stats.UnmapOperations, s.Stats.UnmapBytes>>20,
			s.Stats.FailedUnmap, s.Stats.InvalidUnmap, s.Stats.WrOperations, s.Stats.WrBytes>>20)
	}
	// The caches probe trims more than a gigabyte. Upstream virtio-blk counted none of it until
	// qemu/patches/0002; zero there is a QEMU built without that patch.
	if trimmed && unmapOps == 0 {
		t.Errorf("QEMU counted no discard after the guest's fstrim:%s", unmap)
	}
	// -U: the image is open in the running QEMU, and this only reads its tables.
	raw, err := exec.Command(filepath.Join(out, "bin", "qemu-img"), "map", "-U", "--output=json", overlay).Output()
	if err != nil {
		t.Fatalf("qemu-img map: %v", err)
	}
	var extents []struct {
		Length int64 `json:"length"`
		Depth  int   `json:"depth"`
		Zero   bool  `json:"zero"`
		Data   bool  `json:"data"`
		// Offset is where in the file a cluster's data is: on a zero cluster, a cluster that
		// stays allocated (ZERO_ALLOC) rather than one given back.
		Offset *int64 `json:"offset"`
	}
	if err := json.Unmarshal(raw, &extents); err != nil {
		t.Fatalf("reading qemu-img map: %v", err)
	}
	var data, zero, zeroAlloc int64
	for _, e := range extents {
		if e.Depth != 0 {
			continue // the base's, below the overlay
		}
		switch {
		case e.Data:
			data += e.Length
		case e.Zero && e.Offset != nil:
			zeroAlloc += e.Length
		case e.Zero:
			zero += e.Length
		}
	}
	return fmt.Sprintf("discards QEMU took:%s; the overlay's own clusters: %d MB data, %d MB zero still allocated, %d MB zero given back",
		unmap, data>>20, zeroAlloc>>20, zero>>20)
}

// scopeQEMU is the pid of the QEMU in the scope at cgroup, waited for: systemd-run creates the
// scope and then execs spin-machine, which execs QEMU.
func scopeQEMU(cgroup string) (int, error) {
	for range 100 {
		raw, _ := os.ReadFile(cgroup + "/cgroup.procs")
		for f := range strings.FieldsSeq(string(raw)) {
			pid, _ := strconv.Atoi(f)
			if comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); strings.HasPrefix(string(comm), "qemu-system") {
				return pid, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("no QEMU in %s after 5 s", cgroup)
}

func kb(s string) float64  { return num(s) / 1024 }
func num(s string) float64 { n, _ := strconv.ParseFloat(s, 64); return n }

// rss is the process pid's resident memory in MB: its anonymous and shared memory, which is the
// guest's RAM, and the file pages it maps.
func rss(pid int) (float64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for l := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			return kb(strings.Fields(v)[0]), nil
		}
	}
	return 0, fmt.Errorf("/proc/%d/status has no VmRSS", pid)
}

// guestRAM is the resident memory, in MB, of pid's mapping that is the guest's RAM - the one
// anonymous mapping of exactly the machine's memory, sizeMB - and how much of it the host backs
// with transparent huge pages.
func guestRAM(pid, sizeMB int) (resident, huge float64, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps", pid))
	if err != nil {
		return 0, 0, err
	}
	var inRAM, found bool
	for l := range strings.Lines(string(raw)) {
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
		case strings.Contains(f[0], "-") && !strings.HasSuffix(f[0], ":"):
			if inRAM && found {
				return resident, huge, nil
			}
			inRAM = false
		case f[0] == "Size:" && len(f) > 1:
			inRAM = num(f[1]) == float64(sizeMB)*1024
		case f[0] == "Rss:" && inRAM && len(f) > 1:
			resident, found = kb(f[1]), true
		case f[0] == "AnonHugePages:" && inRAM && len(f) > 1:
			huge = kb(f[1])
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("/proc/%d/smaps has no mapping of %d MB", pid, sizeMB)
	}
	return resident, huge, nil
}

// cpuMS is the CPU pid has used, user and system, in ms: utime and stime of /proc/<pid>/stat,
// counted in clock ticks of 10 ms.
func cpuMS(pid int) (float64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The name in parentheses may hold spaces; the fields that follow it do not.
	_, rest, ok := strings.Cut(string(raw), ") ")
	f := strings.Fields(rest)
	if !ok || len(f) < 13 {
		return 0, fmt.Errorf("reading /proc/%d/stat: %q", pid, raw)
	}
	return (num(f[11]) + num(f[12])) * 10, nil
}

// resident is how many MB of the file at p are in the host's page cache, and the file's size in
// MB.
func resident(p string) (float64, float64, error) {
	vec, size, err := residentPages(p)
	if err != nil {
		return 0, 0, err
	}
	n := 0
	for _, in := range vec {
		if in {
			n++
		}
	}
	return float64(n*os.Getpagesize()) / (1 << 20), float64(size) / (1 << 20), nil
}

// residentPages is which pages of the file at p are in the host's page cache, by mincore over a
// mapping of it, and the file's size in bytes.
func residentPages(p string) ([]bool, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil, 0, err
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, 0, fmt.Errorf("mapping %s: %w", p, err)
	}
	defer func() { _ = unix.Munmap(m) }() // a mapping only read for its residency
	page := os.Getpagesize()
	vec := make([]byte, (len(m)+page-1)/page)
	// x/sys/unix has no Mincore on Linux.
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		return nil, 0, fmt.Errorf("mincore on %s: %w", p, errno)
	}
	in := make([]bool, len(vec))
	for i, v := range vec {
		in[i] = v&1 == 1
	}
	return in, fi.Size(), nil
}

// bootSet is the pages of each file of the release tree at dir that a boot from an evicted tree
// left in the host's page cache: what a boot reads.
func bootSet(dir string) (map[string][]bool, error) {
	set := map[string][]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		vec, _, err := residentPages(p)
		if err != nil {
			return err
		}
		if slices.Contains(vec, true) {
			set[p] = vec
		}
		return nil
	})
	return set, err
}

// prefetch reads every page of set into the host's page cache, a run of resident pages at a
// time, and says how many MB that was: what a host would do once, as it starts, so that its
// first boot is not a cold one.
func prefetch(set map[string][]bool) (float64, error) {
	page := int64(os.Getpagesize())
	var total int64
	buf := make([]byte, 1<<20)
	for p, vec := range set {
		f, err := os.Open(p)
		if err != nil {
			return 0, err
		}
		for i := 0; i < len(vec); {
			if !vec[i] {
				i++
				continue
			}
			j := i
			for j < len(vec) && vec[j] {
				j++
			}
			for off, end := int64(i)*page, int64(j)*page; off < end; {
				n, err := f.ReadAt(buf[:min(int64(len(buf)), end-off)], off)
				off += int64(n)
				total += int64(n)
				if err != nil {
					break // io.EOF at the file's last, partial page
				}
			}
			i = j
		}
		_ = f.Close() // read-only
	}
	return float64(total) / (1 << 20), nil
}

// evictTree drops every file of the release tree at dir from the host's page cache: what the
// first machine on a host that has just started reads from the disk. Not drop_caches, which takes
// every other runner's cache on the host with it.
func evictTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // opened only to advise on
		return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	})
}
