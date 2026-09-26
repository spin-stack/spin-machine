// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Where the kernel's own boot goes, initcall by initcall.
//
// The kernel is the largest single block in a cold boot — far larger than the firmware and
// larger than everything before it put together — and until this existed there was no
// breakdown of it, only a total. What made that hard is that the obvious way to get one is
// wrong: `initcall_debug` with the messages on the console puts a serial write, and a VM
// exit, inside every interval it reports. A gap between two adjacent timestamps then
// includes the cost of printing the line before it, and the numbers come out inflated in
// proportion to how much is being printed — which is exactly the thing being measured.
//
// So the console stays quiet for the whole boot and the ring buffer is read afterwards.
// `quiet` raises the console's threshold and does not touch what printk stores, and
// initcall_debug's two lines are KERN_DEBUG printks from init/main.c's trace callbacks, so
// they are in the buffer either way. A unit ordered after multi-user.target then prints the
// buffer once, after the part being measured is over.
//
// One boot is not a measurement. The same machine on the same host read 59.5, 96.8 and
// 167.7 ms for the kernel's own boot in three consecutive runs, because the host was busy,
// so every number here is a p50 over REPS boots and the p95 is printed beside it to say how
// settled it is. A wide spread means the host was shared, not that the kernel changed.
//
// Variants are interleaved rather than run in blocks, for the reason TestBootCost is: a
// block schedule hands one variant a slow minute and reads it as a difference.
//
//	SPIN_INITCALL_PROBE=1   run at all
//	REPS=<n>                boots per variant (default 10)
//	TOP=<n>                 how many initcalls to print (default 30)
//	SPIN_KERNEL_B=<vmlinux> adds a variant booting this kernel instead of the release's,
//	                        which is how a config change is compared without two runs on a
//	                        host that is not the same host from one minute to the next
func TestKernelInitcalls(t *testing.T) {
	if os.Getenv("SPIN_INITCALL_PROBE") == "" {
		t.Skip("set SPIN_INITCALL_PROBE=1: boots a VM and needs sudo to write into its overlay")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	top := envInt(t, "TOP", 30)
	reps := envInt(t, "REPS", 10)

	// Redirected to /dev/ttyS0 inside the shell, with StandardOutput=null, and that detail
	// is the whole measurement.
	//
	// The obvious form — StandardOutput=journal+console — routes the dump through the
	// journal, which forwards it to the kernel log, so every reprinted line arrives on the
	// console carrying a *second*, fresh stamp:
	//
	//	[    0.199752] sh[106]: [    0.007375] IOAPIC[0]: apic_id 0, version 17, …
	//
	// A parser reading the first stamp on the line then reads the clock of the dump instead
	// of the clock of the boot. It does not look like a failure: the initcall durations are
	// still right, because those are text, and every number derived from a timestamp is
	// silently describing how long the console took to print a megabyte. It reported the
	// kernel reaching "Freeing unused kernel image" at 931 ms on a machine that boots in
	// about 200, which is the only reason it was caught.
	const dump = `[Unit]
Description=Print the kernel ring buffer for the initcall probe
After=multi-user.target
[Service]
Type=oneshot
StandardOutput=null
ExecStart=/bin/sh -c "{ echo SPIN-DMESG-BEGIN; dmesg; echo SPIN-DMESG-END; } > /dev/ttyS0"
[Install]
WantedBy=multi-user.target
`
	addKernelVariant(t)

	base := variant{
		cpus: "2", memory: "2048",
		files: map[string]string{
			"/etc/systemd/system/spin-dmesg.service": dump,
		},
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/spin-dmesg.service": "/etc/systemd/system/spin-dmesg.service",
		},
	}

	runs := map[string][]parsed{}
	for i := range reps {
		for _, cv := range cmdlineVariants {
			v := base
			v.label = cv.label
			// log_buf_len because initcall_debug is two lines per initcall and the default
			// ring wraps: a wrapped buffer loses the early initcalls, which are the
			// interesting ones. 2M and not 8M — 8M allocates 37 MB and cost 4.9 ms of the
			// boot being measured.
			v.extra = strings.TrimSpace("initcall_debug log_buf_len=2M " + cv.extra)
			console := bootUntil(t, out, v, cv.kernel, "SPIN-DMESG-END", 90*time.Second)
			// The raw buffer of the first boot, for a question this report does not answer
			// yet. Written only when asked: a test that drops a megabyte in the working
			// directory on every run is a test people stop running.
			if f := os.Getenv("SPIN_CONSOLE_OUT"); f != "" && i == 0 && cv.label == cmdlineVariants[0].label {
				if err := os.WriteFile(f, []byte(console), 0o644); err != nil {
					t.Fatalf("writing the console to %s: %v", f, err)
				}
				t.Logf("raw console of the first boot written to %s", f)
			}
			runs[cv.label] = append(runs[cv.label], parse(t, console))
		}
	}
	compare(t, runs)
	report(t, runs[cmdlineVariants[0].label], top, reps)
}

