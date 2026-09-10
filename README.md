# spin-machine

[![CI](https://github.com/spin-stack/spin-machine/actions/workflows/ci.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/ci.yml)
[![QEMU](https://github.com/spin-stack/spin-machine/actions/workflows/qemu.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/qemu.yml)
[![Kernel](https://github.com/spin-stack/spin-machine/actions/workflows/kernel.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/kernel.yml)
[![Base image](https://github.com/spin-stack/spin-machine/actions/workflows/image.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/image.yml)

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
CLAUDE.md   how to work in here
qemu/       Dockerfile + devices.mak
kernel/     Dockerfile + config-<version>-<arch>
image/      mkosi configuration producing rootfs.qcow2 (ext4 inside)
hack/       release
```

```
task build         # everything, into _output/
task shell         # boot the machine and look around inside it
task lint          # gofmt, vet, and whether the scripts and Taskfiles parse
task test          # the machine definition
task verify:args   # and whether the QEMU in _output/ accepts what it produces
task fingerprint   # this machine's identity
task release       # one tarball, one version
```

Each part's targets live beside what they build — `qemu/Taskfile.yml`, `kernel/Taskfile.yml`,
`image/Taskfile.yml` — so `task qemu:build` is next to `qemu/Dockerfile`. The root
`Taskfile.yml` holds the vars every part reads and the targets that cross all of them.

**What is deliberately not here: the software that runs inside a guest.** This repository
builds a machine. It knows nothing about what boots on it, and a release is not bootable on
its own by design — whoever runs guests brings the initrd. There is no exception to that.

## What it publishes

One tarball:

| path under `usr/share/spin-stack/` | |
|---|---|
| `bin/qemu-system-x86_64` | static; KVM only — it refuses to emulate, on purpose |
| `bin/qemu-system-x86_64-tcg` | for CI, which has no `/dev/kvm` |
| `bin/qemu-img` | |
| `qemu/{bios.bin,bios-256k.bin,pvh.bin,kvmvapic.bin,efi-virtio.rom}` | |
| `kernel/vmlinux` | plus `kernel-config` |
| `image/rootfs.qcow2` | read-only, 0444 |
| `machine.env` | the version and the three checksums that decide template validity |
| `SOURCES` | every upstream source by version, URL and SHA-256, and the written offer |
| `packages.txt` | every package and exact version in the base image |

`LICENSE` and `NOTICE` sit at the root of the tarball, next to `install.sh`.

`task build` writes that same tree into `_output/`, byte for byte the layout above, and
`machine.Open` reads either. There is one layout: nothing rearranges the files on the way
out of a build, into a tarball or into a consumer, because the three used to differ and
what fell out of the translation between them was a path that existed and held the
previous release's kernel.

```go
rel, err := machine.Open("/usr/share/spin-stack")  // says which file is missing, if one is
spec := rel.Spec()                                 // QEMU, Kernel, Firmware
img, err := rel.Rootfs()
```

## The machine

`machine/` is the definition, and it is Go rather than a document because a definition
nothing executes drifts. `task shell` boots a VM through it, so a slot that moved or a
kernel argument that stopped working stops working here first.

```go
spec := machine.Spec{
    QEMU: …, Kernel: …, Initrd: …, Firmware: …,
    BootCPUs: 2,
    Memory:   machine.Memory{SizeMB: 2048, File: "/…/pc.ram", Shared: true},
    Disks:    []machine.Disk{{Path: overlay, Format: "qcow2"}},
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

## Moving a VM to another machine

Stopping a VM here and resuming it there is a lifecycle, and this repository does not
implement one — it builds the machine that lifecycle runs on, and offers the two arguments
it needs: `Memory.File` (the guest's RAM lives in a file, so it can stay there while the
device state is written elsewhere) and `IncomingDefer` (start with no state and wait to be
told where it is, over QMP, because the capability that keeps the RAM in the file has to be
agreed before the first byte is read).

**`Spec.CPU` is what decides whether a VM can move at all.** The default, `host`, shows the
guest this host's own feature set through CPUID — which the guest reads once and never
questions. Resume that somewhere without AVX-512 and it executes an instruction that is not
there. So `Fingerprint` folds the host's CPU model in whenever the model derives from the
host, and a state saved here does not match a machine elsewhere.

Naming a model — the oldest microarchitecture in the fleet — makes every host show the same
CPU, and the host's own silicon drops out of the fingerprint. Measured, restoring a state
saved with `Broadwell-v4`:

| CPU on the second host | |
|---|---|
| `Broadwell-v4` | resumes |
| `Skylake-Client-v4` (richer) | resumes |
| `Nehalem` (poorer) | refuses: `Failed to set special registers` |

Save with the baseline, and any host that meets or exceeds it can take the VM.

**A named model carries `enforce=on`**, and without it naming one means nothing. QEMU's
default is to warn about features the host cannot provide and start anyway, having quietly
removed them — so the same model name gives a different guest CPU on different machines.
Measured: asking a Raptor Lake host for `Skylake-Server-v4` gives five warnings about
missing AVX-512 and exit 0. With `enforce=on` it is `Host doesn't support requested
features` and exit 1, before the VM exists.

The device set has to match too, down to whether there is a serial port — restoring without
one says `Unknown section or instance 'serial'`. All of it is in the fingerprint, except
the disks and NICs: those are cold-plugged onto a guest that is already running, so they are
not there when the state is loaded and counting them would stop a VM finding its template.

## Looking inside the image

```
task shell                    # systemd; the console autologins as root
task shell INIT=/bin/bash     # a bare shell, for when systemd is the broken thing
```

The default is `/sbin/init` because that is what this image is: a userland whose first
process is systemd. Booting a bare shell answers a different question — `systemd-analyze`
in it replies *"System has not been booted with systemd as init system (PID 1)"*, which is
true and useless.

It boots this QEMU and this kernel over a throwaway qcow2 overlay on `rootfs.qcow2`, through
`spin-machine boot`, with the serial console on stdio.

It boots with no initrd at all: `root=/dev/vda rw init=/sbin/init`, which the kernel can
serve because virtio-blk and ext4 are built in and `image/build.sh` writes a partitionless
filesystem. There was a debug initramfs here until 2026-09-10 — static Go that mounted the
API filesystems, found the root disk and exec'd — and it was a second init, doing what the
init a consumer brings already does. What removed the need for it was turning `systemd-udevd`
back on.

Three things the shell has found, all of them true of the image and none of them visible
from outside:

- **The serial login exists because udev does.** `serial-getty@ttyS0` carries
  `BindsTo=dev-ttyS0.device`, and a `.device` unit exists only if udev announced it. While
  this image masked `systemd-udevd` — on the theory that a VM's hardware is fixed and
  discovering it is boot time spent — there was no login on the serial port at all, a
  drop-in clearing `BindsTo` did not lift it, and the image had to carry an agetty unit of
  its own with the dependency removed. Masking bought nothing measurable and cost a
  ten-second `dev-ttyS0.device` timeout on every boot that had no initrd; the numbers are in
  `optimize-systemd.sh`, where the decision is.
- **`ssh.service` used to fail five times and give up.** The image ships no host keys — they
  are identity — and the distribution's `sshd-keygen.service` carries
  `ConditionFirstBoot=yes`, so it did not run before `sshd` was asked to validate a
  configuration with no keys. Fixed by generating them at boot instead
  (`spin-machine-sshd-keygen.service`): every boot here *is* a first boot, since the root
  filesystem is a fresh overlay, so the keys live exactly as long as the VM does.
- **Two thirds of the boot was systemd asking the console questions.** The kernel execs
  `/sbin/init` at 59 ms and systemd's first log line arrived at 852, with nothing running in
  between. It was a terminfo query and a terminal reset, 334 ms of timeout each, waiting for
  a serial port with a file behind it to answer. `TERM=dumb` on the command line stops it
  asking: 855 ms to 163–207 ms for the whole boot. See `machine/cmdline.go`.

## The parts

### QEMU

One build, **statically linked**, against musl on Alpine.

A dynamically linked QEMU is not one artefact — it is an artefact plus the libraries it was
compiled against. Extracted onto a host with different ones it dies at start-up
(`liburing.so.2: cannot open shared object file`), so a release had to ship either a
container to run it in or the loader and the libraries beside it, and a consumer had to know
which. Static ends the question: the binary runs wherever the kernel does. musl rather than
glibc because glibc links statically but does it badly — anything resolving a user or host
name still wants to `dlopen` an NSS module, and a static binary cannot.

Measured before adopting it, same host and machine line: process start to a QMP `quit`,
**44 ms glibc against 43 ms musl** over five runs. Not measured: a long-running guest under
load, where musl's allocator and its smaller default thread stack differ from glibc's.

Both the KVM binary and a TCG one for runners with no `/dev/kvm`: a host serving tenants
must fail at start-up rather than run a tenant's VM at a tenth of the speed, and the build
asserts both halves, since neither is visible in a file listing.

`--enable-tools` ships `qemu-img`, which is not a convenience: a container's writable layer
is a qcow2 overlay created by running it, so a release with the emulator and without it
cannot give a container anywhere to write. `qemu-nbd` comes out of the same flag and is
deliberately not shipped — it exports a disk over NBD, which nothing here does.

`vmdk` is kept, and nothing in this repository reads one. It is here because the format a
disk is opened as is stated by whoever attaches it, so the set of formats this binary can
open is a promise to whoever runs guests rather than a description of what the machine does.
Dropping it is a one-line change, and it invalidates every template in existence — as any
change to this binary does.

**PVH, not BIOS.** The kernel is an ELF `vmlinux` with Xen PVH notes and QEMU enters it
through `pvh.bin`. There is no bootloader and no UEFI: the same guest under UEFI + Secure
Boot was measured at +356 ms and rejected.

### Guest kernel

A version and a config file. It is here because a kernel and the machine that boots it are
one release: changing either invalidates every template taken against the previous pair.

**Memory that moves.** A VM given a ceiling gets a `virtio-mem` device covering the gap
between its boot size and that ceiling, with nothing plugged. The host grows and shrinks it
over QMP; there are no ACPI DIMM slots, because a DIMM can be added and, in practice, not
removed — unplugging one needs the guest to offline a whole memory block and a single
unmovable page in it makes that fail. Measured on a 1 GiB VM with a 4 GiB ceiling:
0 → 3072 MiB and back to 2 MiB.

The kernel command line carries `memhp_default_state=online` for it. Without that the
growth stops at the boot size and says nothing: memory added and never onlined is memory
the guest cannot use but must still describe, so the driver declines to take more — asked
for 2048 and then 3072 MiB, it plugged 1024 and stayed there.

Separately, every VM gets a `virtio-balloon-pci` with `free-page-reporting=on`, which is
how a guest hands back memory it merely stopped using. The two answer different questions:
the balloon returns what is free, virtio-mem changes how much there is.

**BPF.** The kernel carries what modern BPF development needs, and it did not before:
`BPF_JIT` (every program ran interpreted), `DEBUG_INFO_BTF` (without it there is no
`/sys/kernel/btf/vmlinux`, so no `vmlinux.h`, no `bpftool btf dump` and no CO-RE at all),
`FUNCTION_TRACER` for fentry/fexit attachment, `FTRACE_SYSCALLS`, and the networking program
types — `NET_CLS_BPF`, `NET_ACT_BPF`, `NET_SCH_INGRESS`, `XDP_SOCKETS`, `BPF_STREAM_PARSER`.
Verified in a guest: `/sys/kernel/btf/vmlinux` is 4,552,359 bytes and
`net.core.bpf_jit_enable` is 1.

It costs **+21.4 ms** of kernel boot, measured over five runs each against the same config
without those symbols (137.4 ms to 158.8 ms, kernel start to `Freeing unused kernel image`),
and 5.0 MB of image — 4.55 MB of which is the `.BTF` section, which `strip -s` keeps because
it is allocated and the guest reads it back out of its own image. `ftrace: allocating 41727
entries` is the new work that shows up in the log.

`BPF_LSM` is deliberately not enabled: it needs `CONFIG_SECURITY`, and what it buys is
enforcing access policy inside a VM that holds one workload — a boundary drawn inside the
boundary this machine already is.

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
qemu-img create -f qcow2 -F qcow2 -b rootfs.qcow2 overlay.qcow2
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
mistakes. It also *pulls* one: `task qemu:fetch` unpacks the published QEMU of the pinned
version in seconds, and `task verify:args` hands it the command line `machine.Spec.Args`
builds. The device set is decided in `qemu/devices.mak` and the device and property names
are written out by hand in `machine/machine.go`; nothing else connects the two, and a
device dropped from the allowlist reads exactly like a typo added here — a VM that does not
start, found by whoever boots one. Each artefact has a path-triggered workflow of its own, because QEMU and the
kernel are tens of minutes each and the base image is ~1.5 GB of apt: `qemu.yml`,
`kernel.yml`, `image.yml`. Each ends in the verification that belongs to it, so a build
that lost a device, a kernel that lost its PVH notes, or an image that grew an identity
fails in the workflow that produced it.

`verify:consumer` is the end-to-end check and is opt-in, because this repository ships no
runtime to boot with: pass `CONSUMER_RUNTIME` and `CONSUMER_INITRD` and it unpacks the
tarball and asks three questions of **the image inside it** — does a write reach the disk
and come back, is that write invisible to the next VM, and is the base byte-identical
afterwards. "It printed something" answers none of the three.

`release.yml` builds all three and packs one tarball. Versions are **CalVer**,
`v20260909.01`: a release of this repository is the machine as it stood on a date. There is
no API here to promise compatibility about, and the one thing a version could promise —
that templates still match — is decided by the fingerprint of the artefacts, not by a
number anybody chose. Pushing a `v*` tag releases that version; running the workflow by
hand with no input generates the next sequence for today, tags the commit, and puts the
three checksums in the release notes.

## Licence

Apache-2.0, matching the rest of this stack — and not only for consistency: three scripts
under `image/mkosi.extra/` came from another Apache-2.0 project here, so a different licence
would make this a mixed-licence tree for nothing. `NOTICE` names them and states which was
modified, as section 4(b) requires.

**That licence covers the recipes, not what they build.** A release tarball is almost
entirely other people's software: QEMU and Linux are GPL-2.0, and the base image is an
Ubuntu userland under a dozen licences. Shipping their binaries carries obligations a
LICENSE file in a source tree does not discharge — GPL-2.0 section 3, and section 6 of the
LGPL for the libraries now inside the statically linked QEMU.

So a release answers them:

- `SOURCES` names every upstream source by version, URL and SHA-256, says how it was built,
  and carries the written offer.
- `packages.txt` lists all 209 packages in the base image with exact versions, which is
  what `apt-get source <package>=<version>` needs.
- The two upstream tarballs are pinned by SHA-256 in the Dockerfiles and **verified on every
  build**, not only after a download — the source lives in a cache mount that outlives the
  build that filled it. That is also what makes `SOURCES` true rather than aspirational:
  what it names is what was compiled.
