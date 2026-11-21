// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spin-stack/machine/qmp"
)

// StartOptions is what Start needs besides the Spec: the descriptors only the caller can open,
// where QEMU's output goes, and how the caller shapes the process.
type StartOptions struct {
	// Files are QEMU's descriptors 3, 4, … in this order: the ones the Spec names by number that
	// only the caller can open, such as a NIC's TapFD. Start opens the rest after them.
	Files []*os.File

	// Stdin, Stdout and Stderr are QEMU's. Stderr also goes to Exit, its last 4 KiB. A writer
	// that stops taking what QEMU writes stops QEMU at its next write, and holds its exit.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Console, when set, is where the guest's serial console goes, through a pipe Start hands
	// QEMU in place of Spec.Console: Start then reads the console, and Exit says when it showed
	// a panic. Unset, the console is Spec.Console's, and Exit says nothing of a panic.
	Console io.Writer

	// Observer is the monitor's, as qmp.New takes it: events from the greeting on.
	Observer qmp.Observer

	// Pressure, when set, is this machine's watch on the listener of the guests' pressure
	// (PressureListener.Watch): the machine's probe reports to it what its guest says over vsock,
	// what the PSI triggers on QEMU's cgroup say, and every QMP command on Start's monitor that is
	// slow (docs/machine.md, "Pressure"). QEMU's cgroup is read as it starts, so Cmd puts QEMU in
	// its own (SysProcAttr.UseCgroupFD), not a move after Start. Start refuses the watch for a Spec
	// with no vsock, for a QEMU in its caller's own cgroup, where the host's pressure would be every
	// machine's at once, and where the host's triggers cannot be armed, as by a caller that is not
	// root. A cgroup that several machines share passes, and its Stalls are all of theirs.
	Pressure *PressureWatch

	// Cmd shapes the process before it starts: its credentials, root, namespaces and cgroup
	// (SysProcAttr), its environment, more arguments. It leaves Stdin, Stdout, Stderr and
	// ExtraFiles as Start set them. Start opens every descriptor before Cmd runs and starts the
	// process on the calling goroutine, so a privilege dropped here is dropped after the opens,
	// and a caller that entered a namespace on a locked thread starts QEMU inside it.
	Cmd func(*exec.Cmd)
}

// Process is a QEMU that Start started: the machine, through the monitor Start connected, and the
// process under it, which Start's caller owns until it has exited.
type Process struct {
	*QEMU

	cmd    *exec.Cmd
	stderr *tail
	// console watches the console Start reads, when it reads one.
	console *panicWatch
	// sockets are the monitors' paths Start bound, removed once QEMU has exited.
	sockets []string
	// cgroup is the cgroup QEMU runs in, when it is not its caller's: what Usage reads.
	cgroup string
	// watch and host are the machine's probe, when StartOptions.Pressure set one: removed and
	// closed once QEMU has exited.
	watch *PressureWatch
	host  *hostTriggers

	// attached is the monitor once the handshake is over, or nil when it failed: the exit
	// reads the last SHUTDOWN off it.
	attached chan *QEMU
	exited   chan struct{}
	exit     Exit
	// stopping is held through a Stop, so that a second one waits for the first's end.
	stopping sync.Mutex
}

// Exit is how a QEMU ended.
type Exit struct {
	// State is the process's end as the kernel reported it: an exit status or a signal.
	State *os.ProcessState
	// End is the last SHUTDOWN QEMU sent on Start's monitor, or nil when it sent none, as a QEMU
	// killed sends none, or when the monitor was closed before it.
	End *End
	// Panic is the console's line saying the guest's kernel panicked, when Start read the
	// console (StartOptions.Console) and it said one. A panic otherwise ends as GuestReset.
	Panic string
	// Stderr is the last 4 KiB QEMU wrote on its stderr.
	Stderr string
}

