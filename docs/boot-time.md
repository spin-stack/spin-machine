# Boot time: where it stands

This page follows the goal's two numbers (AGENTS.md, "The goal"). It says where each stands and where
its time goes. It lists what the machine carries for them and what each
item earns, what was tried and rejected, and what is open. `docs/log.md` tells each change, dated.
Each release's own numbers are in `docs/releases.md`. What the guest's memory and disk cost is in
`docs/memory-and-disk.md`. How a number is measured is [measuring.md](measuring.md), and the lab's
commands are in `docs/cli.md`, "Measuring".

Every number here comes from the bench fleet (spin-stack/ci: AWS c8id.4xlarge, Xeon 6975P-C,
nested KVM, scratch on local NVMe) in spin's shape, as p50 or p50/p95 in ms, unless a row says
otherwise. An interval is the bootstrap 95% interval of a Δp50.

## Where the two numbers stand

| Number | Probe | p50/p95 | Over 100 ms | Run |
|---|---|---|---|---|
| Cold boot to the first SSH login | `ssh-first`: from before QEMU's exec to `true` run over SSH | 456/477 | +356 | report 44811347183, v20261120.04 |
| The same, QEMU started ahead | `prestart`, `prestarted boot`: from the request | 428/440 | +328 | the same |
| A checkpoint restored to the first login | `restore-first`, `evicted`: the state read from NVMe, out of the page cache | 108/111 | +8 | the same |
| The same, QEMU started ahead | `prestart`, `restored into prestarted` | 83.2/87.1 | under | the same |

These are the release's numbers, booted interleaved with v20261120.03 (`docs/releases.md`). Two
runs are two machines, so compare a new number only with one booted in the same run (`docs/measuring.md`,
rule 3).

**The restore is under 100 ms when QEMU is started ahead, and the cold boot is not close.** A
cold boot cannot reach 100 ms by userspace work alone: QEMU, the firmware and the kernel take
~140 ms before PID 1 runs (budget below). Without systemd, sshd accepted at 189 ms in spin's shape
(lab 44750322384, 2026-11-20). Every phase before PID 1 has to shrink as well.

### What spin runs, and what the probes boot

The probes boot and log in as spin does, unless a run asks otherwise (docs/cli.md, "Measuring"):

