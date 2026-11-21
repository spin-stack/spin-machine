// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/machine/internal/pressure"
	"github.com/spin-stack/machine/qmp"
)

// Pressure is what a machine's probe reports as it happens: a Stall, an OOMKill, a SlowCommand,
// or GuestLost. docs/machine.md, "Pressure", says what each means and which say a machine suffers
// and which say it causes.
type Pressure interface {
	// When is when the host learned of it.
	When() time.Time
	pressure()
}

// Resource is what a machine stalls on: CPUPressure, MemoryPressure or IOPressure. Its String
// is PSI's name for it: cpu, memory, io.
type Resource = pressure.Resource

const (
	CPUPressure    = pressure.CPU
	MemoryPressure = pressure.Memory
	IOPressure     = pressure.IO
)

// Vantage is where a Stall was seen from.
type Vantage int

const (
	// Guest is the guest kernel's own PSI: its tasks waited for a vCPU, for memory it does not
	// have, or for its disks. What a guest short of what it was given feels. It does not see the
	// host taking its vCPUs away: that is time the guest does not know it lost.
	Guest Vantage = iota + 1
	// Host is the PSI of the cgroup QEMU runs in: QEMU's threads, vCPUs included, waited for the
	// host's CPU, memory or IO. What the host does to the machine, steal included.
	Host
)

func (v Vantage) String() string {
	switch v {
	case Guest:
		return "guest"
	case Host:
		return "host"
	}
	return fmt.Sprintf("vantage(%d)", int(v))
}

// Stall is a PSI trigger that fired: tasks stalled on Resource for at least Threshold within
// Window, seen from Seen.
type Stall struct {
	At                time.Time
	Seen              Vantage
	Resource          Resource
	Threshold, Window time.Duration
	// Total is the resource's "some" stall so far, from the guest's boot or the cgroup's making:
	// two Stalls of one vantage and resource say how much stall fell between them.
	Total time.Duration
}

// OOMKill is the guest's OOM killer having killed Kills processes since the last report. A fast
// OOM can kill before any PSI trigger fires (two 1000 MiB hogs did, 2026-11-21).
type OOMKill struct {
	At    time.Time
	Kills uint64
}

// SlowCommand is a QMP command on Start's monitor that took Took, at least slowCommand and
// slowFactor times Baseline, what that command took before. A machine loading or saving a state
// answers no command until it is done, and nothing then is a SlowCommand.
type SlowCommand struct {
	At       time.Time
	Command  string
	Took     time.Duration
	Baseline time.Duration
}

// GuestLost is the guest's sender gone: its connection ended, or said something that is not a
// pressure line. The guest's vantage is blind until it dials again, which it does by itself.
type GuestLost struct {
	At  time.Time
	Err error
}

func (s Stall) When() time.Time       { return s.At }
func (o OOMKill) When() time.Time     { return o.At }
func (c SlowCommand) When() time.Time { return c.At }
func (g GuestLost) When() time.Time   { return g.At }
func (Stall) pressure()               {}
func (OOMKill) pressure()             {}
func (SlowCommand) pressure()         {}
func (GuestLost) pressure()           {}

const (
	// slowCommand and slowFactor make a command slow. Idle, query-status answered in 74 µs, and
	// in at most 7 ms under a guest's CPU load; small synced writes on a contended host disk
	// held the monitor 175-950 ms (laptop, 2026-11-21, #391).
	slowCommand = 100 * time.Millisecond
	slowFactor  = 4
)

// PressureListener is the host's one listener for the guests' pressure senders, on vsock port
// pressure.Port: a host port is bound once per host, so the listener is the caller's, made once
// and handed to every Start through a PressureWatch. It routes each connection by the CID the
// host's kernel reports for its peer, which a guest cannot choose.
type PressureListener struct {
	f *os.File

	mu     sync.Mutex
	byCID  map[uint32]*PressureWatch
	closed bool
}

