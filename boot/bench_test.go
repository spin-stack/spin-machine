// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/boot"
)

// variant is one configuration to boot, and the whole point is that two of them differ in
// exactly one thing.
type variant struct {
	label  string
	cpus   string
	memory string
	extra  string            // appended to the kernel command line
	mask   []string          // units masked by writing into this boot's own overlay
	files  map[string]string // written into the overlay: path under / -> content
}

// Masks are written into the overlay and never passed as `systemd.mask=`. That parameter is
// implemented by systemd-debug-generator, which optimize-systemd.sh symlinks to /dev/null,
// so it parses, reaches /proc/cmdline and does nothing: verified 2026-09-10 with
// `systemd.mask=chrony.service` on the command line and `systemctl is-active chrony`
// answering `active`. Everything this repository concluded from that parameter was concluded
// from a boot in which nothing had been masked.
var udevUnits = []string{
	"systemd-udevd.service",
	"systemd-udevd-control.socket",
	"systemd-udevd-kernel.socket",
	"systemd-udev-trigger.service",
}

// A drop-in on the instance beats one on the template, which is where the image's own TERM
// drop-in lives, so these override it for one boot without touching the image.
func gettyDropin(body string) map[string]string {
	return map[string]string{
		"/etc/systemd/system/serial-getty@ttyS0.service.d/zz-bench.conf": "[Service]\n" + body,
	}
}

// A SECOND OF THE `usable` COLUMN IS A sleep(1) INSIDE agetty, and it cannot be configured
// away. Settled 2026-09-10 by strace'ing agetty in the guest:
//
//	read(4, "", 4096) = 0
//	close(4)          = 0
//	clock_nanosleep(CLOCK_REALTIME, 0, {tv_sec=1, tv_nsec=0}, …) = 0
//	ioctl(0, TCFLSH, TCIFLUSH) = -1 EIO
//
// It is util-linux's "let the line settle, then flush the input queue" hack, taken on any
// line agetty does not consider a virtual console. Every plausible switch was measured
// against it over 7-9 boots each and every one landed within noise of 1250 ms: Type=idle
// removed, TTYReset=no, TERM=dumb for the getty, the baud list dropped, --keep-baud dropped,
// --noissue, --local-line (so it is not carrier detect, which was the standing guess) and
// --delay 0 (which is a *different* sleep, not this one). Replacing agetty with a shell that
// echoes and waits prints at 400 ms, which is what proves the second is agetty's own and not
// the tty handoff or the console.
//
// So the honest reading of `usable` is: subtract a second. A machine is a working Linux
// system about 250 ms after it is launched, and then agetty sleeps. Nothing a workspace does
// waits for agetty — it is the interactive console login and nothing else — so this is a
// number to know rather than a thing to fix.
//
// An empty ExecStart= is required before a new one: a drop-in appends otherwise, and the unit
// would run two gettys on the one line.
const gettyEcho = "ExecStart=\nExecStart=-/bin/sh -c 'echo SPIN-READY-login: ; sleep infinity'\n"

// Everything below replaces agetty with an echo, because its unconditional sleep(1) is
// larger than everything being compared and would hide all of it. `as shipped` is the one
// row that keeps agetty, so the difference between it and `baseline` is that second.
func without(units ...string) variant {
	return variant{cpus: "2", memory: "2048", mask: units, files: gettyDropin(gettyEcho)}
}

func labelled(l string, v variant) variant { v.label = l; return v }

var variants = []variant{
	{label: "as shipped", cpus: "2", memory: "2048"},
	{label: "baseline", cpus: "2", memory: "2048", files: gettyDropin(gettyEcho)},
	// The critical chain into multi-user.target, measured 2026-09-10:
	//
	//   multi-user.target @175ms
	//   └─systemd-logind.service @111ms +63ms
	//     └─basic.target @102ms
	//       └─dbus-broker.service @151ms +7ms
	//         └─sysinit.target @96ms
	//           └─systemd-udev-trigger.service @60ms +35ms
	//             └─system.slice @34ms
	//
	// logind is the tail and the most expensive single unit; chrony is the other one that
	// blame puts in the tens of milliseconds. Both are rows rather than deletions because a
	// unit on the critical chain does not always give its time back when removed — something
	// else becomes the tail.
	labelled("sin logind", without("systemd-logind.service")),
	labelled("sin chrony", without("chrony.service")),
	labelled("sin ambos", without("systemd-logind.service", "chrony.service")),
	labelled("udev masked", without(udevUnits...)),
}

