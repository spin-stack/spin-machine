// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/spin-machine/boot"
)

// TestPageCache asks two questions of the page caches a machine has - the host's, which its disk
// is read through, and the guest's, which is the machine's own memory - that nothing else here
// measures:
//
//  1. What the guest's disk holds twice, and whether the guest's cache comes back (cachesProbe):
//     a workspace that writes and reads its disk has those blocks in its own page cache and,
//     through QEMU's writeback cache, in the host's as well. The overlay's residency in the host's
//     cache, read with mincore, after the guest wrote and read a file, with the disk as it is and
//     with --disk-direct-over-backing (O_DIRECT for the overlay, the base still cached); what the
//     copy is worth, as the file read again and as 8 KiB files read back shuffled; QEMU's resident
//     memory at each step; and, from the overlay's allocated size and fstrim's own report, whether
//     the guest's discard reaches the file.
//
//  2. Whether a guest can let go without losing what it works with (reclaimProbe):
//     memory.reclaim asked for a file read once while a working set read twice is cached beside
//     it - what comes back to the host, and what reading the working set again costs, with free
//     page reporting at its default order and at lower ones.
//
//     SPIN_PAGE_CACHE=1   run at all
//     SPIN_PAGE_CACHE_ONLY=<regexp>  only the subtests whose name matches: caches, reclaim
//     REPS=<n>            boots per variant (default 5)
//     FILE_MB=<n>         the file the guest writes and reads (default 512, a quarter of the
//     guest's memory: the overlay is made 2 GiB and the guest's root grown into it)
//
// What both answered, and what else was measured and turned down, is docs/memory-and-disk.md.
//
// Needs /dev/kvm, a built release and sudo (the guest's workload is a unit written into its
// overlay, as bench's variants are).
func TestPageCache(t *testing.T) {
	if os.Getenv("SPIN_PAGE_CACHE") == "" {
		t.Skip("set SPIN_PAGE_CACHE=1: this boots VMs and fills their caches")
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
		{"caches", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, cachesProbe) }},
		{"reclaim", func(t *testing.T) { cacheSteps(t, out, reps, fileMB, reclaimProbe) }},
	} {
		if only.MatchString(sub.name) {
			t.Run(sub.name, sub.run)
		}
	}
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
	// flags are spin-machine's, and cmdline is added to the guest's kernel command line.
	flags   []string
	cmdline string
}

var writeback = cacheVariant{label: "writeback"}

// The guest's side of each probe: a unit that runs once the machine is up, takes each step and
// says so on the console with what the guest sees - its page cache and free memory, in kB - and
// how long the step's I/O took, then waits for the host to measure before the next. The I/O is
// timed by /proc/uptime, which only goes forward: the wall clock can step as the machine comes
// up, and on the lab runner it stepped back under a write, which timed it at -183 ms. Each script is a format: %[1]d is the file's size in MB.
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
	variants: []cacheVariant{
		writeback,
		{label: "direct overlay", flags: []string{"--disk-direct-over-backing"}},
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
// and where its discards went.
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
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2", "-F", "qcow2", "-b", base, overlay, "2G")
	edit := without()
	edit.setup = `resize2fs "$(findmnt -no SOURCE "$MNT")" >/dev/null`
	edit.files["/usr/local/sbin/pagecache.sh"] = fmt.Sprintf(probe.script, fileMB)
	edit.files["/etc/systemd/system/pagecache.service"] = cacheUnit
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
	// spin-machine execs QEMU in place, so its pid is QEMU's.
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
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	timer := time.AfterFunc(cmp.Or(time.Duration(envInt(t, "CACHE_TIMEOUT_S", 240))*time.Second, 4*time.Minute), kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	pid := cmd.Process.Pid
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
		if s.ramRSS, s.ramHuge, err = guestRAM(pid, 2048); err != nil {
			t.Fatal(err)
		}
		if s.overlayCache, err = resident(overlay); err != nil {
			t.Fatal(err)
		}
		if s.baseCache, err = resident(base); err != nil {
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
	run.notes = append(run.notes, discardNote(t, out, qmpSock, overlay, probe.trims))
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

// resident is how many MB of the file at p are in the host's page cache, by mincore over a
// mapping of it.
func resident(p string) (float64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return 0, err
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mapping %s: %w", p, err)
	}
	defer func() { _ = unix.Munmap(m) }() // a mapping only read for its residency
	page := os.Getpagesize()
	vec := make([]byte, (len(m)+page-1)/page)
	// x/sys/unix has no Mincore on Linux.
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		return 0, fmt.Errorf("mincore on %s: %w", p, errno)
	}
	n := 0
	for _, v := range vec {
		n += int(v & 1)
	}
	return float64(n*page) / (1 << 20), nil
}