// String is how QEMU ended and what it said last, as an error carries it.
func (e Exit) String() string {
	s := e.State.String()
	if e.End != nil {
		s += fmt.Sprintf(", the machine having ended (%s)", e.End.Reason)
	}
	if e.Panic != "" {
		s += ", the guest having panicked: " + e.Panic
	}
	if said := strings.TrimSpace(e.Stderr); said != "" {
		s += ": " + said
	}
	return s
}

const (
	// greetingWait bounds Start's handshake, which is over when QEMU's main loop answers
	// qmp_capabilities: once the machine is made, or never, for a QEMU that refused it and
	// exited, which ends the handshake at once.
	greetingWait = 30 * time.Second
	// answerWait bounds each command Stop sends. A QEMU loading a state answers none, and Stop
	// kills it after this long.
	answerWait = time.Second
	// powerOffWait is how long Stop leaves a running guest to power off once its button is
	// pressed. systemd took 0.64 s (QEMU.Shutdown).
	powerOffWait = 3 * time.Second
	// quitWait is how long Stop leaves QEMU to exit once it has asked it to.
	quitWait = time.Second
	// tailSize is how much of QEMU's stderr Exit keeps.
	tailSize = 4 << 10
)

// Start starts QEMU from s, connects to it and returns once its monitor answers: the machine is
// made and held, for Boot or for Resume. ctx bounds the start, never the process, which runs until
// it exits, Stop or Kill.
//
// Start opens and hands QEMU every descriptor s implies and o.Files does not hold, so QEMU opens no
// path itself and can run with no access to the host: a listening socket at each monitor's Socket,
// /dev/kvm under KVM, /dev/vhost-vsock for a vsock, /dev/vhost-net for each NIC, and the console
// when Spec.Console is a file: or StartOptions.Console is set. It adds a monitor of its own, which
// it binds and connects to before QEMU exists: the connect waits in the socket's backlog, QEMU
// accepts it at its start, and the greeting is the readiness, with nothing polled. A QEMU that exits
// before it answers fails Start at once, with what it said on its way out.
func Start(ctx context.Context, s Spec, o StartOptions) (*Process, error) {
	h := &handover{first: 3 + len(o.Files)}
	defer h.close()
	own, err := h.monitors(&s)
	if err != nil {
		return nil, err
	}
	if err := h.devices(&s, openDevice); err != nil {
		_ = own.Close()
		h.removeSockets()
		return nil, err
	}
	console, err := h.console(&s, o.Console)
	if err != nil {
		_ = own.Close()
		h.removeSockets()
		return nil, err
	}
	fail := func(err error) (*Process, error) {
		_ = own.Close()
		_ = console.Close() // a nil *os.File refuses the close, and there is nothing to close
		h.removeSockets()
		return nil, err
	}
	args, err := s.Args()
	if err != nil {
		return fail(err)
	}
	observer := o.Observer
	if o.Pressure != nil {
		if s.VsockCID == 0 {
			return fail(errors.New("watching a machine's pressure: its Spec has no vsock for its guest to report on"))
		}
		if err := o.Pressure.register(uint32(s.VsockCID)); err != nil {
			return fail(fmt.Errorf("watching a machine's pressure: %w", err))
		}
		observer = o.Pressure.observe(observer, s.Restore)
	}
	p := &Process{
		watch:    o.Pressure,
		stderr:   &tail{w: o.Stderr},
		sockets:  h.sockets,
		attached: make(chan *QEMU, 1),
		exited:   make(chan struct{}),
	}
	// #nosec G204 -- the binary and the command line of the caller's Spec
	p.cmd = exec.Command(s.QEMU, args...)
	p.cmd.ExtraFiles = append(slices.Clip(o.Files), h.files...)
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = o.Stdin, o.Stdout, p.stderr
	if o.Cmd != nil {
		o.Cmd(p.cmd)
	}
	if err := p.cmd.Start(); err != nil {
		if p.watch != nil {
			p.watch.unregister()
		}
		return fail(fmt.Errorf("starting %s: %w", s.QEMU, err))
	}
	h.close()
	// Before the reaper starts, which closes what is armed here once QEMU exits.
	cgroupErr := p.findCgroup()
	var armErr error
	if p.watch != nil {
		armErr = p.armHost(cgroupErr)
	}
	var read chan struct{}
	if console != nil {
		p.console = &panicWatch{w: o.Console}
		read = make(chan struct{})
		go func() {
			defer close(read)
			_, _ = io.Copy(p.console, console)
			_ = console.Close()
		}()
	}
	go p.wait(read)
	if armErr != nil {
		p.attached <- nil
		_ = own.Close()
		return nil, fmt.Errorf("watching the pressure of %s: %w; QEMU %s", s.QEMU, armErr, p.Kill())
	}

	hctx, cancel := context.WithTimeout(ctx, greetingWait)
	defer cancel()
	go func() {
		select {
		case <-p.exited:
			cancel()
		case <-hctx.Done():
		}
	}()
	c, err := qmp.New(hctx, qmp.Unix(own), observer)
	if err != nil {
		p.attached <- nil
		if hctx.Err() == nil {
			// The monitor ended under the handshake: QEMU is on its way out, and its exit says why.
			select {
			case <-p.exited:
			case <-time.After(answerWait):
			}
		}
		select {
		case <-p.exited:
			return nil, fmt.Errorf("starting %s: QEMU ended before its monitor answered: %s", s.QEMU, p.exit)
		default:
			return nil, fmt.Errorf("starting %s: its monitor did not answer: %w; QEMU %s", s.QEMU, err, p.Kill())
		}
	}
	p.QEMU = Attach(c)
	p.attached <- p.QEMU
	return p, nil
}

