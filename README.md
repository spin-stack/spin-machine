# spin-machine

A virtual machine: QEMU, the guest kernel, the base image, and the definition of the
machine they make.

The four are one thing, and the reason is not tidiness. A VM restored from a template
loads device and CPU state into a machine that has to be the same shape as the one the
template was frozen from, and nothing checks that at run time. So the machine's identity
is computed from the things that decide its shape — `machine.Spec.Fingerprint` hashes the
QEMU binary, the kernel and the initrd *by content*, together with the four arguments that
decide what a guest sees. Two machines with the same fingerprint can exchange templates.
Two with different fingerprints cannot, and a release in which any of the three files moved
has a different fingerprint by construction.

One repository, one version, one generation of templates.

```
machine/    what the machine is: PCI slot map, shape, memory backing, kernel command line
cmd/        spin-machine (boot one, print its fingerprint) and the debug init it boots
qemu/       Dockerfile + devices.mak
kernel/     Dockerfile + config-<version>-<arch>
image/      mkosi configuration producing base.qcow2 (ext4 inside)
hack/       release
```

```
task build         # everything, into _output/
task shell         # boot the machine and look around inside it
task lint          # gofmt, vet, and whether the scripts and Taskfiles parse
task test          # the machine definition
task fingerprint   # this machine's identity
task release       # one tarball, one version
```

Each part's targets live beside what they build — `qemu/Taskfile.yml`, `kernel/Taskfile.yml`,
`image/Taskfile.yml` — so `task qemu:build` is next to `qemu/Dockerfile`. The root
`Taskfile.yml` holds the vars every part reads and the targets that cross all of them.

**What is deliberately not here: the software that runs inside a guest.** This repository
builds a machine. It knows nothing about what boots on it, and a release is not bootable on
its own by design — whoever runs guests brings the initrd. `cmd/spin-machine-init` is the
one exception and is a debugging tool, not a release artefact.

## What it publishes

One tarball:

| path under `usr/share/spin-stack/` | |
|---|---|
| `bin/qemu-system-x86_64` | KVM only — it refuses to emulate, on purpose |
| `bin/qemu-system-x86_64-tcg` | for CI, which has no `/dev/kvm` |
| `bin/qemu-img` | |
| `qemu/{bios.bin,bios-256k.bin,pvh.bin,kvmvapic.bin,efi-virtio.rom}` | |
| `kernel/vmlinux` | plus `kernel-config` |
| `image/base.qcow2` | read-only, 0444 |
| `machine.env` | the version and the three checksums that decide template validity |

## The machine

`machine/` is the definition, and it is Go rather than a document because a definition
nothing executes drifts. `task shell` boots a VM through it, so a slot that moved or a
kernel argument that stopped working stops working here first.

```go
spec := machine.Spec{
    QEMU: …, Kernel: …, Initrd: …, Firmware: …,
    BootCPUs: 2,
    Memory:   machine.Memory{SizeMB: 2048, File: "/…/pc.ram", Shared: true},
    Disks:    []machine.Disk{{Path: "base.qcow2", Format: "qcow2", Readonly: true}},
    VsockCID: 7,
}
args, err := spec.Args()          // the QEMU command line
fp, err := spec.Fingerprint()     // which templates this machine may restore from
```

Three things in it are load-bearing and invisible from outside:

- **Every device sits at a fixed slot on bus 0.** That is what lets the kernel be told
  `pci=lastbus=0`, which stops a scan of all 256 buses — 8192 configuration reads, every
  one a VM exit — and was measured at 87.6 ms to 51.1 ms, kernel to init. It is a
  constraint and not a flag: a device behind a PCIe root port would be on bus 1 and would
  simply not exist for the guest, with no error.
- **`pc.ram`.** When guest memory comes from a `memory-backend-file`, the object is named
  `pc.ram` — what QEMU calls the machine's main RAM block when it makes one itself —
  because migration matches RAM blocks by name across save and restore. A template is taken
  with the file mapped shared, so the pages the VM dirties land in it; a VM restoring from
  one maps the same file private, sees the template's memory, and keeps its writes to
  itself. That is why one template file can serve many VMs without being copied.
- **`vmgenid`.** Every VM restored from a template starts with the template's memory,
  including the state of the guest's random pool. Two guests restored from one template
  would otherwise produce the same "random" bytes until something reseeded them.

`Fingerprint` always computes the shape as if RAM were file-backed, because a template
always is. Otherwise a VM that has not been given a memory file yet could never find the
template it would itself produce.

## Looking inside the image