// bootUntil boots one machine and returns its console once marker has appeared.
//
// Separate from bootOnce because that one stops at a login prompt, which is the right place
// to stop when a boot time is what is wanted and the wrong one here: everything this reads
// is printed after it.
func bootUntil(t *testing.T, out string, v variant, kernel, marker string, timeout time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)

	cmdline := "root=/dev/vda rw init=/sbin/init"
	if v.extra != "" {
		cmdline += " " + v.extra
	}
	args := []string{"boot", "--release", out, "--disk", overlay,
		"--memory", v.memory, "--cpus", v.cpus,
		"--console", "file:/dev/stdout", "--append", cmdline}
	if kernel != "" {
		args = append(args, "--kernel", kernel)
	}
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	_ = w.Close()
	defer func() {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
		_ = r.Close()
	}()

	deadline := time.Now().Add(timeout)
	var b strings.Builder
	buf := make([]byte, 65536)
	for {
		if err := r.SetReadDeadline(deadline); err != nil {
			t.Fatalf("setting the console deadline: %v", err)
		}
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if strings.Contains(b.String(), marker) {
			return b.String()
		}
		if err != nil {
			t.Fatalf("never saw %q in %s; console tail:\n%s",
				marker, timeout, tail([]byte(b.String()), 2000))
		}
	}
}

var (
	// [    0.123456] initcall acpi_init+0x0/0x1a0 returned 0 after 1234 usecs
	reDone = regexp.MustCompile(`initcall ([^ ]+) returned (-?\d+) after (\d+) usecs`)
	// [    1.234567] Freeing unused kernel image (initmem) memory: 2780K
	reStamp = regexp.MustCompile(`^\[\s*(\d+)\.(\d{6})\]`)
	// The same thing again further along the line, which means the dump was re-stamped on
	// its way out and the first stamp belongs to the dump rather than to the boot.
	reDouble = regexp.MustCompile(`^\[\s*\d+\.\d{6}\].*\[\s*\d+\.\d{6}\]`)
	// A process id in a userspace line's prefix — `systemd-journald[62]:`. Gaps are keyed by
	// the message on the far side of them, and a pid in that key splits one gap across as
	// many buckets as the boot happened to hand out pids: the largest gap in this machine's
	// boot, kernel handover to journald's first line, came out as two entries with half the
	// samples each and no warning that it had.
	rePID = regexp.MustCompile(`\[\d+\]:`)
)

type initcall struct {
	name string
	us   int
	ret  int
}

// gap is the time between two adjacent ring-buffer stamps, and what was printed on either
// side of it. The messages are what make it usable: a duration on its own says there is
// 300 ms somewhere and not what the kernel was doing.
type gap struct {
	ms            float64
	after, before string
}

func clip(s string) string {
	if len(s) > 88 {
		return s[:88]
	}
	return s
}

// The command lines to compare. The first is the baseline and the one the per-initcall
// table below is taken from.
//
// Boot parameters only, because they cost nothing: a kernel config change invalidates every
// template in existence, so it is worth knowing whether the saving is there at all before
// anybody pays for it. thash_entries and uhash_entries size the TCP and UDP hash tables,
// which inet_init allocates — 8.7 ms of initcall time, with another 8.2 ms of gap around
// "IP idents hash table entries: 32768 (order: 6, 262144 bytes, linear)" on a machine that
// will never hold 32768 connections.
type cmdlineVariant struct{ label, extra, kernel string }

var cmdlineVariants = []cmdlineVariant{
	{label: "baseline"},
	{label: "small hashes", extra: "thash_entries=2048 uhash_entries=2048"},
}

