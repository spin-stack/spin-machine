// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// runDelay is the most a higher priority for QEMU could give either goal number: the time QEMU's
// threads spent runnable on the host and not running, between the moment before its exec and the
// first login. A higher weight, a nice value or a real-time class only changes who runs when two
// threads want one CPU, so it can win back that wait and nothing else.
//
// Each boot and restore samples /proc/PID/task/TID/schedstat (time on a CPU, time runnable and
// waiting for one: the scheduler's run_delay) of every QEMU thread every schedEvery, keeping the
// last sample of a thread that exits, and splits the wait at the guest's first run, from QEMU's
// trace (tracedQEMU). The guest's steal time is read after the login as a check: KVM reports a
// vCPU thread's run_delay to the guest as steal, so the two should agree. The host's own steal is
// what the bench fleet's hypervisor took from the host's CPUs meanwhile, out of reach of any
// priority set on the host: the bench is nested.
//
// Each row is a crowd of workspaces already running, as density boots them, so the wait can be
// read on an idle host and a busy one in one run.
type runDelay struct{}

func (runDelay) Name() string { return "run-delay" }
func (runDelay) About() string {
	return "ms QEMU's threads waited for a CPU between the moment before its exec and the first login, cold and restored, with the row's crowd running"
}

func (runDelay) Options() []Option {
	return append(shape,
		Option{"crowds", "0 16", "the numbers of workspaces already running, a row each: 16 busy ones fill the bench host's 16 CPUs"},
		Option{"load", "cpu", "what the running ones do: idle, or cpu (each vCPU busy)"})
}

func (runDelay) Rows(o Options) []string { return o.Fields("crowds") }
func (runDelay) Phases() int             { return 2 }

func (runDelay) Needs(o Options) []Need {
	return append(append(bootNeeds, Need{"boot", "vsock-cid"}, Need{"boot", "qmp"}, Need{"boot", "restore"}, Need{"boot", "qemu"},
		Need{"save", "to"}, Need{"restore", "disk"}), plugNeeds(o)...)
}

func (runDelay) Metrics() []Metric {
	var out []Metric
	for _, p := range []struct{ name, what string }{{"cold", "the cold boot"}, {"restore", "the restore, evicted"}} {
		out = append(out,
			Metric{p.name + "_login", "ms", p.what + "'s first login, from before QEMU's exec (QEMU traced)"},
			Metric{p.name + "_wait", "ms", "QEMU's threads' run_delay summed, from the exec to the first login: a priority change's ceiling"},
			Metric{p.name + "_wait_before", "ms", "of it, before the guest first ran"},
			Metric{p.name + "_wait_vcpu", "ms", "of it, the vCPU threads'"},
			Metric{p.name + "_ran", "ms", "QEMU's threads' time on a CPU, summed, from the exec to the first login"},
			Metric{p.name + "_preempted", "count", "the vCPU threads' involuntary context switches"},
			Metric{p.name + "_threads", "count", "QEMU's threads seen"},
			Metric{p.name + "_wait_spin", "ms", "spin-machine boot's own threads' run_delay over the same time"},
			Metric{p.name + "_steal_guest", "ms", "the guest's steal time at the first login, in 10 ms ticks"},
			Metric{p.name + "_steal_host", "ms", "the host's steal time over the same time, summed over its CPUs, in 10 ms ticks"},
		)
	}
	return out
}

// mutate-lane: TestEveryProbeMeasuresATree
func (runDelay) Measure(_ context.Context, m *Measurement) error {
	n, err := strconv.Atoi(m.Row)
	if err != nil || n < 0 {
		return fmt.Errorf("a crowd of %q workspaces is not a number of them", m.Row)
	}
	stop, err := m.bootCrowd(n, m.Options["load"])
	defer stop()
	if err != nil {
		return err
	}
	if m.Phase == 0 {
		return runDelayCold(m)
	}
	return runDelayRestore(m)
}

// delayCheckpoint is a checkpoint with the guest's steal time when it was saved, from which the
// restored guest's counts on.
type delayCheckpoint struct {
	checkpoint
	steal time.Duration
}