// wait reaps QEMU, and makes its Exit once all it said is in: its stderr, the console when Start
// reads it (read), and the last SHUTDOWN on its monitor.
func (p *Process) wait(read <-chan struct{}) {
	_ = p.cmd.Wait()
	if p.watch != nil {
		p.watch.unregister()
	}
	if read != nil {
		<-read
	}
	p.exit = Exit{State: p.cmd.ProcessState, Stderr: p.stderr.String()}
	if p.console != nil {
		p.exit.Panic = p.console.panicked()
	}
	if m := <-p.attached; m != nil {
		// QEMU's end closed the monitor, but the client may not have read its last lines yet.
		select {
		case <-m.Done():
		case <-time.After(answerWait):
		}
		if end, ok := m.End(); ok {
			p.exit.End = &end
		}
	}
	for _, s := range p.sockets {
		_ = os.Remove(s)
	}
	if p.host != nil {
		p.host.close()
	}
	close(p.exited)
}

// findCgroup sets the cgroup QEMU runs in, when it is not its caller's own: a cgroup QEMU shares
// with its caller, and with every machine the caller started, says nothing of this one.
// mutate-lane: TestThePressureProbeReportsEachPressure
func (p *Process) findCgroup() error {
	qemu, err := cgroupOf(strconv.Itoa(p.cmd.Process.Pid))
	if err != nil {
		return err
	}
	self, err := cgroupOf("self")
	if err != nil {
		return err
	}
	if qemu == self {
		return fmt.Errorf("QEMU is in its caller's cgroup %s, which every machine it started shares", qemu)
	}
	p.cgroup = qemu
	return nil
}

// armHost arms the triggers on QEMU's cgroup, found or not (cgroupErr).
// mutate-lane: TestThePressureProbeReportsEachPressure
func (p *Process) armHost(cgroupErr error) error {
	if cgroupErr != nil {
		return cgroupErr
	}
	h, err := armHost(p.cgroup, p.watch)
	if err != nil {
		return err
	}
	p.host = h
	return nil
}

// Pid is QEMU's process id.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Exited is closed once QEMU has exited and Wait has its Exit. Done, unlike it, is closed by the
// monitor's Close as well.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Wait returns how QEMU ended, once it has.
func (p *Process) Wait() Exit {
	<-p.exited
	return p.exit
}

// Kill ends QEMU at once, whatever it is doing, and returns how it ended: SIGKILL, unless it had
// exited already.
func (p *Process) Kill() Exit {
	select {
	case <-p.exited:
	default:
		_ = p.cmd.Process.Kill()
	}
	return p.Wait()
}

