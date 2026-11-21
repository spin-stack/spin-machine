// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Probe is one measurement of a tree. The experiment runs it over every tree and repetition,
// interleaved, and writes down what it recorded.
type Probe interface {
	Name() string
	// About is what the probe measures and from when, said once above its table.
	About() string
	Options() []Option
	Metrics() []Metric
	// Rows are the ways one repetition measures a tree: one, or several when the probe compares
	// ways of doing the same thing to it (a state restored evicted, cached, fast).
	Rows(o Options) []string
	// Phases is how many passes the experiment makes. Each pass runs every repetition, tree and
	// row, interleaved, before the next starts: a restore probe makes every state before it
	// restores any, because a cold boot after a restored guest had run was ~1.8x slower on the
	// bench fleet (2026-11-13) and would have measured that.
	Phases() int
	// Needs are the spin-machine commands and flags the probe runs with options o, which not
	// every release's has: a release's tree runs the spin-machine of its own commit
	// (TreeSpec.SpinMachine "release"), and boot --restore, --held and start arrived with
	// v20261119.02.
	Needs(o Options) []Need
	// Measure does one pass of one measurement, recording into m. An error made by Miss loses the
	// measurement and the experiment goes on; any other stops it.
	Measure(ctx context.Context, m *Measurement) error
}

// Detailer is a probe with more to say than its metrics' p50s: the initcalls behind a kernel's
// total, the gaps between its lines.
type Detailer interface {
	Detail(r *Result) string
}

// Metric is one number a probe records for each measurement.
type Metric struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"` // ms, MiB, count
	About string `json:"about,omitempty"`
}

// Option is a setting a probe takes, with its default.
type Option struct {
	Name, Default, About string
}

// Options are the settings an experiment was run with, every probe option present.
type Options map[string]string

// Int is option name as a number; settle checked it is one.
func (o Options) Int(name string) int {
	n, _ := strconv.Atoi(o[name])
	return n
}

// Bool is option name as a boolean; settle checked it is one.
func (o Options) Bool(name string) bool {
	b, _ := strconv.ParseBool(o[name])
	return b
}

// Fields is option name split on spaces: spin-machine boot flags.
func (o Options) Fields(name string) []string { return strings.Fields(o[name]) }

// shape is the machine every probe boots unless a run asks for another: the one spin boots a
// workspace in, so that the goal is measured on the machine its users get. spin-stack/spin at
// c12bd87 (2026-11-20): internal/runner/vmm/vmm.go's canonicalBootCPUs, canonicalMemory,
// defaultMaxCPUs and defaultMaxMemory. The hotplug controller's targets are spin's
// controllerTargets (internal/runner/qemu/spec.go, 16 since c0ffbc0, 2026-11-20): 4 for disks
// attached at run time and the rest for extensions. 16 against the 4 the probes booted before read
// within noise, first login -3.8 [-11.1, +4.5] (lab 44847364468, 2026-11-21). spin plugs
// cores before the boot only for a workspace that asks for more than one, which the canonical
// workspace does not. Its ceilings cost the boot: the kernel took 86.6 ms to "Freeing" in it
// against 62 in the 2 vCPUs and 2 GiB with no hotplug the probes booted before (lab 44587603255).
//
// It is spin's after the start as well: spin sizes every workspace to 2048 MiB right after it
// starts (Plug), and logs in as root through its supervisor's forwarder to ssh.service (login.go,
// spinsPath). Measured either way on 2026-11-20, the guest left at 128 MiB OOM-killed its way
// through the boot (lab 44784196287), and spin's cold first login read 532 ms where the probes'
// own read 445 (lab 44785179901): neither is a number spin's users get.
var shape = []Option{
	{"cpus", "1", "vCPUs at boot"},
	{"max-cpus", "16", "the vCPU hotplug ceiling (0: no hotplug)"},
	{"memory", "128", "MiB of memory at boot"},
	{"max-memory", "8192", "MiB the memory may grow to through virtio-mem (0: fixed)"},
	{"hotplug-disks", "16", "targets on the hotplug controller"},
	{"plugged", "2048", "MiB the guest is sized to through virtio-mem right after its start, as spin sizes a workspace (0: left at memory)"},
	{"login", "spin", "spin: as root, through a forwarder to ssh.service on 127.0.0.1:22, spin's path; lane: as spin, through systemd's vsock sshd, an sshd@ a connection"},
	{"flags", "", "more spin-machine boot flags for every boot"},
}