// TestBootCost boots each variant many times, interleaved, and prints what each phase cost.
//
// Interleaved (A B C A B C …) rather than in blocks, because the host is shared with
// whatever else is running on it: a block schedule gives one variant all of a slow minute
// and reads it as a regression. Every variant sees the same minutes.
//
// It is a test rather than a script for the reason the shell version failed — its own
// per-line stamping loop was slow enough to throttle QEMU's console writes, and reported a
// boot at 1424 ms that the guest inside it reported at 190 ms. The part that can be wrong
// that way is boot.Watch, and it has tests that do not need a VM.
func TestBootCost(t *testing.T) {
	if os.Getenv("SPIN_BOOT_BENCH") == "" {
		t.Skip("set SPIN_BOOT_BENCH=1: this boots dozens of VMs and takes minutes")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 20)

	if !canSudo() {
		for _, v := range variants {
			if len(v.mask) > 0 {
				t.Skipf("masking a unit means writing into the overlay through qemu-nbd, "+
					"which needs sudo; %q cannot be measured here", v.label)
			}
		}
	}

	samples := map[string]map[boot.Phase][]time.Duration{}
	failures := map[string]int{}
	for rep := range reps {
		for _, v := range variants {
			run := bootOnce(t, out, v)
			if samples[v.label] == nil {
				samples[v.label] = map[boot.Phase][]time.Duration{}
			}
			for _, p := range []boot.Phase{boot.Firmware, boot.Kernel, boot.PID1, boot.Started, boot.Usable} {
				if d, ok := run.At[p]; ok {
					samples[v.label][p] = append(samples[v.label][p], d)
				}
			}
			// Not reaching a login is the failure this exists to catch, and it is not the
			// same as being slow. A variant that never gets there has no p50 worth reading.
			if !run.Reached(boot.Usable) {
				failures[v.label]++
				if failures[v.label] == 1 {
					t.Logf("%s: no login prompt on rep %d; console tail:\n%s",
						v.label, rep+1, tail(run.Output, 800))
				}
			}
		}
	}

	labels := make([]string, 0, len(samples))
	for l := range samples {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	var b strings.Builder
	fmt.Fprintf(&b, "\nmilliseconds from the moment before QEMU is exec'd, p50/p95 over %d boots\n\n", reps)
	fmt.Fprintf(&b, "%-14s %10s %10s %10s %10s %10s %8s\n", "CONFIGURATION", "FIRMWARE", "KERNEL", "PID1", "STARTED", "USABLE", "NO LOGIN")
	for _, l := range labels {
		fmt.Fprintf(&b, "%-14s", l)
		for _, p := range []boot.Phase{boot.Firmware, boot.Kernel, boot.PID1, boot.Started, boot.Usable} {
			fmt.Fprintf(&b, " %10s", cell(samples[l][p]))
		}
		fmt.Fprintf(&b, " %8d\n", failures[l])
	}
	t.Log(b.String())
}

// TestBootTrace boots once and prints the console with the host's clock against it, which is
// the only way to see a gap that belongs to neither systemd nor the kernel.
//
// It exists because the guest and the host disagreed about a whole second: systemd reported
// agetty started at 171 ms of guest-monotonic time and the host did not see a login prompt
// until 1247 ms. Both numbers were right; what was between them was not visible from either
// side alone.
func TestBootTrace(t *testing.T) {
	if os.Getenv("SPIN_BOOT_TRACE") == "" {
		t.Skip("set SPIN_BOOT_TRACE=1 to print one boot's console against the host clock")
	}
	out := releaseDir(t)

	body := "ExecStartPre=/bin/sh -c 'echo SPIN-GETTY-EXEC > /dev/console'\n"
	// SPIN_GETTY_EXEC replaces agetty itself, which is how to find out whether a delay
	// belongs to agetty or to the tty it is handed. Substituting a shell that prints and
	// waits separates the two in one boot:
	//
	//   SPIN_GETTY_EXEC="/bin/sh -c 'echo SPIN-READY-login: ; sleep infinity'"
	if x := os.Getenv("SPIN_GETTY_EXEC"); x != "" {
		body += "ExecStart=\nExecStart=-" + x + "\n"
	}
	v := variant{label: "trace", cpus: "2", memory: "2048", files: map[string]string{
		// A marker written the moment systemd starts the service, so the console itself says
		// which side of the gap the exec falls on.
		"/etc/systemd/system/serial-getty@ttyS0.service.d/trace.conf": "[Service]\n" + body,
	}}
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)

	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), "boot",
		"--release", out, "--disk", overlay, "--memory", v.memory, "--cpus", v.cpus,
		"--console", "file:/dev/stdout",
		"--append", "root=/dev/vda rw init=/sbin/init loglevel=7 systemd.show_status=true "+
			"systemd.log_level=info systemd.log_target=console")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	kill := func() {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	timer := time.AfterFunc(30*time.Second, kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	// Assembled into lines, each stamped with the arrival of its *first* byte, because a
	// serial console delivers a byte at a time: a first version printed one row per read and
	// every row was a single character, so no row ever contained a whole marker to look for.
	// The first byte is the interesting one — it is when the writer had something to say.
	var b strings.Builder
	var line []byte
	var lineAt time.Duration
	flush := func() {
		if len(line) == 0 {
			return
		}
		fmt.Fprintf(&b, "%8.1fms  %s\n", float64(lineAt.Microseconds())/1000, strings.TrimRight(string(line), "\r\n"))
		line = line[:0]
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			at := time.Since(t0)
			for _, c := range buf[:n] {
				if len(line) == 0 {
					lineAt = at
				}
				line = append(line, c)
				if c == '\n' {
					flush()
				}
			}
			if strings.Contains(string(line), "login:") {
				flush()
				break
			}
		}
		if err != nil {
			break
		}
	}
	flush()
	t.Log("\n" + b.String())
}

