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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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

	memoryMB int
	maxMemMB int
	cpus     int
	maxCPUs  int
	memFile  string
	memShare bool

	vsockCID int
	qmp      string
	console  string

	init    string
	root    string
	profile bool
	extra   string
	verbose bool
}

func flags(fs *flag.FlagSet, o *options) *flag.FlagSet {
	fs.StringVar(&o.release, "release", "_output", "a release tree: bin/, share/spin-stack/qemu/, vmlinux, base.qcow2")
	fs.StringVar(&o.qemu, "qemu", "", "QEMU binary (default: <release>/bin/qemu-system-x86_64)")
	fs.StringVar(&o.kernel, "kernel", "", "kernel image (default: <release>/vmlinux)")
	fs.StringVar(&o.initrd, "initrd", "", "initrd (default: none)")
	fs.StringVar(&o.firmware, "firmware", "", "firmware directory (default: <release>/share/spin-stack/qemu)")

	fs.StringVar(&o.disk, "disk", "", "disk image (default: <release>/base.qcow2)")
	fs.StringVar(&o.diskFormat, "disk-format", "qcow2", "format of the disk image; never guessed")
	fs.BoolVar(&o.readonly, "disk-readonly", false, "open the disk read-only")
	fs.StringVar(&o.serial, "disk-serial", "", "virtio-blk serial the guest can resolve the disk by")

	fs.IntVar(&o.memoryMB, "memory", 2048, "guest memory in MiB")
	fs.IntVar(&o.maxMemMB, "max-memory", 0, "memory hotplug ceiling in MiB (0: no hotplug)")
	fs.IntVar(&o.cpus, "cpus", 2, "boot vCPUs")
	fs.IntVar(&o.maxCPUs, "max-cpus", 0, "vCPU hotplug ceiling (0: no hotplug)")
	fs.StringVar(&o.memFile, "memory-file", "", "back guest RAM with this file instead of anonymous memory")
	fs.BoolVar(&o.memShare, "memory-share", false, "map the memory file shared, which is what freezing a template needs")

	fs.IntVar(&o.vsockCID, "vsock-cid", 0, "give the machine a vhost-vsock device with this context id")
	fs.StringVar(&o.qmp, "qmp", "", "listen for QMP on this Unix socket")
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
func (o *options) spec() (machine.Spec, error) {
	rel, err := filepath.Abs(o.release)
	if err != nil {
		return machine.Spec{}, err
	}
	or := func(v, fallback string) string {
		if v != "" {
			return v
		}
		return fallback
	}

	s := machine.Spec{
		QEMU:     or(o.qemu, filepath.Join(rel, "bin", "qemu-system-x86_64")),
		Kernel:   or(o.kernel, filepath.Join(rel, "vmlinux")),
		Initrd:   o.initrd,
		Firmware: or(o.firmware, filepath.Join(rel, "share", "spin-stack", "qemu")),
		BootCPUs: o.cpus,
		MaxCPUs:  o.maxCPUs,
		Memory: machine.Memory{
			SizeMB: o.memoryMB,
			MaxMB:  o.maxMemMB,
			File:   o.memFile,
			Shared: o.memShare,
		},
		VsockCID:  o.vsockCID,
		QMPSocket: o.qmp,
		Serial:    o.console,
	}

	disk := or(o.disk, filepath.Join(rel, "base.qcow2"))
	if disk != "-" {
		s.Disks = []machine.Disk{{
			Path:     disk,
			Format:   o.diskFormat,
			Readonly: o.readonly,
			Serial:   o.serial,
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
