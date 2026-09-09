// SPDX-License-Identifier: Apache-2.0

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
	"errors"
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
// ICH9 function block, which here is the LPC bridge alone — the SATA and SMBus
// functions beside it are turned off in the machine string, see Shape. 0x01 is
// left free — the q35 convention puts VGA there, and this machine has no
// display adapter at all.
const (
	SlotVsock   = 0x02
	SlotRNG     = 0x03
	SlotBalloon = 0x04

	SlotDiskBase = 0x05
	SlotDiskMax  = 0x0f

	SlotNICBase = 0x10
	SlotNICMax  = 0x19

	// Root ports for devices that arrive while the machine runs. Taken off the
	// top of the NIC range, the way SlotMem was: every slot below is where it
	// was, and ten NICs is still more than anything asks for.
	SlotHotplugBase = 0x1a
	SlotHotplugMax  = 0x1d

	// Taken off the top of the NIC range rather than inserted anywhere earlier:
	// every slot below this one is where it was, so this did not renumber a
	// single existing device. Fifteen NICs was not a number anything needed.
	SlotMem = 0x1e
)

// MaxDisks and MaxNICs bound the fixed slot ranges above. Exceeding either is a
// configuration error, caught before a command line is built rather than by the
// guest not finding a device.
const (
	MaxDisks = SlotDiskMax - SlotDiskBase + 1
	MaxNICs  = SlotNICMax - SlotNICBase + 1

	// MaxHotplugDiskPorts bounds Spec.HotplugDiskPorts.
	MaxHotplugDiskPorts = SlotHotplugMax - SlotHotplugBase + 1
)

// HotplugPortID names the root port a device arriving at run time is attached to. Whoever
// hotplugs the device passes it as the device's bus, and the numbering is the machine's:
// port i is the i'th disk this machine can be given while it runs.
func HotplugPortID(i int) string {
	return fmt.Sprintf("rp%d", i)
}

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