// ListenPressure binds the host's vsock port for the guests' senders, and accepts on it until
// Close.
// mutate-lane: TestThePressureProbeReportsEachPressure
func ListenPressure() (*PressureListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("making a vsock socket for the guests' pressure: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: pressure.Port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("binding vsock port %d for the guests' pressure: %w", pressure.Port, err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listening on vsock port %d for the guests' pressure: %w", pressure.Port, err)
	}
	l := &PressureListener{f: os.NewFile(uintptr(fd), "vsock-pressure"), byCID: map[uint32]*PressureWatch{}}
	go l.accept()
	return l, nil
}

// Close stops accepting and ends every connection: the machines watched see no more of their
// guests.
func (l *PressureListener) Close() error {
	err := l.f.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for _, w := range l.byCID {
		if w.conn != nil {
			_ = w.conn.Close()
		}
	}
	return err
}

// Watch is one machine's watch, for StartOptions.Pressure. notify is told every Pressure the
// machine meets, from more than one goroutine, and must not block: the probe's readers wait for it.
func (l *PressureListener) Watch(notify func(Pressure)) *PressureWatch {
	return &PressureWatch{l: l, notify: notify}
}

// PressureWatch is one machine's watch: its notify, and what it last heard of its guest. Start
// registers it under the Spec's vsock CID and removes it once QEMU has exited; one Start may use it.
type PressureWatch struct {
	l      *PressureListener
	notify func(Pressure)

	// cid, conn and ended are the listener's, under its lock: the CID the machine is watched
	// under, its guest's one connection, and a channel closed once QEMU has exited.
	cid   uint32
	conn  *os.File
	ended chan struct{}

	mu sync.Mutex
	// oom is the guest's oom_kill as last heard, and heard whether there is one yet.
	oom   uint64
	heard bool
	// baseline is each command's usual time on Start's monitor.
	baseline map[string]time.Duration
	// busy is whether the machine is loading or saving a state.
	busy bool
}

func (w *PressureWatch) register(cid uint32) error {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	if w.cid != 0 {
		return errors.New("the pressure watch is another machine's: one Start takes one watch")
	}
	if w.notify == nil {
		return errors.New("the pressure watch has nothing to tell")
	}
	if _, ok := w.l.byCID[cid]; ok {
		return fmt.Errorf("vsock CID %d is watched for another machine already", cid)
	}
	w.cid, w.ended = cid, make(chan struct{})
	w.l.byCID[cid] = w
	return nil
}

// unregister is QEMU's exit: the CID is free for another machine, and the guest's connection,
// which vhost-vsock resets as QEMU exits, ends as no GuestLost.
func (w *PressureWatch) unregister() {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	if w.l.byCID[w.cid] == w {
		delete(w.l.byCID, w.cid)
		close(w.ended)
	}
}

