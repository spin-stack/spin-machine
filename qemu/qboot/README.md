# qboot, with WRITE_POINTER

qboot at `8ca302e86d685fa05b16e2b208888243da319941` stops in its ACPI loader when
q35 supplies the WRITE_POINTER command needed by vmgenid. The patch adds that
command and fw_cfg DMA writes. The pointer payload is little-endian; the DMA
descriptor is big-endian. A destination offset is applied with SELECT+SKIP before
WRITE. Invalid pointer widths, missing/unallocated sources, out-of-bounds writes,
missing DMA and DMA errors fail rather than silently losing the vmgenid address.
The destination is a writable fw_cfg file, not a guest allocation.

Protocol references:
[QEMU fw_cfg](https://www.qemu.org/docs/master/specs/fw_cfg.html) and
[the ACPI linker loader](https://github.com/qemu/qemu/blob/master/hw/acpi/bios-linker-loader.c).
The patch is GPL-2.0, like upstream; see COPYING and the repository NOTICE.

## Build

`task qemu:build` builds it, in the `qboot` stage of `qemu/Dockerfile`, and it lands beside
the SeaBIOS blobs as `_output/qemu/qboot.bin`, where a release, `task qemu:fetch` and CI all
find it. The stage clones the pinned commit, verifies the SHA-256 of the tar `git archive`
writes for it, applies `write-pointer.patch` with no fuzz, and compiles with upstream's
meson.build flags plus `-Os`. It asserts on what came out: 65536 bytes, the ROM window it is
linked for, and the patch's code in the tree that was compiled.

It is built with the kernel's pinned Debian and not QEMU's Alpine, because the compiler is
part of the firmware - see the toolchain note below.

## Probe

`task boot:firmware` (`TestFirmwareCost` in `boot/firmware_test.go`) boots qboot with vmgenid
off, then with it on: the table published, the guest saved, restored into a new QEMU with
`guid=auto`, and `crng reseeded due to virtual machine fork` required in the restored guest's
log. Then it interleaves SeaBIOS and qboot boots and prints p50, p95 and the difference.
`SPIN_QBOOT=` names another qboot to try.

The initrd is `boot/probeinit`, built by the test, or `SPIN_PROBE_INITRD=`. Its `/init` must
mount proc, print dmesg lines containing `vmgenid` and then `SPIN-READY`, stay alive, and keep
printing new dmesg output so the host sees the reseed after restore. The same initrd serves
both firmwares, so its own work cancels out.

Measured on the lab runner (AMD Ryzen 9 5900X, run 36650049960, 2026-09-29), 20 boots
interleaved: SeaBIOS 121.89 ms p50, qboot 114.76, 7.14 ms saved; and `report`'s whole matrix,
106 rows, boots on qboot (run 36650559189).

## Measurements, 2026-09-21

QEMU 11.1.1, KVM, i9-13900HK, q35 with SATA and SMBus disabled, 2 vCPUs,
2 GiB, CPU host, no disks or NICs, vmgenid present. The diagnostic initrd used a
static BusyBox and printed the marker after mounting proc/sysfs and filtering
dmesg. No cache dropping or warmup phase; 20 boots per firmware per run for the
first two rows and 10 for the third, interleaved, host wall clock before spawning
QEMU to receipt of SPIN-READY. These are diagnostic init timings, not PVH-entry,
systemd readiness, SSH, or full machine topology measurements.

| Run | Firmware built by | SeaBIOS p50 / p95 | Patched qboot p50 / p95 | p50 reduction |
|---|---|---|---|---|
| Initial | host GCC 15.2.0 | 101.32 / 112.25 ms | 96.50 / 105.25 ms | 4.83 ms |
| Rebuilt using checked-in recipe and probe | host GCC 15.2.0 | 100.89 / 107.40 ms | 94.57 / 101.68 ms | 6.32 ms |
| Containerised recipe, 10 boots, 2026-09-26 | Debian 14.2.0 in `Dockerfile` | 98.43 / 100.41 ms | 94.57 / 97.62 ms | 3.86 ms |

All three reproduced the original stall and observed reseeding after restoration. Raw
samples and artifact hashes for the first two are in `measurements.json`.

## The toolchain is part of the number

The first two rows were built by whatever GCC the host had — 15.2.0 — and produced
`7a316e3c…` for the patched binary. `Dockerfile` pins Debian trixie, whose GCC is
14.2.0, and produces `bf7ddddc…`. Same commit, same patch, same flags, different
compiler, and the saving moved from 6.32 ms to 3.86 ms: **the compiler accounts for
more of the difference than a third of what qboot itself saves.**

This is the same result the SeaBIOS experiment recorded in `boot/phases.go` — two
builds of one firmware differing by as much as the change being measured — and it is
why the recipe is pinned rather than convenient. It also means a number in this file
is only comparable to another number built the same way, and the third row is the only
one that can be reproduced from the repository as it stands.

The 3.9–6.3 ms observed reduction is useful but is not evidence of a larger saving in a
complete userspace boot: measured against this machine's real boot, the firmware is
about 10 ms of a few hundred.

Before selecting qboot for a release, verify the full device topology, CPU/memory
hotplug, reset, and systemd/SSH readiness. Firmware contents are currently absent
from Spec.Fingerprint: supporting a selectable firmware requires including them
in machine identity, otherwise two different firmware builds could match templates.
Keep the existing SeaBIOS release unchanged until those parts are handled.
