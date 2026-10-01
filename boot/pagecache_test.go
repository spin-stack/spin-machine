// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"cmp"
	"fmt"
	"io/fs"
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

// TestPageCache asks three questions of the page caches a machine has - the host's, which its disk
// is read through, and the guest's, which is the machine's own memory - that nothing else here
// measures:
//
//  1. What a boot costs cold: the first machine on a host that has just started, or whose cache
//     something else has taken, reads the base image, the kernel and QEMU from the disk. The
//     release's files are dropped from the host's cache (POSIX_FADV_DONTNEED, file by file - never
//     drop_caches, which would take every other runner's cache on the host too) before each cold
//     boot, and a warm boot follows each; interleaved, p50/p95, and how much of the base image a
//     cold boot read.
//
//  2. What the guest's disk holds twice: a workspace that writes and reads its disk has those
//     blocks in its own page cache and, through QEMU's writeback cache, in the host's as well -
//     memory the host pays for a copy only the guest reads. The overlay's residency in the host's
//     cache, read with mincore, after the guest wrote and read a file, with the disk as it is and
//     with --disk-direct-over-backing (O_DIRECT for the overlay, the base still cached). And what
//     the copy is worth: the file read again once the guest dropped it, and the first 128 MB of
//     it split into 8 KiB files and read back in a shuffled order - a build's sources, which the
//     host's copy serves from memory and O_DIRECT sends to the disk one small read at a time.
//
//  3. Whether the guest's page cache comes back: free page reporting returns what the guest
//     frees, and page cache is not free. QEMU's resident memory at each step of the same boot -
//     idle, the file written, the guest's cache full of it, the guest dropping its cache, the file
//     removed - says what a workspace that read a lot costs the host until it lets go, and when it
//     does.
//
//     SPIN_PAGE_CACHE=1   run at all
//     REPS=<n>            boots per variant (default 5)
//     FILE_MB=<n>         the file the guest writes and reads (default 512, a quarter of the
//     guest's memory: the overlay is made 2 GiB and the guest's root grown into it)
//
// Needs /dev/kvm, a built release and sudo (the guest's workload is a unit written into its
// overlay, as bench's variants are).
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

	t.Run("cold boot", func(t *testing.T) { coldBoots(t, out, reps) })
	t.Run("caches", func(t *testing.T) { cacheSteps(t, out, reps, fileMB) })
}