// mutate-lane: TestThePressureProbeReportsEachPressure
func (l *PressureListener) accept() {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return
	}
	for {
		var fd int
		var peer unix.Sockaddr
		var aerr error
		err := rc.Read(func(s uintptr) bool {
			fd, peer, aerr = unix.Accept4(int(s), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
			return !errors.Is(aerr, unix.EAGAIN)
		})
		if err != nil {
			return // closed
		}
		if aerr != nil {
			// Out of descriptors or memory: the next accept would fail the same way at once.
			if !errors.Is(aerr, unix.ECONNABORTED) && !errors.Is(aerr, unix.EINTR) {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		c := os.NewFile(uintptr(fd), "vsock-pressure-conn")
		vm, _ := peer.(*unix.SockaddrVM)
		w := (*PressureWatch)(nil)
		if vm != nil {
			w = l.take(vm.CID, c)
		}
		if w == nil {
			_ = c.Close()
			continue
		}
		go l.serve(c, w)
	}
}

// take makes c the one connection of the machine watched under cid, ending the one before: a
// guest dials again after a restore's reset, and a guest dialling again and again holds one
// descriptor of the host, not one each. It returns the machine's watch, or nil for a CID nobody
// watches or once the listener is closed.
func (l *PressureListener) take(cid uint32, c *os.File) *PressureWatch {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byCID[cid]
	if w == nil || l.closed {
		return nil
	}
	if w.conn != nil {
		_ = w.conn.Close()
	}
	w.conn = c
	return w
}

// serve reads the guest's connection c until it ends, and tells its machine the guest is lost
// unless the connection was replaced, the machine has exited, or the listener closed.
func (l *PressureListener) serve(c *os.File, w *PressureWatch) {
	err := w.read(c)
	_ = c.Close()
	l.mu.Lock()
	current := w.conn == c && !l.closed
	l.mu.Unlock()
	if !current {
		return
	}
	// QEMU's exit resets the connection a moment before its reaper sees the exit, and closes ended.
	select {
	case <-w.ended:
		return
	case <-time.After(answerWait):
	}
	w.notify(GuestLost{At: time.Now(), Err: err})
}

// read takes the guest's lines off c until it ends, and says why it ended.
func (w *PressureWatch) read(c io.Reader) error {
	in := bufio.NewReaderSize(c, pressure.MaxLine)
	first := true
	for {
		line, err := in.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return fmt.Errorf("the guest sent a line longer than %d bytes", pressure.MaxLine)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("the guest closed its connection")
			}
			return fmt.Errorf("reading the guest's pressure: %w", err)
		}
		l, err := pressure.Parse(bytes.TrimSuffix(line, []byte("\n")))
		if err != nil {
			return err
		}
		if _, hello := l.(pressure.Hello); first != hello {
			return fmt.Errorf("the guest's first line is not its hello, or a hello came again: %q", line)
		}
		first = false
		w.heardGuest(l, time.Now())
	}
}

// heardGuest turns a guest's line into what it tells the caller.
func (w *PressureWatch) heardGuest(l pressure.Line, at time.Time) {
	switch l := l.(type) {
	case pressure.Hello:
		w.kills(l.OOMKills, at)
	case pressure.OOM:
		w.kills(l.Kills, at)
	case pressure.Stall:
		w.notify(Stall{At: at, Seen: Guest, Resource: l.Resource, Threshold: l.Threshold, Window: l.Window, Total: l.Total})
	}
}

// kills reports the kills since the count last heard. The first count heard is the base: a machine
// resumed from a checkpoint hears the kills its guest had before it, which were the machine's
// before it to report.
func (w *PressureWatch) kills(n uint64, at time.Time) {
	w.mu.Lock()
	news := uint64(0)
	if w.heard && n > w.oom {
		news = n - w.oom
	}
	w.oom, w.heard = n, true
	w.mu.Unlock()
	if news > 0 {
		w.notify(OOMKill{At: at, Kills: news})
	}
}

// observe wraps the caller's Observer: every command is timed, and the migration's events say
// when the machine is loading or saving a state. restore is whether it starts loading one. A guest
// booted cold has killed nothing yet, so a kill before its sender first dials is news; a guest
// resumed had kills its machine before reported, so its first count is the base.
func (w *PressureWatch) observe(o qmp.Observer, restore bool) qmp.Observer {
	w.busy = restore
	w.heard = !restore
	w.baseline = map[string]time.Duration{}
	command, event := o.Command, o.Event
	o.Command = func(ctx context.Context, name string) (context.Context, func(error)) {
		start, busy := time.Now(), w.isBusy()
		end := func(error) {}
		if command != nil {
			ctx, end = command(ctx, name)
		}
		return ctx, func(err error) {
			end(err)
			w.timed(name, time.Since(start), busy || w.isBusy(), time.Now())
		}
	}
	o.Event = func(e qmp.Event) {
		if e.Name == "MIGRATION" {
			var m migrationNews
			if json.Unmarshal(e.Data, &m) == nil {
				w.mu.Lock()
				w.busy = m.Status != completed && !ended(m.Status)
				w.mu.Unlock()
			}
		}
		if event != nil {
			event(e)
		}
	}
	return o
}

