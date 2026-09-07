# spin-machine

QEMU, the guest kernel and the base image: what a VM is made of before any workload.

These three were duplicated, mirrored or missing across `spinbox` and `storage`, and they
are one thing. spinbox decides whether a VM may be restored from a template by hashing the
QEMU binary, the kernel and the machine arguments **by content**
(`internal/host/vm/qemu/template.go`); a release in which any of them moved is a different
machine. One repository, one version, one generation of templates.

```
qemu/       Dockerfile + devices.mak — one build, the union of spinbox's and storage's flags
kernel/     Dockerfile + config-<version>-<arch> — moved out of spinbox
image/      mkosi configuration producing base.qcow2 (ext4 inside)
hack/       release
```

```
task build            # everything, into _output/
task verify:image     # boot base.qcow2 and run a command in it
task release          # one tarball, one version
```

## What it publishes

One tarball, laid out the way a spinbox release is, so a host installs it the same way:

| path under `usr/share/spin-stack/` | |
|---|---|
| `bin/qemu-system-x86_64` | KVM only — it refuses to emulate, on purpose |
| `bin/qemu-system-x86_64-tcg` | for CI, which has no `/dev/kvm` |
| `bin/qemu-img`, `bin/qemu-nbd` | |
| `qemu/{bios.bin,bios-256k.bin,pvh.bin,kvmvapic.bin,efi-virtio.rom}` | |
| `kernel/vmlinux` | plus a `spinbox-kernel-x86_64` symlink |
| `image/base.qcow2` | read-only, 0444 |
| `machine.env` | the version and the three checksums that decide template validity |

## The three parts

### QEMU

`spinbox/Dockerfile.qemu` and `storage/Dockerfile.qemu` both pinned 11.1.1, both built
`--target-list=x86_64-softmmu`, and both used a device allowlist that was byte-for-byte the
same list. `qemu/Dockerfile` is the union of the two flag sets. Where they disagreed:

- **`--enable-tools`.** storage's Agent shells out to `qemu-img` for every qcow2 chain;
  spinbox's build shipped none, so `spinbox run` had to be given a `--qemu-img` flag
  pointing at *storage's* build to create a container's writable layer at all. Both flags
  can go once both consume this release.
- **TCG.** Built twice. `/opt/qemu` cannot emulate — a host serving tenants must fail at
  start-up without `/dev/kvm` rather than run a tenant's VM at a tenth of the speed — and
  `/opt/qemu-tcg` can, because CI's runners have no `/dev/kvm`. The build asserts both
  halves; neither is visible in a file listing.
- **`vmdk`.** Still enabled, and it is the one flag held open for a consumer: containerd's
  erofs-snapshotter hands spinbox a `merged.vmdk`, and `--disable-vmdk` turns every VM into
  `Unknown driver 'vmdk'` at start-up. It becomes safe once nothing reads that file.

Two flags the original briefing listed as storage's and that storage does not pass —
`--enable-vhost-user-blk-server` and `--without-default-features` — are not enabled here;
storage's own comments reject both. Checked against the file on 2026-09-07.

**PVH, not BIOS.** The guest kernel is an ELF `vmlinux` with Xen PVH notes and QEMU enters
it through `pvh.bin`. There is no bootloader and no UEFI: booting the same guest under
UEFI + Secure Boot was measured at +356 ms and rejected.

**Migration is load-bearing.** spinbox restores every VM from a template with
`migrate`/`migrate-incoming`, `x-ignore-shared` and a `memory-backend-file`. A new QEMU
release invalidates every template on every host, by design — a template restored into a
machine of another shape is undefined.

### Guest kernel

Moved from spinbox's `Dockerfile` unchanged in what it produces. It is a version and a
config file; it never depended on spinbox's source.

storage's `Dockerfile.guest-kernel` builds nothing — it exists to move spinbox's kernel
through a registry and back out so the lane can name it by digest instead of by a relative
path into someone's home directory (ADR-0021, ADR-0022). That whole mechanism is plumbing
across a repository boundary that should not be there, and it disappears.

**The initrd stays in spinbox.** It carries `vminitd`, built from spinbox's source, and it
is the one guest artefact that belongs to the runtime rather than to the machine.

**What it costs.** The kernel config is not stable furniture: in one week it gained
`CONFIG_VMGENID` and `CONFIG_PTP_1588_CLOCK_KVM`, lost `CONFIG_CRYPTO_JITTERENTROPY`
(−3.7 ms of boot) and had its symbol table stripped (−11.8 ms), each driven by a
measurement taken in spinbox. Moving the kernel puts a repository hop in that loop. That
friction is arguably the right kind — these are exactly the changes that invalidate every
template in the fleet, and they should not be quiet — but it is real.

`kernel/Dockerfile` builds on `debian:bookworm` deliberately: spinbox built this kernel with
bookworm's gcc, and a different compiler is a different `vmlinux`, which is every template
in the fleet invalidated. Worth doing on purpose, not while moving a file.

### Base image

A **qcow2 base, read-only, with a fresh qcow2 overlay per VM**:

```
qemu-img create -f qcow2 -F qcow2 -b base.qcow2 overlay.qcow2
```

Copy-on-write moves from the filesystem (overlayfs in the guest, over an erofs-in-VMDK and
a raw ext4 produced by containerd's erofs-snapshotter) to the block layer, where QEMU does
it. It is also the shape storage's chain is made of: base plus overlays.

The contents are spinbox's `images/sandbox`, ported to mkosi: Ubuntu 26.04, systemd as the
container's PID 1, the development tools a workspace expects, and the boot optimizations
that were each measured — `image/mkosi.extra/usr/local/lib/spin-base/optimize-systemd.sh`
names the milliseconds every mask saved, and is an unmodified copy for that reason. What
changed is the container it arrives in: a filesystem image rather than a stack of tar
layers.

systemd is in here and the machine still has no init of its own. The guest kernel boots an
initrd spinbox builds, whose `/init` is vminitd; the container's filesystem is mounted under
that at a bundle path and entered with `chroot`. The systemd here is the container's first
process — what `ENTRYPOINT ["/sbin/init"]` meant. Nothing is designed around crun's bundle
semantics either: spinbox is growing a path that chroots and execs with no OCI runtime at
all (`internal/guest/vminit/process/direct.go`), and this image does not care which runs it.

**ext4**, because the guest kernel has `EXT4_FS`, `EROFS_FS` and `OVERLAY_FS` and
explicitly not `XFS_FS`, `BTRFS_FS` or `SQUASHFS`. Anything else starts with a kernel config
change one directory over — which `kernel/Dockerfile` now asserts.

**Partitionless**, not a bootable disk with an ESP and a GPT, which nothing here would read.
mkosi produces the tree (`Format=directory`); `image/build.sh` runs `mkfs.ext4 -d` — no loop
device, no mount — and wraps the result with `qemu-img convert`.

#### The two traps

- **The base must not be written to.** Every VM maps it read-only through a backing file and
  many VMs share one. A build step that opens it read-write invalidates every overlay in
  existence — silently, because the overlays keep working until they read a cluster that
  moved. It is written `0444` and `task verify:image` checks its checksum across a boot.
- **No identity in the image.** No hostname, no machine-id, no `/etc/resolv.conf`, no SSH
  host keys, no random seed. spinbox restores VMs from a frozen template and hands them
  their identity afterwards over RPC; anything baked in is shared by every VM that ever
  boots from it. The guest already refuses to bind-mount a `/etc/resolv.conf` that is not
  there, deliberately. `image/build.sh` asserts each of these on the finished filesystem —
  `/etc/machine-id` present and *empty*, everything else absent.

## Why mkosi does not run under BuildKit

mkosi assembles a root filesystem by unsharing a user and mount namespace, and BuildKit
forbids it (`mkosi was forbidden to unshare namespaces`, `PermissionError` on `unshare(2)`).
The alternatives were `RUN --security=insecure`, which needs a `buildkitd` started with an
entitlement flag — so `task build:image` would fail on any machine whose builder was created
the ordinary way — or a container run with the privileges mkosi needs. `image/Dockerfile`
pins the toolchain, `image/build.sh` is the build, and `task build:image` runs it with
`--cap-add SYS_ADMIN` and seccomp/apparmor unconfined. Not `--privileged`.

## Status

Built and verified end to end on 2026-09-07, on this tree:

```
task build && task verify:machine
```

- **QEMU 11.1.1** builds, both binaries. The build's own assertions pass: virtio-blk,
  virtio-net, vhost-vsock and virtconsole present, no e1000/rtl8139/vmxnet3, q35 and no
  pc-i440fx, `qemu-img` and `qemu-nbd` present, and the accelerator split — the production
  binary refuses to emulate, the CI one can.
- **The kernel** builds. Its config comes out byte-identical to spinbox's after
  `olddefconfig`, the PVH notes survive the strip, and the stripped `vmlinux` is the same
  size as spinbox's (34,298,960 bytes). It is not byte-identical — a kernel never is
  across builds — so it is a new machine fingerprint, which is what moving it costs.
- **base.qcow2** builds (863 MB) and boots.
- **The whole release boots as one machine**: this QEMU, this kernel, this base image, plus
  spinbox's initrd, driven through `spinbox run` with `SPINBOX_CONFIG` pointed at the
  unpacked tarball. 151 ms to a running guest. `--qemu-img` was not passed and did not need
  to be — it was found beside the QEMU binary, which is the flag this repository set out to
  remove.
- A write inside the VM lands in the overlay, is not visible to a VM booted from the base
  alone, and the base comes back byte-identical.

Three things the ports got wrong and that are fixed in the tree, each found by running it:

- `check-docker-config.sh` needs `apparmor_parser` and `sysctl`. spinbox's kernel stages had
  both by accident, from the golang base image they descended from.
- `qemu-img --help` wraps its format list across lines, so a `grep 'Supported formats:.*vmdk'`
  matched nothing and failed a build whose every other assertion had passed.
- `debugfs -R` exits 0 whether or not the file it was asked about exists. Every filesystem
  check in `image/build.sh` reads its output instead.