```
task shell                    # systemd; log in as root, password spinbox
task shell INIT=/bin/bash     # a bare shell, for when systemd is the broken thing
```

The default is `/sbin/init` because that is what this image is: a userland whose first
process is systemd. Booting a bare shell answers a different question — `systemd-analyze`
in it replies *"System has not been booted with systemd as init system (PID 1)"*, which is
true and useless.

It boots this QEMU and this kernel over a throwaway qcow2 overlay on `base.qcow2`, through
`spin-machine boot`, with the serial console on stdio.

The initrd it boots is `cmd/spin-machine-init`: static Go that mounts `/proc`, `/sys`,
devtmpfs and devpts, finds the root disk, moves onto it and execs. That is deliberately
where it stops — everything past it, an RPC channel to the host or a container lifecycle,
belongs to whatever software runs guests. What is here is the part that is the same
whoever that is, which is also why it resolves a disk by virtio-blk serial as well as by
`/dev` node: a node depends on the order the guest probed the bus in, and a serial does
not.

Three things the shell has already found, all of them true of the image and none of them
visible from outside:

- **systemd starts no login on the serial port.** It starts `getty@tty1`, a virtual console
  on a machine whose QEMU has no display adapter compiled in. Enabling the distribution's
  `serial-getty@ttyS0` does not help either: it carries `BindsTo=dev-ttyS0.device`, and a
  `.device` unit exists only if udev announced it — and this image masks `systemd-udevd`,
  because a VM's hardware is fixed and udev is boot time spent discovering it. So the debug
  init writes its own ten-line agetty unit into the throwaway overlay, and the shared base
  never carries a debugging convenience production has no use for.
- **`ssh.service` fails five times and gives up.** The image has no SSH host keys, by
  design — they are identity, and identity is not baked into an image many VMs share — and
  `sshd-keygen.service`, which would generate them, has `ConditionFirstBoot=yes` and does
  not run before `sshd -t` is asked to validate a configuration with no keys. Whoever wants
  SSH in a guest has to provision the keys the way the rest of a VM's identity is
  provisioned.
- **Nothing mounts `/proc` before an init runs**, so a machine booted straight to a shell
  has `df` warning, `free` failing and `poweroff` answering *"Running in chroot, ignoring
  request"*. That is what PID 1 gets before an init has run, not a fault in the image.

## The parts

### QEMU

One build. Both the KVM binary and a TCG one for runners with no `/dev/kvm`: a host serving
tenants must fail at start-up rather than run a tenant's VM at a tenth of the speed, and the
build asserts both halves, since neither is visible in a file listing.

`--enable-tools` ships `qemu-img`, which is not a convenience: a container's writable layer
is a qcow2 overlay created by running it, so a release with the emulator and without it
cannot give a container anywhere to write. `qemu-nbd` comes out of the same flag and is
deliberately not shipped — it exports a disk over NBD, which nothing here does.

`vmdk` is kept, and it is the one format held open for a consumer that has not moved to the
qcow2 base image. Dropping it is a one-line change, and it invalidates every template in
existence — as any change to this binary does.

**PVH, not BIOS.** The kernel is an ELF `vmlinux` with Xen PVH notes and QEMU enters it
through `pvh.bin`. There is no bootloader and no UEFI: the same guest under UEFI + Secure
Boot was measured at +356 ms and rejected.

### Guest kernel

A version and a config file. It is here because a kernel and the machine that boots it are
one release: changing either invalidates every template taken against the previous pair.

The config is not stable furniture. In one week it gained `CONFIG_VMGENID` and
`CONFIG_PTP_1588_CLOCK_KVM`, lost `CONFIG_CRYPTO_JITTERENTROPY` (−3.7 ms of boot) and had
its symbol table stripped (−11.8 ms). Every one of those changes invalidates every template
in the fleet, which is the design — they should not be quiet.

`kernel/Dockerfile` builds on `debian:bookworm` deliberately: a different compiler is a
different `vmlinux`, which is a different fingerprint. Worth doing on purpose, not while
moving a file.

### Base image

A **qcow2 base, read-only, with a fresh qcow2 overlay per VM**:

```
qemu-img create -f qcow2 -F qcow2 -b base.qcow2 overlay.qcow2
```

Copy-on-write happens in the block layer, where QEMU does it, rather than in a filesystem
stacked inside the guest.

The contents are an Ubuntu 26.04 userland with systemd as the container's first process,
the tools a workspace expects, and boot optimizations that were each measured —
`image/mkosi.extra/usr/local/lib/spin-base/optimize-systemd.sh` names the milliseconds every
mask saved, and is an unmodified copy for that reason.