// A second kernel, if one was built. Not a hard-coded path: an experimental vmlinux is not
// in this repository and a variant naming a file nobody has is a variant that fails for
// everyone who did not build it.
func addKernelVariant(t *testing.T) {
	t.Helper()
	k := os.Getenv("SPIN_KERNEL_B")
	if k == "" {
		return
	}
	abs, err := filepath.Abs(k)
	if err != nil {
		t.Fatalf("resolving SPIN_KERNEL_B=%q: %v", k, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("SPIN_KERNEL_B=%s: %v", abs, err)
	}
	cmdlineVariants = append(cmdlineVariants, cmdlineVariant{label: "kernel B", kernel: abs})
}

// initcallsOfInterest are printed side by side for every variant, because a variant that
// moved the total is only interesting once it is clear which initcall moved.
var initcallsOfInterest = []string{
	"acpi_init", "inet_init", "ksm_init", "hugepage_init", "kcompactd_init",
	"virtio_pci_driver_init", "virtio_blk_init",
}

// compare prints one row per variant, and is the only part of this test that answers a
// question of the form "is it worth changing".
func compare(t *testing.T, runs map[string][]parsed) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-14s %10s %10s %10s   %s\n", "VARIANT", "FREEING", "INITCALLS", "OUTSIDE", "(p50 ms)")
	for _, cv := range cmdlineVariants {
		rs := runs[cv.label]
		freeing, totals, outside := make([]float64, 0, len(rs)), make([]float64, 0, len(rs)), make([]float64, 0, len(rs))
		for _, r := range rs {
			totals = append(totals, float64(r.total)/1000)
			if r.freeing > 0 {
				freeing = append(freeing, r.freeing*1000)
				outside = append(outside, r.freeing*1000-float64(r.total)/1000)
			}
		}
		sort.Float64s(freeing)
		sort.Float64s(totals)
		sort.Float64s(outside)
		fmt.Fprintf(&b, "%-14s %10.1f %10.1f %10.1f\n", cv.label,
			pct(freeing, 50), pct(totals, 50), pct(outside, 50))
	}

	fmt.Fprintf(&b, "\n%-26s", "INITCALL (p50 ms)")
	for _, cv := range cmdlineVariants {
		fmt.Fprintf(&b, " %14s", cv.label)
	}
	fmt.Fprintln(&b)
	for _, name := range initcallsOfInterest {
		fmt.Fprintf(&b, "%-26s", name)
		for _, cv := range cmdlineVariants {
			var xs []float64
			for _, r := range runs[cv.label] {
				for n, us := range r.calls {
					if strings.HasPrefix(n, name+"+") {
						xs = append(xs, float64(us)/1000)
					}
				}
			}
			sort.Float64s(xs)
			if len(xs) == 0 {
				fmt.Fprintf(&b, " %14s", "-")
				continue
			}
			fmt.Fprintf(&b, " %14.2f", pct(xs, 50))
		}
		fmt.Fprintln(&b)
	}
	t.Log(b.String())
}

// parsed is one boot's ring buffer, reduced.
type parsed struct {
	calls   map[string]int // initcall -> usecs
	gaps    map[string]gap // "before" message -> the gap ahead of it
	total   int            // usecs in all initcalls
	freeing float64        // seconds to "Freeing unused kernel image"
}

func parse(t *testing.T, console string) parsed {
	t.Helper()
	out := parsed{calls: map[string]int{}, gaps: map[string]gap{}}
	var prev float64 = -1
	var prevMsg string
	for _, line := range strings.Split(console, "\n") {
		if reDouble.MatchString(line) {
			t.Fatalf("a console line carries two kernel stamps, so the dump is being "+
				"re-stamped on its way out and every timing here would be the console's "+
				"and not the boot's:\n%s", strings.TrimSpace(line))
		}
		if m := reStamp.FindStringSubmatch(line); m != nil {
			sec, _ := strconv.Atoi(m[1])
			us, _ := strconv.Atoi(m[2])
			at := float64(sec) + float64(us)/1e6
			msg := clip(rePID.ReplaceAllString(strings.TrimSpace(line[len(m[0]):]), "[]:"))
			// Monotonic only: the dump reprints the buffer, and a reprint that went
			// backwards would produce a negative gap and sort to the top.
			if prev >= 0 && at >= prev {
				out.gaps[msg] = gap{ms: (at - prev) * 1000, after: prevMsg, before: msg}
			}
			prev, prevMsg = at, msg
			if strings.Contains(line, "Freeing unused kernel image") {
				out.freeing = at
			}
		}
		if m := reDone.FindStringSubmatch(line); m != nil {
			us, _ := strconv.Atoi(m[3])
			out.calls[m[1]] = us
			out.total += us
		}
	}
	if len(out.calls) == 0 {
		t.Fatalf("no initcall lines in the ring buffer; was initcall_debug on the command "+
			"line and log_buf_len large enough?\nconsole tail:\n%s",
			tail([]byte(console), 2000))
	}
	return out
}