// mutate-lane: TestEveryProbeMeasuresATree
func runDelayCold(m *Measurement) error {
	overlay, err := m.Overlay("overlay.qcow2", m.Key.Edits()...)
	if err != nil {
		return err
	}
	traced, err := tracedQEMU(m, m.Dir)
	if err != nil {
		return err
	}
	c := &delayCheckpoint{checkpoint: checkpoint{overlay: overlay, state: filepath.Join(m.Dir, "state"), cid: NewCID()}}
	sock := filepath.Join(m.Socks, "cold.qmp")
	host := hostSteal()
	t0 := time.Now()
	mc, err := m.Launch(nil, append(m.BootArgs(overlay, c.bootFlags(m, sock)...), traced)...)
	if err != nil {
		return err
	}
	defer mc.Stop()
	w := watchSched(mc.cmd.Process.Pid)
	plugged := m.Plug(sock)
	login, err := m.Key.LoggedIn(mc, c.cid, t0)
	s := w.end()
	if err != nil {
		return err
	}
	stealHost := hostSteal() - host
	if err := plugged(); err != nil {
		return err
	}
	if c.steal, err = guestSteal(m, c.cid); err != nil {
		return err
	}
	if err := m.SpinMachine("save", "--qmp", sock, "--to", c.state); err != nil {
		return err
	}
	select {
	case <-mc.Done():
	case <-time.After(30 * time.Second):
		return Miss("QEMU was still running 30s after its state was saved")
	}
	if err := evict(c.state); err != nil {
		return err
	}
	t, err := readQMPTrace(m.Dir, s.qemu)
	if err != nil {
		return err
	}
	if err := recordDelay(m, "cold", login, s, t, c.steal, stealHost); err != nil {
		return err
	}
	m.State = c
	return nil
}

// mutate-lane: TestEveryProbeMeasuresATree
func runDelayRestore(m *Measurement) error {
	c := m.State.(*delayCheckpoint)
	if err := evict(c.state); err != nil {
		return err
	}
	dir := filepath.Join(m.Dir, "restored")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making a directory for the restore's trace: %w", err)
	}
	traced, err := tracedQEMU(m, dir)
	if err != nil {
		return err
	}
	sock := filepath.Join(m.Socks, "restored.qmp")
	host := hostSteal()
	t0 := time.Now()
	mc, err := m.Launch(nil, append(m.BootArgs(c.overlay, c.bootFlags(m, sock, "--restore")...), traced)...)
	if err != nil {
		return err
	}
	defer mc.Stop()
	w := watchSched(mc.cmd.Process.Pid)
	if err := waitSocket(sock, t0.Add(30*time.Second)); err != nil {
		w.end()
		return Miss("the restoring QEMU never opened its monitor: %v; spin-machine: %s", err, strings.TrimSpace(mc.Stderr.String()))
	}
	if err := m.SpinMachine(c.restoreArgs(m, sock)...); err != nil {
		w.end()
		return err
	}
	login, err := m.Key.LoggedIn(mc, c.cid, t0)
	s := w.end()
	if err != nil {
		return err
	}
	stealHost := hostSteal() - host
	steal, err := guestSteal(m, c.cid)
	if err != nil {
		return err
	}
	mc.Stop()
	t, err := readQMPTrace(dir, s.qemu)
	if err != nil {
		return err
	}
	return recordDelay(m, "restore", login, s, t, steal-c.steal, stealHost)
}

// recordDelay records path's metrics from what the watch saw, split at the guest's first run in
// QEMU's trace, and each thread name's wait as detail.
func recordDelay(m *Measurement, path string, login time.Duration, s schedSummary, t qmpTrace, stealGuest, stealHost time.Duration) error {
	if s.threads == 0 {
		return Miss("no thread of QEMU was seen")
	}
	if s.waited == 0 && s.ran == 0 {
		return Miss("QEMU's threads show no time at all: this host's /proc/PID/schedstat counts nothing")
	}
	if t.Running.IsZero() {
		return Miss("QEMU's trace holds no guest running")
	}
	m.RecordMs(path+"_login", login)
	m.RecordMs(path+"_wait", s.waited)
	m.RecordMs(path+"_wait_before", s.waitedBy(t.Running))
	m.RecordMs(path+"_wait_vcpu", s.vcpuWaited)
	m.RecordMs(path+"_ran", s.ran)
	m.Record(path+"_preempted", s.preempted)
	m.Record(path+"_threads", float64(s.threads))
	m.RecordMs(path+"_wait_spin", s.spinWaited)
	m.RecordMs(path+"_steal_guest", stealGuest)
	m.RecordMs(path+"_steal_host", stealHost)
	for name, d := range s.byName {
		m.Detail(path+"\x00"+name, ms(d))
	}
	m.Logf("%s: login %v, QEMU waited %v (%v of it before the guest ran, %v the vCPUs'), ran %v",
		path, login.Round(time.Millisecond), s.waited.Round(10*time.Microsecond),
		s.waitedBy(t.Running).Round(10*time.Microsecond), s.vcpuWaited.Round(10*time.Microsecond), s.ran.Round(10*time.Microsecond))
	return nil
}