- **The shape.** spin (spin-stack/spin, `internal/runner/vmm/vmm.go`) boots 1 vCPU and 128 MiB,
  with ceilings of 16 vCPUs and 8 GiB. Its hotplug controller has 16 targets, 4 of them for disks
  attached while it runs and the rest for the extensions it boots with
  (`internal/runner/qemu/spec.go`, `controllerTargets`, since spin's c0ffbc0 of 2026-11-20). The
  probes boot 16 too. Against the 4 they booted before, 16 read within noise: first login −3.8
  [−11.1, +4.5] (lab 44847364468, 2026-11-21), so the budget's rows taken with 4 stand.
- **The memory.** spin sizes every workspace through virtio-mem right after it starts, to 2048 MiB
  unless the workspace asks for more (`installconfig.MinMemoryMB`, the least and the default). The
  probes plug 2048 (`plugged=2048`). A guest left at 128 MiB runs its OOM killer through every boot
  (lab 44784196287).
- **The login.** spin turns off systemd's vsock sshd (`systemd.ssh_auto=0`). It reaches a
  long-running `ssh.service` as root through its supervisor's forwarder to 127.0.0.1:22
  (`login=spin`). The image's own vsock lane is `login=lane`.
- **Checkpoints.** A checkpoint is mapped-ram, saved and loaded on 8 multifd channels
  (`machine.DefaultChannels`). Before the RAM load, the resume reads the state ahead.
- **The host.** spin's host must set THP defrag to `defer`, and spin does not set it
  (`docs/memory-and-disk.md`, "What the host must set"). The probes leave the bench host's policy
  as it is, unless `restore-together` is given `defrag=`.
- **Still not spin's.** spin boots `/sbin/vminitd` from its own initrd before systemd. Its
  supervisor starts `ssh.service` when it installs the user CA. The probes boot the image's init.

Most numbers taken before 2026-11-21 used another shape (2 vCPUs, 2 GiB, no ceilings, the
"ladder" shape) and another login (the lane). The "Shape" column of each row below says which. A
row marked spin from before that date passed spin's shape options by hand (#319). Read a number
in the ladder shape as a lead in spin's shape.

### The bench fleet is nested, not metal

On a c8i.metal-48xl (the same CPU), the cold first login read 332 ms against 378-392 nested
(v20261114.02, ladder shape, lab 44537498507), so nesting costs ~50-60 ms. Most of that is VM
exits: the PCI walk takes 5.00 ms nested against 2.22 on metal, about 29 µs an exit against 13
(labs 44535161371, 44535156851). A production host that is not nested starts closer to the goal.

## Where the time goes

Each table splits one number into phases, in spin's shape, and gives each phase a source. A phase
"by difference" is two numbers of one run subtracted. A phase marked "approximate" comes from
another run or another release. The last column is one way to spend 100 ms. It is an allocation,
not a measurement: it says how far each phase has to move.

### Cold boot

| Phase | p50 | Source | How well known | For 100 ms |
|---|---|---|---|---|
| QEMU's exec to a guest ready to run | ~28 | cold 456 less the boot into a QEMU started ahead, 428 (report 44811347183) | by difference, one run | 0, started ahead |
| … of it, QMP: QEMU's main loop free to `cont` answered, 14 commands | 6.2 | `transport` `cold_qmp`, lab 44912471522 (v20261121.01); 4.5 of it QEMU waiting on spin-machine's next command | measured | |
| qboot and the bzImage's decompression, to the kernel's clock | 33.6 | `boot:firmware-stages`, lab 44787491631 (v20261120.02's kernel); ~21 of it is the decompression KASLR needs | measured, another run | 5 |
| The kernel's clock to "Freeing unused kernel image" | 79.9 | report 44811347183 | measured | 25 |
| PID 1 to sshd accepting | ~255 | sshd accepts 396 (lab 44841266997) less the three rows above | by difference, across runs | 35 |
| sshd's accept to the first login | 68.4 | `ssh-first` `accept_to_login`, lab 44841266997 | measured | 35 |
| … of it, what any login costs (a second login) | 35.9 | `ssh-first` `second_login`, the same | measured | |
| … of a login, vsock: 10 round trips of 57.5 µs through the forwarder | 0.6 | `transport` `login_turns`, `forward_rtt`, lab 44912471522 | measured | |
| **Total** | **465** | lab 44841266997, v20261120.04 (456 in the report's run) | | **100** |

**PID 1 to sshd is more than half the cold boot.** On one vCPU, everything systemd starts before
sshd accepts is on the login's path. The groups in "What the machine carries" show how much
masks, generators and the coldplug filter took off it. The no-systemd floor accepted at 189 ms in
spin's shape. Before PID 1, ~140 ms of QEMU, firmware and kernel remain, ~113 of them with QEMU
started ahead. Each phase from the first row to the fourth has to shrink to reach 100 ms.

### Restore, QEMU started ahead

| Phase | p50 | Source | How well known | For 100 ms |
|---|---|---|---|---|
| spin-machine's exec and its dial | 2.3 | `split` (a probe on branch `restore-split-lab`, not on main), lab 44809015139 (v20261120.03) | measured, one run | 1 |
| QMP round trips to `migrate-incoming` (17 of them) | 6.7 | `transport` `restore_qmp`, lab 44912471522 (v20261121.01); 5.2 of it QEMU waiting on spin-machine. The read-ahead runs beside it | measured, one run | 3 |
| The RAM load (8 channels, 181 MiB of data, evicted) | 38.8 before the read-ahead; ~27 after it | the same; the read-ahead −11.9 to running (lab 44809512339) | measured, then by difference | 25 |
| Device state and `cont` | 4.5 | the same | measured, one run | 3 |
| … after it, the guest running to the restore's last answer (`remove-fd`, `query-status`) | 2.7 | `transport` `restore_after`, lab 44912471522 | measured, one run | |
| Running to the first login | ~35 | 83.2 less running (~47); laptop: the connection ~5, to "Accepted publickey" ~22, the session ~4 | by difference; split on a laptop | 35 |
| **Total** | **83.2** | report 44811347183 | | **67** |

A restore that starts QEMU itself adds ~25 ms (108 against 83.2). From the page cache, the RAM
load is ~10 ms instead of ~50 on a laptop, so the evicted load is the disk. Eight restores let go
at once read ~190-240 ms each under `defer`, against ~100 alone ("Open experiments", the burst).

## What the machine carries

This table lists every change the tree makes for boot time, with what it earns in spin's shape.
It is the current state. `docs/log.md` tells how each change came in. A row's evidence is one of
these:

- *measured*: its own arm, in spin's shape;
- *group*: an arm that took back its group (below);
- *ladder*, *lane*, *runner* or *laptop*: measured in another shape, on another login, on the lab
  runner of 2026-09 (Ryzen 9 5900X), or as a lead;
- *mechanism*: what it removes is visible, but no goal number has timed it;
- *unmeasured*.

A row that is not *measured* or *group* is a candidate for deletion, which rule 5 of
`docs/measuring.md` decides.
Functional rows (security, correctness, a consumer's contract) are listed so that nobody takes
them for boot time.

| Item | Where | Earns (Δp50 of taking it back) | Evidence | Run |
|---|---|---|---|---|
| Kernel patches 0001-0005, 0007, 0008, 0010 | kernel/patches | cold +67.5 [+62.5, +72.4], with 0006, 0009 and 0011 (since deleted, worth nothing) | group A | lab 44818953987 |
| `BLK_DEV_LOOP_MIN_COUNT=0`, `SERIAL_8250_RUNTIME_UARTS=1` | kernel/config-7.3-x86_64 | cold +5.3 [−3.2, +15.0]: inside the noise, but the upper bound does not rule out its ~2-4 ms ceiling | measured, 40 a side | lab 44839538785 |
| `KERNEL_LZ4` | kernel/config-7.3-x86_64 | the alternatives lost: an uncompressed payload +9 ms in copies (laptop) | laptop | #326 |
| `VT=n`, `X86_MSR=n`, `X86_CPUID=n` | kernel/config-7.3-x86_64 | function: dead code, less surface | - | kernel/Dockerfile |
| qboot instead of SeaBIOS | qemu, `machine.DefaultBIOS` | cold +68.5 [+62.7, +71.7] | group F | lab 44818953987 |
| QEMU 0001 (device creation in one memory transaction) with qboot's `pam.patch` | qemu/patches, qemu/qboot | cold +11.5 [+6.7, +20.2] together; neither is measured alone | group B | lab 44818953987 |
| QEMU 0002, virtio-scsi `max_target`, which spin-machine passes | qemu/patches, machine | cold −8.9 [−15.5, −0.9] when it came in | measured | lab 44788216590 |
| qboot `no-pci-walk.patch` | qemu/qboot | 5.0 ms nested in `firmware-stages`, not on a goal number | mechanism | lab 44535161371 |
| qboot `mtrr.patch`, `write-pointer.patch` | qemu/qboot | function: write-back mappings, vmgenid | - | - |
| `hpet=off` | machine | nothing on record: a timer the guest would enumerate and then not use | unmeasured | - |
| ICH9 `sata=off`, `smbus=off` | machine | kernel to init 62.3 → 60.6 (09-08) | laptop, 400 boots | `fa1803f` |
| Hotplug disks on virtio-scsi, not PCIe root ports | machine | boot −31, attach 135 → 1.5 (09-29) | runner | #25 |
| Every machine started held, so QEMU can start ahead | machine | the "QEMU started ahead" rows: ~28 ms cold, ~25 restored | measured | report 44811347183 |
| Checkpoints on 8 multifd channels | machine | restored into a QEMU started ahead 150 → 103 against 1 channel; 16 channels lose to 8 when 4 restore at once, +3.2 [+1.1, +5.8] | measured | labs 44789688221, 44792429636 |
| The resume reads the state ahead | machine | restored into a QEMU started ahead −12.9 [−14.7, −9.8] | measured | lab 44809512339 |
| `pci=lastbus=0` | machine/cmdline.go | kernel to init 87.6 → 51.1 (2026-09): 8192 configuration reads, each an exit | runner, mechanism | log.md, 09 |
| `no_timer_check`, `tsc=reliable` | machine/cmdline.go | nothing on record (09-07) | unmeasured | - |
| `TERM=dumb` | machine/cmdline.go | 687 → 19 ms to systemd's first line (09-10), while systemd queries the console | laptop, mechanism | `b5f361f` |
| `rootfstype=ext4` | machine/cmdline.go | lead (11-19) | laptop | - |
| `quiet`, `loglevel=3`, `panic=1`, `net.ifnames=0`, `biosdevname=0`, cgroup v2, `memhp_default_state` | machine/cmdline.go | console, function | - | - |
| systemd built from Ubuntu's source with the `stage1` profile | image/systemd | first login −47, −38, −47: no BPF verifier in PID 1 | ladder | labs 44531842773, 44537549305, 44538877783 |
| systemd's tools linked static | image/systemd | −10 and −1, inside the noise | ladder | the same |
| SELinux off, `pcre2` on | image/systemd | function | - | - |
| `-O ^orphan_file` | image/build.sh | lead (11-19) | laptop | - |
| `lazy_itable_init=0` | image/build.sh | function: no 16 MiB of zeros in every overlay | - | - |
| Masks of the units of `ConditionNeedsUpdate` (ldconfig, sysusers, hwdb-update, journal-catalog-update, update-done) | optimize-systemd.sh | load-bearing: without `/etc/.updated` they rebuild hwdb.bin and the linker cache at every boot, +260 ms | laptop, 2 boots | log.md, 11-21 |
| 16 more unit masks (journal-flush, fsck-root, modules-load, pstore, random-seed, four modprobe@, …) | optimize-systemd.sh | cold +57.5 [+49.0, +63.0] | group C-masks | lab 44818953987 |
| 8 generators disabled, 3 tmpfiles rules masked | optimize-systemd.sh | cold +16.7 [+11.6, +23.3] | group C-generators | lab 44818953987 |
| udev's coldplug filtered (`10-coldplug.conf`), `cpu` kept for hot-added cores | image | cold +14.9 [+8.2, +19.8]; restored +463.7, a guest saved with its coldplug still running | group C-coldplug | lab 44818953987 |
| ssh.socket, ssh.service and sshd-keygen without default dependencies; logind and dbus-broker without PrivateTmp; udev at `StartupCPUWeight=1` | image drop-ins | −59.4 [−65.9, −56.2] with spin's forwarder early as well; the image's half alone −17.4 | measured | labs 44805051310, 44807457606 |
| logind, its Varlink socket, `user-runtime-dir@`, `user@` without default dependencies; `dbus.socket` without sysinit | image drop-ins, configure-system.sh | lane 478 → 402 (11-13) | lane | lab 43518915618 |
| Sessions of class `user-early-light` | configure-system.sh | lane 472 vs 436 | lane, mechanism: a root session would start `user@0` | lab 43518044150 |
| Host keys made from the boot's first dispatch | spin-machine-sshd-keygen.service | lane 489 → 468 | lane | lab 43483372488 |
| MOTD and `pam_motd` off; `mlkem768x25519-sha256` alone | configure-system.sh | −12 on the first login; 104 → 40 a login against sntrup761 | ladder, mechanism: a root login runs the same PAM and kex | #240, lab 43479854381 |
| timedated, firstboot, machine-id-commit masked; volatile journald; the timeouts; docker and containerd by socket; `nsswitch` | optimize-systemd.sh | function: time from the host, no first-boot prompt, no identity | - | - |
| `10-seats.conf`, the getty's TERM, `/tmp`'s size | image drop-ins | function | - | - |
| The pressure probe's sender, after multi-user.target, at the lowest priority | spin-machine-pressure.service | function (docs/machine.md, "Pressure"); adding it: first login +0.5 [−2.6, +6.7], restored +0.2 [−3.0, +2.7] | measured | labs 44921602512, 44921841077 |

**What only the lane uses.** spin's path earns nothing from `sshd-vsock.socket.d/10-lane.conf`,
`sshd@.service.d/10-lane.conf`, `sshd@.service.d/keygen.conf` or the masked
`systemd-nologin.conf`. The probes still offer `login=lane`, and the no-systemd floor uses it.
Whether the image keeps a vsock login of its own is the maintainer's decision ("Open questions").

**What spin does, not the image.** spin no longer lingers its user (cold −36.1 [−45.8, −23.6],
labs 44785179901, 44785565313). Its supervisor's forwarder order is the proposal in "Open
experiments".

### How the groups were decided

The groups were decided by one `prestart` run in spin's shape, 20 boots each, against
v20261120.04 as published (lab 44818953987). The base read 453/464 cold, 124/144 restored and
93.0/120 restored into a QEMU started ahead. Each cell is the Δp50 of taking the group back.

| Arm | Takes back | Tree | Cold | Restored | Into prestarted |
|---|---|---|---|---|---|
| A | every kernel patch | `--kernel run:44812049304` | +67.5 [+62.5, +72.4] | +8.1 [−8.4, +20.1] | +12.1 [−6.5, +21.2] |
| A1 | 0006, 0009, 0011 | `--kernel run:44812044088` | +1.2 [−4.5, +8.3]; at 40 boots −2.5 [−8.4, +1.8] | −5.2 | +1.3 |
| A2 | `DEFERRED_STRUCT_PAGE_INIT` | `--kernel run:44837940315` (lab 44839538785, 40 boots) | −2.5 [−9.3, +3.8] | +1.1 | −4.0 |
| A3 | the loop and 8250 counts | `--kernel run:44837906170` (lab 44839538785, 40 boots) | +5.3 [−3.2, +15.0] | +1.7 | −4.6 |
| B | QEMU 0001, qboot's `pam.patch` | `--qemu run:44812062232` | +11.5 [+6.7, +20.2] | +4.4 | +0.9 |
| C-masks | 16 unit masks | `--edit carry-masks` | +57.5 [+49.0, +63.0] | −0.7 | +0.1 |
| C-generators | 8 generators, 3 tmpfiles rules | `--edit carry-generators` | +16.7 [+11.6, +23.3] | −5.3 | −1.3 |
| C-logging | PID 1's quiet logging, the printk sysctl | `--edit carry-logging` | +2.4 [−2.3, +7.8]; at 40 boots −1.8 [−7.7, +3.2] | −4.0 | +0.6 |
| C-coldplug | the coldplug filter | `--edit carry-coldplug` | +14.9 [+8.2, +19.8] | +463.7 | +358.0 |
| E | `rcu_expedited`, `driver_async_probe` | `--append 'driver_async_probe= rcupdate.rcu_expedited=0'` | −1.4 [−4.9, +3.1] | +1.4 | +1.7 |
| F | qboot, for SeaBIOS | `--boot --bios=bios-256k.bin` | +68.5 [+62.7, +71.7] | −1.9 | +7.4 |

A1, A2, C-logging and E were deleted (#345, #356, #354, #352). The rest earn what the cold column
says. On one vCPU, every unit, generator and exit before the login is on the login's path. The
`carry-*` edits stay in `internal/lab`, so the ablation can run again when the shape, the kernel or
systemd moves. An edit checks what it undoes, so an edit whose item was deleted fails on later
releases.

## Measured and rejected

An idea already here needs a new reason, not a new run. The tables group ideas by the part they
touch, oldest first within a group. The "Shape" column says which shape a row was measured in,
and a row measured in another shape than spin's has such a reason.

### Kernel and its command line

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| Trim the 8250 driver | 154 µs | runner | `c03ed89` | 09-10 |
| Modular kernel | 0.57 ms; breaks "a release is one machine" | runner | `1a8e25c` | 09-10 |
| ACPI params (acpi=noirq, pnpacpi=off, …) | 69.9 vs 68.6 ms, slower | runner | `64d2901` | 09-10 |
| initcall_blacklist ksm/kcompactd | 0; event_trace's 6 ms is the profiler | runner | `64d2901` | 09-10 |
| `CONFIG_XFRM=n` | moves BTF's 9 ms first parse elsewhere | runner | `e285dec` | 09-10 |
| A generous vCPU ceiling | maxcpus=16 +5 ms, 64 +9 ms of kernel; the ceiling stays (invariant) | runner | `f32d527` | 09-10 |
| MICROVM | no ACPI CPU hotplug | - | `b151186` | 09-10 |
| `CONFIG_VT=n` for speed | 0 to login; 0.31 ms of kernel | runner | `e2f6b8f`, `9b5f0dd` | 09-26 |
| KSM and THP off | −0.50 ms | runner | `cda04b1` | 09-26 |
| COMPACTION off | impossible: virtio-mem needs CONTIG_ALLOC | - | `cda04b1` | 09-26 |
| erofs removed for speed | noise (100.46 vs 102.22) | runner | `5a3d509` | 09-26 |
| Smaller TCP hash tables | moved nothing | runner | `dae6a14`, #48 | 09-26 |
| ZRAM | the runaway killed later (20 vs 14 s), worst login 3.8 s | runner | kernel/Dockerfile, `430d2b3` | 09-28 |
| `pci=noearly` | the guest does not boot | - | #240 | 11-12 |
| A configuration-space snapshot over fw_cfg (QEMU and kernel) | 209 vs 211 with 0005 | ladder | lab 43514884598 | 11-13 |
| `scsi_mod.scan=manual` | kernel −9.3, but since Linux 5.10 a disk added at run time, at boot, or under `-S` never appears, silently. `max_target` took its place | spin | lab 44570773103; `drivers/scsi/virtio_scsi.c`, `scsi_scan.c` | 11-19 |
| `CONFIG_SCSI_SCAN_ASYNC` | −2.6 [−8.9, +6.8]: the scan is waited for before "Freeing", and `max_target` leaves it little | spin | #325, lab 44788216590 | 11-20 |
| The decompressor without `X86_VERBOSE_BOOTUP` | to the kernel's clock 35.1 → 32.4 ms, under the 3 ms set before the run | spin | #328, lab 44787878809 | 11-20 |
| An uncompressed bzImage payload, KASLR kept | to the kernel's clock −3 ms, but +3.3 in qboot's copy and +6-7 in QEMU's start | laptop | #326 | 11-20 |
| The bzImage's decompression made cheaper, in general | ~21 ms before the kernel's clock with the bzImage against PVH; most of it is the copy and relocation KASLR needs, and KASLR stays. No candidate left | spin | lab 44787491631 | 11-20 |
| `driver_async_probe` for rng, balloon and vsock; `rcupdate.rcu_expedited=1` (carried since 11-13 and 09-07) | taken back −1.4 [−4.9, +3.1]: one vCPU has no second CPU to probe on. Deleted | spin | lab 44818953987, #352 | 11-21 |
| Kernel 0006 (BTF parsed in the background), 0009 (a booting AP polled by local_clock()), 0011 (queue_select written only on a change) | taken back −2.5 [−8.4, +1.8] at 40 boots. Deleted | spin | labs 44818953987, 44822157367, #345 | 11-21 |
| `DEFERRED_STRUCT_PAGE_INIT` (carried since 11-13, for 2 GiB at boot) | taken back −2.5 [−9.3, +3.8] at 40 boots: spin boots 128 MiB, which the kernel does not defer, and plugged memory is never deferred. Deleted | spin | lab 44839538785, #356 | 11-21 |

### Firmware, QEMU and the machine

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| `usb=off`, `vmport=off`, `smm=off`, `i8042=off` | 63.0/62.6/62.3/62.5 vs 62.9 ms | laptop | `fa1803f` | 09-08 |
| A named CPU model instead of `host`, for memories that resume on any host (`--cpu baseline`) | the newest model a nested c8id runs is Cascadelake-Server-v5, not SapphireRapids or GraniteRapids; cold first login +27.2 [+13.0, +30.8], sshd accepting +23.1 [+9.7, +28.5], warm login and RSS unmoved; the cause is not measured. `host` with a pool's common features is the next candidate (spin-stack/spin#1417) | spin | lab 44855890737 | 11-21 |
| Less guest RAM (512 MiB) | 1241 vs 1256 ms, nothing | runner | `80f984b` | 09-10 |
| `cache=none` on disks | breaks the shared base's caching, fails on tmpfs | - | `8b7391a` | 09-07 |
| Overlay O_DIRECT | small reads 1490 vs 530 ms | runner | docs/memory-and-disk.md | 10-01 |
| Board init wrapped in one memory transaction | the guest prints nothing | runner | `f10383b`, lab 36662499370 | 09-30 |
| Base on virtio-pmem and DAX | ~155 MB less per VM, but a side channel | runner | #63, #65 | 10-01 |
| Remove virtio-rng | −13.6 ms to the console prompt, never on a goal number; what seeds the RNG without it is open ("Open questions") | ladder | lab 43321809471 | 11-12 |
| QEMU copies the kernel's ELF segments on threads | laptop QEMU start 30.9 → 21.6; nested it adds nothing | ladder | labs 44580080420, 44581350077, #290 | 11-19 |
| Lower the vCPU ceiling, drop the balloon or ACPI | not tried: an invariant | - | AGENTS.md | 11-13 |

### systemd and the image

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| Mask udev | saves 0, and boots never reach a login (10 s ttyS0 timeout) | runner | `4cbd229`, `80f984b` | 09-10 |
| agetty tweaks (TTYReset, --noissue, --delay 0, …) | within the noise of agetty's sleep(1) | runner | `80f984b` | 09-10 |
| chrony or another time daemon | 10 ms; cannot know it was restored | runner | `c7af4d3` | 09-10 |
| systemd-timesyncd | SNTP only, no refclock | - | `c7af4d3` | 09-10 |
| 19 udev rules masked; loop and 8250 counts as command-line parameters | 0 to the console prompt | runner | `e2f6b8f` | 09-26 |
| A console unit instead of serial-getty | +340 ms until TERM=dumb, then equal | runner | `c6e48de`, `7b08e5a` | 09-26 |
| Fewer unit files | 300 extra: 236 vs 246 | runner | `9fab3f1` | 09-26 |
| systemd-oomd | the kernel always beat it | runner | `337c08d` | 09-28 |
| CPUWeight/IOWeight/MemoryLow on ssh.service | no change | runner | `f7cd1ab` | 09-28 |
| Getty without BindsTo dev-ttyS0 | 342 vs 345 | ladder | lab 43303190733 | 11-12 |
| Keygen wanted by multi-user.target | 518 vs 514 | ladder | lab 43312419007 | 11-12 |
| Mask sshd-socket- and run-generators | login 489, no change | ladder | lab 43483372488 | 11-13 |
| Mask 11 always-failing units | 484, no change | ladder | lab 43483372488 | 11-13 |
| Mask `modprobe@drm` (logind is ordered after it) | 327/345 vs 328/337. It is masked all the same, since 09-07, one of the 16 masks of group C-masks | ladder | lab 44558035927 | 11-19 |
| Mask `systemd-udev-load-credentials` | −8 then −6 ms, p95 unchanged: not separable at 20 boots | ladder | labs 44537549305, 44538877783 | 11-19 |
| systemd with `-Db_lto=true` | Ubuntu already passes `-flto=auto`: the same packages | - | image/systemd/Dockerfile | 11-19 |
| Masks of 15 units and a generator the image does not have | they masked nothing. Deleted without a run; image/build.sh refuses such a mask | - | #352 | 11-21 |
| PID 1's `LogLevel=err`, `LogTarget=journal`, `ShowStatus=no`, `StatusUnitFormat=name`; `kernel.printk = 3 3 3 3` | taken back −1.8 [−7.7, +3.2] at 40 boots; the console stays quiet through the command line. Deleted | spin | labs 44818953987, 44822157367, #354 | 11-21 |

### The login

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| sntrup761 (OpenSSH's default kex) | 104 vs 40 ms a login | ladder | #245, lab 43479854381 | 11-13 |
| The vsock socket and sshd@ without default dependencies | no sooner; dropped 7 of 22 first connections. Carried since with the lane's other drop-ins (`sshd-vsock.socket.d`, `sshd@.service.d`), which the lane kept together (lab 43518915618) | laptop | `c3c0fd1` | 11-13 |
| CPUWeight/IOWeight/Nice on the first login's lane | −2 ms over the filtered coldplug; second login 63 vs 45 | lane | lab 43518915618 | 11-13 |
| One sshd for every connection (vsock `Accept=no`, `sshd -D` early) | first login 382 vs 381; second 40 vs 48 | lane | lab 43524721490 | 11-13 |
| A login without pam_systemd (a bound) | 380 vs 436, but no session and no XDG_RUNTIME_DIR | lane | lab 43518044150 | 11-13 |
| logind's Varlink socket without default dependencies | accept to login 131 → 120, first login inside the noise; kept with the lane's other drop-ins | ladder | labs 44558035927, 44558053261 | 11-19 |

### The restore and checkpoints

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| mapped-ram with multifd ×4, 11-13 | not comparable by its own run's log; channels were measured again on 11-20 and kept | ladder | #246, lab 43483852339 | 11-13 |
| Fast snapshot load (mapped-ram and postcopy-ram on a `file:` URI) as the restore | running at 38 ms, but the login at 293/325 instead of 152/161: every page touched is a fault served by one read. `LoadOptions.Fast` stays for large states | ladder | lab 44585840498 | 11-19 |
| Base memory in a shared file, left out of the checkpoint (`x-ignore-shared`) | state 0.3 MiB, save 15 ms, but neither goal number improves: cold 1089 ms on disk, 709-719 on tmpfs | ladder | labs 44663272360, 44664989139, #306 | 11-20 |
| 16 multifd channels as the default | −10.1 alone, but +3.2 [+1.1, +5.8] against 8 when 4 restore at once | spin | lab 44792429636 | 11-20 |
| `direct-io` (O_DIRECT) on the channels | within the noise evicted, +45 ms cached | laptop | log.md, 11-21 | 11-21 |

### QMP and vsock

Both transports were measured in spin's shape on the bench, 20 boots, with the `transport` probe
(lab 44912471522, v20261121.01, 2026-11-21). Each ceiling is what the goal number would gain if the
part cost nothing. None is above the noise (±7-9 ms cold, ±13 restored at 20 boots), so none was
built.

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| Faster QMP in general | the cold boot waits on 14 commands, 6.2 ms from QEMU's main loop being free to `cont` answered (1.3% of 470). A restore waits on 17 before `migrate-incoming`, 6.7 ms, and 2.7 after the guest runs. The commands' own run is ~1.2 ms of each path. The rest is round trips: query-status alone is 131 µs from the client, 18 µs in QEMU. spin-machine's start and dial are off the path: its first command waits 17 ms (cold) and 19.5 (restored) for QEMU | spin | lab 44912471522 | 11-21 |
| Pipelining the bind (every command sent before the first answer) | ceiling: QEMU's wait on the client, 4.5 ms cold and 5.2 restored, under the noise. The read-ahead overlaps the restore's half, so its share is smaller | spin | the same | 11-21 |
| Fewer round trips (no `query-status` before the bind, places read from the Spec instead of `query-block` and `qom-get`, `Last(RESUME)` for the `query-status` after a load) | ceiling ~1.3 ms cold, ~1.9 restored | spin | the same, the detail by command | 11-21 |
| Smaller answers | nothing large is on either path: `query-block` runs in 0.12 ms. The record's 125 KB answers left the restore's clock with `Resume(here)` | spin | the same | 11-21 |
| QMP over vsock, SEQPACKET, ivshmem, virtio-serial, gRPC or protobuf framing | QMP does not ride vsock: it is a Unix socket between the host and QEMU. vsock carries the login, and a login is 10 round trips of 47.8 µs (57.5 through the forwarder), 0.6 ms of a 32.8 ms warm login. Every vsock change shares that ceiling | spin | the same | 11-21 |
| `kvm.halt_poll_ns`, CPU pinning, for vsock | bounded by the same 0.6 ms a login. What they do to the boot's other wake-ups is a different question, not measured here | spin | the same | 11-21 |

### Memory and the host

| Idea | Result | Shape | Source | Date |
|---|---|---|---|---|
| Lower `memhp_default_state` (unplug policies) | kept `online` | runner | `2903d8c`, #48 | 09-30 |
| DAMON_RECLAIM, MGLRU eviction | slower, or not selective | runner | docs/memory-and-disk.md | 10-01 |
| `page_reporting_order` 0 or 3 | ~100 MB transient vs ~180 MB of huge pages lost | runner | docs/memory-and-disk.md, #83 | 10-01 |
| Prefetch the boot's files | 47 ms to win 24 | runner | run 36803912357 | 10-01 |
| A second limit (`memory.high`) | `memory.max` suffices | - | docs/memory-and-disk.md | 10-01 |
| KSM, free page hinting, virtio-fs DAX | security, correctness, unsupported | - | docs/memory-and-disk.md | 10-01 |
| A burst of restores bound by host CPU0 | no: busy time, interrupts and QEMU's threads spread over all 16 CPUs, and irqbalance runs | spin | lab 44817486118 | 11-21 |

## Open experiments

The rows are ranked by ceiling. A row stays here until it is decided, and then moves.

| Experiment | Number | Phase, p50, ceiling | Decided by | State |
|---|---|---|---|---|
| A burst of restores: 8 at once read ~190-240 ms each under `defer`, against ~100 alone | restore, as a host sees it; spin restores concurrently with no host-wide limit | the burst's ~100-140 ms over one restore alone | `restore-together`, `together=1 8`, evicted, with the change against without it | open. The host's CPUs are 72-78% busy in the burst, and direct reclaim runs as the states fill the page cache (lab 44817486118). Next candidates: `vm.watermark_scale_factor`, and the CPU one restore costs |
| spin's forwarder started without default dependencies, as the image starts ssh.service | cold | −51 more than the image's half alone (−68.5 against −17.4, lab 44807457606) | `prestart` with `--edit forwarder-early`, cold and both restore rows | blocked: restore p95 tails of 430-545 ms with it (lab 44808050222), unexplained. A proposal for spin's supervisor once explained |
| A login without OpenSSH's exec per connection and PAM | both | a warm login, ~25-43 ms, is in both numbers | a bound first: `prestart` with the login replaced | lead (log.md, 11-20, "spin's login, spin's memory"). spin's path already runs one sshd, and PAM and pam_systemd are what spin's sessions are made of |
| The budget's PID 1 to sshd: unit loading | cold | ~32 ms of loading 192 units, ~21 ms of CPU (11-19, image/systemd/experiments/pid1-marks.patch) | `ssh-first`, an arm per unit set reached at boot | open: it shrinks only with fewer units reached at boot |
| Carried items never measured alone: kernel patches 0005, 0007, 0008 and 0010 each (group A is +67.5 together); `pci=lastbus=0` (2026-09, before patches 0004 and 0010); logind's early varlink socket (measured on 2 vCPUs); QEMU 0001 and qboot's `pam.patch` apart; `no_timer_check`, `tsc=reliable`; `hpet=off`; systemd's static tools; `-O ^orphan_file`; `rootfstype=ext4`; the lane's drop-ins on spin's path | cold | each ≤ a few ms; B together +11.5 | rule 5, an arm each, 40 boots | open. The static tools and `orphan_file` need an image build (`--systemd` builds only an experiment patch) |
| What functional items cost, last measured on another machine: BPF JIT and BTF (+21.4 ms to "Freeing", 09-07, a vmlinux through PVH, before the ftrace patches), lockdown at confidentiality (nothing told from noise, 10-01, a host at load 6-10) | cold | a few ms each, unknown in spin's shape | `ssh-first` with a `--kernel` built without each, 20 boots | open. They stay for function, so a number says what they cost and decides nothing |
| The no-systemd floor, each release | a bound | how much of the cold number is systemd's to give back: 445 → 240 in spin's shape | `ssh-first` on the release with `--edit floor --append init=/usr/local/sbin/spin-floor` and `login=lane` | last measured 11-20 (lab 44750322384), before the early login |
| The restore with more than 2 GiB of RAM | restore | unmeasured (#242's checklist) | `restore-first` with `plugged` above 2048 | open |

The floor is a bound and never a candidate: systemd stays (AGENTS.md, "What must not break").

## Open questions

No lab run answers these questions.

- **Does the image keep a vsock login of its own?** spin does not use the lane (see "What the
  machine carries"). This is the maintainer's decision.
- **Is `10-seats.conf` still needed?** The control that should fail passes on systemd 259 (#240).
- **What seeds the guest's RNG without virtio-rng?** It has to be answered before the removal is
  measured.
- **Host keys in checkpoints.** A guest checkpointed after boot carries host keys, so the restorer
  must re-key (`image/README.md`, #248). spin's vminitd writes a host key of its own at every boot
  it hands off (`internal/guest/vminit/handoff/handoff.go`, `HostKeyPath`, spin's d4f62bd). A
  restored guest runs no vminitd, so whether a restore gets a new key is still to confirm in spin.
- **musl under long-running load.** No run has measured it (`31a57e7`).
- **`page_reporting_order`.** Revisit only if memory per host becomes the limit
  (`docs/memory-and-disk.md`).
- **`machine.Type` off pc-q35-11.1.** QEMU is 11.2 and the machine is still pc-q35-11.1
  (`machine/shape.go`, `internal/build/machinetype.go`). It is a change of its own.
- **The kernel CVE judge.** 9 CVEs apply with a fix later in 7.3, and 1 has no fix (`0ce27ad`,
  against 7.3.1; the kernel is 7.3.5 on 2026-11-20).
- **The min_ttl_ms evidence disagrees.** `47aa681` and the LRU_GEN comment in `kernel/Dockerfile`
  say different things (docs/log.md, 09-28).
- **The guest clock on 09-30.** `1d5e7f5` blames chrony, which had left the image on 09-10
  (`c7af4d3`). The fix (time by `/proc/uptime`) stands either way.