// shapeBefore is the shape every probe booted before shape was spin's, which a record from then
// does not name.
const shapeBefore = "2 vCPUs, 2048 MiB, no hotplug disks"

// shapeOf is the machine options o boot, as a table and a record say it.
func shapeOf(o Options) string {
	cpus := fmt.Sprintf("%d vCPUs", o.Int("cpus"))
	if n := o.Int("max-cpus"); n > 0 {
		cpus = fmt.Sprintf("%d of %d vCPUs", o.Int("cpus"), n)
	}
	memory := fmt.Sprintf("%d MiB", o.Int("memory"))
	if n := o.Int("max-memory"); n > 0 {
		memory = fmt.Sprintf("%d of %d MiB", o.Int("memory"), n)
	}
	disks := "no hotplug disks"
	if n := o.Int("hotplug-disks"); n > 0 {
		disks = fmt.Sprintf("%d hotplug disks", n)
	}
	s := cpus + ", " + memory + ", " + disks
	if n := o.Int("plugged"); n > 0 {
		s += fmt.Sprintf(", sized to %d MiB", n)
	}
	if o["login"] == "spin" {
		s += ", spin's login"
	}
	if f := strings.TrimSpace(o["flags"]); f != "" {
		s += ", " + f
	}
	return s
}

// settle is given with every option p takes filled in, refusing a name p does not take or a value
// that is not what its default is: a typo fails here rather than being ignored.
func settle(p Probe, given Options) (Options, error) {
	out := Options{}
	known := map[string]Option{}
	for _, o := range p.Options() {
		known[o.Name], out[o.Name] = o, o.Default
	}
	for _, name := range slices.Sorted(maps.Keys(given)) {
		o, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("%s takes no option %q; it takes: %s", p.Name(), name, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
		v := given[name]
		if _, err := strconv.Atoi(o.Default); err == nil {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				return nil, fmt.Errorf("%s's option %s is a number, not %q", p.Name(), name, v)
			}
		} else if _, err := strconv.ParseBool(o.Default); err == nil { // a number's default of 1 or 0 parses as one too
			if _, err := strconv.ParseBool(v); err != nil {
				return nil, fmt.Errorf("%s's option %s is true or false, not %q", p.Name(), name, v)
			}
		}
		out[name] = v
	}
	return out, nil
}

// Measurement is one repetition of one row of one tree, carried through every phase.
type Measurement struct {
	Tree    *Tree
	Row     string
	Rep     int
	Phase   int
	Options Options
	// Dir is the measurement's own, on the experiment's scratch disk: overlays and states.
	Dir string
	// Socks is the measurement's own directory for Unix sockets, short enough for sun_path.
	Socks string
	// Key logs into every machine the experiment boots.
	Key *Key
	// Target is the file holding the tree's machine record, which a probe's restores are handed
	// (restore --target) so that none reads it on its clock: read once before anything is measured
	// (readTarget), for a probe that restores. It is empty for a tree whose spin-machine cannot
	// print one or take one, whose restores ask the machine as every restore did before.
	Target string
	// State is the probe's, from one phase to the next.
	State any

	values  map[string]float64
	detail  map[string]float64
	missed  string
	cleanup []func()
	log     io.Writer
}

// Record notes metric's value for this measurement.
func (m *Measurement) Record(metric string, v float64) { m.values[metric] = v }

// RecordMs notes a duration in milliseconds.
func (m *Measurement) RecordMs(metric string, d time.Duration) {
	m.Record(metric, float64(d)/float64(time.Millisecond))
}

// Detail notes something a Detailer reads back, keyed as the probe likes.
func (m *Measurement) Detail(key string, v float64) { m.detail[key] = v }

// Cleanup runs f when the experiment is over, whatever happened: a machine left running would be
// what the measurements after it measured.
func (m *Measurement) Cleanup(f func()) { m.cleanup = append(m.cleanup, f) }

// Logf says what happened, on the experiment's log.
func (m *Measurement) Logf(format string, args ...any) {
	_, _ = fmt.Fprintf(m.log, "%s, %s, repetition %d: %s\n", m.Tree.Name(), m.Row, m.Rep+1, fmt.Sprintf(format, args...))
}

