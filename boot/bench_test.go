// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	label   string
	cpus    string
	memory  string
	extra   string            // appended to the kernel command line
	mask    []string          // units masked by writing into this boot's own overlay
	files   map[string]string // written into the overlay: path under / -> content
	links   map[string]string // symbolic links made in the overlay: path under / -> target
	kernel  string            // a kernel other than the release's, for comparing configs
	profile bool              // boot with `--profile`: initcall profiling, console silent
	setup   string            // shell run with the overlay mounted, $MNT its root
	flags   []string          // spin-machine boot flags after the rest: the feature matrix's axes
	timeout time.Duration     // how long a boot may take before it is a hang; 70 s when zero
}

// Masks are written into the overlay and never passed as `systemd.mask=`. That parameter is
// implemented by systemd-debug-generator, which optimize-systemd.sh symlinks to /dev/null,
// so it parses, reaches /proc/cmdline and does nothing: verified 2026-09-10 with
// `systemd.mask=chrony.service` on the command line and `systemctl is-active chrony`
// answering `active`. Anything concluded from that parameter is concluded from a boot in
// which nothing was masked.
// Rule files for hardware this machine cannot have. udev reads 49 of them and evaluates
// them against the 266 devices it coldplugs; 32 reference cdrom, drm, input, alsa, hidraw,
// tape, v4l, cameras, joysticks, mice, touchpads, graphics and sound cards, or a ProLiant's
// power switch. They arrive with the udev package, not with anything this image asked for.
//
// Conservative on purpose. Not here and not maskable: 60-block and 60-persistent-storage
// (vda), 60-serial (ttyS0), 71-seat (logind), the net-naming rules, 50-udev-default,
// 80-debian-compat and 99-systemd. Masking one of those is a machine that does not boot or
// a disk with no by-uuid link, and the point of the row is the ones that cannot matter.
var impossibleRules = []string{
	"60-cdrom_id.rules",
	"60-drm.rules",
	"60-evdev.rules",
	"60-fido-id.rules",
	"60-gpiochip.rules",
	"60-persistent-alsa.rules",
	"60-persistent-hidraw.rules",
	"60-persistent-input.rules",
	"60-persistent-storage-tape.rules",
	"60-persistent-v4l.rules",
	"60-sensor.rules",
	"61-persistent-storage-android.rules",
	"70-camera.rules",
	"70-joystick.rules",
	"70-mouse.rules",
	"70-touchpad.rules",
	"71-power-switch-proliant.rules",
	"78-graphics-card.rules",
	"78-sound-card.rules",
}