func cell(ds []time.Duration) string {
	p50, ok := boot.Percentile(ds, 0.5)
	if !ok {
		return "—"
	}
	p95, _ := boot.Percentile(ds, 0.95)
	return fmt.Sprintf("%d/%d", p50.Milliseconds(), p95.Milliseconds())
}

// bootOnce runs one boot over a throwaway overlay and returns when the machine powers off or
// the timeout expires.
//
// The overlay is the safe thing and the honest one: it is the shape a workspace's disk has,
// and the base image — which every VM maps read-only, many at once — cannot be touched.
func bootOnce(t *testing.T, out string, v variant) boot.Run {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	if len(v.mask) > 0 || len(v.files) > 0 {
		editOverlay(t, overlay, v)
	}

	cmdline := "root=/dev/vda rw init=/sbin/init"
	if os.Getenv("DEBUG") == "1" {
		// Every message is a write to a serial port, and that write is inside the wall
		// clock being measured. On by choice, off by default, and the same in every variant
		// of a run so a comparison still holds while the absolute is inflated.
		cmdline += " loglevel=7 systemd.show_status=true systemd.log_level=info systemd.log_target=console"
	}
	if v.extra != "" {
		cmdline += " " + v.extra
	}

	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), "boot",
		"--release", out, "--disk", overlay, "--memory", v.memory, "--cpus", v.cpus,
		"--console", "file:/dev/stdout", "--append", cmdline)
	// Its own process group, so the machine can be taken down as a whole. spin-machine execs
	// QEMU as a child and killing the parent leaves the child running: the first version of
	// this left one `qemu-system-x86_64` per boot alive, each still holding an overlay open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	cmd.Stderr = nil

	// t0 before Start, so exec'ing QEMU and the firmware are inside the measurement. They
	// are what the old harness left out at this end.
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	kill := func() {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	// The cap is for a machine that hangs. The normal path is Watch returning at the login
	// prompt, and then this kills a machine that is working perfectly — which is the point:
	// nothing after the phase being measured is being measured.
	timer := time.AfterFunc(70*time.Second, kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	run, err := boot.Watch(stdout, t0, time.Now, boot.Usable)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	return run
}

// editOverlay applies a variant's masks and drop-ins to its own overlay, which is what
// changing a unit for one boot actually takes: /etc is the highest-priority place a mask or a
// drop-in can live and no kernel parameter reaches it here.
func editOverlay(t *testing.T, overlay string, v variant) {
	t.Helper()
	const dev = "/dev/nbd0"
	mustRun(t, "sudo", "modprobe", "nbd", "max_part=8")
	mustRun(t, "sudo", "qemu-nbd", "--connect="+dev, "-f", "qcow2", overlay)
	defer mustRun(t, "sudo", "qemu-nbd", "--disconnect", dev)

	// The kernel needs a moment to read the partition table it will not find: this image is
	// partitionless, so the device itself is the filesystem.
	var ok bool
	for range 20 {
		if exec.Command("sudo", "blkid", dev).Run() == nil {
			ok = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("%s never showed a filesystem", dev)
	}

	mnt := t.TempDir()
	mustRun(t, "sudo", "mount", dev, mnt)
	defer mustRun(t, "sudo", "umount", mnt)
	for _, u := range v.mask {
		mustRun(t, "sudo", "ln", "-sf", "/dev/null",
			filepath.Join(mnt, "etc/systemd/system", u))
	}
	for path, content := range v.files {
		dst := filepath.Join(mnt, path)
		mustRun(t, "sudo", "mkdir", "-p", filepath.Dir(dst))
		// Through `sudo tee` rather than os.WriteFile: the mount is root's, and a test that
		// silently wrote nothing would show up as "the drop-in changed nothing".
		cmd := exec.Command("sudo", "tee", dst)
		cmd.Stdin = strings.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("writing %s: %v\n%s", path, err, out)
		}
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func canSudo() bool { return exec.Command("sudo", "-n", "true").Run() == nil }

func releaseDir(t *testing.T) string {
	t.Helper()
	out, err := filepath.Abs(filepath.Join("..", "_output"))
	if err != nil {
		t.Fatalf("locating the release: %v", err)
	}
	for _, f := range []string{
		"bin/spin-machine", "bin/qemu-img", "kernel/vmlinux", "image/rootfs.qcow2",
	} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Skipf("no %s in %s — run: task build", f, out)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm: these numbers are only meaningful under KVM")
	}
	return out
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive number", name, v)
	}
	return n
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