// Stop ends QEMU, asking first: a running guest's power button, then QEMU's quit, then SIGKILL,
// each step given a bounded time to end it (powerOffWait, quitWait) and ctx bounding all but the
// last. A monitor that does not answer within answerWait is a QEMU busy, as one loading a state
// is, or a monitor closed: it is killed. Stop returns how QEMU ended, and again on a second call.
func (p *Process) Stop(ctx context.Context) Exit {
	p.stopping.Lock()
	defer p.stopping.Unlock()
	select {
	case <-p.exited:
		return p.exit
	default:
	}
	actx, cancel := context.WithTimeout(ctx, answerWait)
	st, err := p.Status(actx)
	cancel()
	if err != nil {
		return p.Kill()
	}
	if st == Running && p.ask(ctx, p.Shutdown, powerOffWait) {
		return p.exit
	}
	if p.ask(ctx, p.Quit, quitWait) {
		return p.exit
	}
	return p.Kill()
}

// ask sends one of Stop's commands and says whether QEMU exited within d of it.
func (p *Process) ask(ctx context.Context, command func(context.Context) error, d time.Duration) bool {
	actx, cancel := context.WithTimeout(ctx, answerWait)
	err := command(actx)
	cancel()
	if err != nil {
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	select {
	case <-p.exited:
		return true
	case <-wctx.Done():
		return false
	}
}

// handover is what Start hands QEMU besides the caller's Files: the descriptors it opens, numbered
// from first, and the descriptor sets among them.
type handover struct {
	first int
	files []*os.File
	// sockets are the paths of the monitors bound for the caller.
	sockets []string
}

func (h *handover) add(f *os.File) int {
	h.files = append(h.files, f)
	return h.first + len(h.files) - 1
}

// set adds a descriptor set of f to s, and returns its id: one above every set s has.
func (h *handover) set(s *Spec, f *os.File, opaque string) int {
	id := 1
	for _, set := range s.FDSets {
		id = max(id, set.ID+1)
	}
	s.FDSets = append(slices.Clip(s.FDSets), FDSet{ID: id, FDs: []FD{{Num: h.add(f), Opaque: opaque}}})
	return id
}

// close closes Start's copies of what it hands QEMU: QEMU has its own once it has started.
func (h *handover) close() {
	for _, f := range h.files {
		_ = f.Close()
	}
	h.files = nil
}

func (h *handover) removeSockets() {
	for _, s := range h.sockets {
		_ = os.Remove(s)
	}
}

// monitors binds each monitor s names by path, which QEMU is then handed as a descriptor, and adds
// one of Start's own, already connected to: the returned connection.
//
// Start's own is bound in a directory of its own, made and removed here: the connection and the
// listener outlive their name, and nothing else can reach a monitor that has none.
func (h *handover) monitors(s *Spec) (*net.UnixConn, error) {
	s.Monitors = slices.Clone(s.Monitors)
	for i := range s.Monitors {
		m := &s.Monitors[i]
		if m.Socket == "" {
			continue
		}
		f, err := listen(m.Socket)
		if err != nil {
			h.removeSockets()
			return nil, err
		}
		h.sockets = append(h.sockets, m.Socket)
		m.FD, m.Socket = h.add(f), ""
	}
	// In /tmp and not under TMPDIR, which can be longer than the 108 bytes a socket's path has.
	dir, err := os.MkdirTemp("/tmp", "machine")
	if err != nil {
		h.removeSockets()
		return nil, fmt.Errorf("making a directory for the machine's monitor: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "m")
	f, err := listen(path)
	if err != nil {
		h.removeSockets()
		return nil, err
	}
	s.Monitors = append(s.Monitors, Monitor{FD: h.add(f)})
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		h.removeSockets()
		return nil, fmt.Errorf("connecting to the machine's monitor: %w", err)
	}
	return conn, nil
}

// listen is a listening socket at path, as a file to hand QEMU. A socket left at path by a
// machine before is replaced, and anything else there is an error.
func listen(path string) (*os.File, error) {
	if st, err := os.Lstat(path); err == nil && st.Mode().Type() == fs.ModeSocket {
		_ = os.Remove(path)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listening for the machine's monitor: %w", err)
	}
	l.SetUnlinkOnClose(false)
	defer func() { _ = l.Close() }()
	f, err := l.File()
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("handing over the monitor at %s: %w", path, err)
	}
	return f, nil
}

// devices opens each device node s needs that it was not handed: /dev/kvm under KVM,
// /dev/vhost-vsock for a vsock, and /dev/vhost-net for each NIC.
func (h *handover) devices(s *Spec, open func(string) (*os.File, error)) error {
	if s.accel() == "kvm" && s.KVMFDSet == 0 {
		f, err := open("/dev/kvm")
		if err != nil {
			return err
		}
		s.KVMFDSet = h.set(s, f, "/dev/kvm")
	}
	if s.VsockCID != 0 && s.VsockFD == 0 {
		f, err := open("/dev/vhost-vsock")
		if err != nil {
			return err
		}
		s.VsockFD = h.add(f)
	}
	s.NICs = slices.Clone(s.NICs)
	for i := range s.NICs {
		if s.NICs[i].VhostFD != 0 {
			continue
		}
		f, err := open("/dev/vhost-net")
		if err != nil {
			return err
		}
		s.NICs[i].VhostFD = h.add(f)
	}
	return nil
}

func openDevice(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) // mutate-exempt: the mode is read only with O_CREATE, which this has not
	if err != nil {
		return nil, fmt.Errorf("opening %s for the machine: %w", path, err)
	}
	return f, nil
}

