# spin-machine

[![CI](https://github.com/spin-stack/spin-machine/actions/workflows/ci.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/ci.yml)
[![QEMU](https://github.com/spin-stack/spin-machine/actions/workflows/qemu.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/qemu.yml)
[![Kernel](https://github.com/spin-stack/spin-machine/actions/workflows/kernel.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/kernel.yml)
[![Base image](https://github.com/spin-stack/spin-machine/actions/workflows/image.yml/badge.svg)](https://github.com/spin-stack/spin-machine/actions/workflows/image.yml)

A virtual machine: QEMU, the guest kernel, the base image, and the definition of the
machine they make.

The four are one thing, and the reason is not tidiness. A VM resumed from a checkpoint
loads device and CPU state into a machine that has to be the same shape as the one the
checkpoint was saved on, and nothing checks that at run time. So the machine's identity
is computed from the things that decide its shape — `machine.Spec.Fingerprint` hashes the
QEMU binary, the kernel, the initrd and the firmware the guest runs (the BIOS, qboot.bin and pvh.bin)
*by content*, together with the five arguments that decide what a guest sees. Two machines
with the same fingerprint can take each other's checkpoints. Two with different
fingerprints cannot, and a release in which any of those files moved has a different
fingerprint by construction.

One repository, one version, one machine a checkpoint resumes onto.

**What is deliberately not in a release: software that owns a guest.** This repository
builds a machine, and a release is not bootable on its own by design — whoever runs guests
brings the initrd. Tests and performance probes may use caller-supplied diagnostic initrds
or disposable guest helpers, but those are not release artifacts and must stay isolated from
the published machine.

## What it publishes

One tarball:

| path under `usr/share/spin-stack/` | |
|---|---|
| `bin/qemu-system-x86_64` | static; KVM only — it refuses to emulate, on purpose |
| `bin/qemu-system-x86_64-tcg` | for CI, which has no `/dev/kvm` |
| `bin/qemu-img` | |
| `bin/mkfs.ext4` | static e2fsprogs; read `e2fsprogs/mke2fs.conf` through `MKE2FS_CONFIG` |
| `e2fsprogs/mke2fs.conf` | the defaults an ext4 made for this kernel is made with |
| `qemu/{qboot.bin,bios.bin,bios-256k.bin,pvh.bin,kvmvapic.bin,efi-virtio.rom}` | qboot.bin is the BIOS a machine boots with |
| `qemu/patches/`, `qemu/qboot-*.patch`, `qemu/qboot-COPYING` | the changes this tree makes to QEMU and qboot, shipped with the binaries built from them |
| `kernel/vmlinux` | plus `kernel-config` |
| `image/rootfs.qcow2` | read-only, 0444 |
| `machine.env` | the version, the commit it was built from, and the checksums that decide whether a checkpoint resumes |
| `SOURCES` | every upstream source by version, URL and SHA-256, and the written offer |
| `packages.txt` | every package and exact version in the base image |

`LICENSE` and `NOTICE` sit at the root of the tarball, next to `install.sh`.

`task build` writes that same tree into `_output/`, byte for byte the layout above, and
`machine.OpenRelease` reads either. There is one layout: nothing rearranges the files on the way
out of a build, into a tarball or into a consumer. Let the three differ and what falls out
of the translation between them is a path that exists and holds the previous release's
kernel.

## Using it

```go
rel, err := machine.OpenRelease("/usr/share/spin-stack")  // says which file is missing, if one is
spec := rel.Spec()                                        // QEMU, Kernel, Firmware
// … the caller's initrd, memory, CPUs, disks, monitors
args, err := spec.Args()          // the QEMU command line
fp, err := spec.Fingerprint()     // which checkpoints this machine may resume
```

By hand, `spin-machine` (`task tools`) boots one, prints its command line or its
fingerprint, attaches, detaches or saves on a running one, and restores a saved one; `spin-machine <command> -h`
lists each command's flags.

## Building

```
task build         # everything, into _output/
task shell         # boot the machine and look around inside it
task lint          # gofmt, vet, and whether the scripts and Taskfiles parse
task test          # the machine definition
task verify:args   # and whether the QEMU in _output/ accepts what it produces
task fingerprint   # this machine's identity
task release       # one tarball, one version
```

Each part's targets live beside what they build — `task qemu:build` is next to
`qemu/Dockerfile` — and the root `Taskfile.yml` holds the vars every part reads and the
targets that cross all of them. Every version, digest and commit a build takes from outside is
in `versions.yaml`: `task versions` says what is behind, `task bump NAME=...` moves one.

## Where to read next

| | |
|---|---|
| [docs/machine.md](docs/machine.md) | the definition: fixed slots, vmgenid, and what the fingerprint hashes |
| [docs/migration.md](docs/migration.md) | one VM moved to another host, and why the CPU model decides it |
| [docs/memory-and-disk.md](docs/memory-and-disk.md) | what a guest's page cache, freed memory and disk cost its host: the defaults, what was measured to choose them, and what was turned down |
| [docs/releasing.md](docs/releasing.md) | CI, versions, and what a release owes its upstreams |
| [qemu/](qemu/README.md), [kernel/](kernel/README.md), [image/](image/README.md), [e2fsprogs/](e2fsprogs/README.md) | why each part is built the way it is |
| [boot/](boot/phases.go) | what a boot costs, measured from the host |
| [CLAUDE.md](CLAUDE.md) | how to work in here: the release invariants, and the rules for changing them |

## Licence

Apache-2.0, matching the rest of this stack — and not only for consistency: three scripts
under `image/mkosi.extra/` came from another Apache-2.0 project here, so a different licence
would make this a mixed-licence tree for nothing. `NOTICE` names them and states which was
modified, as section 4(b) requires.

That licence covers the recipes, not what they build: a release is mostly GPL and LGPL
binaries, and [docs/releasing.md](docs/releasing.md#what-a-release-owes) says how a release
answers for them.