// Detail is which of QEMU's threads waited: the p50 of each thread name's run_delay, in ms, by
// tree, row and path, the names that waited longest first.
func (runDelay) Detail(r *Result) string {
	var b strings.Builder
	for _, t := range r.Trees {
		for _, row := range r.Rows {
			waits := map[string][]float64{}
			for _, s := range r.Samples {
				if s.Tree != t.Name || s.Row != row {
					continue
				}
				for k, v := range s.Detail {
					waits[k] = append(waits[k], v)
				}
			}
			if len(waits) == 0 {
				continue
			}
			type named struct {
				key string
				p50 float64
			}
			var rows []named
			for k, v := range waits {
				p, _ := Percentile(v, .5)
				rows = append(rows, named{k, p})
			}
			slices.SortFunc(rows, func(a, b named) int {
				return cmp.Or(strings.Compare(strings.Split(a.key, "\x00")[0], strings.Split(b.key, "\x00")[0]), cmp.Compare(b.p50, a.p50))
			})
			fmt.Fprintf(&b, "\n  %s, crowd %s: p50 ms each of QEMU's threads waited for a CPU, by name\n", t.Name, row)
			for _, n := range rows {
				path, name, _ := strings.Cut(n.key, "\x00")
				fmt.Fprintf(&b, "    %-8s %-24s %8.3f\n", path, name, n.p50)
			}
		}
	}
	return b.String()
}

// schedEvery is how often the watch samples. A thread that exits between two samples keeps the
// time of its last one, so a short-lived thread's last few ms can be missed; QEMU's vCPU, main and
// monitor threads live as long as it does.
const schedEvery = 5 * time.Millisecond

// threadSched is one thread's accounting by the host's scheduler: /proc/PID/task/TID/schedstat's
// time on a CPU and time runnable without one (run_delay), in ns.
type threadSched struct {
	name        string
	ran, waited time.Duration
}

// schedPoint is the wait of every QEMU thread seen so far, the exited ones at their last sample, at
// one sample.
type schedPoint struct {
	at     time.Time
	waited time.Duration
}

// schedWatch samples the threads of a spin-machine boot (pid boot) and of the QEMU it starts, from
// its start to end.
type schedWatch struct {
	boot int
	stop chan struct{}
	done chan schedSummary
}

// schedSummary is what a watch saw, each thread at its last sample.
type schedSummary struct {
	qemu        int // QEMU's pid, or 0 when none was found
	threads     int
	ran, waited time.Duration
	vcpuWaited  time.Duration
	preempted   float64
	spinWaited  time.Duration
	byName      map[string]time.Duration
	points      []schedPoint
}

// waitedBy is the wait counted at the last sample at or before at.
func (s schedSummary) waitedBy(at time.Time) time.Duration {
	var d time.Duration
	for _, p := range s.points {
		if p.at.After(at) {
			break
		}
		d = p.waited
	}
	return d
}

// watchSched starts sampling boot's threads and its QEMU's.
// mutate-lane: TestEveryProbeMeasuresATree
func watchSched(boot int) *schedWatch {
	w := &schedWatch{boot: boot, stop: make(chan struct{}), done: make(chan schedSummary, 1)}
	go w.run()
	return w
}

// end takes a last sample and returns what the watch saw.
// mutate-lane: TestEveryProbeMeasuresATree
func (w *schedWatch) end() schedSummary {
	close(w.stop)
	return <-w.done
}

