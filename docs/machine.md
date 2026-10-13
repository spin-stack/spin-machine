# The machine

`machine/` is the definition, and it is Go rather than a document because a definition
nothing executes drifts. `task shell` boots a VM through it, so a slot that moved or a
kernel argument that stopped working stops working here first.

```go
rel, err := machine.OpenRelease("/usr/share/spin-stack")  // says which file is missing, if one is
spec := rel.Spec()                                        // QEMU, Kernel, Firmware
spec.Initrd = …                                           // the caller's: a release carries none
spec.BootCPUs = 2
spec.Memory = machine.Memory{SizeMB: 2048}
spec.Disks = []machine.Disk{{Path: overlay, Format: "qcow2"}}
spec.VsockCID = 7
spec.Monitors = []machine.Monitor{{Socket: "/run/vm/qmp.sock"}}
spec.Cmdline = machine.DefaultCmdline()

args, err := spec.Args()          // the QEMU command line
fp, err := spec.Fingerprint()     // which checkpoints this machine may resume
```

`Args` validates first. Everything it refuses — a disk with no format, a monitor that is
both a path and a descriptor, a kernel command line the kernel would truncate — is
something that otherwise boots and is subtly wrong.

## What is load-bearing and invisible from outside

- **Every device on the command line sits at a fixed slot on bus 0.** A fixed slot makes
  the guest's enumeration order independent of the order code runs in. Bus 0 is what lets
  the kernel be told `pci=lastbus=0`, which stops the search for peer host bridges across
  all 256 buses — 8192 configuration reads, every one a VM exit — measured at 87.6 ms to
  51.1 ms, kernel to init. The flag bounds only that search: disks attached while the machine
  runs are SCSI disks on the `HotplugDisks` controller, not devices behind a bridge. A second
  host bridge would not be found.
- **`vmgenid`.** A VM resumed from a checkpoint starts with the checkpoint's memory,
  including the state of the guest's random pool. Two guests resumed from one checkpoint,
  or one resumed twice, would otherwise produce the same "random" bytes until something
  reseeded them.

## The fingerprint

`Spec.Fingerprint` is the machine's contract. A checkpoint resumes only onto a machine with
the fingerprint of the one it was saved on; across two that differ a resume is undefined
rather than an error. It hashes:

- the shape: the `-machine` argument with its **versioned** machine type
  (`machine.MachineType`, `pc-q35-11.1`), `-accel`, `-cpu`, `-smp` and `-m`;
- the chipset's globals and every device present when state is loaded, each as the command line
  gives it, properties and slot included (`Spec.Contract`): vmgenid, the RNG and the balloon,
  and, when the spec has them, the vsock, the virtio-mem region, the serial port, the hotplug
  controller and the NICs with their MTU;
- the BIOS's size: it is the `pc.bios` RAM block, which a checkpoint carries whole and a resume
  refuses at another length. Every other read-only block is the machine type's or the chipset's
  (`machine.MachineBlocks`), and `task boot:roms` fails when a booted machine maps one that is
  neither;
- the host's CPU model, but only when the guest is shown the host's CPU (`host` under KVM,
  `max` under TCG). See [migration.md](migration.md).

It leaves out what a resume is allowed to differ in — paths, descriptors, ids, the backends
devices are bound to, the vsock context id, MAC addresses — and the disks, which are the VM's
and not the machine's: a host computes its fingerprint before any VM exists, and whoever resumes
a checkpoint gives it the disks it was saved with.

And it leaves out the files a guest does not reload on a resume: the QEMU binary, the kernel,
the initrd and `pvh.bin`. QEMU's migration promise is per versioned machine type — a newer QEMU
loads what an older one of the same `pc-q35-X.Y` saved, the newer type's changes to defaults
switched back off for the older (`hw_compat_X_Y`, `pc_compat_X_Y`) — and a resumed guest runs the
kernel in its saved memory until it reboots. `task boot:compat` holds this tree to that promise:
a checkpoint the newest release saved resumes on this tree's build.

Moving `machine.MachineType` is moving every guest to new hardware, decided, not a side effect of
a QEMU bump: `qemu:verify` says when a QEMU offers a newer type and fails once the pinned one is
deprecated, and `task qemu:compat` prints what the newer types change against it.

`spin-machine fingerprint` prints it with the shape and the contract that went into it, because
the number alone answers "did it change?" and not "what changed?".