func (w *PressureWatch) isBusy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// timed takes what one command took. A command is slow against what it took before, so a command
// never answered before is no SlowCommand: what spin asks again and again is what a stall shows
// in, and a command asked once has no usual time to be slower than.
func (w *PressureWatch) timed(name string, took time.Duration, busy bool, at time.Time) {
	if busy {
		return
	}
	w.mu.Lock()
	base, ok := w.baseline[name]
	slow := ok && took >= slowCommand && took >= slowFactor*base
	if !slow {
		if ok {
			w.baseline[name] = base + (took-base)/8
		} else {
			w.baseline[name] = took
		}
	}
	w.mu.Unlock()
	if slow {
		w.notify(SlowCommand{At: at, Command: name, Took: took, Baseline: base})
	}
}

// hostTriggers are the PSI triggers on the cgroup QEMU runs in, and the pipe that stops their
// watch.
type hostTriggers struct {
	triggers []pressure.Trigger
	stop     *os.File
	done     chan struct{}
}

// armHost arms the cgroup's triggers and watches them until close. A cgroup's pressure files take
// a trigger with a window under 2 s from root alone (EACCES otherwise, 2026-11-21).
// mutate-lane: TestThePressureProbeReportsEachPressure
func armHost(cgroup string, w *PressureWatch) (*hostTriggers, error) {
	h := &hostTriggers{done: make(chan struct{})}
	for _, r := range pressure.Resources() {
		t, err := pressure.Arm(filepath.Join(cgroup, r.String()+".pressure"), r)
		if err != nil {
			h.closeFiles()
			return nil, err
		}
		h.triggers = append(h.triggers, t)
	}
	r, stop, err := os.Pipe()
	if err != nil {
		h.closeFiles()
		return nil, fmt.Errorf("making the host triggers' pipe: %w", err)
	}
	h.stop = stop
	go h.watch(r, w)
	return h, nil
}

// mutate-lane: TestThePressureProbeReportsEachPressure
func (h *hostTriggers) watch(stop *os.File, w *PressureWatch) {
	defer close(h.done)
	defer func() { _ = stop.Close() }()
	fds := make([]unix.PollFd, len(h.triggers)+1)
	for {
		for i, t := range h.triggers {
			fds[i] = unix.PollFd{Fd: int32(t.File.Fd()), Events: unix.POLLPRI}
		}
		fds[len(h.triggers)] = unix.PollFd{Fd: int32(stop.Fd()), Events: unix.POLLIN}
		if _, err := unix.Poll(fds, -1); err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		if fds[len(h.triggers)].Revents != 0 {
			return
		}
		at := time.Now()
		for i, t := range h.triggers {
			if fds[i].Revents&unix.POLLERR != 0 {
				return // the cgroup is gone
			}
			if fds[i].Revents&unix.POLLPRI == 0 {
				continue
			}
			total, err := t.Total()
			if err != nil {
				continue
			}
			w.notify(Stall{At: at, Seen: Host, Resource: t.Resource, Threshold: pressure.Threshold, Window: pressure.Window, Total: total})
		}
	}
}

// close stops the watch and closes the triggers, once the watch has stopped using them.
func (h *hostTriggers) close() {
	_ = h.stop.Close()
	<-h.done
	h.closeFiles()
}

func (h *hostTriggers) closeFiles() {
	for _, t := range h.triggers {
		_ = t.File.Close()
	}
}

// cgroupOf is the cgroup v2 directory process pid is in, under /sys/fs/cgroup.
func cgroupOf(pid string) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	if err != nil {
		return "", fmt.Errorf("reading the cgroup of process %s: %w", pid, err)
	}
	for line := range strings.Lines(string(b)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", p), nil
		}
	}
	return "", fmt.Errorf("process %s is in no cgroup v2: %q", pid, b)
}