// maskedRules is what udev's own override mechanism is: a symlink to /dev/null in
// /etc/udev/rules.d shadows the file of the same name in /usr/lib/udev/rules.d, exactly as
// it works for units.
func maskedRules() map[string]string {
	m := map[string]string{}
	for _, r := range impossibleRules {
		m["/etc/udev/rules.d/"+r] = "/dev/null"
	}
	return m
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

func withFile(m map[string]string, path, content string) map[string]string {
	m[path] = content
	return m
}

var variants = []variant{
	// The image as it is, with a real agetty rather than the echo every row below uses. It
	// reads about a second slower and that is agetty's own sleep, not a fault to hunt: the
	// terminfo timeout found on spin-machine-console.service was tried here too and
	// TERM=dumb on this unit changes nothing (1229 against 1232, 12 boots, 2026-09-26).
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
	// logind is the tail and the most expensive single unit. It is a row rather than a
	// deletion because a unit on the critical chain does not always give its time back when
	// removed — something else becomes the tail. Which is exactly what happened, 15 boots
	// each, p50/p95 to a usable machine:
	//
	//     baseline    262/296        sin logind   238/247
	//     sin chrony  252/287        sin ambos    233/260
	//
	// logind is +63 ms on the chain and worth 24 ms to remove; chrony was 43 ms in blame and
	// worth 10 ms; the two together were worth 29 and not 34, because they overlap. Read
	// `systemd-analyze blame` as a list of suspects, never as a list of savings.
	//
	// The chrony rows are kept as the record of what removing it bought, and cannot be run
	// again: there is no time daemon in the image since 2026-09-10 (see image/mkosi.conf).
	labelled("sin logind", without("systemd-logind.service")),
	// The inverse of what the image ships: logind put back into the boot transaction, which
	// is the configuration this replaced. Since 2026-09-12 the want is overridden with a
	// /dev/null symlink and the first login starts logind over its Varlink socket instead.
	// 25 boots of each, same run, measured that day:
	//
	//     as shipped, deferred      248/275      logind at boot (this row)   272/294
	//     deferred, no drop-in      244/264      at boot, with the drop-in   282/322
	//     masked outright           241/265
	//
	// So deferring it is worth 24 ms, of which the drop-in gives 4 back for its fork, and
	// masking it outright — which breaks every login — would be worth 7 more.
	//
	// This row neutralises the drop-in as well, and that is the whole reason it is written
	// the long way instead of just restoring the want. With the drop-in left in place the
	// same comparison reads 43 ms, because a service is implicitly ordered after the socket
	// that triggers it: logind starting at boot then waits for an ExecStartPre fork that the
	// shipped machine never puts on any path, and the row flatters the change by 19 ms.
	//
	// What makes the deferral possible is that drop-in rather than anything here:
	// pam_systemd decides whether to register a session by calling logind_running(), which
	// is access("/run/systemd/seats/") — a test for "is this a logind system", not "is
	// logind up" — and on a false answer it logs "Skipping logind registration as logind is
	// not running" and returns PAM_SUCCESS. Creating that directory on the Varlink socket,
	// which carries Service=systemd-logind.service, is what gets pam_systemd as far as the
	// connection that starts logind.
	//
	// Two dead ends on the way, both of which measure well *here* and leave a machine whose
	// logins have no session, because the echo marker this row watches needs none:
	//
	//   - Ordering sshd after logind instead repairs SSH and only SSH, with `su -l` and the
	//     console getty still landing with XDG_RUNTIME_DIR unset. Ordering the getty after
	//     logind too repairs those and hands the saving straight back.
	//   - RuntimeDirectory=systemd/seats on the socket, to make the directory without a
	//     fork, creates nothing: a unit with no Exec* line never applies its execution
	//     context. It benchmarked as the fastest row here because it *was* the masked
	//     machine. ExecStartPre=/bin/true makes the directory appear, which is both the
	//     proof and the reason the drop-in does not bother avoiding the fork.
	//
	// `task boot:logind` is what holds the half of this that a boot time cannot show.
	{label: "logind at boot", cpus: "2", memory: "2048",
		files: withFile(gettyDropin(gettyEcho),
			"/etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf", "[Socket]\n"),
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/systemd-logind.service": "/lib/systemd/system/systemd-logind.service",
		}},
	// serial-getty is Type=idle, which holds the service until systemd's job queue is quiet.
	// If that dominates, `usable` has been measuring the queue draining rather than the
	// machine being ready — and it would have been invisible earlier, because the first test
	// of Type=simple was run against a real agetty whose sleep(1) buried it.
	{label: "getty no idle", cpus: "2", memory: "2048",
		files: gettyDropin("Type=simple\n" + gettyEcho)},
	// Devices udev does not have to walk. udev coldplugs 266 of them on this machine, and
	// three families are for hardware it does not have: 64 virtual consoles (CONFIG_VT, on a
	// machine whose QEMU ships no VGA), 8 unused loop devices, and three of the four 16550s.
	// This row switches off the two that are boot parameters.
	//
	// Neither this nor CONFIG_VT=n, which removes a quarter of the devices, changes the time
	// to a login prompt — measured 2026-09-26, and the kernel was built to be sure. udev's
	// work overlaps the boot rather than delaying it, so a ranking of who spent time in early
	// userspace is not a list of savings either.
	//
	// "Not at a login prompt" is the whole claim, and it is not the same as "not at all":
	// the kernel phase can move by a couple of milliseconds that this harness cannot see,
	// because it measures through ~139 ms of userspace whose own spread is wider. A
	// kernel-config change belongs in `task boot:initcalls` first, and only here once it is
	// large enough that a login prompt could show it.
	labelled("fewer devices", variant{cpus: "2", memory: "2048",
		files: gettyDropin(gettyEcho),
		extra: "loop.max_loop=0 8250.nr_uarts=1"}),
	// The other half of udev's work: not how many devices, but how many rule files each one
	// is matched against. 19 of the 49 are for hardware this machine cannot have. No effect
	// either, measured the same day; see the row above for why.
	labelled("fewer udev rules", variant{cpus: "2", memory: "2048",
		files: gettyDropin(gettyEcho),
		links: maskedRules()}),
	// What loading a unit file costs, measured by adding some.
	//
	// systemd reports "Loaded units and determined initial transaction in 212ms" on this
	// machine, and what precedes it is "Modification times have changed, need to update cache"
	// right after /run/systemd/generator.late — the generators write into the search path, so
	// the cache systemd built moments earlier is stale and it rescans. That is how systemd
	// starts, not a cache this image failed to pre-build: the persistent ones (ld.so.cache,
	// the journal catalog, locale-archive) are built at image time and the services that would
	// rebuild them are masked in optimize-systemd.sh.
	//
	// So the question is whether the *number* of units is what costs. The image carries 282 in
	// /usr/lib/systemd/system and 65 in /etc/systemd/system, and this row adds 300 more that
	// nothing wants. It costs nothing: 236 against the baseline's 246 over 12 boots,
	// 2026-09-26, on a run whose p95s were wide enough that a per-unit cost of any size would
	// still have shown at nearly double the count.
	//
	// What that does and does not settle, because the two are easy to confuse. systemd's unit
	// cache holds names and modification times, not parsed units, and a unit nothing
	// references is scanned and never loaded. So this measures the scan, and the scan is free.
	// It does not measure parsing, which happens only for units something pulls in — and
	// those cannot be added without also starting them, which would measure something else.
	//
	// The row stays because "we ship 282 unit files, that must be the boot" is a conclusion
	// somebody will reach again, and this is the only thing that says it was measured.
	labelled("300 more units", variant{cpus: "2", memory: "2048",
		files: gettyDropin(gettyEcho),
		setup: "for i in $(seq 1 300); do printf '[Unit]\\nDescription=filler %s\\n[Service]\\nType=oneshot\\nExecStart=/bin/true\\n' \"$i\" > \"$MNT/etc/systemd/system/spin-filler-$i.service\"; done"}),
	// What a tmpfs /tmp and the boot's tmpfiles pass cost against the /tmp on the disk the
	// image once shipped, which every copy of the disk carried. 20 boots of each, 2026-09-26,
	// p50/p95 to a usable machine: baseline 223/230, this row 220/232 - 3 ms, within noise.
	labelled("tmp on disk", without("tmp.mount", "systemd-tmpfiles-setup.service")),
}