// memGrowthID names the memory a virtio-mem device hands out. It is a separate
// region from pc.ram: that one is the memory the guest boots with and a template
// is made of, this one is empty until somebody asks for it.
const memGrowthID = "mem.growth"

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
	// Cache overrides how the host caches this disk. Empty leaves QEMU's default,
	// which is what this machine wants and is worth saying why.
	//
	// The usual advice for a production VM is cache=none — bypass the host page
	// cache, because the guest caches the same blocks and holding them twice
	// wastes RAM. It is the wrong advice here. The read-only base image is one
	// file that *every* VM on the host maps through a backing chain, so one copy
	// in the host's page cache is one copy shared by all of them; with O_DIRECT
	// each VM would fault its own. The overlay is the part that would benefit,
	// and it shares a -drive with the base.
	//
	// cache=none also fails outright on a filesystem with no O_DIRECT — tmpfs,
	// which is where a scratch overlay usually lands.
	Cache string
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
	// MaxMB, when larger than SizeMB, is the ceiling this VM's memory may grow to
	// and shrink back from while it runs, through virtio-mem. Both numbers are in
	// the machine's shape.
	//
	// Growth is virtio-mem and not ACPI DIMM hotplug, which this machine offers
	// no slots for. A DIMM can be added and, in practice, not removed: unplugging
	// one needs the guest to offline a whole memory block, and a single unmovable
	// page in it makes that fail. virtio-mem plugs and unplugs in small blocks
	// inside one device, so a VM that grew for a build can give the memory back
	// afterwards — which is the half that made this worth having.
	//
	// Nothing is plugged at start-up. The caller sets requested-size over QMP when
	// it wants more, which is also what makes one template serve VMs that end up
	// different sizes: the template is taken with the boot memory and nothing
	// else, and each restored VM grows on its own.
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

	// CPU is the model the guest is shown. Empty means "host", and that is a
	// decision about whether VMs may move between machines.
	//
	// "host" exposes this host's own feature set, which is the fastest thing and
	// the least portable: a guest resumed on another host has already been told
	// through CPUID which instructions exist, and it does not ask again. Restore
	// it somewhere without AVX-512 and it executes an instruction that is not
	// there. Fingerprint therefore folds the host's CPU model in when this is
	// "host", so a template built here does not match a machine elsewhere.
	//
	// Naming a model instead — "Skylake-Server-v4", "EPYC-Rome-v3", whichever is
	// the oldest microarchitecture in the fleet — gives every host the same guest
	// CPU, so templates cross hosts and the host model drops out of the
	// fingerprint. The cost is that guests never see anything newer than the
	// model names, on any machine.
	//
	// The portable choice is a named microarchitecture, and not an x86-64-v2/v3/v4
	// baseline: those are psABI levels that compilers target, and QEMU 11.1.1
	// defines no CPU model by those names — checked in target/i386/cpu.c, which
	// has zero occurrences of the string. The generic models it does define are
	// qemu64 and kvm64, both far older than anything worth running. Adding v3 as
	// a model would mean carrying a QEMU patch, which would cost forward-porting
	// it forever and buy nothing over naming the oldest microarchitecture in the
	// fleet — which is what libvirt and every cluster manager do.
	CPU string

	Disks []Disk
	NICs  []NIC

	// HotplugDiskPorts is how many disks this machine can be given while it runs.
	//
	// A disk that arrives later cannot go where the disks on the command line go: those
	// slots are on the q35 root complex, and QEMU refuses device_add there — "Bus 'pcie.0'
	// does not support hotplugging". What accepts a device at run time is a PCIe root
	// port, so this is that many empty root ports, each one a bus with a free slot.
	//
	// Zero by default, and a machine that asks for none is byte-for-byte the machine it
	// was before this existed. It is not free: each port is a bridge the guest enumerates
	// at boot and a bus it has to scan, and the ports are in the fingerprint, so a machine
	// with them does not share a template with one without.
	//
	// What it buys is a machine that can exist before the workload does — started, resumed
	// and waiting, and given its disk when one turns up.
	HotplugDiskPorts int

	// VsockCID, when non-zero, gives the machine a vhost-vsock device with that
	// context id. It is how anything inside the guest is reached: this machine
	// has no serial port for a caller to drive and no network it is required to
	// have.
	VsockCID int

	// QMPSocket is a Unix socket path QEMU listens on for QMP. Required to do
	// anything to a running machine, including shutting it down.
	QMPSocket string

	// QMPSocket2 is a second monitor, on its own socket, for a second thing that drives
	// this machine.
	//
	// It exists because a QMP socket serves one client: QEMU's socket chardev accepts one
	// connection and the next one waits, so two components cannot share a path. Two
	// monitors are two chardevs, and QEMU serves both at once — each with its own
	// greeting, its own capabilities handshake and its own command stream.
	//
	// The case it is for is a machine whose lifecycle and whose disk are owned by
	// different things: whoever launched it holds the first monitor for as long as it
	// runs, and whatever owns the storage under it has to be able to seal a layer without
	// asking the launcher to relay commands it does not understand.
	//
	// Empty for a machine with one driver, which is every machine that does not have that
	// split.
	QMPSocket2 string

	// Serial is a QEMU chardev spec for the console — "file:/path/console.log",
	// "mon:stdio", or empty for no console at all. The kernel prints to the ISA
	// 16550 the machine has; nothing else uses it.
	Serial string

	// Cmdline is the kernel command line. Build it with Cmdline.String().
	Cmdline string

	// IncomingDefer starts QEMU with no machine state, waiting to be told over
	// QMP where to load it from.
	//
	// Deferred and not a URI on the command line, because a restore that keeps
	// the guest's RAM in the memory file needs the x-ignore-shared capability
	// agreed before the first byte is read, and there is no way to pass a
	// migration capability to exec. Whoever drives that owns the lifecycle; this
	// is the machine argument they need.
	IncomingDefer bool

	// Incoming names that source at exec time instead — a migration URI, most
	// usefully "file:/path/to/state". It is how a VM saved on one machine is
	// resumed, rather than booted, on another.
	//
	// The guest does not know this happened: it continues from the instruction it
	// was stopped at, with the memory, the devices and the clock it had. Which is
	// why the CPU model matters more here than anywhere else — see Shape.
	Incoming string
}

