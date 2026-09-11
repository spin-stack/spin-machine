// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// cacheMode is how the host caches a VM's disk: the writable overlay and the chain under it.
type cacheMode struct {
	label string
	flags []string // spin-machine boot flags
}

var cacheModes = []cacheMode{
	// QEMU's default, which is what every VM here gets today: the overlay and the chain
	// under it both through the host page cache.
	{"writeback", nil},
	// The overlay O_DIRECT, the chain under it cached: machine.Disk.DirectOverBacking.
	{"direct-over-backing", []string{"-disk-direct-over-backing"}},
	// Everything O_DIRECT, which is cache=none: the usual advice, measured to say what it
	// costs a host whose VMs share one base.
	{"none", []string{"-disk-cache", "none"}},
}

// cacheWorkload runs in every guest once it is up, and prints one line the host parses.
//
// Three things a guest does, each against a different part of the chain:
//   - read: files of the base image the guest has not read yet, so every byte comes from
//     the device — the shared chain, which is what the host page cache is for;
//   - seq: 256 MiB written with O_DIRECT in the guest, so the guest's own cache does not
//     absorb it and it reaches the overlay, then flushed;
//   - sync: 2000 writes of 4 KiB, each waited for, which is a database's commit log.
const cacheWorkload = `#!/bin/sh
ms() { echo $(( $(date +%s%N) / 1000000 )); }
t0=$(ms)
bytes=$(find /usr/lib /usr/share -xdev -type f -size +16k 2>/dev/null | head -n 6000 | xargs cat 2>/dev/null | wc -c)
t1=$(ms)
dd if=/dev/zero of=/var/tmp/seq bs=1M count=256 oflag=direct conv=fsync 2>/dev/null
t2=$(ms)
dd if=/dev/zero of=/var/tmp/sync bs=4k count=2000 oflag=dsync 2>/dev/null
t3=$(ms)
echo "CACHEBENCH read_bytes=$bytes read_ms=$((t1-t0)) seq_ms=$((t2-t1)) sync_ms=$((t3-t2))" > /dev/ttyS0
systemctl poweroff
`

// TestCacheModes boots N machines over one base at once, in each cache mode, and reports what
// the guests measured and what the host paid.
//
// What decides the mode at hundreds of machines is the host's page cache, and the question
// has two halves: whether the chain every machine reads stays shared (one copy for all of
// them), and whether what each machine writes — which no other machine reads — takes host
// memory too, twice over, since the guest caches it as well. Page cache is reclaimable, so
// the cost is not memory running out: it is the shared chain being evicted by it, and the
// writeback of it throttling every writer on the device at once.
//
// Host caches are dropped before each mode (sudo), so every mode starts cold.
func TestCacheModes(t *testing.T) {
	if os.Getenv("SPIN_CACHE_BENCH") == "" {
		t.Skip("set SPIN_CACHE_BENCH=1: this boots dozens of VMs, drops the host's caches and takes minutes")
	}
	if !canSudo() {
		t.Skip("dropping the host's page cache between modes needs sudo -n")
	}
	out := releaseDir(t)
	n := 16
	if s := os.Getenv("SPIN_CACHE_BENCH_VMS"); s != "" {
		if n, _ = strconv.Atoi(s); n < 1 {
			t.Fatalf("SPIN_CACHE_BENCH_VMS=%q", s)
		}
	}

	// On the disk and not in t.TempDir(): O_DIRECT is EINVAL on tmpfs, which /tmp often is.
	work, err := os.MkdirTemp(filepath.Join(out), "cachebench-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })

	// A copy of the base this process owns, read-only to every machine exactly as the
	// release's is: the kernel reports page cache state only for a file the caller owns or
	// could write (see residentMiB), and the release's base is root's.
	base := filepath.Join(work, "base.qcow2")
	mustRun(t, "cp", filepath.Join(out, "image", "rootfs.qcow2"), base)

	// The workload goes in a layer of its own over the base, which is the shape a long-lived
	// VM's disk has — a writable tip over read-only layers over the base — and means it is
	// written once rather than into every machine's overlay.
	probe := filepath.Join(work, "probe.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", base, probe)
	editOverlay(t, probe, variant{
		files: map[string]string{
			"/usr/local/bin/cachebench": cacheWorkload,
			"/etc/systemd/system/cachebench.service": "[Unit]\nAfter=multi-user.target\n[Service]\nType=oneshot\n" +
				"ExecStart=/bin/sh /usr/local/bin/cachebench\n[Install]\nWantedBy=multi-user.target\n",
		},
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/cachebench.service": "/etc/systemd/system/cachebench.service",
		},
	})

	for _, mode := range cacheModes {
		t.Run(mode.label, func(t *testing.T) {
			res := runCacheMode(t, out, work, probe, base, mode, n)
			t.Logf("%s, %d machines:", mode.label, n)
			// The bytes cat counts include the holes of sparse files, which the guest reads
			// as zeros without a request; what came off the device is the base's resident
			// size below — measured 2026-09-11: 1417 MiB counted, 212 MiB of base cached.
			t.Logf("  guest read of the base  %s (%.0f MiB counted by cat, holes included)", spread(res.readMS, "ms"), res.readMiB)
			t.Logf("  guest 256 MiB write     %s", spread(res.seqMS, "ms"))
			t.Logf("  guest 2000 sync writes  %s", spread(res.syncMS, "ms"))
			t.Logf("  host  peak Dirty %.0f MiB, peak page cache growth %.0f MiB, lowest MemAvailable %.0f MiB",
				res.peakDirtyMiB, res.peakCachedMiB, res.minAvailMiB)
			t.Logf("  host  resident: base %.0f MiB after dropping caches, %.0f MiB after; probe layer %.0f MiB, overlays %.0f MiB in total",
				res.baseBeforeMiB, res.baseMiB, res.probeMiB, res.overlaysMiB)
			t.Logf("  wall  %v for all %d", res.wall.Round(time.Second), n)
		})
	}
}