// Overlay makes a fresh overlay of the tree at m.Dir/name with the tree's edits and those given,
// and returns its path.
// mutate-lane: TestEveryProbeMeasuresATree
func (m *Measurement) Overlay(name string, edits ...Edit) (string, error) {
	path := m.Dir + "/" + name
	if err := NewOverlay(m.Tree, path); err != nil {
		return "", err
	}
	for _, e := range m.Tree.Manifest.Edits {
		edits = append(edits, e.Edit)
	}
	if len(edits) > 0 {
		if err := EditOverlay(path, edits...); err != nil {
			return "", err
		}
	}
	return path, nil
}

// BootArgs are `spin-machine boot`'s flags for the tree in the experiment's shape, the given
// ones after them.
func (m *Measurement) BootArgs(overlay string, extra ...string) []string {
	args := []string{"boot", "--release", m.Tree.Dir, "--disk", overlay,
		"--memory", m.Options["memory"], "--cpus", m.Options["cpus"], "--max-memory", m.Options["max-memory"],
		"--max-cpus", m.Options["max-cpus"], "--hotplug-disks", m.Options["hotplug-disks"]}
	args = append(args, extra...)
	args = append(args, m.Options.Fields("flags")...)
	return append(args, m.Tree.Manifest.Boot...)
}

// plugNeeds are what Plug runs, when the options size the guest.
func plugNeeds(o Options) []Need {
	if o.Int("plugged") == 0 {
		return nil
	}
	return []Need{{"boot", "qmp"}, {"memory", "qmp"}, {"memory", "to"}}
}

// qmpToPlug is the boot flag that opens the monitor Plug talks to on qmp, when the options size
// the guest; a probe whose boot opens no monitor of its own for anything else boots with it.
func (m *Measurement) qmpToPlug(qmp string) []string {
	if m.Options.Int("plugged") == 0 {
		return nil
	}
	return []string{"--qmp", qmp}
}

// Plug sizes the machine whose monitor is qmp to the plugged option's MiB, as spin does a
// workspace: spin boots every machine at 128 MiB and, right after its start and before it hands the
// guest its filesystem, asks virtio-mem for the workspace's size and waits for the guest to take it
// (spin-stack/spin internal/runner/vmm/vmm.go, resize; 2048 MiB unless the workspace asks for
// more, installconfig.MinMemoryMB). Left at 128 MiB the guest's OOM killer ran through every boot
// of spin's shape on a laptop - logind, user@1000, sshd@ and ssh.service killed, logins lost and
// retried (2026-11-20) - which no workspace of spin's does.
//
// It runs beside the boot, from the moment the monitor listens, and the returned wait says whether
// the guest got there: a measurement of a guest that did not is lost. With the option at 0 it does
// nothing.
// mutate-lane: TestEveryProbeMeasuresATree
func (m *Measurement) Plug(qmp string) (wait func() error) {
	mib := m.Options.Int("plugged")
	if mib == 0 {
		return func() error { return nil }
	}
	done := make(chan error, 1)
	go func() {
		if err := waitSocket(qmp, time.Now().Add(30*time.Second)); err != nil {
			done <- err
			return
		}
		done <- m.SpinMachine("memory", "--qmp", qmp, "--to", strconv.Itoa(mib))
	}()
	return func() error {
		if err := <-done; err != nil {
			return Miss("the guest was not sized to %d MiB: %v", mib, err)
		}
		return nil
	}
}

// MissError is a measurement lost - a machine that never answered - and not an experiment that
// cannot go on: it is counted beside the table.
type MissError struct{ Why string }

func (e *MissError) Error() string { return e.Why }

// Miss is a lost measurement and why.
func Miss(format string, args ...any) error { return &MissError{Why: fmt.Sprintf(format, args...)} }

// RefusedError is a tree this host will not run - QEMU refusing its machine - and not one lost
// measurement: every measurement of the tree still to come is lost with it, in one line, instead
// of each waiting out its own timeout. A named CPU model the host cannot show cost the bench 50
// minutes that way, 20 boots a tree each given 60 s to log in (lab run 44657973748, 2026-11-20).
type RefusedError struct{ Why string }

func (e *RefusedError) Error() string { return e.Why }

// Refuse is a tree this host will not run, and why.
func Refuse(format string, args ...any) error {
	return &RefusedError{Why: fmt.Sprintf(format, args...)}
}

// refusedHere is what QEMU says when it will not make the machine on this host: enforce=on, a CPU
// model naming a feature the host cannot show (machine.Spec.CPU).
const refusedHere = "Host doesn't support requested features"