// coldBoots boots the baseline cold and warm, interleaved, and logs both and what a cold boot read.
func coldBoots(t *testing.T, out string, reps int) {
	v := labelled("baseline", without())
	base := filepath.Join(out, "image", "rootfs.qcow2")
	samples := map[string]map[boot.Phase][]time.Duration{"cold": {}, "warm": {}}
	var read []float64
	for range reps {
		if err := evictTree(out); err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{"cold", "warm"} {
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
	fmt.Fprintf(&b, "cold: the release's files dropped from the host's page cache before the boot\n\n")
	fmt.Fprintf(&b, "%-6s %10s %10s %10s %10s\n", "", "FIRMWARE", "KERNEL", "PID1", "USABLE")
	for _, l := range []string{"cold", "warm"} {
		fmt.Fprintf(&b, "%-6s", l)
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
	t.Log(b.String())
}

// The steps of the guest's workload, in the order it takes them, and what each is.
var cacheStepNames = []string{
	"idle",
	"file written",
	"guest cache full",
	"8 KiB files written",
	"8 KiB files read, shuffled",
	"guest dropped its cache",
	"file removed",
}

// cacheWorkload is the guest's side: a unit that runs once the machine is up, takes each step and
// says so on the console with what the guest sees - its page cache and free memory, in kB - and
// how long the step's I/O took, then waits for the host to measure before the next. %d is the
// file's size in MB. Each read starts with the guest's cache dropped, so what it reads comes
// through the disk: from the host's page cache or, with the overlay O_DIRECT, the device. The I/O is timed by /proc/uptime, which only goes forward: the wall clock
// is chrony's to step as the machine comes up, and on the lab runner it stepped back under a
// write, which timed it at -183 ms.
const cacheWorkload = `#!/bin/sh
f=/var/tmp/pagecache.bin
say() { echo "PAGECACHE $1 $(awk '/^Cached:/{c=$2} /^MemFree:/{m=$2} END{print c, m}' /proc/meminfo) $2" > /dev/ttyS0; sleep 4; }
ms() { awk '{ printf "%%d", $1 * 1000 }' /proc/uptime; }
sync; echo 3 > /proc/sys/vm/drop_caches
say 0 0
t=$(ms); dd if=/dev/zero of=$f bs=1M count=%d conv=fsync status=none || { echo "PAGECACHE-FAILED $(df -m / | tail -1)" > /dev/ttyS0; exit 1; }
say 1 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches
t=$(ms); cat $f > /dev/null; say 2 $(( $(ms) - t ))
d=/var/tmp/pagecache.d; mkdir -p $d
t=$(ms); head -c 128M $f | split -b 8K -a 5 - $d/ && sync; say 3 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches
t=$(ms); find $d -type f | shuf | xargs cat > /dev/null; say 4 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches; sleep 6; say 5 0
rm -rf $f $d; fstrim / 2>/dev/null; sync; sleep 6; say 6 0
echo PAGECACHE-DONE > /dev/ttyS0
`

const cacheUnit = `[Unit]
Description=page cache probe
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/bin/sh /usr/local/sbin/pagecache.sh
`

var cacheLine = regexp.MustCompile(`PAGECACHE (\d) (\d+) (\d+) (\d+)`)

// cacheStep is what one step of the workload measured: the guest's cache and free memory, the
// I/O's time, and on the host, QEMU's resident memory and the overlay's and the base image's
// pages in the host's page cache - all in MB but the time.
type cacheStep struct {
	guestCache, guestFree, ms        float64
	qemuRSS, overlayCache, baseCache float64
}

// cacheSteps boots the workload with the disk as it is and with the overlay O_DIRECT, reps times
// each, interleaved, and logs each step's p50.
func cacheSteps(t *testing.T, out string, reps, fileMB int) {
	variants := []struct {
		label string
		flags []string
	}{
		{"writeback", nil},
		{"direct overlay", []string{"--disk-direct-over-backing"}},
	}
	got := map[string][][]cacheStep{}
	for range reps {
		for _, v := range variants {
			got[v.label] = append(got[v.label], cacheBoot(t, out, v.flags, fileMB))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\na %d MB file written, read and removed in a 2048 MB guest, p50 over %d boots; MB but the I/O's ms.\n", fileMB, reps)
	fmt.Fprintf(&b, "host: QEMU's resident memory, and the overlay's and the base image's pages in the host's page cache\n\n")
	for _, v := range variants {
		fmt.Fprintf(&b, "%s\n%-26s %8s %8s %8s %9s %9s %9s\n", v.label, "STEP", "I/O ms", "G.CACHE", "G.FREE", "QEMU RSS", "OVERLAY", "BASE")
		for i, name := range cacheStepNames {
			var col [6][]float64
			for _, run := range got[v.label] {
				s := run[i]
				for j, x := range []float64{s.ms, s.guestCache, s.guestFree, s.qemuRSS, s.overlayCache, s.baseCache} {
					col[j] = append(col[j], x)
				}
			}
			fmt.Fprintf(&b, "%-26s", name)
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
		fmt.Fprintln(&b)
	}
	t.Log(b.String())
}

// cacheBoot runs the workload in one machine started with flags, and measures each of its steps
// on the host as the guest announces it.
func cacheBoot(t *testing.T, out string, flags []string, fileMB int) []cacheStep {
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
	v := without()
	v.setup = `resize2fs "$(findmnt -no SOURCE "$MNT")" >/dev/null`
	v.files["/usr/local/sbin/pagecache.sh"] = fmt.Sprintf(cacheWorkload, fileMB)
	v.files["/etc/systemd/system/pagecache.service"] = cacheUnit
	v.links = map[string]string{"/etc/systemd/system/multi-user.target.wants/pagecache.service": "../pagecache.service"}
	editOverlay(t, overlay, v)

	args := append([]string{"boot", "--release", out, "--disk", overlay, "--memory", "2048", "--cpus", "2",
		"--console", "file:/dev/stdout", "--append", "init=/sbin/init"}, flags...)
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...)
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

	// spin-machine execs QEMU in place: its pid is QEMU's.
	pid := cmd.Process.Pid
	steps := make([]cacheStep, len(cacheStepNames))
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
		m := cacheLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		s := &steps[i]
		s.guestCache, s.guestFree, s.ms = kb(m[2]), kb(m[3]), num(m[4])
		if s.qemuRSS, err = rss(pid); err != nil {
			t.Fatal(err)
		}
		if s.overlayCache, _, err = resident(overlay); err != nil {
			t.Fatal(err)
		}
		if s.baseCache, _, err = resident(base); err != nil {
			t.Fatal(err)
		}
		seen++
	}
	if seen != len(steps) {
		t.Fatalf("the guest reported %d of %d steps; console tail:\n%s", seen, len(steps), tail([]byte(console.String()), 1500))
	}
	return steps
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

// resident is how many MB of the file at p are in the host's page cache, by mincore over a
// mapping of it, and the file's size in MB.
func resident(p string) (float64, float64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return 0, 0, err
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, 0, fmt.Errorf("mapping %s: %w", p, err)
	}
	defer func() { _ = unix.Munmap(m) }() // a mapping only read for its residency
	page := os.Getpagesize()
	vec := make([]byte, (len(m)+page-1)/page)
	// x/sys/unix has no Mincore on Linux.
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		return 0, 0, fmt.Errorf("mincore on %s: %w", p, errno)
	}
	n := 0
	for _, v := range vec {
		n += int(v & 1)
	}
	return float64(n*page) / (1 << 20), float64(fi.Size()) / (1 << 20), nil
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