// kernelB is the kernel SPIN_KERNEL_B names, resolved, or "" when it names none. A test
// that has one adds it as a row interleaved with the rest rather than run as a second pass,
// which is the only way to compare two kernel configurations on a host that is not the same
// host from one minute to the next: three consecutive runs of one kernel read 59.5, 96.8
// and 167.7 ms.
//
// Not a hard-coded path, because a kernel built from a changed config is not in this
// repository and a row naming a file nobody has fails for everyone who did not build it.
func kernelB(t *testing.T) string {
	t.Helper()
	k := os.Getenv("SPIN_KERNEL_B")
	if k == "" {
		return ""
	}
	abs, err := filepath.Abs(k)
	if err != nil {
		t.Fatalf("resolving SPIN_KERNEL_B=%q: %v", k, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("SPIN_KERNEL_B=%s: %v", abs, err)
	}
	return abs
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
	vs := slices.Clone(variants)
	if k := kernelB(t); k != "" {
		vs = append(vs, variant{label: "kernel B", cpus: "2", memory: "2048",
			files: gettyDropin(gettyEcho), kernel: k})
	}

	if !canSudo() {
		for _, v := range vs {
			if len(v.mask) > 0 {
				t.Skipf("masking a unit means writing into the overlay through qemu-nbd, "+
					"which needs sudo; %q cannot be measured here", v.label)
			}
		}
	}

	samples := map[string]map[boot.Phase][]time.Duration{}
	failures := map[string]int{}
	for rep := range reps {
		for _, v := range vs {
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
		"--append", "init=/sbin/init loglevel=7 systemd.show_status=true "+
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
	if len(v.mask) > 0 || len(v.files) > 0 || len(v.links) > 0 || v.setup != "" {
		editOverlay(t, overlay, v)
	}

	cmdline := "init=/sbin/init"
	if os.Getenv("DEBUG") == "1" {
		// Every message is a write to a serial port, and that write is inside the wall
		// clock being measured. On by choice, off by default, and the same in every variant
		// of a run so a comparison still holds while the absolute is inflated.
		cmdline += " loglevel=7 systemd.show_status=true systemd.log_level=info systemd.log_target=console"
	}
	if v.extra != "" {
		cmdline += " " + v.extra
	}

	args := []string{"boot", "--release", out, "--disk", overlay,
		"--memory", v.memory, "--cpus", v.cpus,
		"--console", "file:/dev/stdout", "--append", cmdline}
	if v.kernel != "" {
		args = append(args, "--kernel", v.kernel)
	}
	args = append(args, v.flags...)
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...)
	// Its own process group, so the machine can be taken down as a whole. spin-machine
	// execs QEMU in place now, so the pid is QEMU's; the group stays because it is what
	// makes that not matter. When spin-machine ran QEMU as a child, killing the parent left
	// the child running: the first version of this left one `qemu-system-x86_64` per boot
	// alive, each still holding an overlay open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	// QEMU's own refusal goes to stderr, and a row that never boots is only worth reporting
	// with it.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

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
	timer := time.AfterFunc(cmp.Or(v.timeout, 70*time.Second), kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	run, err := boot.Watch(stdout, t0, time.Now, boot.Usable)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	if !run.Reached(boot.Usable) {
		// Watch stops at EOF; the process has exited or is about to be killed.
		kill()
		_ = cmd.Wait()
		run.Output = append(run.Output, stderr.Bytes()...)
	}
	return run
}

// editOverlay applies a variant's masks and drop-ins to its own overlay, which is what
// changing a unit for one boot actually takes: /etc is the highest-priority place a mask or a
// drop-in can live and no kernel parameter reaches it here.
func editOverlay(t *testing.T, overlay string, v variant) {
	t.Helper()
	dev := connectNBD(t, overlay)
	defer mustRun(t, "sudo", "qemu-nbd", "--disconnect", dev)

	// The device's size arrives after qemu-nbd returns: a mount before it reads a device of
	// zero bytes and fails with "can't read superblock", which a runner reconnecting right after
	// the previous boot's disconnect did (the kernel logged the capacity change 7 ms after the
	// failed mount). blkid -p and not blkid: without it blkid answers from its cache, and said
	// ext4 about a device that was still empty.
	var ok bool
	for range 20 {
		size, err := exec.Command("sudo", "blockdev", "--getsize64", dev).Output()
		if err == nil && strings.TrimSpace(string(size)) != "0" && exec.Command("sudo", "blkid", "-p", dev).Run() == nil {
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
	// One shell with the overlay mounted, for a variant that needs hundreds of files rather
	// than a handful. The files map writes each path through its own `sudo tee`, which is
	// right for a drop-in and wrong for three hundred units: the setup would take longer than
	// the boots it is preparing.
	if v.setup != "" {
		// MNT through sudo's own assignment rather than the child's environment: sudo resets
		// the environment, so cmd.Env reached sh with MNT unset and the snippet failed under
		// `set -u` — which is the good case. Without -u it would have written 300 unit files
		// into the host's /etc/systemd/system.
		cmd := exec.Command("sudo", "MNT="+mnt, "sh", "-euc", v.setup)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setting up %s: %v\n%s", v.label, err, out)
		}
	}
	for path, target := range v.links {
		// A mask only masks something. udev reads /etc/udev/rules.d before
		// /usr/lib/udev/rules.d and a file of the same name shadows the one below it, so a
		// symlink to /dev/null here switches a rule file off — and a symlink whose name
		// matches no rule file switches nothing off, changes no timing, and reports a row
		// that looks like a measurement. Checked rather than trusted, for the same reason
		// image/build.sh opens the filesystem it just made.
		if target == "/dev/null" && strings.HasPrefix(path, "/etc/udev/rules.d/") {
			shadowed := filepath.Join(mnt, "usr/lib/udev/rules.d", filepath.Base(path))
			if err := exec.Command("sudo", "test", "-e", shadowed).Run(); err != nil {
				t.Fatalf("%s masks nothing: there is no %s in this image",
					path, filepath.Base(path))
			}
		}
		dst := filepath.Join(mnt, path)
		mustRun(t, "sudo", "mkdir", "-p", filepath.Dir(dst))
		mustRun(t, "sudo", "ln", "-sfn", target, dst)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// rawRoot is a private raw copy of a release's root filesystem, edited without mounts or NBD
// by the release's own debugfs: the one the image was made and checked with, not the host's,
// because an older one refuses a filesystem with features it does not know rather than editing
// it. debugfs exits 0 whether or not an edit happened - a write or a symlink into a directory
// that is not there included - so every edit here is read back.
type rawRoot struct {
	t       *testing.T
	path    string // the raw image, for a -drive
	debugfs string
	input   string
}

func newRawRoot(t *testing.T, out, base string) *rawRoot {
	t.Helper()
	dir := t.TempDir()
	r := &rawRoot{t: t, path: filepath.Join(dir, "rootfs.raw"), debugfs: filepath.Join(out, "bin/debugfs"),
		input: filepath.Join(dir, "input")}
	mustRun(t, filepath.Join(out, "bin/qemu-img"), "convert", "-f", "qcow2", "-O", "raw", base, r.path)
	return r
}

func (r *rawRoot) edit(request string) {
	r.t.Helper()
	mustRun(r.t, r.debugfs, "-w", "-R", request, r.path)
}

// mkdirParent makes p's parent. mkdir fails on a directory that is there, which is not a failure
// here; what is made in it is what is checked.
func (r *rawRoot) mkdirParent(p string) {
	_ = exec.Command(r.debugfs, "-w", "-R", "mkdir "+filepath.Dir(p), r.path).Run()
}

func (r *rawRoot) stat(p string) string {
	out, _ := exec.Command(r.debugfs, "-R", "stat "+p, r.path).CombinedOutput()
	return string(out)
}

func (r *rawRoot) write(p, content string) {
	r.t.Helper()
	if err := os.WriteFile(r.input, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.mkdirParent(p)
	r.edit("write " + r.input + " " + p)
	if b, err := exec.Command(r.debugfs, "-R", "cat "+p, r.path).Output(); err != nil || string(b) != content {
		r.t.Fatalf("%s is not in the guest's root as written (%v)", p, err)
	}
}

// remove fails on a path that is not there, so that an experiment removing something the image
// no longer has fails rather than measuring the image as built under another name.
func (r *rawRoot) remove(p string) {
	r.t.Helper()
	if !strings.Contains(r.stat(p), "Inode:") {
		r.t.Fatalf("%s is not in the image, so removing it tests nothing", p)
	}
	r.edit("rm " + p)
	if strings.Contains(r.stat(p), "Inode:") {
		r.t.Fatalf("%s is still in the guest's root", p)
	}
}

func (r *rawRoot) link(l, target string) {
	r.t.Helper()
	r.mkdirParent(l)
	r.edit("symlink " + l + " " + target)
	if !strings.Contains(r.stat(l), "Type: symlink") {
		r.t.Fatalf("%s is not a symlink in the guest's root", l)
	}
}

func canSudo() bool { return exec.Command("sudo", "-n", "true").Run() == nil }

// canEditImages is whether editOverlay can work here: sudo, and an NBD device once the module is
// loaded. A runner in a container can have the first and not the second - modprobe succeeds on
// the host's kernel and /dev/nbd0 is not in the container - and qemu-nbd then fails on every
// boot that edits its overlay.
func canEditImages() bool {
	return canSudo() && loadNBD() == nil
}

// loadNBD makes the NBD devices be there: on a kernel that builds NBD in - Spin OS's, which has no
// modules at all, and so no modprobe that could find one - they are from boot, and modprobe is
// only asked where they are not.
func loadNBD() error {
	if _, err := os.Stat("/dev/nbd0"); err == nil {
		return nil
	}
	if out, err := exec.Command("sudo", "-n", "modprobe", "nbd", "max_part=8").CombinedOutput(); err != nil {
		return fmt.Errorf("sudo modprobe nbd max_part=8: %w: %s", err, out)
	}
	if !nbdAppears("/dev/nbd0") {
		return errors.New("/dev/nbd0 did not appear after modprobe nbd")
	}
	return nil
}

// nbdAppears waits for dev. modprobe returns when the module is loaded, and the device nodes
// come after it, from udev: on a runner that had never loaded it, qemu-nbd right after modprobe
// failed on "/dev/nbd0: No such file or directory" with the node there a moment later.
// connectNBD attaches overlay to an NBD device nobody holds and returns it. Not /dev/nbd0 alone:
// the devices are the host kernel's, and two lab runners on one host each took nbd0 - the second
// got "Failed to set NBD socket" and its run failed (2026-09-29). A device is held while its
// /sys/block/nbdN/pid exists; one taken between that look and the connect fails the connect,
// and the next is tried.
func connectNBD(t *testing.T, overlay string) string {
	t.Helper()
	if err := loadNBD(); err != nil {
		t.Fatal(err)
	}
	devs, err := filepath.Glob("/sys/block/nbd*")
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for _, d := range devs {
		if _, err := os.Stat(filepath.Join(d, "pid")); err == nil {
			continue
		}
		dev := "/dev/" + filepath.Base(d)
		out, err := exec.Command("sudo", "qemu-nbd", "--connect="+dev, "-f", "qcow2", overlay).CombinedOutput()
		if err == nil {
			return dev
		}
		last = fmt.Sprintf("%s: %v: %s", dev, err, out)
	}
	t.Fatalf("no free NBD device among %d; the last tried: %s", len(devs), last)
	return ""
}

func nbdAppears(dev string) bool {
	for range 50 {
		if _, err := os.Stat(dev); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// releaseTree is _output, or SPIN_MACHINE_OUTPUT, without releaseDir's checks: a test that runs
// under TCG cannot ask for /dev/kvm.
func releaseTree(t *testing.T) string {
	t.Helper()
	out, err := filepath.Abs(cmp.Or(os.Getenv("SPIN_MACHINE_OUTPUT"), filepath.Join("..", "_output")))
	if err != nil {
		t.Fatalf("locating the release: %v", err)
	}
	return out
}

// releaseDir is the release tree the benchmarks boot: _output, or SPIN_MACHINE_OUTPUT - a tree
// laid out the same way, for measuring a kernel or an image built somewhere else without writing
// it over what `task build` made.
func releaseDir(t *testing.T) string {
	t.Helper()
	out := releaseTree(t)
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