// Shape is the four arguments that decide what machine a guest sees: the chipset
// and its options, the CPU model, the vCPU count with its hotplug ceiling, and
// the memory size with its ceiling.
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
	//
	// sata=off and smbus=off remove the two ICH9 functions a q35 builds beside
	// the LPC bridge and this machine has no use for: an AHCI controller at
	// 00:1f.2, on a kernel built without CONFIG_ATA, and an SMBus controller at
	// 00:1f.3 with eight SPD EEPROMs behind it, on a kernel with no i2c bus
	// driver to reach them. Both were on the bus with no driver bound, and the
	// guest paid for enumerating them, for their BARs and for their I/O windows
	// (0x0700-0x073f and 0xc040-0xc05f, gone from the guest's /proc/ioports).
	//
	// Kernel to init, medians of 400 boots each, measured 2026-09-08 on the KVM
	// binary: 62.3 ms as it was, 61.3 ms with sata=off, 61.7 ms with smbus=off,
	// 60.6 ms with both. Nothing else moved — the guest's PCI list loses exactly
	// those two functions, the remaining BARs shift down by the page the AHCI
	// controller had, and a boot onto the real root filesystem still reaches its
	// init.
	//
	// Four more machine options were measured the same way and are deliberately
	// not here, because each bought nothing (same date, 300 boots per variant,
	// medians against a 62.9 ms baseline whose run-to-run spread was ±2 ms):
	//
	//   - usb=off, 63.0 ms, is a no-op twice over. This q35 already reports
	//     /machine usb false, and the binary is built from qemu/devices.mak with
	//     no USB controller in it at all, so usb=on is not even startable here:
	//     "unknown type 'ich9-usb-ehci1'".
	//   - vmport=off, 62.6 ms. The VMware backdoor port is emulated but never
	//     touched: nothing in the guest's /proc/ioports claims it, because a
	//     Linux guest decides whether to talk to it from CPUID and this one
	//     finds KVM.
	//   - smm=off, 62.3 ms. The scepticism it deserves is what makes it a
	//     no-change: this machine enters a PVH ELF kernel directly, has no
	//     pflash and no OVMF, and the 0.6 ms is inside the noise, so the option
	//     would be carrying an interaction with firmware nobody here has for a
	//     number that cannot be told from zero.
	//   - i8042=off, 62.5 ms. It does remove the PS/2 controller, its two ports
	//     and port92 — 92 bytes off the DSDT is the whole visible effect — and
	//     the kernel has no CONFIG_SERIO_I8042 to probe any of it with.
	machine := strings.Join(nonEmpty(
		"q35", "accel=kvm", "kernel-irqchip=on", "hpet=off", "acpi=on",
		"sata=off", "smbus=off", backend,
	), ",")

	cpu := s.CPU
	if cpu == "" {
		cpu = "host"
	}
	// migratable=on drops features QEMU cannot save and reload, which is what a
	// restore does. It is not what makes a CPU portable between machines — a
	// common and expensive misreading. It filters by "can this feature be
	// migrated at all", not by "does the other host have it"; under model "host"
	// the other host is still required to be this one.
	//
	// And it exists only on the models that derive their features from the
	// silicon underneath, which is host and max. A named model has a fixed
	// feature set with nothing to filter, and asking anyway is not ignored:
	//
	//   can't apply global Skylake-Server-v4-x86_64-cpu.migratable=on:
	//   Property 'Skylake-Server-v4-x86_64-cpu.migratable' not found
	//
	// at start-up, in whichever lane first names a model.
	if derivedFromHost(cpu) {
		cpu += ",migratable=on"
	} else {
		// enforce=on, and without it naming a model does not mean what it says.
		//
		// QEMU's default is to warn about features the host cannot provide and
		// start anyway, having quietly removed them — so the same model name
		// yields a different guest CPU on different machines, which is the exact
		// thing naming a model was supposed to prevent. Measured here, asking a
		// Raptor Lake host for Skylake-Server-v4: five warnings about missing
		// AVX-512 and exit 0. A VM saved on a host that had them and resumed on
		// one that did not would fault on an instruction it had already been told
		// it has, long after the warning scrolled past.
		//
		// With this, a host that cannot provide the model refuses to start it:
		// "Host doesn't support requested features", exit 1, before the VM exists.
		cpu += ",enforce=on"
	}

	return Shape{
		Machine: machine,
		CPU:     cpu,
		SMP:     smpArg(s.BootCPUs, s.MaxCPUs),
		Memory:  memoryArg(s.Memory.SizeMB, s.Memory.MaxMB),
	}
}

