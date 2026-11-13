// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/boot"
)

// What waited on what between the kernel handing over and a login prompt, on one axis with
// the host's clock: boot.Waits applied to REPS boots. See boot/waits.go for the rule.
//
// The guest and the host share no clock, so the two are joined at one event both see: the
// probe's unit writes SPIN-WAITS-ANCHOR as its first act, the host stamps the read it arrives
// in, and systemd keeps the unit's exec as InactiveExitTimestampMonotonic. The difference is
// where guest CLOCK_MONOTONIC zero - the kernel's start - sits on the host's clock, and it is
// late by the fork-to-echo of one shell and a serial write. That error is a few milliseconds:
// on a laptop on 2026-11-12 the echo'd login prompt landed 2.5 ms before the getty's fork in
// one boot of five, which it cannot have. Read a difference smaller than that as none. What is
// before that zero is QEMU and the firmware; task boot:firmware-stages splits it.
//
// The login is serial-getty@ttyS0 replaced by an echo (gettyEcho), as task boot:bench's
// baseline does, so agetty's own second is not in it. The getty keeps Type=idle, so what is
// between its fork and the prompt is it waiting for the job queue to drain, and that is
// printed as its own row because it is in no unit's timestamps.
//
//	SPIN_BOOT_WAITS=1  run at all
//	REPS=<n>           boots (default 5)
//	CPUS=<n>           vCPUs (default 2, the machine's)
//	TARGET=<unit>     the unit whose start is the login (default serial-getty@ttyS0.service)
//	TOP=<n>            rows per table (default 15)
//	GAP=<ms>           the shortest idle interval printed (default 1)
//	RAW=<dir>          also write each boot's dump there, which is how testdata is made
func TestBootWaits(t *testing.T) {
	if os.Getenv("SPIN_BOOT_WAITS") == "" {
		t.Skip("set SPIN_BOOT_WAITS=1: boots a VM and needs sudo to write into its overlay")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	reps := envInt(t, "REPS", 5)
	top := envInt(t, "TOP", 15)
	minGap := time.Duration(envInt(t, "GAP", 1)) * time.Millisecond
	target := cmp.Or(os.Getenv("TARGET"), "serial-getty@ttyS0.service")
	raw := os.Getenv("RAW")

	v := waitsVariant(cmp.Or(os.Getenv("CPUS"), "2"))

	var boots []waitsBoot
	for rep := range reps {
		b := bootWaits(t, out, v, target, "SPIN-WAITS-END")
		if raw != "" {
			name := filepath.Join(raw, fmt.Sprintf("waits-%02d.txt", rep+1))
			if err := os.WriteFile(name, []byte(b.dump), 0o644); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
		boots = append(boots, b)
	}
	t.Log(waitsReport(boots, target, top, minGap))
}

// waitsVariant is the boot both probes take: the getty an echo, and spin-waits.service printing
// every unit's timestamps once startup has finished.
func waitsVariant(cpus string) variant {
	// Type=simple and a loop on FinishTimestampMonotonic, for the reason TestUserspaceCost
	// gives: this unit is one of the initial transaction's jobs.
	//
	// The units are list-units --all and not `systemctl show '*'`: the pattern matches only
	// the units still loaded and active, 57 of them on 2026-11-12, and drops every oneshot
	// that ran and finished. --all is more than ran; ParseWaits drops the ones that did not.
	// Names is there because a device's Id is whichever of its aliases systemd met first - the
	// sysfs path in one boot, dev-vda.device in the next - and After= names the alias. The
	// names go through a command substitution and not xargs, which reads a backslash as an
	// escape and asked for system-serialx2dgetty.slice, a unit that does not exist. $$ is a
	// literal $ in a unit file.
	const props = "Id,Names,Type,ActiveState,After,InactiveExitTimestampMonotonic,ActiveEnterTimestampMonotonic,InactiveEnterTimestampMonotonic"
	const manager = "UserspaceTimestampMonotonic,SecurityStartTimestampMonotonic,SecurityFinishTimestampMonotonic,GeneratorsStartTimestampMonotonic,GeneratorsFinishTimestampMonotonic,UnitsLoadStartTimestampMonotonic,UnitsLoadFinishTimestampMonotonic,FinishTimestampMonotonic"
	dump := `[Unit]
Description=Print every unit's activation timestamps
After=multi-user.target
[Service]
Type=simple
StandardOutput=null
ExecStart=/bin/sh -c "echo SPIN-WAITS-ANCHOR > /dev/ttyS0; i=0; while [ \"$$(systemctl show -p FinishTimestampMonotonic --value)\" = 0 ] && [ $$i -lt 100 ]; do i=$$((i+1)); sleep 0.05; done; { echo SPIN-WAITS-BEGIN; systemctl show -p ` + manager + `; echo SPIN-WAITS-UNITS; systemctl show -p ` + props + ` -- $$(systemctl list-units --all --plain --no-legend --no-pager | awk '{print $$1}'); echo SPIN-WAITS-END; } > /dev/ttyS0 2>&1"
[Install]
WantedBy=multi-user.target
`
	files := gettyDropin(gettyEcho)
	files["/etc/systemd/system/spin-waits.service"] = dump
	return variant{
		label: "waits", cpus: cpus, memory: "2048", files: files,
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/spin-waits.service": "/etc/systemd/system/spin-waits.service",
		},
	}
}

// waitsBoot is one boot, with its two clocks joined: kernel is guest monotonic zero on the
// host's clock, and usable is the login prompt on the guest's.
type waitsBoot struct {
	w              boot.Waits
	dump           string
	console        string
	kernel, usable time.Duration
	login          string    // the unit whose start is the login
	start          time.Time // the moment before the machine was launched
	ram            hostRAM   // the guest's RAM as the host holds it at the end
}

// hostRAM is guestRAM's answer at the end of a boot: MB of the guest's RAM resident on the
// host, and how much of that is in transparent huge pages - KVM can map a guest page 2M at a
// time only where the host has it in a huge page.
type hostRAM struct{ residentMB, hugeMB float64 }

func (x waitsBoot) target() boot.Unit { u, _ := x.w.Unit(x.login); return u }

// machineCommand is `spin-machine boot` with args, or, for a variant that takes devices out
// of the machine, the release's QEMU run with the command line `spin-machine args` prints for
// the same arguments, less those devices - as boot:firmware-stages and boot:mmio run it to add
// QEMU's trace flags. A machine with a device fewer has another fingerprint, which is a
// fleet-wide decision, so spin-machine has no flag for it on purpose: an experiment does not
// belong in the release's path. Everything else the line holds is the definition's.
func machineCommand(t *testing.T, out string, v variant, args ...string) *exec.Cmd {
	t.Helper()
	bin := filepath.Join(out, "bin", "spin-machine")
	if len(v.without) == 0 {
		return exec.Command(bin, append([]string{"boot", "--release", out}, args...)...)
	}
	line := machineArgs(t, bin, out, args)
	removed := 0
	for i := 0; i+1 < len(line); i++ {
		if line[i] != "-device" {
			continue
		}
		for _, d := range v.without {
			if strings.HasPrefix(line[i+1], d) {
				line = slices.Delete(line, i, i+2)
				i--
				removed++
				break
			}
		}
	}
	if removed != len(v.without) {
		t.Fatalf("took %d devices out of %v; the command line has:\n%s", removed, v.without, strings.Join(line, "\n"))
	}
	return exec.Command(line[0], line[1:]...)
}

// bootWaits boots once and reads the console until end, which is SPIN-WAITS-END or the marker
// of a probe that writes after it.
func bootWaits(t *testing.T, out string, v variant, target, end string) waitsBoot {
	t.Helper()
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)

	cmd := machineCommand(t, out, v, "--disk", overlay, "--memory", v.memory, "--cpus", v.cpus,
		"--console", "file:/dev/stdout", "--append", "init=/sbin/init")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	t0 := time.Now()
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

	// Stamped per read, as boot.Watch does and for its reason: a harness that stamps lines
	// slows the serial port it is timing.
	stamps := map[string]time.Duration{}
	marks := []string{"login:", "SPIN-WAITS-ANCHOR", "SPIN-WAITS-END", end}
	var console []byte
	buf := make([]byte, 65536)
	deadline := time.Now().Add(90 * time.Second)
	for _, done := stamps[end]; !done; _, done = stamps[end] {
		if err := r.SetReadDeadline(deadline); err != nil {
			t.Fatalf("setting the console deadline: %v", err)
		}
		n, err := r.Read(buf)
		at := time.Since(t0)
		// A marker can straddle two reads; it is searched in what has arrived, from a little
		// before this read, and stamped with the read that completed it.
		from := max(0, len(console)-32)
		console = append(console, buf[:n]...)
		for _, m := range marks {
			if _, ok := stamps[m]; !ok && bytes.Contains(console[from:], []byte(m)) {
				stamps[m] = at
			}
		}
		if _, done := stamps[end]; err != nil && !done {
			t.Fatalf("never saw %s; console tail:\n%s", end, tail(console, 2000))
		}
	}
	// What the host holds of the guest's memory, read while the machine still runs.
	var ram hostRAM
	if mb, err := strconv.Atoi(v.memory); err == nil {
		ram.residentMB, ram.hugeMB, _ = guestRAM(cmd.Process.Pid, mb)
	}
	for _, m := range marks[:3] {
		if _, ok := stamps[m]; !ok {
			t.Fatalf("no %q on the console; tail:\n%s", m, tail(console, 2000))
		}
	}

	s := string(console)
	begin := strings.Index(s, "SPIN-WAITS-BEGIN")
	stop := strings.Index(s, "SPIN-WAITS-END")
	if begin < 0 || stop < begin {
		t.Fatalf("no dump between SPIN-WAITS-BEGIN and -END; tail:\n%s", tail(console, 2000))
	}
	dump := s[begin+len("SPIN-WAITS-BEGIN") : stop]
	m, us, err := boot.ParseWaits(dump)
	if err != nil {
		t.Fatalf("reading the dump: %v\n%s", err, tail([]byte(dump), 1500))
	}
	wt := boot.NewWaits(m, us)
	probe, ok := wt.Units["spin-waits.service"]
	if !ok || probe.Start == 0 {
		t.Fatalf("spin-waits.service has no start in its own dump")
	}
	if _, ok := wt.Unit(target); !ok {
		t.Fatalf("%s was not started in this boot: TARGET names a unit this image does not start", target)
	}
	kernel := stamps["SPIN-WAITS-ANCHOR"] - probe.Start
	return waitsBoot{w: wt, dump: dump, console: s, start: t0, ram: ram, kernel: kernel, usable: stamps["login:"] - kernel, login: target}
}