**ext4**, because the guest kernel has `EXT4_FS`, `EROFS_FS` and `OVERLAY_FS` and explicitly
not `XFS_FS`, `BTRFS_FS` or `SQUASHFS`. Anything else starts with a kernel config change one
directory over — which `kernel/Dockerfile` now asserts.

**Partitionless**, not a bootable disk with an ESP and a GPT, which nothing here would read.
mkosi produces the tree (`Format=directory`); `image/build.sh` runs `mkfs.ext4 -d` — no loop
device, no mount — and wraps the result with `qemu-img convert`.

#### The two traps

- **The base must not be written to.** Every VM maps it read-only through a backing file and
  many VMs share one. A build step that opens it read-write invalidates every overlay in
  existence — silently, because the overlays keep working until they read a cluster that
  moved. It is written `0444`, and `task shell` checks its checksum across a boot.
- **No identity in the image.** No hostname, no machine-id, no `/etc/resolv.conf`, no SSH
  host keys, no random seed. A VM restored from a frozen template is handed its identity
  afterwards; anything baked in is shared by every VM that ever boots from it.
  `image/build.sh` asserts each of these on the finished filesystem — `/etc/machine-id`
  present and *empty*, everything else absent.

## Why mkosi does not run under BuildKit

mkosi assembles a root filesystem by unsharing a user and mount namespace, and BuildKit
forbids it (`mkosi was forbidden to unshare namespaces`, `PermissionError` on `unshare(2)`).
The alternatives were `RUN --security=insecure`, which needs a `buildkitd` started with an
entitlement flag — so `task image:build` would fail on any machine whose builder was created
the ordinary way — or a container run with the privileges mkosi needs. `image/Dockerfile`
pins the toolchain, `image/build.sh` is the build, and the task runs it with
`--cap-add SYS_ADMIN` and seccomp/apparmor unconfined. Not `--privileged`.

## CI

Five workflows, and the split is about cost. `ci.yml` runs on every push and builds none of
the three artefacts — it is `task lint` and `task test`, which is fast and catches most
mistakes. Each artefact has a path-triggered workflow of its own, because QEMU and the
kernel are tens of minutes each and the base image is ~1.5 GB of apt: `qemu.yml`,
`kernel.yml`, `image.yml`. Each ends in the verification that belongs to it, so a build
that lost a device, a kernel that lost its PVH notes, or an image that grew an identity
fails in the workflow that produced it.

`release.yml` builds all three and packs one tarball. Versions are **CalVer**,
`v20260909.01`: a release of this repository is the machine as it stood on a date. There is
no API here to promise compatibility about, and the one thing a version could promise —
that templates still match — is decided by the fingerprint of the artefacts, not by a
number anybody chose. Pushing a `v*` tag releases that version; running the workflow by
hand with no input generates the next sequence for today, tags the commit, and puts the
three checksums in the release notes.

## Status

Built and verified on 2026-09-07:

- **QEMU 11.1.1** builds, both binaries, and the build's own assertions pass: virtio-blk,
  virtio-net, vhost-vsock and virtconsole present, no e1000/rtl8139/vmxnet3, q35 and no
  pc-i440fx, `qemu-img` present, and the accelerator split.
- **The kernel** builds, the PVH notes survive the strip, and its config comes out
  unchanged after `olddefconfig`.
- **base.qcow2** builds (~860 MB). `SOURCE_DATE_EPOCH` normalizes timestamps, but the
  image is not claimed to be bit-reproducible: the userland comes from a live archive.
  What a release contains is the checksum in `machine.env`.
- **The machine boots through its own definition.** `task shell` runs
  `spin-machine boot`, which builds the QEMU command line from `machine/`; inside the
  guest, `lspci` shows the RNG at `00:03.0` and the disk at `00:05.0`, exactly the slot map
  the package declares. `poweroff -f` exits 0 and the base image comes back byte-identical.
- **systemd boots as PID 1 and gives a login on the serial console.** `Ubuntu 26.04.1 LTS
  localhost ttyS0` / `localhost login:`, over the ten-line agetty unit the debug init
  writes into the overlay.
- `go test ./...` covers the shape, the slot map, and what does and does not move the
  fingerprint.

Three things found by running it, and fixed in the tree:

- `check-docker-config.sh` needs `apparmor_parser` and `sysctl`, or it fails a config that
  is correct.
- `qemu-img --help` wraps its format list across lines, so a line-oriented `grep` for a
  format matched nothing and failed a build whose every other assertion had passed.
- `debugfs -R` exits 0 whether or not the file it was asked about exists. Every filesystem
  check in `image/build.sh` reads its output instead.
