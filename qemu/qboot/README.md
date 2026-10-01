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

## PAM in three writes

qboot makes 0xc0000-0x100000 ram with seven one-byte configuration writes, one per PAM
register, and QEMU's q35 host bridge rebuilds the guest's memory map for each write that
touches one (`mch_update_pam`) - every address space's flat view and KVM's memory slots.
`pam.patch` sets q35's registers, which start 4-aligned at 0x90, with a long, a word and a
byte. The values are immediates: once PAM0 is ram, 0xf0000-0x100000 reads zeroes until
`setup_hw` has shadowed the BIOS, so a table in `.rodata` would be read back as zeroes.
i440fx keeps the loop; this machine is q35 and nothing here boots the other.

## MTRRs

Stock qboot never touches the MTRRs, so the guest boots with them disabled ("MTRRs disabled by
BIOS"). Linux then maps every range it is asked to map write-back but does not know as RAM
uncached-minus: a virtio-pmem region read at 8.3 MB/s, against 1.1 GB/s under SeaBIOS, which
programs them (lab runs 36818224412 and 36818721809, 2026-10-01). `mtrr.patch` does what SeaBIOS
does, on the boot CPU only - Linux gives its own MTRR state to the CPUs it starts: enabled,
write-back by default, and the 32-bit PCI hole, from the top of low RAM to 4 GiB, uncacheable in
naturally aligned power-of-two ranges. Fixed-range MTRRs stay off. If the hole does not fit the
variable ranges the CPU has, they are left disabled rather than caching MMIO.

## Build

`task qemu:build` builds it, in the `qboot` stage of `qemu/Dockerfile`, and it lands beside
the SeaBIOS blobs as `_output/qemu/qboot.bin`, where a release, `task qemu:fetch` and CI all
find it. The stage clones the pinned commit, verifies the SHA-256 of the tar `git archive`
writes for it, applies `write-pointer.patch`, `pam.patch` and `mtrr.patch` with no fuzz, and compiles with upstream's
meson.build flags plus `-Os`. It asserts on what came out: 65536 bytes, the ROM window it is
linked for, and the patch's code in the tree that was compiled.

It is built with the kernel's pinned Debian and not QEMU's Alpine, because the compiler is
part of the firmware - see the toolchain note below.

## Probe

`task boot:firmware` (`TestFirmwareRuns` in `boot/firmware_test.go`) boots qboot
with vmgenid off, then with it on: the table published, the guest saved, restored into a new
QEMU with `guid=auto`, and `crng reseeded due to virtual machine fork` required in the
restored guest's log. Then it boots SeaBIOS, the fallback. `SPIN_QBOOT=` names another qboot
to try. `task boot:firmware-stages` says where qboot's time goes.

The initrd is `boot/probeinit`, built by the test, or `SPIN_PROBE_INITRD=`. Its `/init` must
mount proc, print dmesg lines containing `vmgenid` and then `SPIN-READY`, stay alive, and keep
printing new dmesg output so the host sees the reseed after restore.

Measured on the lab runner (AMD Ryzen 9 5900X, run 36650049960, 2026-09-29), 20 boots
interleaved: SeaBIOS 121.89 ms p50, qboot 114.76, 7.14 ms saved; and `report`'s whole matrix,
106 rows, boots on qboot (run 36650559189).

## The toolchain is part of the number

The same commit, patch and flags saved 6.3 ms over SeaBIOS built by a host's GCC 15.2.0 and
3.9 ms built by Debian trixie's GCC 14.2.0 (i9-13900HK, a diagnostic initrd, 2026-09-21 and
-26): the compiler moved the result by more than a third of what qboot saves. That is why the
recipe is pinned rather than convenient, and why a number here compares only with one built
the same way. Against this machine's real boot, the firmware is about 10 ms of a few hundred.

Firmware contents are part of Spec.Fingerprint — the BIOS and pvh.bin, by content — so
two firmware builds are two machines, and a checkpoint saved under one does not resume
under the other.