// waitsReport prints the boots as p50s, then the median boot verbatim: a critical path or a
// set of gaps averaged across boots would be a path no boot took.
func waitsReport(boots []waitsBoot, target string, top int, minGap time.Duration) string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }
	p50 := func(ds []time.Duration) time.Duration { d, _ := boot.Percentile(ds, 0.5); return d }
	p95 := func(ds []time.Duration) time.Duration { d, _ := boot.Percentile(ds, 0.95); return d }

	var b strings.Builder
	fmt.Fprintf(&b, "\nms on the host's clock from the moment before QEMU is exec'd, p50/p95 over %d boots\n\n", len(boots))

	// The boot as consecutive phases; each row's value is where it ends, and the column
	// beside it what the phase itself took.
	type phase struct {
		name string
		at   func(waitsBoot) time.Duration
	}
	phases := []phase{
		{"kernel starts (QEMU, firmware)", func(x waitsBoot) time.Duration { return x.kernel }},
		{"PID 1 exec'd (kernel)", func(x waitsBoot) time.Duration { return x.kernel + x.w.Manager.Userspace }},
		{"security setup done", func(x waitsBoot) time.Duration { return x.kernel + x.w.Manager.SecurityFinish }},
		{"generators start (PID 1 setup)", func(x waitsBoot) time.Duration { return x.kernel + x.w.Manager.GeneratorsStart }},
		{"generators done", func(x waitsBoot) time.Duration { return x.kernel + x.w.Manager.GeneratorsFinish }},
		{"units loaded", func(x waitsBoot) time.Duration { return x.kernel + x.w.Manager.UnitsLoadFinish }},
		{target + " forked (jobs)", func(x waitsBoot) time.Duration { return x.kernel + x.target().Start }},
		{"login prompt (Type=idle, exec)", func(x waitsBoot) time.Duration { return x.kernel + x.usable }},
	}
	fmt.Fprintf(&b, "  %-46s %15s %15s\n", "PHASE ENDS AT", "AT", "TOOK")
	var prev []time.Duration
	for _, p := range phases {
		var at, took []time.Duration
		for i, x := range boots {
			at = append(at, p.at(x))
			if prev != nil {
				took = append(took, at[i]-prev[i])
			} else {
				took = append(took, at[i])
			}
		}
		fmt.Fprintf(&b, "  %-46s %7s/%-7s %7s/%-7s\n", p.name, ms(p50(at)), ms(p95(at)), ms(p50(took)), ms(p95(took)))
		prev = at
	}
	// Not a phase of the login: the transaction finishes on its own schedule, before or after
	// the getty's exec, and is here to be set beside systemd-analyze's number.
	var finish []time.Duration
	for _, x := range boots {
		finish = append(finish, x.kernel+x.w.Manager.Finish)
	}
	fmt.Fprintf(&b, "  %-46s %7s/%-7s\n", "(startup finished)", ms(p50(finish)), ms(p95(finish)))

	// Units, across boots: how often each was on the path to the login, and what it cost there.
	type agg struct {
		onPath    int
		own, lag  []time.Duration
		blockers  map[string]int
		ownAnyway []time.Duration
	}
	units := map[string]*agg{}
	get := func(id string) *agg {
		if units[id] == nil {
			units[id] = &agg{blockers: map[string]int{}}
		}
		return units[id]
	}
	for _, x := range boots {
		for _, u := range x.w.Units {
			get(u.Name()).ownAnyway = append(get(u.Name()).ownAnyway, u.Own())
		}
		path, err := x.w.CriticalPath(target)
		if err != nil {
			fmt.Fprintf(&b, "\n  a boot with no critical path to %s: %v\n", target, err)
			continue
		}
		for _, h := range path {
			a := get(h.Unit.Name())
			a.onPath++
			a.own = append(a.own, h.Unit.Own())
			a.lag = append(a.lag, h.Lag)
			a.blockers[cmp.Or(h.Blocker, "(unit load)")]++
		}
	}

	fmt.Fprintf(&b, "\nON THE PATH TO %s: a unit's own time and the lag before it started, p50 where it was on it\n\n", target)
	fmt.Fprintf(&b, "  %-44s %6s %8s %8s  %s\n", "UNIT", "BOOTS", "OWN", "LAG", "WAITED ON")
	ids := slices.Collect(maps.Keys(units))
	slices.SortFunc(ids, func(a, c string) int {
		return cmp.Or(cmp.Compare(units[c].onPath, units[a].onPath),
			cmp.Compare(p50(units[c].own)+p50(units[c].lag), p50(units[a].own)+p50(units[a].lag)), cmp.Compare(a, c))
	})
	n := 0
	for _, id := range ids {
		a := units[id]
		if a.onPath == 0 || n == top {
			continue
		}
		n++
		fmt.Fprintf(&b, "  %-44s %3d/%-2d %8s %8s  %s\n", clipUnit(id), a.onPath, len(boots),
			ms(p50(a.own)), ms(p50(a.lag)), mostOften(a.blockers, a.onPath))
	}

	fmt.Fprintf(&b, "\nSLOWEST UNITS, on the path or not: p50 of their own time\n\n")
	slices.SortFunc(ids, func(a, c string) int {
		return cmp.Or(cmp.Compare(p50(units[c].ownAnyway), p50(units[a].ownAnyway)), cmp.Compare(a, c))
	})
	for _, id := range ids[:min(top, len(ids))] {
		a := units[id]
		fmt.Fprintf(&b, "  %-44s %8s  in %d/%d boots, on the path in %d\n", clipUnit(id),
			ms(p50(a.ownAnyway)), len(a.ownAnyway), len(boots), a.onPath)
	}

	// The median boot, by its login, in its own words.
	med := slices.Clone(boots)
	slices.SortFunc(med, func(a, c waitsBoot) int { return cmp.Compare(a.kernel+a.usable, c.kernel+c.usable) })
	x := med[len(med)/2]
	fmt.Fprintf(&b, "\nTHE MEDIAN BOOT, login at %s ms; guest ms from the kernel's start\n", ms(x.kernel+x.usable))
	if path, err := x.w.CriticalPath(target); err == nil {
		fmt.Fprintf(&b, "\n  critical path, from the end of the unit load at %s\n\n", ms(x.w.Manager.UnitsLoadFinish))
		fmt.Fprintf(&b, "  %8s %8s %7s %7s  %s\n", "START", "READY", "OWN", "LAG", "UNIT")
		for _, h := range path {
			fmt.Fprintf(&b, "  %8s %8s %7s %7s  %s\n", ms(h.Unit.Start), ms(h.Unit.Ready), ms(h.Unit.Own()), ms(h.Lag), h.Unit.Name())
		}
		// A device has no After= to follow: it is ready when udev announces it. What announced
		// it is the coldplug, so its window is the next thing to read.
		if root := path[0].Unit; strings.HasSuffix(root.ID, ".device") {
			for _, id := range []string{"systemd-udevd.service", "systemd-udev-trigger.service"} {
				if u, ok := x.w.Unit(id); ok {
					fmt.Fprintf(&b, "  %8s %8s %7s %7s  (%s, which announces %s)\n", ms(u.Start), ms(u.Ready), ms(u.Own()), "", id, root.Name())
				}
			}
		}
		if u := x.target(); u.Idle {
			fmt.Fprintf(&b, "  %8s %8s %7s %7s  %s\n", ms(u.Ready), ms(x.usable), ms(x.usable-u.Ready), "", "(Type=idle: the job queue draining, then exec)")
		}
	}
	gaps := x.w.Gaps(x.target().Start, minGap)
	fmt.Fprintf(&b, "\n  no unit activating, for at least %s ms: systemd waiting on the kernel, or on itself\n\n", ms(minGap))
	var idle time.Duration
	for _, g := range gaps {
		idle += g.To - g.From
		ended := g.Next
		if g.NextBlocker != "" {
			ended += " after " + g.NextBlocker
		}
		fmt.Fprintf(&b, "  %8s %8s %7s  then %s\n", ms(g.From), ms(g.To), ms(g.To-g.From), cmp.Or(ended, "(nothing)"))
	}
	fmt.Fprintf(&b, "  %8s %8s %7s  in all\n", "", "", ms(idle))

	late := x.w.After(max(x.usable, x.target().Ready))
	fmt.Fprintf(&b, "\n  still starting behind the login prompt: %d units\n\n", len(late))
	for _, u := range late[:min(top, len(late))] {
		ready := "never"
		if u.Ready > 0 {
			ready = "+" + ms(u.Ready-x.usable)
		}
		fmt.Fprintf(&b, "  %8s %7s  %s (%s)\n", ready, ms(max(0, u.Own())), u.Name(), u.State)
	}
	return b.String()
}

func mostOften(m map[string]int, of int) string {
	var best string
	for k, v := range m {
		if best == "" || v > m[best] || (v == m[best] && k < best) {
			best = k
		}
	}
	if len(m) > 1 {
		return fmt.Sprintf("%s (%d of %d)", best, m[best], of)
	}
	return best
}

func clipUnit(s string) string {
	if len(s) > 44 {
		return s[:41] + "..."
	}
	return s
}
