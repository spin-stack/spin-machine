# Base image

A **qcow2 base, read-only, with a fresh qcow2 overlay per VM**:

```
qemu-img create -f qcow2 -F qcow2 -b rootfs.qcow2 overlay.qcow2
```

Copy-on-write happens in the block layer, where QEMU does it, rather than in a filesystem
stacked inside the guest.

The contents are an Ubuntu 26.04 userland with systemd as the container's first process,
the tools a workspace expects, and the boot optimizations in
`mkosi.extra/usr/local/lib/spin-base/optimize-systemd.sh`.

**ext4**, because the guest kernel has `EXT4_FS` and `OVERLAY_FS` and explicitly not
`XFS_FS`, `BTRFS_FS`, `SQUASHFS` or `EROFS_FS`. Anything else starts with a kernel config
change one directory over — which `kernel/Dockerfile` asserts.

**Partitionless**, not a bootable disk with an ESP and a GPT, which nothing here would read.
mkosi produces the tree (`Format=directory`); `build.sh` runs `mkfs.ext4 -d` — no loop
device, no mount — and wraps the result with `qemu-img convert`. The `mkfs.ext4` is the
pinned one from [`e2fsprogs/`](../e2fsprogs/README.md), not the build container's. The base
image reserves no blocks for root (`-m 0`): the reservation is a proportion, so a filesystem
grown from the image onto a larger disk would keep 5% of the larger size.

## The two traps

- **The base must not be written to.** Every VM maps it read-only through a backing file and
  many VMs share one. A build step that opens it read-write invalidates every overlay in
  existence — silently, because the overlays keep working until they read a cluster that
  moved. It is written `0444`, and `task shell` checks its checksum across a boot.
- **No identity in the image.** No hostname, no machine-id, no `/etc/resolv.conf`, no SSH
  host keys, no random seed. A VM is handed its identity by whoever runs it; anything
  baked in is shared by every VM that ever boots from it.
  `build.sh` asserts each of these on the finished filesystem — `/etc/machine-id` present
  and *empty*, everything else absent.

## Logging in: the SSH client has to speak ML-KEM

`sshd` offers one key exchange, `mlkem768x25519-sha256`, and refuses a client that has no
ML-KEM rather than falling back to a weaker one. The client sees:

```
Unable to negotiate with … port 22: no matching key exchange method found. Their offer: mlkem768x25519-sha256
```

A client that connects is one of:

- **OpenSSH 9.9 or later**, which `ssh -V` names. Earlier ones are refused, among them the 9.6
  Ubuntu 24.04 LTS ships (the bench fleet's runners have it, 2026-11-13): an operating system
  whose OpenSSH is older needs a newer one installed.
- **`golang.org/x/crypto/ssh` v0.38.0 or later, built with Go 1.24 or later.** It offers
  ML-KEM first by default, so a client that sets no `KeyExchanges` already connects.
- **Not** the `ssh2` npm package: no release up to 1.17.0 has ML-KEM.

Why one exchange: ML-KEM is post-quantum, and on the bench fleet a login with it cost what one
with curve25519 did (40 and 39 ms; `task boot:ssh-first`, lab run 43479854381, 2026-11-13).
sntrup761x25519, the other post-quantum exchange and the one OpenSSH 8.5-9.8 picks, took
104 ms - its key generation alone was ~39 ms of the client's. The classical exchanges are
gone with it. The drop-in is `/etc/ssh/sshd_config.d/10-spin-kex.conf`, written by
`configure-system.sh` and asserted by `build.sh`; a drop-in that sets `KexAlgorithms` and is
read before it wins, because sshd keeps the first value it reads.

## Looking inside

```
task shell                    # systemd; the console autologins as root
task shell INIT=/bin/bash     # a bare shell, for when systemd is the broken thing
```

The default is `/sbin/init` because that is what this image is: a userland whose first
process is systemd. Booting a bare shell answers a different question — `systemd-analyze`
in it replies *"System has not been booted with systemd as init system (PID 1)"*, which is
true and useless.

It is `spin-machine boot` with no `--disk`: this QEMU and this kernel over `rootfs.qcow2`
under a throwaway overlay (QEMU's `-snapshot`), with the serial console on stdio.

It boots with no initrd at all: `root=/dev/vda rw init=/sbin/init`, which the kernel can
serve because virtio-blk and ext4 are built in and `build.sh` writes a partitionless
filesystem. There is no debug initramfs: one that mounted the API filesystems, found the
root disk and exec'd would be a second init, doing what the init a consumer brings already
does. What removes the need for one is `systemd-udevd` being on.

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
- **`sshd` needs host keys generated at boot.** The image ships none — they are identity —
  and the distribution's `sshd-keygen.service` carries `ConditionFirstBoot=yes`, so it does
  not run before `sshd` is asked to validate a configuration with no keys, and
  `ssh.service` fails five times and gives up. `spin-machine-sshd-keygen.service` generates
  them instead: every boot here *is* a first boot, since the root filesystem is a fresh
  overlay, so the keys live exactly as long as the VM does. Every sshd waits for it,
  including the per-connection `sshd@.service` that `systemd-ssh-generator` starts for vsock
  and `ssh .host`: without that, those connections found no host keys and were dropped.
- **Two thirds of the boot was systemd asking the console questions.** The kernel execs
  `/sbin/init` at 59 ms and systemd's first log line arrived at 852, with nothing running in
  between. It was a terminfo query and a terminal reset, 334 ms of timeout each, waiting for
  a serial port with a file behind it to answer. `TERM=dumb` on the command line stops it
  asking: 855 ms to 163–207 ms for the whole boot. See `machine/cmdline.go`.

## Why mkosi does not run under BuildKit

mkosi assembles a root filesystem by unsharing a user and mount namespace, and BuildKit
forbids it (`mkosi was forbidden to unshare namespaces`, `PermissionError` on `unshare(2)`).
The alternatives were `RUN --security=insecure`, which needs a `buildkitd` started with an
entitlement flag — so `task image:build` would fail on any machine whose builder was created
the ordinary way — or a container run with the privileges mkosi needs. `Dockerfile` pins the
toolchain, `build.sh` is the build, and the task runs it with `--cap-add SYS_ADMIN` and
seccomp/apparmor unconfined. Not `--privileged`.