// derivedFromHost reports whether a CPU model takes its feature set from the
// silicon it runs on, which is what makes a template built with it unusable on
// another machine — and what migratable=on applies to.
func derivedFromHost(model string) bool {
	return model == "host" || model == "max"
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

// memoryArg formats -m.
//
// No slots=, and that is the change: slots are ACPI DIMM sockets, and this
// machine plugs no DIMMs. maxmem on its own is the address space reserved for
// memory devices, which is what virtio-mem needs and all it needs — verified by
// starting a machine with maxmem and no slots.
func memoryArg(sizeMB, maxMB int) string {
	if maxMB > sizeMB {
		return fmt.Sprintf("%d,maxmem=%dM", sizeMB, maxMB)
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
	case s.HotplugDiskPorts < 0 || s.HotplugDiskPorts > MaxHotplugDiskPorts:
		return fmt.Errorf("%d root ports for disks arriving later, and the slot range holds %d",
			s.HotplugDiskPorts, MaxHotplugDiskPorts)
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

	// The balloon, and the only way a running VM here gives memory back.
	//
	// Without it the answer to "how is memory reclaimed?" is "the VM exits". A
	// guest that peaked at 8 GB during a build holds 8 GB of the host until it is
	// shut down, however little of it is still in use — which for a fleet of
	// long-lived development VMs is most of the host, most of the time.
	//
	// free-page-reporting rather than a host-driven target: the guest reports
	// pages it has genuinely freed and QEMU discards them, continuously and with
	// nobody deciding a number. On file-backed memory that is a hole punched in
	// the file, so the host gets the pages back rather than only the mapping. The
	// guest half is CONFIG_PAGE_REPORTING, which this kernel has.
	//
	// deflate-on-oom is the safety valve for the other direction: a guest about
	// to kill a process for want of memory takes some back from the balloon
	// first. It costs nothing when nothing is inflated, and the alternative is a
	// build dying with the host holding memory this VM had already earned.
	//
	// It is safe with templates, which is the part worth writing down. A VM being
	// frozen maps its memory file share=on, so reporting punches holes in the very
	// file that becomes the template — and that is fine, and slightly good: a
	// reported page is one the guest considers free, its content is not relied
	// upon, and it reads back as zero on restore. The template ends up sparser.
	args = append(args, "-device",
		fmt.Sprintf("virtio-balloon-pci,free-page-reporting=on,deflate-on-oom=on,%s,addr=0x%x",
			virtioModern, SlotBalloon))

	if s.VsockCID != 0 {
		args = append(args, "-device",
			fmt.Sprintf("vhost-vsock-pci,guest-cid=%d,%s,addr=0x%x",
				s.VsockCID, virtioModern, SlotVsock))
	}

	// virtio-mem, when this VM is allowed to grow: the region between its boot
	// memory and its ceiling, present as a device and empty.
	//
	// requested-size=0 — nothing is plugged until somebody asks over QMP. That is
	// what lets one template serve VMs of different final sizes: it is taken with
	// the boot memory and an empty device, and each restored VM grows on its own.
	//
	// memory-backend-ram and not a file, unlike the boot memory. The boot memory
	// is file-backed because a template *is* that file; this region is empty when
	// a template is taken, so a second file would be a second empty file to
	// manage. What is plugged at freeze time goes through the migration stream
	// instead, which is correct and only matters for a template taken from a VM
	// that had already grown.
	if s.Memory.MaxMB > s.Memory.SizeMB {
		growth := s.Memory.MaxMB - s.Memory.SizeMB
		args = append(args,
			"-object", fmt.Sprintf("memory-backend-ram,id=%s,size=%dM", memGrowthID, growth),
			"-device", fmt.Sprintf("virtio-mem-pci,id=vmem0,memdev=%s,requested-size=0,%s,addr=0x%x",
				memGrowthID, virtioModern, SlotMem))
	}

	// Empty root ports, for disks this machine will be given while it runs. See
	// Spec.HotplugDiskPorts: the root complex takes no device_add, and a root port does.
	//
	// chassis is the port's identity to the guest's ACPI and has to be unique; the slot
	// number inside a root port is always 0, because a root port has exactly one.
	for i := range s.HotplugDiskPorts {
		args = append(args, "-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d,addr=0x%x",
			HotplugPortID(i), i+1, SlotHotplugBase+i))
	}

	for i, d := range s.Disks {
		// aio=io_uring, and QEMU is built with it for this reason. The default is
		// aio=threads, which hands every request to a worker pool and pays a
		// context switch each way; io_uring submits and completes in batches
		// through one ring shared with the kernel. It was compiled in and never
		// asked for, which is the worst of both — the cost of the dependency
		// without the benefit.
		//
		// discard=unmap lets the guest's TRIM reach the image, so a qcow2 overlay
		// gives its blocks back when files are deleted inside the VM instead of
		// growing to the high-water mark of everything ever written.
		drive := fmt.Sprintf("file=%s,if=none,id=blk%d,format=%s,aio=io_uring,discard=unmap",
			d.Path, i, d.Format)
		if d.Cache != "" {
			drive += ",cache=" + d.Cache
		}
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
		// vhost=on, always. Without it every packet is copied into QEMU, out of
		// it, and back into the kernel; with it the kernel moves frames between
		// the tap and the guest's virtqueues and QEMU is not on the data path at
		// all. QEMU does not default it on, and there is no fallback: a host with
		// no /dev/vhost-net fails at start-up rather than quietly running slowly,
		// which is the right way round — a machine that cannot do this is a
		// different machine.
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=net%d,fd=%d,vhost=on", i, n.TapFD),
			"-device", fmt.Sprintf("virtio-net-pci,netdev=net%d,mac=%s,romfile=,%s,addr=0x%x",
				i, n.MAC, virtioModern, SlotNICBase+i))
	}

	if s.Serial != "" {
		args = append(args, "-serial", s.Serial)
	} else {
		args = append(args, "-serial", "none")
	}

	for _, sock := range []string{s.QMPSocket, s.QMPSocket2} {
		if sock == "" {
			continue
		}
		args = append(args, "-qmp",
			fmt.Sprintf("unix:%s,server=on,wait=off", sock))
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
	h := sha256.New()

	// Length-prefixed, so that no two different machines can produce the same
	// byte stream by moving a delimiter into a value. Every value here is
	// something a caller supplies or a tool prints — a CPU model name, a device
	// list, a line out of /proc/cpuinfo — and none of them is guaranteed to be
	// free of the separator. Prefixing costs nothing and removes the question.
	//
	// The error is discarded because hash.Hash's Write never returns one; that is
	// part of the interface's contract.
	write := func(key, value string) {
		_, _ = fmt.Fprintf(h, "%s=%d:%s\n", key, len(value), value)
	}

	for _, f := range []struct{ name, path string }{
		{"qemu", s.QEMU},
		{"kernel", s.Kernel},
		{"initrd", s.Initrd},
	} {
		if f.path == "" {
			// An absent initrd is part of the identity too: a machine that boots
			// one and a machine that does not are different machines.
			write(f.name, "none")
			continue
		}
		sum, err := fileSum(f.path)
		if err != nil {
			return "", fmt.Errorf("fingerprinting %s: %w", f.name, err)
		}
		write(f.name, sum)
	}
	ident, err := s.Identity()
	if err != nil {
		return "", err
	}
	write("identity", ident)

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Identity is everything the fingerprint hashes except the contents of those
// three files: the machine's shape, its device topology, and the host's own CPU
// when the guest is being shown it.
//
// It is separate because reading the three files is the expensive half — 76 MB
// of SHA-256, 29 ms on a machine measured — and a caller that memoises that half
// needs the other half whole to key the memo on. Under-keying it is the failure
// that has no symptom: a stale fingerprint is a template that matches a machine
// it does not describe, and a restore into it is undefined rather than an error.
// So there is no list here for a caller to keep in step; there is this.
func (s Spec) Identity() (string, error) {
	shape := s.TemplateShape()

	var b strings.Builder
	write := func(key, value string) {
		_, _ = fmt.Fprintf(&b, "%s=%d:%s\n", key, len(value), value)
	}
	write("machine", shape.Machine)
	write("cpu", shape.CPU)
	write("smp", shape.SMP)
	write("memory", shape.Memory)
	write("devices", s.topology())

	// The host's own CPU, but only when the guest is being shown it.
	//
	// Under model "host" the guest is told through CPUID exactly which
	// instructions this silicon has, and it never asks again — so a template
	// taken here describes a CPU the next machine may not have, and restoring it
	// there is a guest executing an instruction that does not exist. Nothing
	// about the QEMU binary, the kernel or the four shape arguments differs
	// between two hosts, so without this a Zen 4 template and a Skylake template
	// have the same fingerprint and each machine happily accepts the other's.
	//
	// Under a named model it is deliberately left out: every host shows the guest
	// the same CPU, which is the entire point of naming one, and folding the host
	// in would partition templates per machine for no reason.
	if derivedFromHost(strings.SplitN(shape.CPU, ",", 2)[0]) {
		cpu, err := readHostCPU()
		if err != nil {
			return "", fmt.Errorf("fingerprinting the host CPU, which model %q exposes to the guest: %w",
				strings.SplitN(shape.CPU, ",", 2)[0], err)
		}
		write("host-cpu", cpu)
	}

	return b.String(), nil
}

// topology is the machine's device list — which models, at which slots — with
// everything about *this* VM removed: no paths, no file descriptors, no context
// id, no MAC.
//
// It is in the fingerprint because a restore loads device state into a machine
// that has to have the same devices, and without it the fingerprint answers the
// question wrongly in both directions. Adding a device to this package changes
// what a template may restore into and would not have moved the hash — a
// balloon was added at slot 0x04 and every existing template would have gone on
// matching a machine it can no longer be restored into. And two VMs given
// different numbers of disks have different device state and had the same
// fingerprint, so one could be handed the other's template.
//
// What is deliberately not in it: the backing files and the identifiers, and the
// disks and NICs themselves.
//
// The files and identifiers, because two VMs with one disk each are the same
// machine whether that disk is a database or a scratch overlay — that is the
// whole reason a template is worth having. Each of the three is a thing a
// restore is known to be allowed to differ in, and each for its own reason: the
// vsock context id is not carried in the migration stream at all, and the guest
// re-reads it when QEMU resets the transport after the restore; a disk is
// cold-plugged onto the running guest afterwards and found with a PCI rescan;
// and what sits behind a NIC is a host-side file descriptor the guest never
// sees. What is present here is the device *model at its slot*, which is what
// the state being loaded describes.
//
// The disks and NICs, because a machine restored from a template does not have
// them yet. A template is built from the emptiest VM there is — it does not know
// which workload it will become — and each restored VM is given its disks and its
// address afterwards, cold-plugged onto a guest that is already running. So the
// device set that has to match is the one present when the state is loaded, and
// counting disks here would mean a VM never finding the template it should
// restore from.
//
// The devices below are the ones that are there at that moment, on both sides.
func (s Spec) topology() string {
	var b strings.Builder
	fmt.Fprintf(&b, "vmgenid;virtio-rng-pci@%#x;virtio-balloon-pci@%#x", SlotRNG, SlotBalloon)
	if s.VsockCID != 0 {
		fmt.Fprintf(&b, ";vhost-vsock-pci@%#x", SlotVsock)
	}
	if s.Memory.MaxMB > s.Memory.SizeMB {
		fmt.Fprintf(&b, ";virtio-mem-pci@%#x", SlotMem)
	}
	if s.Serial != "" {
		b.WriteString(";isa-serial")
	}
	// The empty root ports, which are devices present when the state is loaded even
	// though what they are for is not. A machine restored into one with a different
	// number of them is a machine whose bus does not match its own device state.
	if s.HotplugDiskPorts > 0 {
		fmt.Fprintf(&b, ";pcie-root-port@%#x*%d", SlotHotplugBase, s.HotplugDiskPorts)
	}
	return b.String()
}

// readHostCPU is HostCPUModel, indirected so a test can be two different hosts.
var readHostCPU = HostCPUModel

// HostCPUModel reports the host CPU's model name, which model "host" makes part
// of what a template describes.
//
// The model name and not the feature flags. The flags would be the exact thing —
// they are what the guest is shown — but they also move with microcode updates
// and kernel mitigations, so hashing them would invalidate every template on a
// machine that has not meaningfully changed. The model is the coarse identity
// that separates one host's silicon from another's, which is what this is for.
func HostCPUModel() (string, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("reading /proc/cpuinfo: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, ok := strings.CutPrefix(line, "model name")
		if !ok {
			continue
		}
		if _, value, found := strings.Cut(name, ":"); found {
			return strings.TrimSpace(value), nil
		}
	}
	return "", errors.New("no model name in /proc/cpuinfo")
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