// console hands QEMU the console as a descriptor set: a pipe whose other end it returns, for
// Start to read into w when w is set, or the file a file: console names, opened append-only.
// Any other console is QEMU's to open, and stays as s has it.
func (h *handover) console(s *Spec, w io.Writer) (*os.File, error) {
	if w != nil {
		r, pw, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("making the console's pipe: %w", err)
		}
		s.Console, s.ConsoleFDSet = "", h.set(s, pw, "console")
		return r, nil
	}
	path, ok := strings.CutPrefix(s.Console, "file:")
	if !ok {
		return nil, nil
	}
	// #nosec G304 -- the console the caller's Spec names
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the console %s for the machine: %w", path, err)
	}
	s.Console, s.ConsoleFDSet = "", h.set(s, f, path)
	return nil, nil
}

// tail is the last tailSize bytes written to it, passed on to w when w is set. A w that fails is
// left behind and the tail kept: QEMU's stderr is not stopped for it.
type tail struct {
	w      io.Writer
	mu     sync.Mutex
	b      []byte
	failed bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = t.b[len(t.b)-tailSize:]
	}
	if t.w != nil && !t.failed {
		_, err := t.w.Write(p)
		t.failed = err != nil
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// panicMark is how the guest's kernel starts the line that says it panicked.
const panicMark = "Kernel panic"

// panicWatch passes the console on to w and keeps the first line that says the kernel panicked.
type panicWatch struct {
	w io.Writer
	// line is the console's line so far, up to a length a panic's line fits in.
	line   []byte
	mu     sync.Mutex
	said   string
	failed bool
}

func (c *panicWatch) Write(p []byte) (int, error) {
	n := len(p)
	if !c.failed {
		_, err := c.w.Write(p)
		c.failed = err != nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.keep(p)
			break
		}
		c.keep(p[:i])
		if j := bytes.Index(c.line, []byte(panicMark)); j >= 0 {
			c.mu.Lock()
			if c.said == "" {
				c.said = strings.TrimSpace(string(c.line[j:]))
			}
			c.mu.Unlock()
		}
		c.line, p = c.line[:0], p[i+1:]
	}
	return n, nil
}

// keep adds b to the line so far, which is cut at 512 bytes: a panic's line is shorter.
func (c *panicWatch) keep(b []byte) {
	c.line = append(c.line, b[:min(len(b), 512-len(c.line))]...)
}

func (c *panicWatch) panicked() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.said
}
