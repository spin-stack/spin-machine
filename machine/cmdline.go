// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"fmt"
	"strings"
)

// Cmdline is the kernel command line this machine boots with.
//
// It is part of the machine and not of whoever launches it: most of what is here
// is a statement about the hardware — that the PCI bus stops at 0, that the TSC
// can be trusted, that there is no timer to probe for — and getting one of them
// wrong shows up as boot time, not as an error.
//
// What is *not* here is the guest's init and its arguments. Those belong to
// whatever software runs inside, which this repository does not build and should
// not have an opinion about; pass them in Init and InitArgs.
type Cmdline struct {
	// Console the kernel prints to. "ttyS0" is the ISA 16550 this machine has.
	// Empty means the kernel prints to nothing, which is what a production boot
	// wants: the ring buffer still records everything.
	Console string

	// Quiet and LogLevel set how much reaches the console. LogLevel is the
	// console threshold only — every message is recorded either way.
	Quiet    bool
	LogLevel int

	// Init is the program the kernel runs as PID 1, and InitArgs what follows it
	// after a "--". Empty leaves both out, and the kernel picks its default.
	Init     string
	InitArgs []string

	// Root, when set, is passed as root= and tells the kernel to mount a block
	// device rather than stay on an initramfs. "/dev/vda" is the first disk in
	// Spec.Disks. RootFlags is passed as rw or ro.
	Root         string
	RootReadonly bool

	// Extra is appended verbatim, last, so a caller can say something this
	// machine has no opinion about.
	Extra []string
}

// DefaultCmdline is a production boot: silent console, PCI scan stopped at bus
// 0, and the timing shortcuts a KVM guest can take.
func DefaultCmdline() Cmdline {
	return Cmdline{
		Console:  "ttyS0",
		Quiet:    true,
		LogLevel: 3,
	}
}

// String renders the command line.
func (c Cmdline) String() string {
	var parts []string

	if c.Console != "" {
		parts = append(parts, "console="+c.Console)
	}
	if c.Quiet {
		parts = append(parts, "quiet")
	}
	parts = append(parts, fmt.Sprintf("loglevel=%d", c.LogLevel))

	if c.Root != "" {
		mode := "rw"
		if c.RootReadonly {
			mode = "ro"
		}
		parts = append(parts, "root="+c.Root, mode)
	}

	// Scan PCI bus 0 and stop.
	//
	// arch/x86/pci/mmconfig-shared.c takes the last bus of the ECAM window as
	// the last bus worth probing, and a q35 advertises the window for buses
	// 00-ff, so that is 255. pci_subsys_init then calls
	// pcibios_fixup_peer_bridges(), which walks all 256 buses reading the vendor
	// id at 32 device functions on each looking for a peer host bridge: 8192
	// configuration reads, every one a VM exit, to discover that there is
	// nothing behind a bus this machine never had.
	//
	// Setting it on the command line wins because the parser runs before
	// MMCONFIG init, so the "not set yet" test that picks 255 no longer fires.
	// Measured, full device set, kernel to init: 87.6 / 85.0 ms without,
	// 51.1 / 51.8 with, and pci_subsys_init from 30 ms to 1.
	// `pci=lastbus=255` behaves exactly like no flag at all, which is the check
	// that this is the mechanism and not a coincidence.
	//
	// It is a constraint and not just a flag. A device behind a PCIe root port
	// would be on bus 1 and would simply not exist for the guest — no error, no
	// warning, an absent disk. Every device this machine has is placed by hand
	// at a fixed slot on bus 0 (see the slot constants); whoever changes that
	// has to change this.
	parts = append(parts, "pci=lastbus=0")

	// Panic rather than sit at a prompt nobody is watching. A VM is cattle and
	// the caller notices the process exiting; a guest waiting for a keystroke on
	// a console that is not connected is a VM that has stopped without saying so.
	parts = append(parts, "panic=1")

	// Interfaces named eth0 and not by their PCI path. The path is a function of
	// the slot map above, so predictable naming here would make a NIC's name a
	// consequence of how many disks the VM was given.
	parts = append(parts, "net.ifnames=0", "biosdevname=0")

	// cgroup v2 only. Both hierarchies mounted is two accounting trees for one
	// set of processes, and nothing this machine runs asks for v1.
	parts = append(parts, "systemd.unified_cgroup_hierarchy=1", "cgroup_no_v1=all")

	// Bring memory online as it arrives, because on this machine it does: a VM
	// with a ceiling is given a virtio-mem device it can grow through.
	//
	// Without this the growth silently stops at the boot size. Memory that is
	// added and never onlined is memory the guest cannot use but must still
	// describe, so the driver declines to take more — measured on this machine
	// with 1 GiB of boot memory: asked for 2048 MiB and then 3072 MiB, it plugged
	// 1024 MiB and stayed there, with no error on either side. With this, the
	// same requests plug 2048 and 3072.
	//
	// Unconditional, and not only when there is a ceiling: it costs one token on
	// a machine with no virtio-mem, and a command line that changes with the
	// memory configuration is a second thing that has to agree with the first.
	parts = append(parts, "memhp_default_state=online")

	// A short-lived VM never amortises the tickless machinery's setup cost, and
	// on a guest the timer interrupt it saves is cheap.
	parts = append(parts, "nohz=off")

	// Timing shortcuts a KVM guest can take:
	//   no_timer_check           skip the boot-time timer IRQ delivery probe,
	//                            which exists for hardware that misroutes it.
	//   rcupdate.rcu_expedited=1 expedite RCU grace periods during boot.
	parts = append(parts, "no_timer_check", "rcupdate.rcu_expedited=1")

	parts = append(parts, c.Extra...)

	if c.Init != "" {
		init := "init=" + c.Init
		if len(c.InitArgs) > 0 {
			init += " -- " + strings.Join(c.InitArgs, " ")
		}
		parts = append(parts, init)
	}

	return strings.Join(parts, " ")
}

// Profiling turns a command line into one that measures the boot it performs.
//
// The console goes silent rather than verbose, which is the part that is not
// obvious: a console registers during the device_initcall phase, and registering
// it replays the whole printk ring into it synchronously, inside that initcall.
// A verbose boot therefore reports a large cost in whichever console driver
// registered — the measurement writing itself out. With nothing reaching the
// console, the ring buffer still records everything, which is all a profile
// needs, and something inside the guest reads it afterwards.
//
// log_buf_len is not about the console: the default 256 KiB ring overflows under
// initcall_debug and silently drops the earliest entries, which are exactly the
// early core and subsystem initcalls a profile exists to see.
func (c Cmdline) Profiling() Cmdline {
	c.Quiet = false
	c.LogLevel = 0
	c.Extra = append(c.Extra,
		"initcall_debug", "printk.time=1", "log_buf_len=4M",
		// The initcall tracepoints are the only source for where a level
		// *boundary* falls: initcall_debug times each call and says nothing
		// about the time between them, and that time is the larger half of boot.
		"trace_event=initcall:*", "trace_buf_size=4M",
	)
	return c
}
