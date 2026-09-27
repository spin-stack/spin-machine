// SPDX-License-Identifier: Apache-2.0

// Command spin-machine starts a VM of the machine this repository defines, and
// prints what that machine is.
//
// It exists for two reasons. The first is that a definition nothing executes
// drifts: the package next door says which chipset, which slots and which kernel
// command line, and this is what proves those still boot. The second is that it
// gives this repository a way to run its own artefacts without borrowing a
// runtime from somewhere else — `boot` is what `task shell` runs.
//
// It is also how a check of this machine is done by hand. Whatever a caller does
// to a running machine through the contract this repository promises — a disk on
// a hotplug port, a save to a file — is a command here, so trying it needs no
// script of its own.
//
// It is not a container runtime and does not want to become one. There is no
// supervision and no lifecycle: `boot` execs QEMU and gets out of the way, and the
// commands that act on a running VM are one QMP conversation each.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// main is the only place the process ends. A command returns what happened and main
// decides what it means: help asked for is not a failure, and QEMU's own exit status is
// passed on as it was, since QEMU has already said why on stderr.
func main() {
	err := run(os.Args[1:])
	var code exitCode
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.As(err, &code):
		os.Exit(int(code))
	default:
		fmt.Fprintf(os.Stderr, "spin-machine: %v\n", err)
		os.Exit(1)
	}
}

// exitCode is a failure that has already been reported by the process that failed, and
// has only a status left to pass on.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

const usage = `spin-machine - the machine this repository builds

Usage:
  spin-machine boot        [flags]   start a VM and wait for it
  spin-machine args        [flags]   print the QEMU command line boot would run
  spin-machine fingerprint [flags]   print the machine's identity
  spin-machine attach      [flags]   give a running VM a disk on a hotplug port
  spin-machine detach      [flags]   take it back, once the guest has let it go
  spin-machine save        [flags]   stop a running VM and write its state to a file

spin-machine <command> -h lists a command's flags.
`