type cacheResult struct {
	readMS, seqMS, syncMS                    []int
	readMiB                                  float64
	peakDirtyMiB, peakCachedMiB, minAvailMiB float64
	baseBeforeMiB                            float64
	baseMiB, probeMiB, overlaysMiB           float64
	wall                                     time.Duration
}

var benchLine = regexp.MustCompile(`CACHEBENCH read_bytes=(\d+) read_ms=(\d+) seq_ms=(\d+) sync_ms=(\d+)`)

func runCacheMode(t *testing.T, out, work, probe, base string, mode cacheMode, n int) cacheResult {
	t.Helper()
	mustRun(t, "sudo", "sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches")
	start := meminfo(t)
	baseBefore := residentMiB(t, base)

	var overlays, consoles []string
	for i := range n {
		o := filepath.Join(work, fmt.Sprintf("%s-%02d.qcow2", mode.label, i))
		mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", probe, o)
		overlays = append(overlays, o)
		consoles = append(consoles, o+".console")
	}

	// Sampled while the machines run: the peaks are what a host of hundreds lives with.
	var res cacheResult
	res.minAvailMiB = start["MemAvailable"]
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			m := meminfo(t)
			res.peakDirtyMiB = max(res.peakDirtyMiB, m["Dirty"])
			res.peakCachedMiB = max(res.peakCachedMiB, m["Cached"]-start["Cached"])
			res.minAvailMiB = min(res.minAvailMiB, m["MemAvailable"])
		}
	})

	t0 := time.Now()
	var cmds []*exec.Cmd
	for i, o := range overlays {
		args := append([]string{"boot", "--release", out, "--disk", o, "--memory", "512", "--cpus", "1",
			"--console", "file:" + consoles[i], "--append", "root=/dev/vda rw init=/sbin/init"}, mode.flags...)
		cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting machine %d: %v", i, err)
		}
		cmds = append(cmds, cmd)
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
	})
	done := make(chan error, len(cmds))
	for _, c := range cmds {
		go func() { done <- c.Wait() }()
	}
	deadline := time.After(10 * time.Minute)
	for range cmds {
		select {
		case <-done:
		case <-deadline:
			t.Fatalf("%s: the machines did not all power off in 10 minutes", mode.label)
		}
	}
	res.wall = time.Since(t0)
	close(stop)
	sampler.Wait()

	for i, c := range consoles {
		b, err := os.ReadFile(c)
		if err != nil {
			t.Fatal(err)
		}
		m := benchLine.FindStringSubmatch(string(b))
		if m == nil {
			t.Fatalf("%s: machine %d printed no result; its console ends:\n%s", mode.label, i, tailOf(string(b)))
		}
		bytes, _ := strconv.Atoi(m[1])
		res.readMiB = float64(bytes) / (1 << 20)
		for j, dst := range []*[]int{&res.readMS, &res.seqMS, &res.syncMS} {
			v, _ := strconv.Atoi(m[j+2])
			*dst = append(*dst, v)
		}
	}
	res.baseBeforeMiB = baseBefore
	res.baseMiB = residentMiB(t, base)
	res.probeMiB = residentMiB(t, probe)
	for _, o := range overlays {
		res.overlaysMiB += residentMiB(t, o)
	}
	return res
}

// residentMiB is how much of a file is in the host's page cache: the one number that says
// whether a file is shared by the machines reading it.
//
// cachestat(2), on a file this process owns. The kernel hides page cache state for a file
// the caller can neither write nor owns, so as not to leak another user's: mincore(2) then
// reports every page resident — 829 MiB of the release's 829 MiB base right after
// drop_caches, when a read of it then took 0.58 s from the disk and 0.07 s the second time
// — and cachestat refuses with EPERM. Hence the benchmark's own copy of the base.
func residentMiB(t *testing.T, path string) float64 {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a file this test made or the release's own
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// struct cachestat_range { __u64 off, len; }, len 0 meaning to the end of the file, and
	// struct cachestat { __u64 nr_cache, nr_dirty, nr_writeback, nr_evicted,
	// nr_recently_evicted; } — both from include/uapi/linux/mman.h.
	const sysCachestat = 451
	var rng [2]uint64
	var st [5]uint64
	if _, _, e := syscall.Syscall6(sysCachestat, f.Fd(), uintptr(unsafe.Pointer(&rng)),
		uintptr(unsafe.Pointer(&st)), 0, 0, 0); e != 0 {
		t.Fatalf("cachestat %s: %v", path, e)
	}
	return float64(st[0]*uint64(os.Getpagesize())) / (1 << 20)
}

// meminfo is /proc/meminfo in MiB.
func meminfo(t *testing.T) map[string]float64 {
	t.Helper()
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]float64{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 {
			kb, _ := strconv.ParseFloat(fields[1], 64)
			out[strings.TrimSuffix(fields[0], ":")] = kb / 1024
		}
	}
	return out
}

// spread is min / median / max of a column.
func spread(v []int, unit string) string {
	s := append([]int(nil), v...)
	sort.Ints(s)
	return fmt.Sprintf("min %d %s, median %d %s, max %d %s", s[0], unit, s[len(s)/2], unit, s[len(s)-1], unit)
}

func tailOf(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}