// report reduces every boot to a p50 and a p95, which is the only form these numbers are
// worth reading in. It prints two totals, and the second is the one that is easy to forget
// and usually the larger: memory init, SMP bringup, RCU and the scheduler are not initcalls,
// and no amount of making drivers modular reaches them.
func report(t *testing.T, runs []parsed, top, reps int) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\nkernel boot, p50/p95 over %d boots\n\n", reps)

	totals := make([]float64, 0, len(runs))
	freeings := make([]float64, 0, len(runs))
	nonInit := make([]float64, 0, len(runs))
	for _, r := range runs {
		totals = append(totals, float64(r.total)/1000)
		if r.freeing > 0 {
			freeings = append(freeings, r.freeing*1000)
			nonInit = append(nonInit, r.freeing*1000-float64(r.total)/1000)
		}
	}
	line := func(what string, xs []float64) {
		sort.Float64s(xs)
		fmt.Fprintf(&b, "  %-44s %8.1f %8.1f ms\n", what, pct(xs, 50), pct(xs, 95))
	}
	fmt.Fprintf(&b, "  %-44s %8s %8s\n", "", "P50", "P95")
	line("kernel start to \"Freeing unused kernel image\"", freeings)
	line("in initcalls", totals)
	line("not in initcalls", nonInit)
	fmt.Fprintf(&b, "\n  %d initcalls seen\n\n", countUnion(runs))

	// Gaps, which is where the time that is not in an initcall actually is. Only
	// trustworthy because the console was quiet for the whole boot: with the messages going
	// to a serial port, a gap between two adjacent stamps is mostly the cost of printing
	// the first one, and the biggest gap is wherever the most was printed.
	type agg struct {
		g  gap
		ms []float64
	}
	byBefore := map[string]*agg{}
	for _, r := range runs {
		for k, g := range r.gaps {
			a := byBefore[k]
			if a == nil {
				a = &agg{g: g}
				byBefore[k] = a
			}
			a.ms = append(a.ms, g.ms)
		}
	}
	ranked := make([]*agg, 0, len(byBefore))
	for _, a := range byBefore {
		sort.Float64s(a.ms)
		ranked = append(ranked, a)
	}
	sort.Slice(ranked, func(i, j int) bool { return pct(ranked[i].ms, 50) > pct(ranked[j].ms, 50) })
	fmt.Fprintf(&b, "  %8s %8s  %s\n", "GAP P50", "P95", "BETWEEN (ms)")
	for i, a := range ranked {
		if i >= top/2 {
			break
		}
		fmt.Fprintf(&b, "  %8.1f %8.1f  after:  %s\n  %8s %8s  before: %s\n",
			pct(a.ms, 50), pct(a.ms, 95), a.g.after, "", "", a.g.before)
	}
	fmt.Fprintln(&b)

	// Initcalls, ranked by their own p50.
	type ic struct {
		name string
		ms   []float64
	}
	byName := map[string]*ic{}
	for _, r := range runs {
		for n, us := range r.calls {
			c := byName[n]
			if c == nil {
				c = &ic{name: n}
				byName[n] = c
			}
			c.ms = append(c.ms, float64(us)/1000)
		}
	}
	calls := make([]*ic, 0, len(byName))
	for _, c := range byName {
		sort.Float64s(c.ms)
		calls = append(calls, c)
	}
	sort.Slice(calls, func(i, j int) bool { return pct(calls[i].ms, 50) > pct(calls[j].ms, 50) })

	fmt.Fprintf(&b, "  %8s %8s  %s\n", "P50", "P95", "INITCALL")
	var all float64
	for _, c := range calls {
		all += pct(c.ms, 50)
	}
	for i, c := range calls {
		if i < top {
			fmt.Fprintf(&b, "  %8.2f %8.2f  %s\n", pct(c.ms, 50), pct(c.ms, 95), c.name)
		}
	}
	for _, n := range []int{10, 25, 50} {
		if n > len(calls) {
			continue
		}
		var c float64
		for i := 0; i < n; i++ {
			c += pct(calls[i].ms, 50)
		}
		fmt.Fprintf(&b, "\n  top %d initcalls: %.1f ms (%.0f%% of all initcall time)", n, c, 100*c/all)
	}
	t.Log(b.String() + "\n")
}

func countUnion(runs []parsed) int {
	seen := map[string]bool{}
	for _, r := range runs {
		for n := range r.calls {
			seen[n] = true
		}
	}
	return len(seen)
}