func run(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	cmd, argv := argv[0], argv[1:]

	switch cmd {
	case "boot", "args", "fingerprint":
		var o machineFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		spec, err := o.spec()
		if err != nil {
			return err
		}
		switch cmd {
		case "boot":
			return boot(spec, o.scratch())
		case "args":
			args, err := qemuArgs(spec, o.scratch())
			if err != nil {
				return err
			}
			fmt.Println(strings.Join(append([]string{spec.QEMU}, args...), " \\\n  "))
			return nil
		default:
			return fingerprint(spec)
		}
	case "attach":
		var o attachFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return attach(o)
	case "detach":
		var o detachFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return detach(o)
	case "save":
		var o saveFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return save(o)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// parse gives each command its own flags, so `save -h` lists what save takes and not
// what a boot does.
func parse(cmd string, argv []string, register func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	register(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", cmd, fs.Arg(0))
	}
	return nil
}

// machineFlags is everything boot, args and fingerprint let a caller choose. It is
// deliberately thin: the machine's shape comes from the package, and what is left is
// which release to boot, how big, and what to put in it.
type machineFlags struct {
	release string

	qemu     string
	kernel   string
	initrd   string
	firmware string

	disk       string
	diskFormat string
	readonly   bool
	serial     string
	// diskCache and directOverBacking are machine.Disk's Cache and DirectOverBacking.
	diskCache         string
	directOverBacking bool

	memoryMB     int
	maxMemMB     int
	cpuModel     string
	cpus         int
	maxCPUs      int
	memFile      string
	memShare     bool
	hotplugPorts int

	vsockCID int
	qmp      string
	console  string
	incoming string

	init    string
	root    string
	profile bool
	extra   string
}

func (o *machineFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.release, "release", "_output", "an unpacked release tree; every path below defaults out of it")
	fs.StringVar(&o.qemu, "qemu", "", "QEMU binary (default: <release>/bin/qemu-system-x86_64)")
	fs.StringVar(&o.kernel, "kernel", "", "kernel image (default: <release>/kernel/vmlinux)")
	fs.StringVar(&o.initrd, "initrd", "", "initrd (default: none)")
	fs.StringVar(&o.firmware, "firmware", "", "firmware directory (default: <release>/qemu)")

	fs.StringVar(&o.disk, "disk", "", "disk image, opened as given (default: the base image under a throwaway overlay; - for no disk)")
	fs.StringVar(&o.diskFormat, "disk-format", "qcow2", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only, and mount root ro")
	fs.StringVar(&o.serial, "disk-serial", "", "virtio-blk serial the guest can resolve the disk by")
	fs.StringVar(&o.diskCache, "disk-cache", "", "QEMU cache mode for the disk (default: QEMU's, which is writeback)")
	fs.BoolVar(&o.directOverBacking, "disk-direct-over-backing", false,
		"open the disk O_DIRECT and its backing chain through the host page cache (the disk must have a backing file)")

	fs.IntVar(&o.memoryMB, "memory", 2048, "guest memory in MiB")
	fs.IntVar(&o.maxMemMB, "max-memory", 0, "ceiling this VM may grow to and shrink back from, in MiB, through virtio-mem (0: fixed memory)")
	fs.StringVar(&o.cpuModel, "cpu", "", "CPU model shown to the guest (default: host; name one, e.g. Skylake-Server-v4, to let VMs move between machines)")
	fs.IntVar(&o.cpus, "cpus", 2, "boot vCPUs")
	fs.IntVar(&o.maxCPUs, "max-cpus", 0, "vCPU hotplug ceiling (0: no hotplug)")
	fs.StringVar(&o.memFile, "memory-file", "", "back guest RAM with this file instead of anonymous memory")
	fs.BoolVar(&o.memShare, "memory-share", false, "map the memory file shared, which is what freezing a template needs")
	fs.IntVar(&o.hotplugPorts, "hotplug-ports", 0,
		fmt.Sprintf("empty PCIe root ports a device can be attached to while the VM runs (0-%d)", machine.MaxHotplugPorts))

	fs.IntVar(&o.vsockCID, "vsock-cid", 0, "give the machine a vhost-vsock device with this context id")
	fs.StringVar(&o.qmp, "qmp", "", "listen for QMP on this Unix socket; attach, detach and save connect to it")
	fs.StringVar(&o.incoming, "incoming", "", "resume from a saved state instead of booting, e.g. file:/path/state")
	fs.StringVar(&o.console, "console", "mon:stdio",
		"QEMU chardev for the serial console, e.g. file:/path or unix:/path,server=on,wait=off; empty for none")

	fs.StringVar(&o.init, "init", "", "what the kernel runs as PID 1 inside the guest (default: the kernel's, /sbin/init)")
	fs.StringVar(&o.root, "root", "/dev/vda", "block device the kernel mounts as root; empty to stay on the initrd")
	fs.BoolVar(&o.profile, "profile", false, "boot with initcall profiling: silent console, full ring buffer")
	fs.StringVar(&o.extra, "append", "", "extra kernel command line arguments")
}

// scratch is whether the disk is the base image under an overlay QEMU makes and
// throws away. The base is never written (rule 2 in CLAUDE.md), and a command that
// booted it read-write by default made every caller build an overlay of its own.
func (o *machineFlags) scratch() bool { return o.disk == "" }

// spec turns the flags into a machine, filling in every path from the release
// tree so that the common case is no flags at all.
//
// The tree is opened rather than assumed, so a release missing a part says so
// here, naming the file — and not three seconds later as a QEMU that exits for
// want of an option ROM.
func (o *machineFlags) spec() (machine.Spec, error) {
	rel, err := machine.OpenRelease(o.release)
	if err != nil {
		return machine.Spec{}, err
	}
	or := func(v, fallback string) string {
		if v != "" {
			return v
		}
		return fallback
	}

	s := rel.Spec()
	s.QEMU = or(o.qemu, s.QEMU)
	s.Kernel = or(o.kernel, s.Kernel)
	s.Firmware = or(o.firmware, s.Firmware)
	s.Initrd = o.initrd
	s.CPU = o.cpuModel
	s.BootCPUs = o.cpus
	s.MaxCPUs = o.maxCPUs
	s.Memory = machine.Memory{
		SizeMB: o.memoryMB,
		MaxMB:  o.maxMemMB,
		File:   o.memFile,
		Shared: o.memShare,
	}
	s.HotplugPorts = o.hotplugPorts
	s.VsockCID = o.vsockCID
	if o.qmp != "" {
		s.Monitors = []machine.Monitor{{Socket: o.qmp}}
	}
	s.Incoming = o.incoming
	s.Serial = o.console

	disk := o.disk
	if o.scratch() {
		if disk, err = rel.Rootfs(); err != nil {
			return machine.Spec{}, err
		}
	}
	if disk != "-" {
		s.Disks = []machine.Disk{{
			Path:              disk,
			Format:            o.diskFormat,
			Readonly:          o.readonly,
			Serial:            o.serial,
			Cache:             o.diskCache,
			DirectOverBacking: o.directOverBacking,
		}}
	}

	c := machine.DefaultCmdline()
	c.Init = o.init
	if disk != "-" {
		c.Root = o.root
		c.RootReadonly = o.readonly
	}
	if o.profile {
		c = c.Profiling()
	}
	if o.extra != "" {
		c.Extra = append(c.Extra, strings.Fields(o.extra)...)
	}
	// A console the caller can read means a console the kernel should print to.
	// The default is a silent boot, which is what production wants and what
	// makes a profile measurable.
	if o.console == "" {
		c.Console = ""
	}
	s.Cmdline = c

	return s, nil
}

// qemuArgs is the machine's command line, with -snapshot when the disk is the base
// image. -snapshot puts every drive under a temporary qcow2 overlay that QEMU unlinks
// as soon as it is open, and opens the base beneath it read-only: nothing to create
// beforehand, nothing to clean up after, and a command line `args` prints that is
// as safe to run as the one `boot` runs. It is a host-side detail: the guest sees the
// same disk, and nothing about it is in the fingerprint.
func qemuArgs(s machine.Spec, scratch bool) ([]string, error) {
	args, err := s.Args()
	if err != nil {
		return nil, err
	}
	if scratch && len(s.Disks) > 0 {
		args = append(args, "-snapshot")
	}
	return args, nil
}

func boot(s machine.Spec, scratch bool) error {
	args, err := qemuArgs(s, scratch)
	if err != nil {
		return err
	}
	// The console is on this terminal, so QEMU keeps the process group and the
	// signals reach it the way the user expects.
	cmd := exec.Command(s.QEMU, args...) // #nosec G204 -- a binary and arguments this caller chose
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitCode(exit.ExitCode())
		}
		return fmt.Errorf("running %s: %w", s.QEMU, err)
	}
	return nil
}

