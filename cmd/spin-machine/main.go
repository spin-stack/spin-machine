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
// It is not a container runtime and does not want to become one. There is no
// supervision, no QMP, no lifecycle: it execs QEMU and gets out of the way.
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
	"syscall"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "spin-machine: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `spin-machine - the machine this repository builds

Usage:
  spin-machine boot        [flags]   start a VM and wait for it
  spin-machine save        [flags]   stop a running VM and write its state to a file
  spin-machine args        [flags]   print the QEMU command line it would run
  spin-machine fingerprint [flags]   print the machine's identity

Flags:
`)
	flags(flag.NewFlagSet("", flag.ContinueOnError), &options{}).PrintDefaults()
}

func run(argv []string) error {
	if len(argv) == 0 {
		usage()
		return errors.New("no command given")
	}
	cmd, argv := argv[0], argv[1:]

	var o options
	fs := flags(flag.NewFlagSet(cmd, flag.ExitOnError), &o)
	if err := fs.Parse(argv); err != nil {
		return err
	}

	spec, err := o.spec()
	if err != nil {
		return err
	}

	switch cmd {
	case "boot":
		return boot(spec, &o)
	case "save":
		return save(&o)
	case "args":
		args, err := spec.Args()
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(append([]string{spec.QEMU}, args...), " \\\n  "))
		return nil
	case "fingerprint":
		return fingerprint(spec)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// options is everything this command lets a caller choose. It is deliberately
// thin: the machine's shape comes from the package, and what is left is which
// release to boot, how big, and what to put in it.
type options struct {
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

	memoryMB int
	maxMemMB int
	cpuModel string
	cpus     int
	maxCPUs  int
	memFile  string
	memShare bool

	vsockCID int
	qmp      string
	console  string

	incoming string
	saveTo   string
	init     string
	root     string
	profile  bool
	extra    string
	verbose  bool
}

func flags(fs *flag.FlagSet, o *options) *flag.FlagSet {
	fs.StringVar(&o.release, "release", "_output", "an unpacked release tree; every path below defaults out of it")
	fs.StringVar(&o.qemu, "qemu", "", "QEMU binary (default: <release>/bin/qemu-system-x86_64)")
	fs.StringVar(&o.kernel, "kernel", "", "kernel image (default: <release>/vmlinux)")
	fs.StringVar(&o.initrd, "initrd", "", "initrd (default: none)")
	fs.StringVar(&o.firmware, "firmware", "", "firmware directory (default: <release>/qemu)")

	fs.StringVar(&o.disk, "disk", "", "disk image (default: <release>/rootfs.qcow2)")
	fs.StringVar(&o.diskFormat, "disk-format", "qcow2", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only")
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

	fs.IntVar(&o.vsockCID, "vsock-cid", 0, "give the machine a vhost-vsock device with this context id")
	fs.StringVar(&o.qmp, "qmp", "", "listen for QMP on this Unix socket; `save` connects to one")
	fs.StringVar(&o.incoming, "incoming", "", "resume from a saved state instead of booting, e.g. file:/path/state")
	fs.StringVar(&o.saveTo, "to", "", "save: where to write the state")
	fs.StringVar(&o.console, "console", "mon:stdio", "QEMU chardev for the serial console, or empty for none")

	fs.StringVar(&o.init, "init", "", "what the kernel runs as PID 1 inside the guest")
	fs.StringVar(&o.root, "root", "", "block device the kernel mounts as root, e.g. /dev/vda")
	fs.BoolVar(&o.profile, "profile", false, "boot with initcall profiling: silent console, full ring buffer")
	fs.StringVar(&o.extra, "append", "", "extra kernel command line arguments")
	fs.BoolVar(&o.verbose, "verbose", false, "print the QEMU command line before running it")
	return fs
}

// spec turns the flags into a machine, filling in every path from the release
// tree so that the common case is one flag.
//
// The tree is opened rather than assumed, so a release missing a part says so
// here, naming the file — and not three seconds later as a QEMU that exits for
// want of an option ROM.
func (o *options) spec() (machine.Spec, error) {
	rel, err := machine.Open(o.release)
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
	s.VsockCID = o.vsockCID
	s.QMPSocket = o.qmp
	s.Incoming = o.incoming
	s.Serial = o.console

	// The base image is what this boots unless told otherwise, and "-" is how a
	// caller asks for a machine with no disk at all.
	disk := o.disk
	if disk == "" {
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
	c.Root = o.root
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
	s.Cmdline = c.String()

	return s, nil
}

func boot(s machine.Spec, o *options) error {
	args, err := s.Args()
	if err != nil {
		return err
	}
	if o.verbose {
		fmt.Fprintln(os.Stderr, strings.Join(append([]string{s.QEMU}, args...), " "))
	}

	cmd := exec.Command(s.QEMU, args...) // #nosec G204 -- a binary and arguments this caller chose
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The console is on this terminal, so QEMU has to keep it: no new process
	// group, and signals reach it the way the user expects.
	cmd.SysProcAttr = &syscall.SysProcAttr{}

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
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
func save(o *options) error {
	if o.qmp == "" {
		return errors.New("-qmp is required: this asks a running VM to save itself")
	}
	if o.saveTo == "" {
		return errors.New("-to is required")
	}
	to, err := filepath.Abs(o.saveTo)
	if err != nil {
		return err
	}

	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if _, err := c.run("migrate", map[string]any{"uri": "file:" + to}); err != nil {
		return fmt.Errorf("starting the save: %w", err)
	}

	// Polled rather than waited on an event: migration reports progress through
	// query-migrate, and the completion event is not delivered for every
	// transport. A save that fails leaves a truncated file, so its status is
	// asked for rather than assumed.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		r, err := c.run("query-migrate", nil)
		if err != nil {
			return err
		}
		switch status, _ := r["status"].(string); status {
		case "completed":
			fmt.Fprintf(os.Stderr, "saved to %s\n", to)
			_, _ = c.run("quit", nil)
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("the save %s: %v", status, r["error-desc"])
		}
		if time.Now().After(deadline) {
			return errors.New("the save did not finish within five minutes")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// qmpConn is the smallest QMP client that can drive a save: one command at a
// time, events discarded.
type qmpConn struct {
	c   net.Conn
	dec *json.Decoder
	enc *json.Encoder
}

func dialQMP(socket string) (*qmpConn, error) {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connecting to QMP at %s: %w", socket, err)
	}
	q := &qmpConn{c: c, dec: json.NewDecoder(c), enc: json.NewEncoder(c)}

	var greeting map[string]any
	if err := q.dec.Decode(&greeting); err != nil {
		return nil, fmt.Errorf("reading the QMP greeting: %w", err)
	}
	if _, err := q.run("qmp_capabilities", nil); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *qmpConn) Close() error { return q.c.Close() }

func (q *qmpConn) run(cmd string, args map[string]any) (map[string]any, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	if err := q.enc.Encode(req); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmd, err)
	}
	for {
		var resp map[string]any
		if err := q.dec.Decode(&resp); err != nil {
			return nil, fmt.Errorf("reading the reply to %s: %w", cmd, err)
		}
		if _, isEvent := resp["event"]; isEvent {
			continue
		}
		if e, bad := resp["error"]; bad {
			return nil, fmt.Errorf("%s: %v", cmd, e)
		}
		ret, _ := resp["return"].(map[string]any)
		return ret, nil
	}
}
