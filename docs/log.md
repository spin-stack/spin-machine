# Log: what was done, why, and what it measured

Built from `git log origin/main` (327 commits, to `3e8075f`), all 150 pull requests (merged, closed and open), the 44 releases' notes, the tree at `origin/main` (comments, `docs/`, `boot/*_test.go` row comments), and spin-stack/spin#1206 and spin-stack/open-remote-ssh#8. Hashes are short commit ids on `origin/main`; `#N` is a pull request here; "lab N" is a `lab.yml` run id as the tree or a PR quotes it. Nothing here was re-measured. Written 2026-11-13; add an entry with every change that decides something.

---

## 1. Where we stand (2026-11-13)

**The machine.** Latest release `v20261113.02` (source `43de41e`): QEMU 11.1.3 with 2 local patches, kernel 7.3.4 with 5 local patches, qboot (patched) as the BIOS, e2fsprogs 1.47.4, an Ubuntu 26.04 base image built with mkosi. `fingerprint_sha256=2bd7da6b…`, unchanged since `v20261013.02`: checkpoints have resumed across every release since then (release notes). Merged to main after that release and not yet released: #245 (sshd accepts only `mlkem768x25519-sha256`) and #248 (SSH host keys made from the boot's first dispatch).

**Current numbers.** All from the bench fleet (spin-stack/ci, AWS c8i.4xlarge, Xeon 6975P-C, KVM nested), 2 vCPU / 2 GiB, boots interleaved, p50/p95 in ms from QEMU's exec:

| Metric | Value | Source |
|---|---|---|
| Cold boot to the first SSH login over vsock, v20261113.02 as built | 489/506 | lab 43483372488 (#248, `boot/ssh_first_test.go`) |
| The same, with the host keys made early (main, unreleased) | 468/494 | lab 43483372488 |
| sshd accepts (cold boot) | 309/334 | lab 43483372488 |
| From accept to login | 177/194, then 154/168 with keys early | lab 43483372488 |
| A second SSH login, x/crypto ML-KEM | 40 (curve25519 39; OpenSSH 9.6 sntrup761 104) | lab 43479854381 (#245) |
| Restore a checkpoint (stream format, state evicted from page cache) to the first login | 386/399; VM running at 277/290 | lab 43483852339 (#246, closed, not in tree) |
| Save a checkpoint (stream) | 1017/1861 | lab 43483852339 (#246) |
| Serial console login prompt (`usable`) | 346/358 | lab 43362067503 (#241, `type.conf`) |
| Release report `baseline` row, v20261113.02 | 353.4/366.9 | release notes v20261113.02 |
| Kernel start to "Freeing unused kernel image" | 89.1 (v20261113.01) / 88.9 (patch 0005 measurement) | lab 43361415117 (#241); lab 43327612594 (`kernel/Dockerfile:293-295`) |
| PID 1 idle gap between unit load and first job | ~32 ms p50 | lab 43361486792 (`boot/waits_probe_test.go:391`) |

Older numbers, taken elsewhere, that no bench-fleet number has replaced: QEMU exec to the guest's first instruction is ~27 ms (laptop, `c049a21`, 2026-09-10). Kernel to `Run /sbin/init` was 59-65 ms on the laptop (`b5f361f`, `c03ed89`). Neither compares with a fleet number (§5).

**What the repository says the goal is.** No single target number is written anywhere. What is written:

- CLAUDE.md, rule 1: "A checkpoint resumes on any release with its fingerprint, and the fingerprint is the machine's contract." Rule 2: "The base image is never written to." Rule 3: "No identity in the base image."
- README: "One repository, one version per release, and checkpoints that resume across releases until the machine itself changes."
- `optimize-systemd.sh` (main): the first SSH login "is what a workspace waits for". #250: keep "the metric a workspace waits for (ssh-first)".
- #242 (closed): "Restoring a checkpoint is the path a workspace takes, since it is handed its identity after it is restored."
- The goal has moved. On 2026-09-10 `c2c4c6a` said cold-boot time did not matter: a workspace was restored from a template, so "chasing it would be optimising the one path the product is designed never to take". Templates were removed on 2026-09-30 (#51, `a9d6083`), and after that the cold boot became the main target. Restore was not measured on the bench fleet until 2026-11-13 (#242, then #246).

---

## 2. Chronological log

Format: **date · area · refs**, then what was done, why, the result and its status.

### Week 1: 2026-09-07 to 09-13

**09-07 · all · `2afac47`, `219b98a`.** The repository is created from spinbox's and storage's QEMU, kernel and image (image ported to mkosi, ext4 in a read-only qcow2). qemu-nbd dropped, Taskfile split per part, `task shell` added. *Why:* they were one machine, duplicated across repositories. *Result:* "boots as one machine … in 151 ms" (`2afac47`, consumer's initrd). *Status:* in place.

**09-07 · machine/fingerprint · `5e7d60e`, `b476f6d`, `57e827a`, `b882171`, `17d844f`, `02709d7`.** `machine/` defines the slot map, the shape and the cmdline. `Fingerprint` hashes QEMU, the kernel and the initrd by content, plus the shape. Same day:
- the device topology goes into the hash (`b476f6d`);
- the host CPU goes back in under `-cpu host` (`57e827a`);
- the topology is corrected to leave out disks and NICs (`b882171`: templates have neither);
- the encoding is made length-prefixed (`17d844f`);
- an identity split is added for memoising (`02709d7`).

*Status:* superseded. See the fingerprint cycle in §3.

**09-07 · machine · `656d55b` → `b882171`.** `spin-machine save` and `boot --incoming file:` are added, then reverted the same day as "lifecycle" that belongs to the consumer. Kept: `Spec.CPU` with `enforce=on`. Measured: a state saved with Broadwell-v4 resumes on Broadwell-v4 and Skylake-Client-v4, and Nehalem refuses (`656d55b`, now in `docs/migration.md`). *Status:* save/restore came back as checkpoint commands (`c202568`, 09-28; kept after #51).

**09-07 · tooling · `5e7d60e`, `8aee03a`, `24c8a06`.** `cmd/spin-machine-init` is a debug initramfs, and `spin-machine-console.service` is shipped but not enabled. *Why:* systemd gave no serial login, because the image masked udev, so `dev-ttyS0.device` never appeared. *Status:* the init was deleted `8eeb01c` (09-10) and the console unit deleted `3a7864d` (11-12). See §3.

**09-07 · qemu · `31a57e7`.** QEMU is linked statically against musl. *Measured:* process start to QMP quit, 44 ms glibc vs 43 ms musl over 5 runs. Not measured: a long-running guest under load (musl allocator, thread stack). *Status:* in place; the load question is still open.

**09-07 · image/kernel · `2df3dbf`.** Changes:
- root password removed;
- SSH host keys generated per boot (unit hung off ssh.service);
- chrony via `/dev/ptp0`;
- BPF_JIT and BTF on, at +21.4 ms of kernel boot and 5.0 MB (5 runs each way; also `kernel/README.md`);
- `-no-reboot`.

BPF_LSM was left off. *Status:* BPF on and in place; chrony removed 09-10; BPF_LSM turned on 10-01 (`1400f6a`); the keys moved 3 more times (§3).

**09-07 · qemu/machine · `8b7391a`.** aio=io_uring, discard=unmap, vhost=on requested, qom_cast_debug off. *Rejected:*
- `cache=none`: the shared base must stay in the host page cache, and O_DIRECT fails on tmpfs;
- `--enable-virtfs`;
- four configure flags that do not exist in 11.1.1.

*Status:* in place.

**09-07 · machine/kernel · `b476f6d`, `ee34d31`.** virtio-balloon with free-page-reporting. *Measured:* "costs nothing measurable", 158.9-162.5 ms with vs 157.9-171.6 without, noise ~14 ms. virtio-mem replaces DIMM slots, with `memhp_default_state=online` required: without it a request for 2048 then 3072 MiB plugged 1024 and stopped. Measured 0→2048→3072→512→2 MiB. *Status:* in place.

**09-07 · machine · `58d404e`.** `tsc=reliable` restored; it had been lost in the move from spinbox. Not measured. In place.

**09-08 · machine · `fa1803f`.** ICH9 `sata=off`, `smbus=off`. *Measured:* 400 boots per variant, kernel to init: 62.3 baseline, 61.3 sata=off, 61.7 smbus=off, 60.6 both; an earlier 150-boot series agrees. *Measured and rejected:* `usb=off` 63.0 vs 62.9; `vmport=off` 62.6; `smm=off` 62.3; `i8042=off` 62.5. *Status:* in place.

**09-08 · CI · `fe332b4` (#4).** `task verify:args` starts 9 Specs under the TCG binary with `-S` and requires a QMP answer. *Why:* nothing checked that QEMU accepts the command line. In place (it became `TestQEMUAcceptsEveryArgument`).

**09-09 · kernel cmdline · `9ca453a`.** `nohz=off` removed. *Measured:* a forced tick is 1.7% of a core per idle vCPU. Boot 821-853 ms vs 843-846; restore 113-221 vs 124-216 (template era). In place.

**09-09 · machine · `76aacf9`, `7cafdf6`, `6599829`, `2b46d78`, `354c8a6`.** Changes:
- PCIe root ports for hotplug: no measured change to boot or restore;
- a NIC through a root port via `netdev_add fd=`;
- NIC count and `host_mtu` go into the fingerprint, while MAC and fd stay out;
- a second QMP monitor.

*Status:* root ports were replaced by virtio-scsi on 09-29 (`2b19096`); NIC hotplug was dropped then; MTU and NIC in the fingerprint were superseded by `Spec.Contract` (`fa3041c`).

**09-10 · boot probes/image · `4cbd229`, `bab6f16`, `c2c4c6a`, `722d468`, `b5f361f`.** `task boot:matrix` (shell). *Found:* masking udev buys nothing (826 vs 829 ms), and masking it costs a 10 s `dev-ttyS0.device` timeout with no login. udev was unmasked (`722d468`) and `40-spin-hotadd.rules` added for CPU and memory. Two stalls of 334 ms waiting on the console (XTGETTCAP and a terminal reset) are fixed by `TERM=dumb` on the kernel cmdline: 687→19 ms, and the whole boot 855→163-207 ms (`b5f361f`). The harness itself was contaminated: `unmask_udev` had become a no-op, so the first udev rows (826/829) were wrong (`b5f361f`). *Status:* udev on and TERM=dumb in place. The memory half of the hot-add rule was removed 11-12 (`3c8da99`).

**09-10 · tooling · `8eeb01c`.** Debug initramfs deleted. "this repository does not ship an init". udev on → serial getty works: 204 ms to a login prompt. In place.

**09-10 · boot probes · `80f984b`.** The boot bench is ported to Go and the shell harness deleted. *Found:*
- `systemd.mask=` on the cmdline does nothing (debug-generator masked), so every earlier row that used it measured nothing;
- agetty has an unconditional `sleep(1)`;
- the shell harness throttled the console (it read 1424 ms where the guest said 190);
- a Percentile bug.

*Measured (15 boots, laptop):* as shipped 1256/1276; without agetty 253/320; 512 MiB 1241/1261; 1 vCPU 1309/1326; udev masked: 15 of 15 boots never reached a login. Superseded by `boot:bench`; `boot:bench` was deleted in #250.

**09-10 · kernel · `c03ed89`, `1a8e25c`, `a31d7b2`, `f32d527`, `64d2901`, `e285dec`.** A kernel-phase investigation. Results, each recorded in `kernel/Dockerfile`:
- initcall_debug with a console attached measures the console (103.9 vs 42.8 ms);
- the 8250 trim is worth 154 µs;
- a modular kernel buys 0.57 ms and was rejected;
- acpi_bus_scan scales with the vCPU ceiling: maxcpus=16 +5 ms and maxcpus=64 +9 ms of kernel (63.0/68.1/72.1 ms), recorded at `Spec.MaxCPUs`;
- four ACPI cmdline parameters are worth 0 (69.9 vs 68.6);
- ksm_init and kcompactd_init are worth 0;
- `event_trace_enable_again`'s 6 ms is the profiler;
- inet_init's 9 ms is BTF's first parse, not XFRM.

*Status:* rejected (§4).

**09-10 · image · `6aba37b`, `064c4f4`, `c7bab94`, `c7af4d3`, `08c56e1`.** Changes and measurements:
- logind 63 ms on the chain, worth 24 ms; chrony worth 10 (15 boots, laptop);
- Type=idle on the getty measured at 25 ms (291/344 vs 266/283);
- `disable-udev-settle.rules` deleted as wrong;
- chrony deleted: 248/322 vs 261/288; "a restore is invisible from the inside", and correcting the clock belongs to whoever restores;
- blame figures that summed to ~380 ms out of a 175 ms userspace are kept only as history.

*Status:* chrony removed and in place.

**09-10 · qemu · `b151186`.** The device list is turned into an allowlist (`--without-default-devices`): 363→225 instantiable types and a 23.7→8.7 MB binary. MICROVM rejected ("no ACPI CPU hotplug"). Boot unchanged (240/274 vs 245/265, laptop). In place.

**09-10 · boot probes · `c049a21`, `8eef89d`, `a0efd65`.** QEMU's startup split: 1.4 ms launcher, ~27 ms exec to first instruction, ~7 ms guest to first console byte. The QMP greeting is not a checkpoint: it comes before the board is built, so the real QMP reply is at 18-20 ms, not 8. Cold-start cost is the host zeroing pages (18.8%) plus rom_reset; a restore pays neither (09-26). Not acted on.

**09-11 · machine · `21a830c`.** `Disk.DirectOverBacking`, with a cache-modes bench of 16 VMs. Writes were inconclusive: the disk was 94% full and times moved tenfold. `mincore` reports every page resident for a file the caller does not own. *Status:* the field is still in `machine/machine.go:182`; the bench was deleted in #48; `Disk.Cache` was removed in #94.

**09-11 · image · `ec008b0`.** sshd `TrustedUserCAKeys` (the CA file is written by the consumer). #101 (10-03): without the CA nothing logs in. In place.

**09-12 · machine · `9177475`.** `Spec.Accel` (tcg). TCG guest to PID 1 in 6.5 s vs 33 ms under KVM. In place.

**09-12 · image · `bb6bb89` (#5).** logind out of the boot transaction; it is started by the first login through the varlink socket and a `/run/systemd/seats` drop-in (`10-seats.conf`). *Measured:* 249/287 vs 273/297, 25 boots, two runs (laptop). `boot:logind` holds sessions. *Status:* **reversed 11-12** (#240). See §3.

### Week 2: 2026-09-14 to 09-20

**09-14 · image · `9335335`, `fe7d4ab`, `891e2f7`, `b5908ee`.** Changes:
- the image is sized in two passes: 974 MiB with 132 free, was 1163 with 317;
- e2fsprogs 1.47.4 is built static and pinned;
- `-m 0` for reserved blocks;
- a 64 MiB journal.

Bug: `891e2f7` (shipped in v20260914.01) made a 4.08 GiB filesystem, because `-b` read the size as a block count. Fixed in `b5908ee`. In place.

**09-14 · kernel · `b9d1985`.** A trixie toolchain and a reproducible vmlinux. *Why:* v20260914.01 and .02 had different kernels from the same source. bookworm's pahole 1.24 encoded `.BTF` in parallel, in a non-deterministic order, and the banner carried the build date. Two `--no-cache` builds now give the same vmlinux. Usable 503/575 vs 509/547 with gcc 12 (laptop, 20 boots). In place.

**09-15 · CI · `c767cd5`.** The BuildKit `type=gha` cache had never stored anything: the runtime token does not reach `run:` steps, and every build shared one scope. Fixed with named scopes. Superseded by the registry cache (`1666cb5`, 11-12).

**09-18 · machine · `da17b34`, `f023b56`, `5d4a01c`, `4947281`.** A restore opens its template read-only. Disks can be given as a chain of descriptors (`-add-fd`), and so can the console, `/dev/kvm`, vhost-vsock and vhost-net. In place, apart from the template parts.

**09-20 · CI · `f63aa38` (#10).** Workflows move to Blacksmith. Superseded 10-24 (`f29e59d`).

### Week 3: 2026-09-21 to 09-27

**09-21 · machine · `3d10256`, `56f6fea`.** `FingerprintCache` added. *Status:* removed `2d9b7e3` (09-29), with no caller; the identity split went in `c5baaa7` (10-01).

**09-24 · machine · `35df9cf`.** No VMX/SVM in the guest. Measured with 3 hot-added cores: vmx=1,0,0,0 plus a warning before, vmx=0 on all after. Fingerprint moved. In place.

**09-24 · image · `a01c847`.** Host keys made without RSA. RSA was 309 ms in blame in a guest, and on the host 141 ms vs 6 (ECDSA) and 5 (Ed25519). In place.

**09-26 · image · `f36ae13`.** `/tmp` is a tmpfs again and tmpfiles-setup is unmasked; both had been masked by the imported optimizer. 223/230 vs 220/232, within noise. In place; `/tmp` was capped at a quarter of memory in `47aa681`.

**09-26 · machine · `8063bcd`.** discard=unmap on the format node of a chain. Before this, fstrim freed nothing and deleted data stayed readable. Not measured. In place.

**09-26 · firmware/qboot · `e4f456f` (#12).** The qboot experiment: `write-pointer.patch` (vmgenid needs WRITE_POINTER), built in a pinned container. The compiler moves the number: 6.32 ms saved with GCC 15.2 vs 3.86 with GCC 14.2 (`qemu/qboot/README.md`). Not adopted yet, because firmware was not in the fingerprint. *Status:* adopted 09-29 (`f2f4da0`).

**09-26 · boot probes · `5221690`, `c6e48de`, `adc52cd`, `e2f6b8f`, `b9707e3`, `7b08e5a`.** Probes added: `boot:initcalls`, `boot:userspace`, `boot:systemd`. Findings:
- p50 98.2 ms kernel to "Freeing", of which 54.5 ms is 680 initcalls;
- udev coldplugs 266 devices, 64 of them `ttyN`;
- `CONFIG_VT=n` was predicted at 5-12 ms and measured at 0 (228/237 vs 228/246);
- 19 udev rule files masked: 0;
- device-count boot parameters: 0;
- the console-unit swap measured 340 ms worse, and the cause was finally found to be a 334 ms XTGETTCAP timeout, fixed with `TERM=dumb` on that unit (221/233 vs 558/572).

*Status:* findings recorded. The rows were deleted in `3f669ef` and #90. `boot:userspace` and `boot:systemd` were deleted in #250.

**09-26 · kernel · `dae6a14`, `5a3d509`, `cda04b1`, `9b5f0dd`.** A floor kernel (tinyconfig + PVH): release 97.95 vs floor 52.33 ms, so the config costs 45.6 ms (BTF, ACPI, ftrace). lz4 instead of gzip for an initrd: 6.4 ms. erofs removed (100.46 vs 102.22, noise). KSM and THP off: -0.50 ms, rejected. `CONFIG_VT` off as dead config; the 2.09 ms quoted for it was withdrawn (0.31 ms over 40 boots on an idle host). *Status:* erofs and VT off are in place. The floor probe and `kernel/minimal` were deleted in #48 (09-30).

**09-26 · image · `9fab3f1`, `ad05544`, `9b5f0dd`.** Unit-file count does not matter: 300 extra units gave 236 vs 246. Unit loading costs 26 ms per `systemd --test`, not the 212 the debug log suggested. Masking the tpm2 and factory-reset generators is worth 1.0 ms of that 26. An empty `/etc/machine-id` skips the preset pass. *Status:* the masks are in place; `boot:unitload` was deleted in #88.

**09-26 · machine · `ab4844c`.** `drop-cache=off` on chain images: a restore had been issuing `POSIX_FADV_DONTNEED` on the shared base. Restore exec-to-running 29 ms with an overlay vs 24 without disks; `cont` 8.9→2.8 ms; a cold cache has been measured at 251 ms. In place.

**09-26 · CI · `aa157cf`, `1ef95d1`.** The Blacksmith builder v2 required a `cache-key` and silently fell back to a local builder without it. Fixed with `nofallback`. Superseded 10-24.

**09-27 · machine · `7ec3601`, `2f5a7b0`.** ACPI PCI hotplug is turned off and the guest's pciehp used instead. *Why:* device_del never completed, and spin's lane waited 30 s. Fingerprint moved. *Status:* superseded by virtio-scsi (09-29).

**09-27 · tooling · `0b1f470`, `51c1fe9`, `2f03d09`, `e7e52db`.** `boot` with no disk uses `-snapshot`. Added: attach/detach, and the rule that checks go through the CLI. boot now execs QEMU, and the QMP handshake has a 5 s deadline. Validation was hardened, and the fingerprint moved again (`-accel` became its own argument). In place.

### Week 4: 2026-09-28 to 10-04

**09-28 · machine/CLI · `c202568`.** `save --template` and `restore` added, plus `boot:restore`: restore ~20 ms; first request 14 ms warm, 33 cold, 31 after a reclaim. *Status:* the template parts were removed 09-30 (#51); restore was not measured again until 11-13.

**09-28 · tooling · `30b717b`, `a8e2737`.** Every pin goes into `versions.yaml` with a gate test; the versions tool moved to go-tools. In place.

**09-28 · kernel · `2fd0fff`, `430d2b3`.** 7.2.8 with PSI; then MGLRU, PARAVIRT_SPINLOCKS, PARAVIRT_TIME_ACCOUNTING, fq_codel and hung-task detectors. 322 vs 333 ms usable (15 boots). ZRAM rejected. In place.

**09-28 · boot probes/image · `f7cd1ab`, `47aa681`, `337c08d`.** `boot:ssh-load`: CPU load does not slow a login, and 16 MiB available doubles one to ~210-310 ms. CPUWeight/IOWeight/MemoryLow on ssh.service changed nothing and are not shipped. MGLRU `min_ttl_ms=1000` and `/tmp` at a quarter of memory. **The sources disagree** on min_ttl_ms: `47aa681` says worst login 1369-7106 ms without vs 94-128 with; `kernel/Dockerfile:192-194` says 611-1785 vs 105-106. *Status:* in place; `boot:memory-policy` was deleted in #88, and `boot:ssh-load` was deleted in #250.

**09-28 · CI · `5332e97`, `3bef421`, `e6a0a1b`, `6b0f338`.** Actions pinned by commit, actionlint, the race detector, `task mutate`, a coverage floor. The floor was set at 85% against a local 85.7%; CI measures 84.7%, so it broke main and was lowered to 80% (`e6a0a1b`). In place.

**09-29 · machine/qemu · `43fb03b` (#24), `2b19096` (#25).** Hotplugged disks go on virtio-scsi instead of PCIe root ports. *Measured 2026-09-29, 4 ports vs 1 controller:* boot -31 ms; attach 135 → 1.5 ms; detach 6.1 s → 0.7 ms. NIC hotplug dropped. In place.

**09-29 · boot probes/CI · `a503db7` (#23), `e9e0c8e`, `de9b7db` (#29).** A report feature matrix (96 rows) published with every release, and `lab.yml` on the self-hosted KVM runner. *Why:* a laptop read 61 vs 105 ms minutes apart. Fixed along the way:
- the lab went green having measured nothing (a skip) (`8d8e97b`);
- a concurrency group cancelled queued experiments (`a3ba5a6`);
- two runners both took nbd0 (`b21e100`);
- an NBD device was mounted before it had a size (`8946f67`).

In place, with the lab on the bench fleet since 10-24.

**09-29/30 · kernel · `349dacb` (#33), `20fd702`, `3e26963` (#40).** Four local patches: 0001 objtool writes NOPs, -7.0 ms; 0002 skips the weak-function scan, 11.9 ms; 0003 rewrites the eval maps on first use, -9.0 ms; 0004 PCI config through ECAM, -4.5 ms. Kernel boot 109.4 → 73.7 ms (lab runner, workspace shape, 15-20 boots); the whole report boots on it (run 36651858261). Fingerprint moved. In place.

**09-29 · firmware · `ae6ab48` (#37), `f2f4da0` (#39).** qboot becomes the default BIOS and the firmware goes into the fingerprint. SeaBIOS 121.89 vs qboot 114.76 ms p50, 7.14 ms saved (lab runner, run 36650049960); 106 rows boot (run 36650559189). In place; the firmware *contents* were later dropped from the fingerprint and only its size kept (`fa3041c`).

**09-30 · qemu/firmware · `6f65249` (#42), `f10383b` (#43).** qboot writes the PAM registers in 3 writes instead of 7, at ~0.5 ms each. QEMU patch 0001 wraps device creation in one memory transaction (438 flat-view rebuilds). The first version also wrapped board init and the guest printed nothing (lab run 36662499370). In place.

**09-30 · kernel · `98c3589` (#47).** 7.3-rc5, 1.2 ms slower than 7.2.8 (30 boots). Superseded by 7.3 (10-26) and then 7.3.x.

**09-30 · boot probes · `f2c47f7` (#48).** Probes deleted with their conclusions kept: `boot:floor` and `kernel/minimal`, `boot:kernel-pages` (#44, added the same day), `boot:memory-unplug` (#46, same day), `cache_bench`, the report's disk axis (96 → 32 rows), the console-swap rows, and the SeaBIOS timing. See §3.

**09-30 · machine · `a9d6083` (#51).** **Templates removed** (spin #194). `Memory.File`, `x-ignore-shared`, `save/restore --template`, `boot:restore` and `docs/templates.md` all go. The fingerprint now means "a checkpoint resumes only onto a machine with the same fingerprint". Fingerprint moved. In place.

**09-30/10-01 · boot probes (memory/disk) · #57-#62, #64, #67-#74, #77, #78, #83, #84, #89.** The page-cache lab on construct (AMD Ryzen 9 5900X, Spin OS metal), recorded in `docs/memory-and-disk.md`. Kept as defaults: writeback overlay; hourly fstrim; discard accounting; qboot MTRRs; `page_reporting_order` stays 9. Rejected: §4. Sub-probes deleted in #89; `caches` and `reclaim` remain.

**10-01 · firmware · `4511798` (#75).** qboot programs the MTRRs. virtio-pmem read at 8.3 MB/s uncached-minus vs 1.1 GB/s with SeaBIOS (runs 36818224412, 36818721809). No boot cost: 103.5 vs 103.1 ms. Fingerprint moved. In place.

**10-01 · image · `5b7b1dc` (#76).** fstrim hourly: 657 → 21 MB on disk, 657 → 20 MB in the host's cache (run 36815822140). In place.

**10-01 · qemu · `aaf2d86` (#79).** Upstream's virtio-blk discard accounting is carried as a patch: before it, `query-blockstats` showed 0 unmaps for 1.1 GiB trimmed (run 36815822140); with it, 10-12 operations and 1123 MB (run 36884702758). "to delete in the bump that brings it". In place.

**10-01 · kernel · `1400f6a` (#80).** Lockdown at confidentiality plus the BPF LSM; no `/dev/mem` or `/proc/kcore`. "No cost that can be told from noise." *Side effect, found later:* SPIN_GRAPH and SPIN_CPU_SHARE silently traced nothing until #240 (11-13). In place.

**10-01 · CI/release · `2cd98f0` (#81).** Release notes say what changed, and the report boots each row 20 times, KVM only. *Why:* at 3 boots a row every KVM row read 5-11% slower between two releases that 20 alternated boots put within 5 ms. Also: `v20261001.01` shipped a `qboot.bin` without `mtrr.patch` in the tarball, because hack/release named the patches one by one. In place.

**10-01 · boot probes · #88, #90.** `unitload` deleted (it compared the image with itself) and `memory-policy` deleted. Bench rows reduced to "as shipped" and "baseline", with findings left as notes. In place.

**10-01 · machine · `c5baaa7` (#97), `ca7b54f` (#98), #94.** The fingerprint becomes one hash, and `Incoming` one field. The release verdict was blind to `machine/` changes, so `fingerprint_sha256` is now recorded. `Disk.Cache` removed. In place.

### Week 5: 2026-10-05 to 10-11

**10-09 · qemu · `f0ec1e0` (#103).** QEMU 11.1.2. "A new machine: every checkpoint taken on 11.1.1 stops resuming." That stranded v20261002.01's checkpoints (v20261013.01). Led to the next entry.

### Week 6: 2026-10-12 to 10-18

**10-13 · CI/release · `d692a7c`, `03aeb28`, `96a39d2`, `845fb5d`, `f033995` (#109), `b207bee` (#110), `302ced2` (#111), `99e31f9`.** Supply chain:
- signing and attestation, an SBOM, release from main only, apk locks;
- a reusable builder (#109, #110), reverted the same day (#111): it never achieved SLSA L3 and needed guards;
- an OSV scan, kept;
- attestations checked by digest, because the 338 MB tarball was over cosign's 128 MiB limit.

In place, apart from the reusable builder.

**10-13 · machine/fingerprint · `fa3041c` (#114).** **The fingerprint is the machine's contract.** `MachineType=pc-q35-11.1`. It hashes: the shape; `Spec.Contract` (every device as Args writes it); the BIOS's size; and the host CPU under `-cpu host`. It no longer hashes the QEMU, kernel, initrd or pvh.bin bytes. Added `boot:roms`, `boot:compat` (a v20261002.01 checkpoint resumes on 11.1.2), and `hack/machinetype`. One break by design. In place; `fingerprint_sha256=2bd7da6b…` from v20261013.02 onward.

**10-13/14 · CI · `7628981`, `4f7d55e`, `8c6d1c7`.** compat runs on main and before a release, not on PRs, because the KVM runner refuses pull_request jobs. cosign is built from its pin. Docker Hub images come from mirror.gcr.io after a 429. In place.

### Week 7: 2026-10-19 to 10-25

**10-19 · image · `fb39fd5` (#210).** GNU coreutils instead of uutils: 21 OSV findings against rust-coreutils 0.10.0; image 855M vs 877M. In place.

**10-19 · CI · `7bf5429` (#197).** One OSV issue per package; there had been 77 open. In place.

**10-24 · CI · `c75f144` (#214), `f29e59d` (#215), `d67afc4`, `97a6de1`.** Every workflow step is one task. Builds move to GitHub's runners. **Report and lab move to the bench fleet** (one instance type) and compat to the kvm fleet. A fix: vhost-vsock permissions were reset by udev after `modprobe` (`97a6de1`). In place. The first report on the fleet (v20261026.01) compared a Ryzen 9 5900X with a Xeon 6975P-C, and its notes say so (§5).

### Week 8: 2026-10-26 to 11-01

**10-26/29 · kernel · `1c12413`, `0bb00ce`, `fc1079c`.** Kernel 7.3; the apk locks moved; the kernel now tracks mainline. In place.

### Week 9: 2026-11-02 to 11-08

**11-03 · qemu/kernel · `7c37270`, `b4b67c3`, `6e7ba47`.** QEMU 11.1.3, kernel 7.3.1. The apk lock broke because Alpine's mirror dropped python3 3.14.8. In place.

### Week 10: 2026-11-09 to 11-13

**11-09 · kernel/CI · `00db122` (#223), `0ce27ad` (#224), `8d495c5` (#225).** Kernel 7.3.3, with the config named by series and a daily proposal job. A kernel CVE judge: of 18221 CVEs against 7.3.1, 9 apply with a fix later in 7.3 and 1 has no fix. The apk lock broke again (zlib 1.4). In place.

**11-09 · tooling · `ed4ad86`, `e1d40ad`, `6e25bb3`.** Retries and tokens for GitHub downloads after 504s. Task and go-tools as released binaries (455 → 81 modules). In place.

**11-11 · kernel · `cce320f` (#229).** 7.3.4. In place.

**11-12 · docs/image · `e452afc` (#232), `3a7864d`.** A comment sweep: several comments still said a QEMU or kernel bump strands checkpoints, which has been false since `fa3041c`. Dead code deleted: `spin-machine-console.service`, the modprobe blacklist, and the getty@tty2-6 masks. In place.

**11-12 · image · `02c58b9` (#235).** bubblewrap, socat and ripgrep for agent sandboxes. In place.

**11-12 · CI · `c6c29b5` (#236), `ec2c7fb` (#237), `1666cb5` (#239).** A daily apk-lock refresh PR. *Why:* the lock broke three times (`6e7ba47`, `8d495c5`, release run 43157106109). The BuildKit cache moved to ghcr: on the fleet the gha cache read a store nobody wrote, so compat compiled cold on every run and failed on main 4 times from 11-09; both release attempts on 11-12 failed. In place.

**11-12 · image · `3c8da99` (#238).** The hot-add rule onlines CPUs only. udev's memory "online" write raced the kernel's `memhp_default_state=online` and logged EINVAL for every block. Closes #230. In place.

**11-12/13 · image/kernel/boot probes · #240 (`bc41f44`, `a8df865`, `28823fb`, `8fb1dd9`, `e5b8003`).**
- Kernel patch 0005, each PCI capability list walked once: kernel 95.1 → 88.9 ms; ECAM accesses 1,032 → 680 (lab 43327612594).
- **logind back in the boot, and spin lingers:** first SSH login 570/626 → 510/525 (lab 43308825692, 10 boots).
- MOTD and pam_motd off: -7 ms per login, -12 on the first.
- `sshd@` waits for the host keys. Before this, SSH over vsock "worked not at all".
- New probes: `boot:waits`, `boot:jobs` (+ jobwatch, godbus), `boot:ssh-first` (+ vsockproxy), `boot:exits`, `boot:mmio`.

Measured and not shipped:
- the getty without BindsTo: 342 vs 345 (lab 43303190733);
- keygen in the boot: 518 vs 514 (lab 43312419007);
- virtio-rng removed: -13.6 ms to the prompt (lab 43321809471), not done because it would move the fingerprint;
- `pci=noearly`: the guest does not boot;
- PID 1 spends ~25 ms in the BPF verifier.

*Side effect:* v20261113.01's console prompt came 21-25% later (release notes). *Status:* patch 0005 and the exits/mmio/jobs probes were deleted in #249 (11-13).

**11-13 · image · `43de41e` (#241).** serial-getty Type=simple. *Why:* with linger, `user@1000` runs inside the boot transaction, and Type=idle held the getty for 91.8 ms (lab 43361486792). 346/358 vs 423/441 (lab 43362067503); v20261112.01 was 344/353. Released in v20261113.02. *Status:* removed in #249 (11-13): nothing a workspace does waits for the console's prompt.

**11-13 · boot probes/machine · #242, #246 (both closed).** Restore measured, and a **mapped-ram** checkpoint layout proposed twice:
- #242 (draft): `--channels`; mapped-ram ×4 restore-to-login 394/410 vs stream 877/1888 (lab 43364516874). Those numbers were later declared contaminated by restored VMs leaking between repetitions.
- #246: mapped-ram put in `Spec.Contract`, which would move the fingerprint, plus `boot:score`. On the fleet, with every cold boot run before every restore: restore login 385/400 mapped-ram vs 386/399 stream (lab 43483852339); also lab 43482829958, 373/402 vs 376/416. Closed: "does not earn moving the fingerprint". A laptop lead of 188/358 vs 412/732 was called "a lead, not a finding".

*Status:* rejected; #242 and #246 closed.

**11-13 · image · `d895543` (#245), `5e0cc2f`.** sshd offers only `mlkem768x25519-sha256`. ML-KEM costs the same as curve25519: first login 461 for both; a second login 40 vs 39; OpenSSH 9.6 with sntrup761 took 489 first and 104 for a second (lab 43479854381, also 43463472762). `boot:ssh-first` logs in with x/crypto/ssh, and vsockproxy is removed. *Status:* merged 18:30Z. #245's own body said "Do not merge before the clients move": open-remote-ssh#8 merged at 18:57Z, and spin-stack/spin#1206 (the spin CLI refusing OpenSSH < 9.9) merged after its CI and KVM lanes passed. Not yet released.

**11-13 · image · `3e8075f` (#247 → #248).** SSH host keys are made from the boot's first dispatch (wanted by sysinit.target, `DefaultDependencies=no`). First login 489/506 → 468/494; accept to login 177 → 154 (lab 43483372488). Rejected in the same run: masking two generators (489), and masking 11 always-failing units (484). Not released.

**11-13 · boot probes · #249, #250.** Removed:
- kernel patch 0005 ("6 ms of a ~470 ms first SSH login, for a local patch carried across every kernel bump");
- the getty Type=simple drop-in ("nothing a workspace does waits for that prompt");
- `boot:exits`, `boot:mmio`, `boot:jobs`/jobwatch/godbus.

#250 also removes `boot:bench`, `boot:trace`, `boot:userspace`, `boot:systemd` and `boot:ssh-load`, keeping "a gate …, the release's report, the metric a workspace waits for (ssh-first), and one tool per layer".

**11-13 · goal · #251, #252, #254.** The goal is written down: a cold boot to the first SSH login in under 100 ms (AGENTS.md), with restore as the second number. The rules that held exploration back are removed (no init in a release, the fingerprint as an event to avoid, one default per experiment, the bench fleet alone deciding); hotplug and dynamic memory become an invariant that is never traded for boot time.

**11-13 · boot probes · branch `floor-kernel` (experiment).** The floor without systemd: PID 1 a script that mounts what sshd needs, makes an Ed25519 key and has socat hand vsock connections to `sshd -i`. First login 467 → 220 ms (lab 43492686353), 506 → 240 (lab 43493135230); guest uptime puts init at ~90 ms of kernel and the keys 10-20 ms later. What is left at the floor: QEMU and firmware ~44 ms (boot:firmware-stages, lab 43494513791: QEMU's own start 35.9, qboot ~8.5, of QEMU's start 7.9 copying vmlinux), kernel ~80-90, init ~20, the login ~44. `/proc/uptime` starts partway into the kernel, so it hides ~35 ms that an earlier reading charged to QEMU.

**11-13 · CI · spin-stack/ci#14, #15, spin-machine#253.** A `bench-metal` fleet (c8i.metal-48xl, then r7i.metal-24xl behind it) and lab.yml's `host` input. It has not run: metal spot scored 1-3 of 10 for placement and failed `UnfulfillableCapacity` in every zone; on-demand failed `VcpuLimitExceeded` at a quota of 96 vCPU. An increase to 288 was requested the same day (case open).

**11-13 · kernel · #255 (this change).** Where the kernel's ~100 ms goes on the bench fleet (boot:initcalls lab 43495583323): initcalls 52 (acpi_init 12, of which `acpi_pci_root_create` - bus 0's enumeration - 11.2 traced on a kernel without forced lockdown, lab 43499868986; inet_init 7.9, which is vmlinux BTF's first parse; the virtio probes ~17), before the first initcall ~15-20 (SPIN_EARLY, lab 43507472288; ~4 of it the probe's own 18 MB log buffer), SMP ~4, the root mount ~6. Kept: patch 0005 back, 0006 (BTF parsed in the background from a core_initcall), `CONFIG_DEFERRED_STRUCT_PAGE_INIT=y`: kernel 96.3 → 87.1 ms (lab 43505126131), first login 466 → 456 as built and 223 → 214 at the floor (lab 43501992969). `driver_async_probe` for rng, balloon and vsock on the command line: 214 → 209 at the floor (lab 43506390106).

**11-13 · image · the first login's lane (this change).** Where systemd's ~250 ms over the floor went, on the bench fleet: ~60 before sshd accepts (sysinit, udev, journald), ~105 in pam_systemd - logind answers a session of class `user` only once the user's manager has started and orders its scope after `systemd-user-sessions` (systemd v259.9 source, `logind-session.c`) - and ~65 in a handshake competing for the CPU. The vsock socket, `sshd@`, dbus, logind, `user-runtime-dir@` and `user@` without default dependencies, no `/run/nologin` at boot (pam_nologin refused the lane's first login: "Access denied for user spin by PAM account configuration"), sessions of class `user-early-light`, and udev's coldplug filtered to block, net, virtio, misc, input, rtc, ptp and ttyS0: first login 478 → 402 ms (lab 43518915618). Measured and left out: priority (`CPUWeight`, `Nice`) on the lane, −2 ms over the filter and a slower second login (63 vs 45 ms).

**11-13 · kernel/qemu · branches `kernel-snapshot`, `qemu-snapshot` (rejected).** QEMU publishes bus 0's configuration space over fw_cfg after the firmware (etc/spin/pci-config) and the kernel answers boot-time configuration reads from it until each dword is written. It works (`PCI: configuration space of 5 devices on bus 0`) and is worth -14 ms against the same kernel without it (lab 43514550646), but only 2-3 ms against 0005, which already removes most repeated reads: 209/217 against 211/222 (lab 43514884598). Not worth a QEMU patch and a 250-line kernel patch.

**11-13 · boot/qemu · branch `first-login-report` (this change).** v20261113.03's report read +11-18% on every row: the report times the serial console's prompt, and the getty drop-in removed in #250 left it Type=idle, waiting for the lane's units. The first login it does not time went 478/500 (v20261113.02, lab 43524334205) -> 381/410 ms (v20261113.03, lab 43524721490), two runs and not interleaved. The report now logs into the release over SSH too (`first_login`), and compare writes it first. The QEMU build asserts static-pie, full RELRO, an NX stack and stack canaries, which come from QEMU's and Alpine's defaults and no flag here. Measured and left out: one sshd for every connection (the vsock socket with Accept=no, an `sshd -D` from the first dispatch), first login 382/393 against 381/410 and a second 40 against 48 ms (lab 43524721490).

**11-13 · image · udev's coldplug keeps cpu (this change).** The coldplug filtered in `630fa2c` left out cpu, and a core attached before systemd starts - spin gives a workspace its cores while its own init is PID 1 - is onlined by that replay reaching `40-spin-hotadd.rules` and by nothing else: with v20261113.03 every workspace came up with one core whatever it was sized to (spin's kvm run 43527839765, four tests "still waiting for the guest to have its floor's two cores"). `task boot:hotadd` attaches a core before systemd and reads `online` after: `0` on v20261113.03's image, `0-1` with cpu in the filter. What cpu costs the first login was not measured; it is one device per core.

---

## 3. Rework cycles

Each cycle: the sequence, what each step measured, and the lesson.

### 3.1 udev: masked, unmasked, then argued over for weeks
1. Initial import: udev masked "to save boot time" (`8aee03a`). The serial getty could not start, so a debug init was written to work around it (`5e7d60e`, `8aee03a`, `24c8a06`).
2. 09-10 `4cbd229`: masking buys nothing (826 vs 829). The first harness had left `systemd-udev-trigger` masked and read "udev changes nothing".
3. 09-10 `722d468`: unmasked. `b5f361f`: the harness's udev row was itself a no-op.
4. 09-10 `064c4f4`: unmasking udev quietly enabled the serial getty that the console unit existed to avoid. This was left open as "a decision".
5. 09-26 `adc52cd`/`e2f6b8f`: udev's 46 ms "overlaps" the boot; removing devices, rules or VT gives 0.
6. 11-12 `3c8da99`: the memory half of the hot-add rule, added 09-10, raced the kernel and was removed.

**Lesson:** an optimisation imported without its measurement survived 3 days and cost a debug init, a console unit and a harness. Measure before inheriting a mask, and make a variant fail when its edit matches nothing (`e2f6b8f` added that check).

### 3.2 logind: out of the boot, then back in
1. 09-10 `6aba37b`: logind 63 ms on the chain, worth 24 ms to the console prompt (laptop).
2. 09-12 `bb6bb89` (#5): logind is started at the first login through the seats directory, worth 24 ms (249 vs 273). Two dead ends were recorded, and `boot:logind` was written to hold it.
3. 11-12 #240: back in the boot, with spin lingering. First SSH login 570 → 510 (lab 43308825692). The comment says the 09-12 change "moved logind, dbus-broker and user@1000.service into the first login instead, which is what a workspace waits for."
4. 11-13: the side effect, console prompt +77 ms, was fixed with getty Type=simple (#241), and that fix was removed the same day (#249).
5. #240's notes: the `SPIN_LOGIND_NO_SEATS=1` control "was written to fail, and on this image … it passes". `10-seats.conf` may now be unnecessary. Not settled.

**Lesson:** the 09-12 decision optimised the console prompt, a metric nobody waits for, and paid for it in the first login, which was not measured until 11-12. Name the metric that matters before optimising; the first SSH login probe arrived two months after the change it would have rejected.

### 3.3 Serial getty Type=idle / Type=simple
1. 09-10 `80f984b`: removing Type=idle was within noise against agetty's `sleep(1)`.
2. 09-10 `064c4f4`: Type=idle worth 25 ms (291/344 vs 266/283), read as "systemd's job queue draining".
3. 09-26 `b9707e3`/`7b08e5a`: Type=idle was eliminated as the cause of the 340 ms console-swap cost.
4. 11-13 #241: Type=simple shipped, 346/358 vs 423/441, after linger made the idle hold 91.8 ms.
5. 11-13 #249: removed, since the console prompt is not a workspace metric.

**Lesson:** the value of this setting depends on what else is in the boot transaction. It was re-measured each time something upstream changed (linger), and its worth turns on a metric (the console prompt) that does not matter (#249). Decide which metrics count before shipping fixes for the others.

### 3.4 SSH host key generation (moved 5 times)
1. 09-07 `8aee03a`: ssh fails five times per boot; "SSH keys are identity and belong wherever the rest of a VM's identity comes from" (not fixed).
2. 09-07 `2df3dbf`: generated per boot, hung off ssh.service. Hanging it off ssh.socket made an ordering cycle.
3. 09-24 `a01c847`: RSA dropped (309 ms in blame).
4. 11-12 #240 `28823fb`: `sshd@` must wait for the keys. Before this, SSH over vsock "worked not at all"; nothing had tested vsock SSH since the socket path changed.
5. 11-12 lab 43312419007: keygen wanted by multi-user.target, 518 vs 514. Nothing gained, because default dependencies kept it after basic.target. Not shipped.
6. 11-13 #247 → #248: wanted by sysinit.target with `DefaultDependencies=no`, 489 → 468 (lab 43483372488). Consumer consequence: a checkpoint taken after boot always carries host keys, so the restorer must re-key (`image/README.md`).

**Lesson:** the per-connection vsock path ran without host keys until a probe logged in over it. A functional probe of the real login path (`boot:ssh-first`) should come before timing work on it. Step 5 failed for a reason (`DefaultDependencies`) that reading the unit's ordering would have shown.

### 3.5 Save/restore and templates in the CLI; restore measurement; mapped-ram (proposed twice)
1. 09-07 `656d55b`: save/resume added; reverted the same day as lifecycle (`b882171`).
2. 09-26 `20e14d9`/`ab4844c`: TEMPLATES.md, template restore 24 ms, `drop-cache=off`.
3. 09-28 `c202568`: `save --template`/`restore`, plus `boot:restore`.
4. 09-30 #51: templates removed upstream (spin #194). `boot:restore` deleted, two days after it was written.
5. 11-13 #242: restore measured again, and mapped-ram proposed with `--channels`. Its numbers (lab 43364516874) were contaminated by leaked VMs.
6. 11-13 #246: mapped-ram in the contract. The fleet shows no win (385 vs 386); closed.

**Lesson:** the product's path (template restore, then checkpoint resume) changed outside this repository, and probes were built and deleted around it within days. Restore of a checkpoint was not measured at all between 09-30 and 11-13. The mapped-ram decision now has a fleet result; do not reopen it without a new reason, for example a multi-VM concurrent restore or larger RAM, which #242's checklist names as unmeasured.

### 3.6 Fingerprint: what it hashes (rewritten ~8 times)
1. Binaries plus shape (09-07).
2. Plus device topology, `b476f6d`.
3. Topology corrected to exclude disks and NICs, `b882171`.
4. NIC count and MTU back in, `6599829`/`2b46d78`.
5. Root ports, then virtio-scsi, in.
6. Firmware by content in, `f2f4da0`.
7. Templates removed, `a9d6083`.
8. One hash, `c5baaa7`.
9. **Contract, not binaries**, `fa3041c` (10-13), after the QEMU 11.1.2 bump stranded every checkpoint (`f0ec1e0`).

Each step moved every fingerprint. Comments that said "a QEMU or kernel bump strands checkpoints" survived until `e452afc` (11-12). The verdict tool was blind to `machine/` changes until #98.

**Lesson:** rule 1's meaning changed on 10-13. Check claims about the fingerprint against `Spec.Contract` and `boot:compat`, not against older comments, and treat any proposal that touches `Spec.Contract` (virtio-rng removal, mapped-ram) as a fleet-wide event.

### 3.7 Console unit (spin-machine-console.service)
Sequence: created 09-07 (`24c8a06`); made redundant by udev 09-10 (`064c4f4`); measured 340 ms worse as a swap; TERM=dumb fixed it, 09-26 (`7b08e5a`); "viable", but the exposure question was left open; deleted 11-12 (`3a7864d`).

**Lesson:** two months of a unit nothing enabled. Delete as soon as nothing references a unit.

### 3.8 Debug init and diagnostic initrds
`cmd/spin-machine-init` was created 09-07 and deleted 09-10 (`8eeb01c`). Diagnostic initrds came back as `boot/probeinit` (09-29, `a81b6b5`), now allowed by CLAUDE.md as isolated diagnostics.

**Lesson:** the rule ("no init in a release") was clarified rather than broken. CLAUDE.md's "Diagnostic input is different" paragraph now records this.

### 3.9 Probes added then deleted (short-lived)
- `boot:matrix` (09-10 → 09-10)
- `kernel/minimal` + `boot:floor` (09-26 → 09-30)
- `boot:kernel-pages` (09-30 → 09-30)
- `boot:memory-unplug` (09-30 → 09-30)
- cache bench (09-11 → 09-30)
- `boot:restore` (09-28 → 09-30)
- `boot:unitload` (09-26 → 10-01)
- `boot:memory-policy` (09-28 → 10-01)
- page-cache sub-probes (09-30 → 10-01)
- bench rows (09-10 … → 10-01)
- console-swap variants (09-26 → 09-30)
- `boot:exits`, `boot:mmio`, `boot:jobs` (11-12 → 11-13, #249)
- `boot:bench`, `boot:trace`, `boot:userspace`, `boot:systemd`, `boot:ssh-load` (→ 11-13, #250)

This is the cycle CLAUDE.md asks for ("remove … once it no longer has an active question"). The cost is that the answers live only in commit messages and comments.

**Lesson:** this log, or `docs/memory-and-disk.md`-style records, is where an answered probe's result should land. Before writing a probe, check §4 and the row comments in `boot/harness_test.go` and `boot/ssh_first_test.go`.

### 3.10 /tmp and tmpfiles; erofs; BPF_LSM; chrony
- **/tmp:** masked by the imported optimizer; unmasked 09-26 (`f36ae13`, noise); capped at a quarter 09-28.
- **erofs:** removed from the kernel 09-26 (`5a3d509`); the build assertion relaxed 09-30 (#66) for the pmem/DAX experiment, which was rejected (#63, #65).
- **BPF_LSM:** off 09-07 ("a boundary inside the boundary"); on 10-01 (`1400f6a`), and the stale note was fixed 10-05 (`ebe2a6e`).
- **chrony:** added 09-07, removed 09-10.

**Lesson:** the imported scripts carried decisions without numbers. Each has been re-decided one at a time.

### 3.11 CI cache and runners (rebuilt ~5 times)
- gha cache that stored nothing (`c767cd5`, 09-15);
- Blacksmith (09-20);
- the builder's silent fallback (`aa157cf`/`1ef95d1`, 09-26);
- export from main only (`bdec6cc`);
- GitHub runners for builds plus the bench/kvm fleets (10-24);
- the gha cache unreadable on the fleet, so a registry cache (`1666cb5`, 11-12).

Also the reusable builder, added and reverted on 10-13.

**Lesson:** each move was taken without checking that the cache was actually hit. A cache-hit assertion would have caught `c767cd5` and `1666cb5` on day one.

### 3.12 apk locks broke three times
`6e7ba47` (11-03), `8d495c5` (11-09), release run 43157106109 / `ec2c7fb` (11-12); fixed structurally by a daily refresh (`c6c29b5`).

**Lesson:** pinning to a mirror that serves only the newest build needs a refresh loop from the start.

---

## 4. Measured and rejected

| Idea | Result | Source | Date |
|---|---|---|---|
| `usb=off`, `vmport=off`, `smm=off`, `i8042=off` | 63.0/62.6/62.3/62.5 vs 62.9 ms | `fa1803f` | 09-08 |
| Mask udev | Saves 0, and boots never reach a login (10 s ttyS0 timeout) | `4cbd229`, `80f984b` | 09-10 |
| Trim the 8250 driver | 154 µs | `c03ed89`, kernel/Dockerfile | 09-10 |
| Modular kernel | 0.57 ms; breaks "a release is one machine" | `1a8e25c` | 09-10 |
| ACPI params (acpi=noirq, pnpacpi=off, …) | 69.9 vs 68.6 ms, slower | `64d2901` | 09-10 |
| initcall_blacklist ksm/kcompactd | 0; event_trace's 6 ms is the profiler | `64d2901` | 09-10 |
| CONFIG_XFRM=n | Moves BTF's 9 ms first-parse elsewhere | `e285dec` | 09-10 |
| Generous vCPU ceiling | maxcpus=16 +5 ms, 64 +9 ms of kernel | `f32d527` | 09-10 |
| MICROVM | No ACPI CPU hotplug | `b151186` | 09-10 |
| agetty tweaks (TTYReset, --noissue, --delay 0, …) | Within noise of agetty's sleep(1) | `80f984b` | 09-10 |
| Less guest RAM (512 MiB) | 1241 vs 1256 ms, nothing | `80f984b` | 09-10 |
| chrony / time daemon | 10 ms; cannot know it was restored | `c7af4d3` | 09-10 |
| systemd-timesyncd | SNTP only, no refclock | `c7af4d3` | 09-10 |
| `cache=none` on disks | Breaks shared base caching, fails on tmpfs | `8b7391a` | 09-07 |
| Overlay O_DIRECT | Small reads 1490 vs 530 ms | docs/memory-and-disk.md | 10-01 |
| CONFIG_VT=n for speed | 0 to login; 0.31 ms kernel (2.09 withdrawn) | `e2f6b8f`, `9b5f0dd` | 09-26 |
| 19 udev rules masked; loop/8250 count params | 0 | `e2f6b8f`, harness_test.go | 09-26 |
| Console unit instead of serial-getty | +340 ms until TERM=dumb, then equal | `c6e48de`, `7b08e5a` | 09-26 |
| Fewer unit files | 300 extra: 236 vs 246 | `9fab3f1` | 09-26 |
| KSM + THP off | -0.50 ms | `cda04b1` | 09-26 |
| COMPACTION off | Impossible: virtio-mem needs CONTIG_ALLOC | `cda04b1` | 09-26 |
| erofs removal for speed | Noise (100.46 vs 102.22) | `5a3d509` | 09-26 |
| Smaller TCP hash tables | Moved nothing | `dae6a14`, #48 | 09-26 |
| ZRAM | Runaway killed later (20 vs 14 s), worst login 3.8 s | kernel/Dockerfile, `430d2b3` | 09-28 |
| systemd-oomd | Kernel always beat it | `337c08d` | 09-28 |
| CPUWeight/IOWeight/MemoryLow on ssh.service | No change | `f7cd1ab` | 09-28 |
| Wrapping board init in a memory transaction | Guest prints nothing | `f10383b`, lab 36662499370 | 09-30 |
| Lowering `memhp_default_state` (unplug policies) | Kept `online` | `2903d8c`, #48 | 09-30 |
| Base on virtio-pmem + DAX | ~155 MB less per VM, but a side channel | #63, #65, docs | 10-01 |
| DAMON_RECLAIM, MGLRU eviction | Slower / not selective | docs/memory-and-disk.md | 10-01 |
| `page_reporting_order` 0/3 | ~100 MB transient vs ~180 MB huge pages lost | docs, #83 | 10-01 |
| Prefetch the boot's files | 47 ms to win 24 | docs, run 36803912357 | 10-01 |
| Second limit (`memory.high`) | `memory.max` suffices | docs | 10-01 |
| KSM, free page hinting, virtio-fs DAX | Security / correctness / unsupported | docs | 10-01 |
| Reusable release builder (SLSA L3 attempt) | Not L3, needed guards | #109-#111 | 10-13 |
| Getty without BindsTo dev-ttyS0 | 342 vs 345 | lab 43303190733 | 11-12 |
| Keygen wanted by multi-user.target | 518 vs 514 | lab 43312419007 | 11-12 |
| `pci=noearly` | Guest does not boot | #240 | 11-12 |
| Remove virtio-rng | -13.6 ms to prompt, but moves fingerprint; not done | lab 43321809471 | 11-12 |
| Mask sshd-socket- and run-generators | Login 489 (no change) | lab 43483372488 | 11-13 |
| Mask 11 always-failing units | 484 (no change) | lab 43483372488 | 11-13 |
| Vsock socket + sshd@ without default deps | No sooner; dropped 7 of 22 first connections (laptop) | `boot/ssh_first_test.go:124-128` | 11-13 |
| mapped-ram checkpoints (multifd ×4) | Restore login 385/400 vs 386/399 | #246, lab 43483852339 | 11-13 |
| sntrup761 (OpenSSH default) | 104 vs 40 ms per login | #245, lab 43479854381 | 11-13 |
| Configuration-space snapshot over fw_cfg (QEMU + kernel) | 209 vs 211 with 0005 | lab 43514884598 | 11-13 |
| CPUWeight/IOWeight/Nice on the first login's lane | −2 ms over coldplug filtered; second login 63 vs 45 | lab 43518915618 | 11-13 |
| One sshd for every connection (vsock Accept=no, `sshd -D` early) | First login 382 vs 381; second 40 vs 48 ms | lab 43524721490 | 11-13 |
| Login without pam_systemd (diagnostic bound) | 380 vs 436, but no session, no XDG_RUNTIME_DIR | lab 43518044150 | 11-13 |
| Lower the vCPU ceiling, drop the balloon or ACPI for boot time | Not tried: hotplug and dynamic memory are an invariant | AGENTS.md | 11-13 |

---

## 5. Measurement pitfalls found

- **The instrument measured itself.** Each case and where it is recorded:
  - initcall_debug with a serial console (`c03ed89`);
  - a function_graph wide filter, 26× inflation (`a31d7b2`);
  - the `kvm:kvm_pio` tracepoint, 49.3 vs 27 ms (`c049a21`);
  - `trace_event=initcall` (`64d2901`);
  - systemd debug logging, 212 vs 26 ms (`ad05544`);
  - the `boot:systemd` probe "3× slower" (`7b08e5a`);
  - a shell harness throttling the console, 1424 vs 190 ms (`80f984b`);
  - a log_buf_len of 8M costing 4.9 ms (`5221690`).

  Now avoided by quiet consoles, `--profile`, and asserting the log is whole.
- **Blame, critical chain and log gaps are not savings.** logind was 63 ms in blame and worth 24; chrony 43 and worth 10; udev 46 ms and worth 0 (`6aba37b`, `e2f6b8f`). Recorded in `boot/harness_test.go` comments.
- **Misattribution.** BTF's first parse looked like XFRM (`e285dec`); ksm/kcompactd 2.00 ms is HZ resolution (`cda04b1`); vCPU descheduling was counted inside initcalls (`64d2901`).
- **Checkpoints that lie.** The QMP greeting comes before the board is built (`8eef89d`); SeaBIOS's banner comes before hardware init (`6aba37b`, #45).
- **Variants that did nothing:**
  - `systemd.mask=` with the debug-generator masked (`80f984b`);
  - `unmask_udev` that had become a no-op (`b5f361f`);
  - `rm -f` on an `/etc` path that did not exist (`bb6bb89`);
  - a mask that matched no file (`e2f6b8f` now refuses it);
  - `debugfs -R` exiting 0 (CLAUDE.md);
  - SPIN_GRAPH and SPIN_CPU_SHARE under lockdown (#240).
- **Host noise:**
  - a cold page cache right after `task build`: 98 ms read as a 35 ms regression (`f32d527`);
  - a host building a kernel: CONFIG_VT's 2.09 ms (`9b5f0dd`);
  - three runs of one kernel reading 59.5/96.8/167.7 ms (`5221690`);
  - a laptop reading 61 vs 105 ms minutes apart, on an efficiency core (`de9b7db`);
  - 3 boots a row is noise at 5-11% (#81).

  Now avoided by 20 interleaved boots on the bench fleet, one lab run at a time (CLAUDE.md).
- **Different hosts compared.** The v20261026.01 notes compare a Ryzen 9 5900X with a Xeon 6975P-C: "the times below compare two machines, not two releases". The report's per-row fingerprint also changes because the host CPU is in it. Numbers from construct (docs/memory-and-disk.md, kernel patches 0001-0004, qboot) do not compare with the bench fleet's.
- **Release-to-release report noise.** v20261019.01 → v20261023.01, with only the image changed, shows +6.7% to +14.6% and p95 of 745-880 ms on some rows. Treat single-release deltas under ~10% with suspicion unless the lab repeats them.
- **Nested KVM on the bench fleet.** A cold boot after a restored guest has run on the host is ~1.8× slower across all phases; a laptop shows nothing of the kind (#246). Restored VMs leaked between repetitions and contaminated lab 43364516874 (#242's numbers). Now avoided by running every cold boot before every restore, and checking the state is out of the page cache with mincore (#246, closed: this method is not in the tree).
- **Page cache.** `POSIX_FADV_DONTNEED` declines recently touched pages, 4.8 vs 251 ms (`ab4844c`). `mincore` lies for files the caller does not own (`21a830c`). `drop_caches` was forbidden on the shared runner (#57). fstrim right after rm trims nothing (`251638d`). QEMU 11.1.1 never counted discards (`aaf2d86`).
- **Lab plumbing:**
  - green on a skip (`8d8e97b`);
  - concurrency groups cancelling queued runs (`a3ba5a6`);
  - two runners on nbd0 (`b21e100`);
  - 108-byte socket paths (`299777b`, `f2c47f7`);
  - `modprobe` on a module-less kernel (`5a70721`, `213cd07`);
  - udev resetting vhost-vsock permissions (`97a6de1`);
  - BuildKit comparing a file with itself (`b9d1985`).
- **Guest clock.** `1d5e7f5` says the guest's wall clock "stepped back … chrony setting the time" on the lab runner on 09-30, yet chrony was removed from the image on 09-10 (`c7af4d3`). The cause named there is not supported by the tree. The fix (time by `/proc/uptime`) stands either way.
- **Absolute numbers drift between runs and releases.** One baseline, "as built" first SSH login, reads 570/626 in lab 43308825692 (11-12, v20261112.01) and 489/506 in lab 43483372488 (11-13, v20261113.02). Compare deltas only within one interleaved run, never absolutes across runs.

---

## 6. Open questions

- **Release #245 only with its clients.** spin-stack/spin#1206 and open-remote-ssh#8 are merged; users need OpenSSH 9.9 or newer, or a spin and open-remote-ssh built from them, before a release carrying #245 reaches them.
- **Restore as the north.** #242 calls restore "the path a workspace takes". On the fleet a restore reaches login at 386/399 vs a cold boot at 487/516 (#246). Not measured:
  - concurrent restores on one host;
  - RAM above 2 GiB;
  - channel counts.

  These are #242's checklist.
- **Is `10-seats.conf` still needed?** The control that should fail passes on systemd 259 (#240, "Not settled here").
- **virtio-rng removal.** -13.6 ms to the prompt (lab 43321809471), a fingerprint change, not taken (#240).
- **PID 1 time.** ~25 ms in the BPF verifier for systemd's socket-bind program, "measured, nothing done" (#240). The ~32 ms transaction-build gap is "explained, not removable" (`boot/waits_probe_test.go:391-399`).
- **musl under long-running load.** Never measured (`31a57e7`).
- **`page_reporting_order`.** Revisit "only if memory per host becomes the limit" (docs/memory-and-disk.md).
- **Moving `MachineType` off pc-q35-11.1.** "its own change" (CLAUDE.md rule 1, `hack/machinetype`).
- **The kernel CVE judge.** 9 CVEs apply with a fix later in 7.3, and 1 has no fix (`0ce27ad`, against 7.3.1; the kernel is now 7.3.4).
- **QEMU patch 0002.** To be deleted when a QEMU tag carries upstream 331ce936 (`aaf2d86`).
- **Host keys in checkpoints.** A guest checkpointed after boot carries host keys, so the restorer must re-key (`image/README.md`, #248). This is a consumer contract to confirm in spin.
- **min_ttl_ms evidence disagrees.** `47aa681` and `kernel/Dockerfile:192-194` (§2, 09-28).