// fingerprint prints the machine's identity and what went into it, because the
// number alone answers "did it change?" and not "what changed?".
func fingerprint(s machine.Spec) error {
	fp, err := s.Fingerprint()
	if err != nil {
		return err
	}
	// The shape a *template* is taken from, which is the one the fingerprint
	// hashes. Printing s.Shape() here instead would show a machine with
	// anonymous RAM next to a number computed from one with a memory file, and
	// the two would look like they disagreed.
	shape := s.TemplateShape()
	out := struct {
		Fingerprint string        `json:"fingerprint"`
		QEMU        string        `json:"qemu"`
		Kernel      string        `json:"kernel"`
		Initrd      string        `json:"initrd,omitempty"`
		Shape       machine.Shape `json:"shape"`
	}{fp, s.QEMU, s.Kernel, s.Initrd, shape}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// The ids a disk on hotplug port i goes by, so that detach finds what attach made
// from the port number alone.
func hotplugDiskID(port int) string  { return machine.HotplugPortID(port) + "-disk" }
func hotplugDriveID(port int) string { return machine.HotplugPortID(port) + "-drive" }

type attachFlags struct {
	qmp      string
	port     int
	disk     string
	format   string
	readonly bool
	serial   string
}

func (o *attachFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.IntVar(&o.port, "port", 0, "hotplug port, 0 to --hotplug-ports minus one")
	fs.StringVar(&o.disk, "disk", "", "disk image (required)")
	fs.StringVar(&o.format, "disk-format", "raw", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only")
	fs.StringVar(&o.serial, "disk-serial", "", "virtio-blk serial the guest can resolve the disk by")
}

// attach puts a virtio-blk disk on a hotplug root port, the way a caller of this
// machine gives it a disk after a restore. It returns when QEMU has the device; the
// guest's pciehp finds it on its own, and whether it did is read from the guest.
func attach(o attachFlags) error {
	if o.qmp == "" || o.disk == "" {
		return errors.New("attach: --qmp and --disk are required")
	}
	path, err := filepath.Abs(o.disk)
	if err != nil {
		return err
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := c.run("blockdev-add", map[string]any{
		"node-name": hotplugDriveID(o.port),
		"driver":    o.format,
		"read-only": o.readonly,
		"file":      map[string]any{"driver": "file", "filename": path},
	}, nil); err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	dev := map[string]any{
		"driver":         "virtio-blk-pci",
		"id":             hotplugDiskID(o.port),
		"drive":          hotplugDriveID(o.port),
		"bus":            machine.HotplugPortID(o.port),
		"disable-legacy": "on",
	}
	if o.serial != "" {
		dev["serial"] = o.serial
	}
	if err := c.run("device_add", dev, nil); err != nil {
		_ = c.run("blockdev-del", map[string]any{"node-name": hotplugDriveID(o.port)}, nil)
		return fmt.Errorf("adding %s on port %d: %w", path, o.port, err)
	}
	fmt.Fprintf(os.Stderr, "attached %s on port %d (%s)\n", path, o.port, machine.HotplugPortID(o.port))
	return nil
}

type detachFlags struct {
	qmp     string
	port    int
	timeout time.Duration
}

func (o *detachFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.IntVar(&o.port, "port", 0, "hotplug port the disk was attached on")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "how long the guest has to let the disk go")
}

// detach asks for the disk on a port back and waits for the guest to give it. A
// device_del is a request: QEMU presses the slot's attention button and the device
// goes only when the guest's pciehp powers the slot off, about five seconds later
// by the PCIe spec's own wait. A guest that never answers is the failure worth
// seeing, so it is an error and not a return on the request.
func detach(o detachFlags) error {
	if o.qmp == "" {
		return errors.New("detach: --qmp is required")
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	id := hotplugDiskID(o.port)
	start := time.Now()
	if err := c.run("device_del", map[string]any{"id": id}, nil); err != nil {
		return fmt.Errorf("removing the disk on port %d: %w", o.port, err)
	}
	if err := c.waitDeleted(id, o.timeout); err != nil {
		return fmt.Errorf("the guest did not release the disk on port %d: %w", o.port, err)
	}
	took := time.Since(start)
	if err := c.run("blockdev-del", map[string]any{"node-name": hotplugDriveID(o.port)}, nil); err != nil {
		return fmt.Errorf("closing the disk on port %d: %w", o.port, err)
	}
	fmt.Fprintf(os.Stderr, "detached port %d in %.1fs\n", o.port, took.Seconds())
	return nil
}

type saveFlags struct {
	qmp string
	to  string
}

func (o *saveFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.StringVar(&o.to, "to", "", "where to write the state (required)")
}

// save stops a running VM and writes everything needed to resume it elsewhere.
//
// It is `migrate` to a file, which is the same mechanism a live migration uses
// with the far end replaced by a path: memory, device state, CPU state. The VM is
// left stopped, because a VM that kept running after its state was captured would
// have written to its disk and the state would no longer describe it.
//
// Deliberately not `migrate -d` with x-ignore-shared, which leaves the memory in
// whatever file backs it. That is right for many VMs on one host sharing a
// template and wrong for the thing this is for: one file that can be copied to
// another machine and resumed there.
func save(o saveFlags) error {
	if o.qmp == "" || o.to == "" {
		return errors.New("save: --qmp and --to are required")
	}
	to, err := filepath.Abs(o.to)
	if err != nil {
		return err
	}

	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := c.run("migrate", map[string]any{"uri": "file:" + to}, nil); err != nil {
		return fmt.Errorf("starting the save: %w", err)
	}

	// Polled rather than waited on an event: migration reports progress through
	// query-migrate, and the completion event is not delivered for every
	// transport. A save that fails leaves a truncated file, so its status is
	// asked for rather than assumed.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var m struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		if err := c.run("query-migrate", nil, &m); err != nil {
			return err
		}
		switch m.Status {
		case "completed":
			fmt.Fprintf(os.Stderr, "saved to %s\n", to)
			_ = c.run("quit", nil, nil)
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("the save %s: %s", m.Status, m.ErrorDesc)
		}
		if time.Now().After(deadline) {
			return errors.New("the save did not finish within five minutes")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// qmpConn is the smallest QMP client these commands need: one command at a time,
// and the events that arrive in between kept for whoever waits on one.
type qmpConn struct {
	c      net.Conn
	dec    *json.Decoder
	enc    *json.Encoder
	events []qmpMessage
}

// qmpMessage is anything QEMU sends after its greeting: an event, or the reply to
// the command in flight, which carries either a return value or an error.
type qmpMessage struct {
	Event string `json:"event"`
	Data  struct {
		Device string `json:"device"`
	} `json:"data"`
	Return json.RawMessage `json:"return"`
	Error  *qmpError       `json:"error"`
}

// qmpError is QEMU refusing a command, in its own words: "GenericError: Bus 'rp0'
// not found" rather than a Go map of them.
type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string { return e.Class + ": " + e.Desc }

func dialQMP(socket string) (*qmpConn, error) {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connecting to QMP at %s: %w", socket, err)
	}
	q, err := newQMP(c)
	if err != nil {
		return nil, fmt.Errorf("QMP at %s: %w", socket, err)
	}
	return q, nil
}

// newQMP reads the greeting and negotiates capabilities on c. It owns c from here:
// a monitor that fails the handshake is closed, not handed back half open.
func newQMP(c net.Conn) (*qmpConn, error) {
	q := &qmpConn{c: c, dec: json.NewDecoder(c), enc: json.NewEncoder(c)}
	var greeting json.RawMessage
	if err := q.dec.Decode(&greeting); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("reading the greeting: %w", err)
	}
	if err := q.run("qmp_capabilities", nil, nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpConn) Close() error { return q.c.Close() }

// run sends cmd and waits for its reply, decoding the return value into out when out
// is not nil.
func (q *qmpConn) run(cmd string, args map[string]any, out any) error {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	if err := q.enc.Encode(req); err != nil {
		return fmt.Errorf("sending %s: %w", cmd, err)
	}
	for {
		var m qmpMessage
		if err := q.dec.Decode(&m); err != nil {
			return fmt.Errorf("reading the reply to %s: %w", cmd, err)
		}
		switch {
		case m.Event != "":
			q.events = append(q.events, m)
			continue
		case m.Error != nil:
			return fmt.Errorf("%s: %w", cmd, m.Error)
		case out != nil:
			if err := json.Unmarshal(m.Return, out); err != nil {
				return fmt.Errorf("decoding the reply to %s: %w", cmd, err)
			}
		}
		return nil
	}
}

// waitDeleted returns once QEMU reports DEVICE_DELETED for the device id — which
// may already have arrived while a command's reply was being read.
func (q *qmpConn) waitDeleted(id string, timeout time.Duration) error {
	deleted := func(m qmpMessage) bool { return m.Event == "DEVICE_DELETED" && m.Data.Device == id }
	for _, m := range q.events {
		if deleted(m) {
			return nil
		}
	}
	if err := q.c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer func() { _ = q.c.SetReadDeadline(time.Time{}) }()
	for {
		var m qmpMessage
		if err := q.dec.Decode(&m); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("no DEVICE_DELETED for %s within %s", id, timeout)
			}
			return fmt.Errorf("waiting for DEVICE_DELETED: %w", err)
		}
		if deleted(m) {
			return nil
		}
	}
}
