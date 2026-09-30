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

`Spec.Fingerprint` is the machine's identity. A checkpoint resumes only onto a machine with
the fingerprint of the one it was saved on; across two that differ a resume is undefined
rather than an error. It hashes:

- the QEMU binary, the kernel, the initrd and the firmware the guest runs (the BIOS,
  `qboot.bin` by default, and `pvh.bin`), **by content** — not by path;
- the shape: the `-machine`, `-accel`, `-cpu`, `-smp` and `-m` arguments;
- the devices present when state is loaded: vmgenid, the RNG and the balloon, and, when the
  spec has them, the vsock, the virtio-mem region, the serial port, the hotplug controller,
  and the NICs by count and MTU;
- the host's CPU model, but only when the guest is shown the host's CPU (`host` under KVM,
  `max` under TCG). See [migration.md](migration.md).

It leaves out what a resume is allowed to differ in — paths, descriptors, the vsock context
id, MAC addresses — and the disks, which are the VM's and not the machine's: a host computes
its fingerprint before any VM exists, and whoever resumes a checkpoint gives it the disks it
was saved with.

`spin-machine fingerprint` prints it with the shape that went into it, because the number
alone answers "did it change?" and not "what changed?".

