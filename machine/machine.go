// Package machine defines the virtual machine this repository builds: which
// chipset, which devices at which PCI slots, which kernel command line, and how
// guest RAM is backed.
//
// It lives here, next to the QEMU binary, the kernel and the base image, because
// those four things are one thing. A VM restored from a template loads device and
// CPU state into a machine that has to be the same shape as the one the template
// was frozen from, and nothing checks that at run time. So the shape is not a
// detail of whoever launches a VM; it is part of what a release *is*, and it has
// to move with the binary and the kernel it was written for.
//
// The rule that follows: Fingerprint hashes the QEMU binary, the kernel and the
// initrd by content, together with the four arguments that decide the machine's
// shape. Two machines with the same fingerprint can exchange templates. Two with
// different fingerprints cannot, and a release in which any of the three files
// moved has a different fingerprint by construction.
//
// What is here is what a machine is. What is not here is everything about
// running one: allocating a vsock context id, opening a TAP file descriptor,
// talking QMP, supervising the process, deciding what the guest's init is. A
// caller owns all of that and passes the results in.
package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Fixed PCI slot assignments on the q35 root complex (pcie.0).
//
// Every device sits directly on bus 0 — there are no PCIe root ports — which is
// what lets the kernel command line carry pci=lastbus=0 and skip scanning buses
// 1 through 255. Pinning each device to a slot rather than letting QEMU assign
// one makes guest enumeration order deterministic: it stops depending on the
// order of calls made here, so reordering code cannot silently renumber devices
// inside the guest.
//
// The machine owns both ends of the bus: 0x00 is the host bridge and 0x1f the
// ICH9 LPC/SATA/SMBus function block. 0x01 is left free — the q35 convention
// puts VGA there, and this machine has no display adapter at all.
const (
	SlotVsock = 0x02
	SlotRNG   = 0x03
	// 0x04 is free.

	SlotDiskBase = 0x05
	SlotDiskMax  = 0x0f

	SlotNICBase = 0x10
	SlotNICMax  = 0x1e
)

// MaxDisks and MaxNICs bound the fixed slot ranges above. Exceeding either is a
// configuration error, caught before a command line is built rather than by the
// guest not finding a device.
const (
	MaxDisks = SlotDiskMax - SlotDiskBase + 1
	MaxNICs  = SlotNICMax - SlotNICBase + 1
)

// virtioModern forces virtio 1.0 (modern-only) on a PCI virtio device.
//
// disable-legacy=on drops the legacy I/O BAR and the transitional device ID, so
// the guest skips the legacy probe path entirely. The kernel this machine boots
// is virtio 1.0 capable, so the transitional mode QEMU would otherwise negotiate
// buys nothing. Measured neutral for boot time; the win is a smaller device
// surface, not speed.
const virtioModern = "disable-legacy=on"

// MemoryBackendID names the RAM object when guest memory is file-backed.
//
// The machine references it by id, and migration matches RAM blocks by name
// across save and restore, so it must be identical on both sides. It is "pc.ram"
// because that is what QEMU calls the machine's main RAM block when it creates
// one itself: keeping the name means a template taken from a machine with
// anonymous RAM and one taken from a machine with file-backed RAM describe the
// same block.
const MemoryBackendID = "pc.ram"

// DefaultMemorySlots is how many hotplug slots a machine with a memory ceiling
// gets. Slots are described in the machine's ACPI tables, so the number is part
// of the machine's shape and cannot be changed for a VM that must restore from
// an existing template.
const DefaultMemorySlots = 4

// Disk is one virtio-blk device.
type Disk struct {
	// Path to the image file.
	Path string
	// Format as QEMU names it: qcow2, raw, vmdk. Required — it is not guessed
	// from the file name, because a wrong guess is a guest that boots and finds
	// a disk full of nothing, and because letting QEMU probe the format of a
	// file the guest can write is how an image is talked into being read as
	// another format.
	Format string
	// Readonly opens the image read-only. A shared base image must be opened
	// this way by every VM that maps it.
	Readonly bool
	// Serial the guest can resolve the device by, through
	// /sys/block/<dev>/serial. Worth setting on every disk: a /dev node depends
	// on the order the guest probed devices in, and a serial does not.
	Serial string
	// Locking asks QEMU to hold an image lock on the file. Only meaningful for a
	// writable disk, and only worth setting when something outside QEMU takes
	// the same lock to find out whether a VM is running on the image.
	Locking bool
}

// NIC is one virtio-net device, backed by a TAP file descriptor the caller has
// already opened and passed to the QEMU process.
type NIC struct {
	// TapFD is the descriptor number in the QEMU process, so 3 or above.
	TapFD int
	MAC   string
}