// Exited says why a machine that should still be running is not, once it has exited: a refusal of
// the tree when QEMU would not make it on this host, else a lost measurement. It is nil while the
// machine runs.
func (mc *Machine) Exited() error {
	select {
	case <-mc.done:
	default:
		return nil
	}
	said := strings.TrimSpace(mc.Stderr.String())
	if strings.Contains(said, refusedHere) {
		return Refuse("QEMU will not make this tree's machine on this host: %s", lastLine(said))
	}
	return Miss("the machine exited before it could be logged into: %s", lastLine(said))
}

// lastLine is s's last line, what a failing program usually says last.
func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Machine is one `spin-machine boot`, in a process group of its own so that QEMU goes with it.
type Machine struct {
	cmd    *exec.Cmd
	done   chan struct{}
	Stderr *bytes.Buffer
}

// Launch starts the tree's spin-machine with args. The machine is killed when m is cleaned up if
// nothing has stopped it.
// mutate-lane: TestEveryProbeMeasuresATree
func (m *Measurement) Launch(stdout io.Writer, args ...string) (*Machine, error) {
	cmd := exec.Command(m.Tree.SpinMachine(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if stdout == nil {
		cmd.Stdout = io.Discard
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching %s %s: %w", m.Tree.SpinMachine(), strings.Join(args, " "), err)
	}
	mc := &Machine{cmd: cmd, done: make(chan struct{}), Stderr: stderr}
	go func() { _ = cmd.Wait(); close(mc.done) }()
	m.Cleanup(mc.Stop)
	return mc, nil
}

// PID is QEMU's process: spin-machine boot's child, since boot starts QEMU through machine.Start,
// or boot's own pid in a release from before, whose boot exec'd QEMU in its place.
// mutate-lane: TestEveryProbeMeasuresATree
func (mc *Machine) PID() int { return qemuPID(mc.cmd.Process.Pid) }

// qemuPID is pid when it is QEMU, else its child that is. An exec'd QEMU can have a child too: the
// helper the old boot started before its exec, a zombie QEMU never reaps. Each thread lists the
// children it forked, and a Go program forks from whichever thread it is on, so every thread is
// read.
func qemuPID(pid int) int {
	if isQEMU(pid) {
		return pid
	}
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	for _, t := range tasks {
		b, _ := os.ReadFile(t)
		for _, f := range strings.Fields(string(b)) {
			if child, err := strconv.Atoi(f); err == nil && isQEMU(child) {
				return child
			}
		}
	}
	return pid
}

// isQEMU is whether pid runs a QEMU system emulator, by its name (comm, cut at 15 bytes).
func isQEMU(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.HasPrefix(string(b), "qemu-system")
}

// rssMiB is VmRSS in a /proc/PID/status, in MiB.
func rssMiB(status string) (float64, bool) { return statusMiB(status, "VmRSS") }

// statusMiB is the field of a /proc/PID/status given in kB, in MiB.
func statusMiB(status, field string) (float64, bool) {
	for line := range strings.Lines(status) {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			f := strings.Fields(v)
			if len(f) != 2 || f[1] != "kB" {
				return 0, false
			}
			kb, err := strconv.Atoi(f[0])
			return float64(kb) / 1024, err == nil
		}
	}
	return 0, false
}

// Done is closed once the machine's process has exited.
// mutate-lane: TestEveryProbeMeasuresATree
func (mc *Machine) Done() <-chan struct{} { return mc.done }

// Stop kills the machine's process group and waits for it.
// mutate-lane: TestEveryProbeMeasuresATree
func (mc *Machine) Stop() {
	select {
	case <-mc.done:
	default:
		_ = syscall.Kill(-mc.cmd.Process.Pid, syscall.SIGKILL)
		<-mc.done
	}
}

// SpinMachine runs the tree's spin-machine with args to completion.
// mutate-lane: TestEveryProbeMeasuresATree
func (m *Measurement) SpinMachine(args ...string) error {
	return run(m.Tree.SpinMachine(), args...)
}

// Probes are the probes there are, by name.
var Probes = map[string]Probe{
	"density":          density{},
	"initcalls":        initcalls{},
	"prestart":         prestart{},
	"restore-first":    restoreFirst{},
	"restore-together": restoreTogether{},
	"run-delay":        runDelay{},
	"ssh-first":        sshFirst{},
	"transport":        transport{},
}

// ProbeNames are the probes' names, sorted.
func ProbeNames() []string { return slices.Sorted(maps.Keys(Probes)) }

func removeAll(dir string) func() { return func() { _ = os.RemoveAll(dir) } }

var errNoTrees = errors.New("an experiment measures at least one tree")
