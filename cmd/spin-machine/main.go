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
// to a running machine through the contract this repository promises — a disk given
// while it runs, a save to a file — is a command here, so trying it needs no
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
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	bootreport "github.com/spin-stack/spin-machine/boot"
	"github.com/spin-stack/spin-machine/machine"
)

// main is the only place the process ends, except that boot becomes QEMU and QEMU ends
// it. A command returns what happened and main decides what it means: help asked for is
// not a failure.
func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "spin-machine: %v\n", err)
		os.Exit(1)
	}
}

const usage = `spin-machine - the machine this repository builds

Usage:
  spin-machine boot        [flags]   start a VM and wait for it
  spin-machine args        [flags]   print the QEMU command line boot would run
  spin-machine fingerprint [flags]   print the machine's identity
  spin-machine attach      [flags]   give a running VM a disk while it runs
  spin-machine detach      [flags]   take it back, once the guest has let it go
  spin-machine memory      [flags]   grow or shrink a running VM between its memory and its ceiling
  spin-machine save        [flags]   stop a running VM and write its state to a file
  spin-machine restore     [flags]   load a saved state into a VM booted with --incoming defer
  spin-machine compare     [flags]   say what changed between two reports of the feature matrix

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
	case "compare":
		var o compareFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return compare(o, os.Stdout)
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
	case "memory":
		var o memoryFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return memory(o, os.Stdout)
	case "save":
		var o saveFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return save(o)
	case "restore":
		var o restoreFlags
		if err := parse(cmd, argv, o.register); err != nil {
			return err
		}
		return restore(o)
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
//
// The flag package prints a parse error itself and then returns it, and main prints
// what it is returned: the same error twice, the second time under a usage listing
// nobody asked for. So the package prints nothing, help goes to stdout because it was
// asked for, and an error is returned once with the command it belongs to.
func parse(cmd string, argv []string, register func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	register(fs)
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return err
		}
		return fmt.Errorf("%s: %w (spin-machine %s -h lists its flags)", cmd, err, cmd)
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
	bios     string

	disk       string
	diskFormat string
	readonly   bool
	serial     string
	// directOverBacking is machine.Disk's DirectOverBacking.
	directOverBacking bool

	memoryMB     int
	maxMemMB     int
	cpuModel     string
	accel        string
	cpus         int
	maxCPUs      int
	hotplugDisks int

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
	fs.StringVar(&o.bios, "bios", "", "the BIOS, a file in the firmware directory (default: "+machine.DefaultBIOS+"; bios-256k.bin is SeaBIOS)")

	fs.StringVar(&o.disk, "disk", "", "disk image, opened as given (default: the base image under a throwaway overlay; - for no disk)")
	fs.StringVar(&o.diskFormat, "disk-format", "qcow2", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only, and mount root ro")
	fs.StringVar(&o.serial, "disk-serial", "", "serial the guest can resolve the disk by")
	fs.BoolVar(&o.directOverBacking, "disk-direct-over-backing", false,
		"open the disk O_DIRECT and its backing chain through the host page cache (the disk must have a backing file)")

	fs.IntVar(&o.memoryMB, "memory", 2048, "guest memory in MiB")
	fs.IntVar(&o.maxMemMB, "max-memory", 0, "ceiling this VM may grow to and shrink back from, in MiB, through virtio-mem (0: fixed memory)")
	fs.StringVar(&o.cpuModel, "cpu", "", "CPU model shown to the guest (default: host; name one, e.g. Skylake-Server-v4, to let VMs move between machines)")
	fs.StringVar(&o.accel, "accel", "", "kvm (default) or tcg; tcg runs the release's TCG build unless --qemu names another")
	fs.IntVar(&o.cpus, "cpus", 2, "boot vCPUs")
	fs.IntVar(&o.maxCPUs, "max-cpus", 0, "vCPU hotplug ceiling (0: no hotplug)")
	fs.IntVar(&o.hotplugDisks, "hotplug-disks", 0,
		fmt.Sprintf("disks attach can give the VM at once while it runs (0-%d)", machine.MaxHotplugDisks))

	fs.IntVar(&o.vsockCID, "vsock-cid", 0, "give the machine a vhost-vsock device with this context id")
	fs.StringVar(&o.qmp, "qmp", "", "listen for QMP on this Unix socket; attach, detach and save connect to it")
	fs.StringVar(&o.incoming, "incoming", "", "resume from a saved state instead of booting, e.g. file:/path/state; defer to wait for restore")
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
	// The KVM build refuses -accel tcg, so emulating is a different binary as well as a
	// different flag, and the release has both.
	if o.accel == "tcg" && o.qemu == "" {
		if s.QEMU, err = rel.QEMUTCG(); err != nil {
			return machine.Spec{}, err
		}
	}
	s.Accel = o.accel
	s.QEMU = or(o.qemu, s.QEMU)
	s.Kernel = or(o.kernel, s.Kernel)
	s.Firmware = or(o.firmware, s.Firmware)
	s.BIOS = o.bios
	s.Initrd = o.initrd
	s.CPU = o.cpuModel
	s.BootCPUs = o.cpus
	s.MaxCPUs = o.maxCPUs
	s.Memory = machine.Memory{
		SizeMB: o.memoryMB,
		MaxMB:  o.maxMemMB,
	}
	s.HotplugDisks = o.hotplugDisks
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

// boot becomes QEMU: it returns only if the exec failed.
//
// Exec and not a child. A child leaves this process between whoever started the machine
// and the machine: a signal meant for the VM reaches a Go program that dies of it and
// leaves QEMU running, orphaned, holding its disks — the boot benchmarks once left one
// QEMU per boot that way. After an exec the pid the caller holds is QEMU's, its exit
// status is QEMU's, and the terminal, the process group and the signals are QEMU's
// without anything here to pass them on.
func boot(s machine.Spec, scratch bool) error {
	args, err := qemuArgs(s, scratch)
	if err != nil {
		return err
	}
	qemu, err := exec.LookPath(s.QEMU)
	if err != nil {
		return fmt.Errorf("finding QEMU: %w", err)
	}
	// #nosec G204 -- a binary and arguments this caller chose
	if err := syscall.Exec(qemu, append([]string{qemu}, args...), os.Environ()); err != nil {
		return fmt.Errorf("executing %s: %w", qemu, err)
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
	out := struct {
		Fingerprint string        `json:"fingerprint"`
		QEMU        string        `json:"qemu"`
		Kernel      string        `json:"kernel"`
		Initrd      string        `json:"initrd,omitempty"`
		Shape       machine.Shape `json:"shape"`
	}{fp, s.QEMU, s.Kernel, s.Initrd, s.Shape()}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// The ids the disk at hotplug target i goes by, so that detach finds what attach made
// from the target alone.
func hotplugDiskID(target int) string  { return fmt.Sprintf("hd%d", target) }
func hotplugDriveID(target int) string { return fmt.Sprintf("hd%d-drive", target) }

type compareFlags struct {
	old, new  string
	threshold float64
}

func (o *compareFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.old, "old", "", "the report to compare against, as `task report` writes it (required)")
	fs.StringVar(&o.new, "new", "", "the report being judged (required)")
	fs.Float64Var(&o.threshold, "threshold", 0.05, "a usable-time change smaller than this fraction is not reported")
}

// compare writes, as Markdown, what changed from one report of the feature matrix to the
// next: a release against the one before it, or an experiment against the release it
// started from.
func compare(o compareFlags, w io.Writer) error {
	if o.old == "" || o.new == "" {
		return errors.New("compare: --old and --new are required")
	}
	var reports []bootreport.Report
	for _, p := range []string{o.old, o.new} {
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading the report: %w", err)
		}
		var r bootreport.Report
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("reading the report %s: %w", p, err)
		}
		reports = append(reports, r)
	}
	bootreport.Diff(w, reports[0], reports[1], o.threshold)
	return nil
}

type attachFlags struct {
	qmp      string
	target   int
	disk     string
	format   string
	readonly bool
	serial   string
}

func (o *attachFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.IntVar(&o.target, "target", 0, "hotplug target, 0 to the VM's --hotplug-disks minus one")
	fs.StringVar(&o.disk, "disk", "", "disk image (required)")
	fs.StringVar(&o.format, "disk-format", "raw", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only")
	fs.StringVar(&o.serial, "disk-serial", "", "serial the guest can resolve the disk by")
}

// attach puts a disk at a hotplug target of the machine's SCSI controller, the way a
// caller of this machine gives it a disk after a restore. It returns when QEMU has the
// device; the guest sees it at once, and whether it did is read from the guest.
func attach(o attachFlags) error {
	if o.qmp == "" || o.disk == "" {
		return errors.New("attach: --qmp and --disk are required")
	}
	if err := checkTarget(o.target); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	path, err := filepath.Abs(o.disk)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", o.disk, err)
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := c.run("blockdev-add", map[string]any{
		"node-name": hotplugDriveID(o.target),
		"driver":    o.format,
		"read-only": o.readonly,
		"file":      map[string]any{"driver": "file", "filename": path},
	}, nil); err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	dev := machine.HotplugDisk(o.target, hotplugDiskID(o.target), hotplugDriveID(o.target), o.serial)
	if err := c.run("device_add", dev, nil); err != nil {
		_ = c.run("blockdev-del", map[string]any{"node-name": hotplugDriveID(o.target)}, nil)
		return fmt.Errorf("adding %s at target %d: %w", path, o.target, err)
	}
	fmt.Fprintf(os.Stderr, "attached %s at target %d\n", path, o.target)
	return nil
}

type detachFlags struct {
	qmp     string
	target  int
	timeout time.Duration
}

func (o *detachFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.IntVar(&o.target, "target", 0, "hotplug target the disk was attached at")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "how long QEMU has to report the disk gone")
}

// detach takes the disk at a target back and waits for QEMU to say it has gone. A SCSI
// disk goes as soon as it is asked for, where one behind a PCIe root port waited out
// pciehp's five-second attention-button window; a device QEMU never reports gone is
// still the failure worth seeing, so it is an error and not a return on the request.
func detach(o detachFlags) error {
	if o.qmp == "" {
		return errors.New("detach: --qmp is required")
	}
	if err := checkTarget(o.target); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	id := hotplugDiskID(o.target)
	start := time.Now()
	if err := c.run("device_del", map[string]any{"id": id}, nil); err != nil {
		return fmt.Errorf("removing the disk at target %d: %w", o.target, err)
	}
	if err := c.waitDeleted(id, o.timeout); err != nil {
		return fmt.Errorf("QEMU did not report the disk at target %d gone: %w", o.target, err)
	}
	took := time.Since(start)
	if err := c.run("blockdev-del", map[string]any{"node-name": hotplugDriveID(o.target)}, nil); err != nil {
		return fmt.Errorf("closing the disk at target %d: %w", o.target, err)
	}
	fmt.Fprintf(os.Stderr, "detached target %d in %.1fs\n", o.target, took.Seconds())
	return nil
}

// checkTarget refuses a target no machine can have, here rather than as QEMU's own
// refusal. Whether this VM was started with a controller is QEMU's to answer.
func checkTarget(target int) error {
	if target < 0 || target >= machine.MaxHotplugDisks {
		return fmt.Errorf("target %d: a machine has targets 0 to %d at most", target, machine.MaxHotplugDisks-1)
	}
	return nil
}

type memoryFlags struct {
	qmp     string
	sizeMB  int
	timeout time.Duration
}

func (o *memoryFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.IntVar(&o.sizeMB, "size", -1, "MiB above the boot memory to have plugged, 0 to give it all back (required)") // mutate-exempt: any negative default reads as not given, so -2 is the same answer
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "how long the guest has to get there")
}

// memory asks the VM's virtio-mem device for a size and waits for the guest to reach it. It
// prints the size plugged when it stopped and how long that took, also when it did not get
// there: a guest that cannot give memory back - it sits in blocks the kernel will not offline -
// is the answer to the question a shrink asks, and it is an error only because the size asked
// for was not reached.
//
// Growing needs the guest to online what is plugged (memhp_default_state=online), and shrinking
// needs it to offline blocks, which it can only do for blocks holding nothing it cannot move.
func memory(o memoryFlags, w io.Writer) error {
	if o.qmp == "" || o.sizeMB < 0 {
		return errors.New("memory: --qmp and --size are required")
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	path := "/machine/peripheral/" + machine.VirtioMemID
	want := uint64(o.sizeMB) << 20
	start := time.Now()
	if err := c.run("qom-set", map[string]any{"path": path, "property": "requested-size", "value": want}, nil); err != nil {
		return fmt.Errorf("asking for %d MiB: %w", o.sizeMB, err)
	}
	deadline := start.Add(o.timeout)
	for {
		var size uint64
		if err := c.run("qom-get", map[string]any{"path": path, "property": "size"}, &size); err != nil {
			return fmt.Errorf("reading the plugged size: %w", err)
		}
		took := time.Since(start)
		if size == want {
			fmt.Fprintf(w, "plugged %d MiB in %d ms\n", size>>20, took.Milliseconds())
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "plugged %d MiB after %d ms, asked for %d\n", size>>20, took.Milliseconds(), o.sizeMB)
			return fmt.Errorf("the guest reached %d MiB of the %d asked for within %s", size>>20, o.sizeMB, o.timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type saveFlags struct {
	qmp     string
	to      string
	timeout time.Duration
}

func (o *saveFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "the running VM's QMP socket (required)")
	fs.StringVar(&o.to, "to", "", "where to write the state (required)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long the save may take")
}

// save stops a running VM and writes everything needed to resume it: a checkpoint.
//
// It is `migrate` to a file, which is the same mechanism a live migration uses
// with the far end replaced by a path: memory, device state and CPU state, all in
// the one file, so it can be copied and resumed on another machine with the same
// fingerprint. The VM is left stopped, because a VM that kept running after its
// state was captured would have written to its disk and the state would no longer
// describe it.
func save(o saveFlags) error {
	if o.qmp == "" || o.to == "" {
		return errors.New("save: --qmp and --to are required")
	}
	to, err := filepath.Abs(o.to)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", o.to, err)
	}

	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := c.run("migrate", map[string]any{"uri": "file:" + to}, nil); err != nil {
		return fmt.Errorf("starting the save: %w", err)
	}
	if err := c.waitMigration(o.timeout); err != nil {
		return fmt.Errorf("the save: %w", err)
	}
	fmt.Fprintf(os.Stderr, "saved to %s\n", to)
	// The state is written and complete; what is left is a stopped VM nobody will
	// resume in place. quit's reply may never arrive — QEMU can close the monitor
	// before sending it — so an error here says nothing about the save, which is the
	// thing this command was for.
	_ = c.run("quit", nil, nil)
	return nil
}

type restoreFlags struct {
	qmp     string
	from    string
	timeout time.Duration
}

func (o *restoreFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.qmp, "qmp", "", "QMP socket of a VM booted with --incoming defer (required)")
	fs.StringVar(&o.from, "from", "", "the state save wrote (required)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long the load may take")
}

// restore loads a saved state into a VM waiting for one, and runs it.
//
// `boot --incoming file:PATH` loads the same file in one step. This is the other
// form, and the one a caller that resumes checkpoints uses: the machine is started
// with -incoming defer (Spec.Incoming "defer"), and the state is named over QMP once
// the caller is ready for it. Driving that by hand is what this command is for.
func restore(o restoreFlags) error {
	if o.qmp == "" || o.from == "" {
		return errors.New("restore: --qmp and --from are required")
	}
	from, err := filepath.Abs(o.from)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", o.from, err)
	}
	c, err := dialQMP(o.qmp)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := c.run("migrate-incoming", map[string]any{"uri": "file:" + from}, nil); err != nil {
		return fmt.Errorf("loading %s: %w", from, err)
	}
	if err := c.waitMigration(o.timeout); err != nil {
		return fmt.Errorf("loading %s: %w", from, err)
	}
	// A VM saved stopped comes back stopped: QEMU carries the source's run state across
	// and does not autostart a machine that was paused when it was captured. A save of a
	// running VM arrives running.
	var st struct {
		Status string `json:"status"`
	}
	if err := c.run("query-status", nil, &st); err != nil {
		return err
	}
	if st.Status != "running" {
		if err := c.run("cont", nil, nil); err != nil {
			return fmt.Errorf("starting the restored VM (it was %s): %w", st.Status, err)
		}
	}
	fmt.Fprintf(os.Stderr, "restored %s\n", from)
	return nil
}

// waitMigration returns once the migration in flight, either direction, has completed.
//
// Polled rather than waited on an event: migration reports progress through
// query-migrate, and the completion event is not delivered for every transport. A
// save that fails leaves a truncated file, so its status is asked for rather than
// assumed.
//
// Every 2 ms, because whatever is left of the interval when the migration completes is
// added to the restore: a poll is one QMP round trip, and the 100 ms a long save could
// afford would be up to 100 ms more on every resume.
func (q *qmpConn) waitMigration(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var m struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		if err := q.run("query-migrate", nil, &m); err != nil {
			return err
		}
		switch m.Status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("%s: %s", m.Status, m.ErrorDesc)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not finished within %s", timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// qmpConn is the smallest QMP client these commands need: one command at a time, and
// the one event anything here waits on kept when it arrives in between.
type qmpConn struct {
	c   net.Conn
	dec *json.Decoder
	enc *json.Encoder
	// deleted is the id of every device QEMU has reported DEVICE_DELETED for. Only that
	// event is kept: a save polls for minutes, and every other event it sees is one
	// nothing reads.
	deleted []string
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

// handshakeTimeout bounds the greeting and qmp_capabilities. QEMU writes the greeting
// as it accepts, so the only monitor that takes longer is one that has not accepted:
// its one connection is somebody else's, and the kernel queued this one behind it.
const handshakeTimeout = 5 * time.Second

func dialQMP(socket string) (*qmpConn, error) {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connecting to QMP at %s: %w", socket, err)
	}
	q, err := newQMP(c, handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("QMP at %s: %w", socket, err)
	}
	return q, nil
}

// newQMP reads the greeting and negotiates capabilities on c. It owns c from here:
// a monitor that fails the handshake is closed, not handed back half open.
//
// The handshake has a deadline and nothing after it does. A monitor serves one client
// and queues the next without a word, so without one a second `spin-machine attach`
// against a VM whose launcher holds its monitor waits forever and says nothing.
func newQMP(c net.Conn, timeout time.Duration) (*qmpConn, error) {
	q := &qmpConn{c: c, dec: json.NewDecoder(c), enc: json.NewEncoder(c)}
	if err := q.handshake(timeout); err != nil {
		_ = c.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpConn) handshake(timeout time.Duration) error {
	if err := q.c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("setting the handshake deadline: %w", err)
	}
	var greeting json.RawMessage
	if err := q.dec.Decode(&greeting); err != nil {
		if isTimeout(err) {
			return fmt.Errorf("no greeting within %s: another client may hold the monitor, which serves one at a time", timeout)
		}
		return fmt.Errorf("reading the greeting: %w", err)
	}
	if err := q.run("qmp_capabilities", nil, nil); err != nil {
		return err
	}
	if err := q.c.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing the handshake deadline: %w", err)
	}
	return nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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
		case m.Event == "DEVICE_DELETED":
			q.deleted = append(q.deleted, m.Data.Device)
			continue
		case m.Event != "":
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
	if slices.Contains(q.deleted, id) {
		return nil
	}
	if err := q.c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("setting a deadline for DEVICE_DELETED: %w", err)
	}
	defer func() { _ = q.c.SetReadDeadline(time.Time{}) }()
	for {
		var m qmpMessage
		if err := q.dec.Decode(&m); err != nil {
			if isTimeout(err) {
				return fmt.Errorf("no DEVICE_DELETED for %s within %s", id, timeout)
			}
			return fmt.Errorf("waiting for DEVICE_DELETED: %w", err)
		}
		if m.Event == "DEVICE_DELETED" && m.Data.Device == id {
			return nil
		}
	}
}