// Memory describes guest RAM.
type Memory struct {
	SizeMB int
	// MaxMB, when larger than SizeMB, gives the machine hotplug slots up to that
	// ceiling. Both numbers are in the machine's shape.
	MaxMB int
	// File backs guest RAM with a file rather than anonymous memory. Empty means
	// anonymous.
	//
	// This is what makes a template possible: the pages a VM dirties land in a
	// file that other VMs can then map.
	File string
	// Shared maps that file MAP_SHARED. A VM being frozen into a template needs
	// this — the pages it dirties must reach the file the restores will read. A
	// VM restoring from one passes false, mapping the same file MAP_PRIVATE: it
	// sees the template's memory and anything it writes stays private to it.
	// That is the whole copy-on-write story, and it is why one template file can
	// serve many VMs without being copied.
	Shared bool
}

// Spec is one virtual machine.
type Spec struct {
	// The three files whose contents define the machine.
	QEMU   string
	Kernel string
	Initrd string

	// Firmware is the directory QEMU loads option ROMs and BIOS blobs from. It
	// is the `qemu/` directory of a release; without it the machine cannot find
	// pvh.bin, which is the only way into this kernel.
	Firmware string

	BootCPUs int
	MaxCPUs  int
	Memory   Memory

	Disks []Disk
	NICs  []NIC

	// VsockCID, when non-zero, gives the machine a vhost-vsock device with that
	// context id. It is how anything inside the guest is reached: this machine
	// has no serial port for a caller to drive and no network it is required to
	// have.
	VsockCID int

	// QMPSocket is a Unix socket path QEMU listens on for QMP. Required to do
	// anything to a running machine, including shutting it down.
	QMPSocket string

	// Serial is a QEMU chardev spec for the console — "file:/path/console.log",
	// "mon:stdio", or empty for no console at all. The kernel prints to the ISA
	// 16550 the machine has; nothing else uses it.
	Serial string

	// Cmdline is the kernel command line. Build it with Cmdline.String().
	Cmdline string

	// IncomingDefer starts QEMU with no machine state, waiting to be told where
	// to load it from. Without it the source of a restore has to be known at
	// exec time, which would mean re-execing QEMU to change templates.
	IncomingDefer bool
}

// Shape is the four arguments that decide what machine a guest sees: the chipset
// and its options, the CPU model, the vCPU count with its hotplug ceiling, and
// the memory size with its slots and ceiling.
//
// It is one type with one constructor because it has two consumers that must
// never disagree: the command line QEMU is given, and the fingerprint that
// decides which templates this machine may restore from. If the fingerprint
// stopped describing the command line, a VM would restore from a template of
// another machine and the failure would be silent.
type Shape struct {
	Machine string
	CPU     string
	SMP     string
	Memory  string
}

// Shape returns the machine's shape.
//
// It reports the shape of a machine with *file-backed* RAM whenever the spec has
// a memory file, because that changes the machine string. A caller comparing
// fingerprints across templates should build the spec it would actually run.
func (s Spec) Shape() Shape {
	backend := ""
	if s.Memory.File != "" {
		backend = "memory-backend=" + MemoryBackendID
	}

	// hpet=off: the HPET is a timer the guest would enumerate, initialise and
	// then not use, because a KVM guest reads the TSC and the KVM clock.
	// kernel-irqchip=on keeps interrupt delivery in the kernel rather than
	// bouncing every one through userspace. acpi=on is not optional: memory
	// hotplug is announced through ACPI, and so is the machine's slot table.
	machine := strings.Join(nonEmpty(
		"q35", "accel=kvm", "kernel-irqchip=on", "hpet=off", "acpi=on", backend,
	), ",")

	slots := DefaultMemorySlots
	if s.Memory.MaxMB <= s.Memory.SizeMB {
		slots = 0
	}

	return Shape{
		Machine: machine,
		// migratable=on restricts the CPU to features that survive a migration,
		// which is what a restore is. Without it QEMU exposes host features it
		// cannot guarantee on the other side, and the other side here is the
		// same host at a later time — with a different microcode, perhaps.
		CPU:    "host,migratable=on",
		SMP:    smpArg(s.BootCPUs, s.MaxCPUs),
		Memory: memoryArg(s.Memory.SizeMB, slots, s.Memory.MaxMB),
	}
}