// mutate-lane: TestEveryProbeMeasuresATree
func (w *schedWatch) run() {
	qemu := 0
	threads := map[int]threadSched{}
	spin := map[int]threadSched{}
	var points []schedPoint
	sample := func() {
		if qemu == 0 {
			if p := qemuPID(w.boot); p != w.boot || isQEMU(p) {
				qemu = p
			}
		}
		for tid, t := range taskScheds(w.boot) {
			spin[tid] = t
		}
		if qemu == 0 {
			return
		}
		for tid, t := range taskScheds(qemu) {
			threads[tid] = t
		}
		var waited time.Duration
		for _, t := range threads {
			waited += t.waited
		}
		points = append(points, schedPoint{at: time.Now(), waited: waited})
	}
	tick := time.NewTicker(schedEvery)
	defer tick.Stop()
	for {
		sample()
		select {
		case <-w.stop:
			sample()
			w.done <- summarize(qemu, threads, spin, points, preemptions(qemu, threads))
			return
		case <-tick.C:
		}
	}
}

// summarize sums what the watch saw: QEMU's threads, the vCPUs' among them, and spin-machine's.
func summarize(qemu int, threads, spin map[int]threadSched, points []schedPoint, preempted float64) schedSummary {
	s := schedSummary{qemu: qemu, threads: len(threads), preempted: preempted, points: points, byName: map[string]time.Duration{}}
	for _, t := range threads {
		s.ran += t.ran
		s.waited += t.waited
		if isVCPU(t.name) {
			s.vcpuWaited += t.waited
		}
		s.byName[t.name] += t.waited
	}
	for _, t := range spin {
		s.spinWaited += t.waited
	}
	return s
}

// isVCPU is whether a QEMU thread's name is a vCPU's: KVM's are "CPU N/KVM".
func isVCPU(name string) bool {
	return strings.HasPrefix(name, "CPU ") && strings.HasSuffix(name, "/KVM")
}

// taskScheds is every live thread of pid, by tid.
func taskScheds(pid int) map[int]threadSched {
	out := map[int]threadSched{}
	dirs, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*", pid))
	for _, d := range dirs {
		tid, err := strconv.Atoi(filepath.Base(d))
		if err != nil {
			continue
		}
		b, err := os.ReadFile(d + "/schedstat")
		if err != nil {
			continue // exited since the glob
		}
		ran, waited, ok := parseSchedstat(string(b))
		if !ok {
			continue
		}
		name, _ := os.ReadFile(d + "/comm")
		out[tid] = threadSched{name: strings.TrimSpace(string(name)), ran: ran, waited: waited}
	}
	return out
}

// parseSchedstat reads a /proc/PID/task/TID/schedstat: "ran waited timeslices", the times in ns.
func parseSchedstat(s string) (ran, waited time.Duration, ok bool) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return 0, 0, false
	}
	r, err1 := strconv.ParseInt(f[0], 10, 64)
	w, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return time.Duration(r), time.Duration(w), true
}

// preemptions is the involuntary context switches of qemu's vCPU threads still running.
func preemptions(qemu int, threads map[int]threadSched) float64 {
	var n float64
	for tid, t := range threads {
		if !isVCPU(t.name) {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/status", qemu, tid))
		if err != nil {
			continue
		}
		for line := range strings.Lines(string(b)) {
			if v, ok := strings.CutPrefix(line, "nonvoluntary_ctxt_switches:"); ok {
				c, _ := counted(strings.TrimSpace(v))
				n += c
			}
		}
	}
	return n
}

// statSteal is the steal column of /proc/stat's first line (user nice system idle iowait irq
// softirq steal), in USER_HZ ticks of 10 ms, as a duration.
func statSteal(stat string) (time.Duration, bool) {
	first, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(first)
	if len(f) < 9 || f[0] != "cpu" {
		return 0, false
	}
	ticks, ok := counted(f[8])
	return time.Duration(ticks) * 10 * time.Millisecond, ok
}

// hostSteal is the host's steal time, summed over its CPUs.
func hostSteal() time.Duration {
	d, _ := statSteal(readFileOrEmpty("/proc/stat"))
	return d
}

// guestSteal is the steal time the guest on cid has counted since it booted, summed over its CPUs.
// mutate-lane: TestEveryProbeMeasuresATree
func guestSteal(m *Measurement, cid uint32) (time.Duration, error) {
	_, out, err := m.Key.Run(cid, xssh.KeyExchangeMLKEM768X25519, "head -1 /proc/stat")
	if err != nil {
		return 0, fmt.Errorf("reading the guest's steal time: %w", err)
	}
	d, ok := statSteal(out)
	if !ok {
		return 0, Miss("the guest's /proc/stat has no steal column: %q", out)
	}
	return d, nil
}