// TemplateShape is the shape a template of this machine is taken from, which is
// the shape Fingerprint hashes.
//
// It is Shape with RAM always file-backed, whatever this spec was configured
// with. Every template and every VM restored from one has its memory in a file,
// so the lookup that decides whether a VM may restore has to give the same
// answer for a VM that has not been given a memory file yet — otherwise a VM
// could never find the template it would itself produce.
func (s Spec) TemplateShape() Shape {
	forTemplate := s
	if forTemplate.Memory.File == "" {
		forTemplate.Memory.File = "-"
	}
	return forTemplate.Shape()
}

func smpArg(bootCPUs, maxCPUs int) string {
	if maxCPUs > 0 && maxCPUs != bootCPUs {
		return fmt.Sprintf("%d,maxcpus=%d", bootCPUs, maxCPUs)
	}
	return fmt.Sprintf("%d", bootCPUs)
}

func memoryArg(sizeMB, slots, maxMB int) string {
	if slots > 0 && maxMB > sizeMB {
		return fmt.Sprintf("%d,slots=%d,maxmem=%dM", sizeMB, slots, maxMB)
	}
	return fmt.Sprintf("%d", sizeMB)
}

func nonEmpty(values ...string) []string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	return kept
}

// Validate reports what is wrong with a spec, before a command line is built
// from it. Every check here is something that otherwise fails as a guest that
// boots and finds the world subtly wrong.
func (s Spec) Validate() error {
	switch {
	case s.QEMU == "":
		return fmt.Errorf("no QEMU binary")
	case s.Kernel == "":
		return fmt.Errorf("no kernel")
	case s.Firmware == "":
		// The machine boots by entering a PVH ELF kernel through pvh.bin, which
		// QEMU finds under this directory. Without it the failure is a rom-open
		// error that reads like something else entirely.
		return fmt.Errorf("no firmware directory: QEMU has no pvh.bin to enter the kernel through")
	case s.BootCPUs < 1:
		return fmt.Errorf("BootCPUs is %d", s.BootCPUs)
	case s.MaxCPUs != 0 && s.MaxCPUs < s.BootCPUs:
		return fmt.Errorf("MaxCPUs %d is below BootCPUs %d", s.MaxCPUs, s.BootCPUs)
	case s.Memory.SizeMB < 1:
		return fmt.Errorf("memory is %d MB", s.Memory.SizeMB)
	case s.Memory.Shared && s.Memory.File == "":
		return fmt.Errorf("Memory.Shared with no Memory.File to share")
	case len(s.Disks) > MaxDisks:
		return fmt.Errorf("%d disks, and the slot range holds %d", len(s.Disks), MaxDisks)
	case len(s.NICs) > MaxNICs:
		return fmt.Errorf("%d NICs, and the slot range holds %d", len(s.NICs), MaxNICs)
	}
	for i, d := range s.Disks {
		if d.Path == "" {
			return fmt.Errorf("disk %d has no path", i)
		}
		if d.Format == "" {
			return fmt.Errorf("disk %d (%s) has no format: it is not guessed", i, d.Path)
		}
	}
	return nil
}

// Args returns the QEMU command line, without the binary itself.
func (s Spec) Args() ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	shape := s.Shape()
	args := []string{
		"-L", s.Firmware,

		// Every device this machine has is named below. Without -nodefaults QEMU
		// adds a NIC, a display, a serial port and a floppy controller of its
		// own choosing, and a guest that enumerates them pays for all of them.
		"-nodefaults",
		"-nographic",

		// The binary is built with seccomp support, so all four restrictions
		// apply: obsolete syscalls denied; setuid/setgid denied, since QEMU
		// never changes user here; fork and exec denied, because nothing is
		// spawned — a TAP arrives as a file descriptor and no network helper is
		// compiled in; and sched_setaffinity denied, so vCPU pinning has to be
		// done from outside rather than from inside QEMU.
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",

		"-machine", shape.Machine,
		"-cpu", shape.CPU,
		"-smp", shape.SMP,
		"-m", shape.Memory,
	}

	if s.Memory.File != "" {
		share := "off"
		if s.Memory.Shared {
			share = "on"
		}
		args = append(args, "-object",
			fmt.Sprintf("memory-backend-file,id=%s,size=%dM,mem-path=%s,share=%s",
				MemoryBackendID, s.Memory.SizeMB, s.Memory.File, share))
	}

	// S3 and S4 are suspend states this machine cannot come back from and that a
	// guest can ask for by accident. Disabling them in the chipset means the
	// request never reaches the point of stopping the VM.
	args = append(args, "-global", "ICH9-LPC.disable_s3=1", "-global", "ICH9-LPC.disable_s4=1")

	// A reset ends the process instead of starting the machine again.
	//
	// The kernel command line carries panic=1, whose stated purpose is that a
	// wedged guest becomes a process that exits rather than a VM sitting at a
	// prompt nobody is watching. Without this it does not: the guest panics,
	// reboots, panics again, and QEMU never exits — measured by booting an init
	// that returns immediately, which produced a reboot loop rather than the
	// expected exit. A VM here is cattle; something that wants a fresh machine
	// starts one.
	args = append(args, "-no-reboot")

	args = append(args, "-kernel", s.Kernel)
	if s.Initrd != "" {
		args = append(args, "-initrd", s.Initrd)
	}
	if s.Cmdline != "" {
		args = append(args, "-append", s.Cmdline)
	}

	// The VM Generation ID, whose value QEMU randomises for every VM it starts.
	//
	// It exists for restores. Every VM restored from a template starts with the
	// template's memory, which includes the state of the guest's random pool:
	// two guests restored from the same template would otherwise produce the
	// same "random" bytes until something reseeded them. The kernel watches this
	// device and reseeds when the value it sees differs from the one in the
	// memory it woke up with, which is exactly this case.
	args = append(args, "-device", "vmgenid,guid=auto")

	args = append(args, "-device",
		fmt.Sprintf("virtio-rng-pci,%s,addr=0x%x", virtioModern, SlotRNG))

	if s.VsockCID != 0 {
		args = append(args, "-device",
			fmt.Sprintf("vhost-vsock-pci,guest-cid=%d,%s,addr=0x%x",
				s.VsockCID, virtioModern, SlotVsock))
	}

	for i, d := range s.Disks {
		drive := fmt.Sprintf("file=%s,if=none,id=blk%d,format=%s", d.Path, i, d.Format)
		if d.Readonly {
			drive += ",readonly=on"
		}
		if d.Locking {
			drive += ",file.locking=on"
		}
		dev := fmt.Sprintf("virtio-blk-pci,drive=blk%d,%s,addr=0x%x",
			i, virtioModern, SlotDiskBase+i)
		if d.Serial != "" {
			dev += ",serial=" + d.Serial
		}
		args = append(args, "-drive", drive, "-device", dev)
	}

	for i, n := range s.NICs {
		// romfile= loads no option ROM. The card is only ever driven by a guest
		// that already has the driver compiled in, and it never boots from the
		// network, so the ROM is firmware nothing reads.
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=net%d,fd=%d", i, n.TapFD),
			"-device", fmt.Sprintf("virtio-net-pci,netdev=net%d,mac=%s,romfile=,%s,addr=0x%x",
				i, n.MAC, virtioModern, SlotNICBase+i))
	}

	if s.Serial != "" {
		args = append(args, "-serial", s.Serial)
	} else {
		args = append(args, "-serial", "none")
	}

	if s.QMPSocket != "" {
		args = append(args, "-qmp",
			fmt.Sprintf("unix:%s,server=on,wait=off", s.QMPSocket))
	}

	if s.IncomingDefer {
		args = append(args, "-incoming", "defer")
	}

	return args, nil
}

// Fingerprint identifies the machine this spec describes, by content.
//
// It hashes the QEMU binary, the kernel and the initrd — the files, not their
// paths — together with the four arguments that decide the machine's shape. Two
// machines with the same fingerprint present the same thing to a guest and can
// exchange templates; two with different fingerprints cannot, and a restore
// across them is undefined rather than an error.
//
// The consequence is deliberate: a release in which any of the three files moved
// has a different fingerprint, so every template taken against the previous one
// stops matching and is rebuilt. That is the design. A template restored into a
// machine of another shape is memory and device state loaded into hardware that
// is not the hardware it came from.
//
// The shape is always taken as if RAM were file-backed, because a template and
// every VM restored from one have their memory in a file whatever the spec being
// asked was configured with.
func (s Spec) Fingerprint() (string, error) {
	shape := s.TemplateShape()

	h := sha256.New()
	for _, f := range []struct{ name, path string }{
		{"qemu", s.QEMU},
		{"kernel", s.Kernel},
		{"initrd", s.Initrd},
	} {
		if f.path == "" {
			// An absent initrd is part of the identity too: a machine that boots
			// one and a machine that does not are different machines.
			fmt.Fprintf(h, "%s=none\n", f.name)
			continue
		}
		sum, err := fileSum(f.path)
		if err != nil {
			return "", fmt.Errorf("fingerprinting %s: %w", f.name, err)
		}
		fmt.Fprintf(h, "%s=%s\n", f.name, sum)
	}
	fmt.Fprintf(h, "machine=%s\ncpu=%s\nsmp=%s\nmemory=%s\n",
		shape.Machine, shape.CPU, shape.SMP, shape.Memory)

	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- a path this caller chose
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
