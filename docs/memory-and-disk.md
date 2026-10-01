# What a guest's memory and disk cost its host

A measurement record: what this machine does with a guest's page cache, its freed memory and
its disk, what was measured to decide each default, and what was tried and turned down. Every
number here was taken on construct - the KVM runner, Spin OS metal, two WD Red SN700 NVMe
disks - between 2026-09-30 and 2026-10-01, through `.github/workflows/lab.yml` and the probes
in `boot/` (`boot/pagecache_test.go`, `boot/report_test.go`). A number with no run beside it
came from the run named at the top of its section.

Decisions that span spin as well as this machine - the guest's reclaim service, the qcow2
layer format, the host's dirty-page limit - are recorded in spin's
`docs/decisions/ADR-0028-sharing-a-hosts-memory-and-disk.md`; this file is what the machine
contributes and how it was measured.

## The defaults, and why

| Default | Why (measured) | Where |
|---|---|---|
| The overlay is opened writeback; the host keeps a copy of what the guest wrote | Without the host's copy, 16 384 small files read in random order take 1490 ms instead of 530 | spin's launcher |
| `fstrim.timer` hourly in the image, up to 10 minutes' random delay | A trim takes the overlay from 657 to 21 MB on disk and from 657 to 20 MB in the host's cache; the distribution's weekly timer returned it days later | #76, `image/mkosi.extra/usr/local/lib/spin-base/configure-system.sh` |
| QEMU carries upstream's discard accounting for virtio-blk | Without it `query-blockstats` reports 0 unmap operations however much a guest trims | #79, `qemu/patches/0002-hw-virtio-blk-account-discard-operations.patch` |
| qboot programs the MTRRs, write-back by default | Without MTRRs the guest maps device memory uncached-minus: a virtio-pmem region read at 8.3 MB/s, 1.1 GB/s with them | #75, `qemu/qboot/mtrr.patch` |
| `page_reporting_order` stays at the kernel's 9 | A lower order gives ~100 MB more back per VM, briefly, and costs ~180 MB of the guest's RAM its huge pages for good | #83, below |
| The release's boot report boots each row 20 times, KVM only | At 3 boots a row the report flagged every KVM row 5-11% slower between two releases that 20 alternating boots put within 5 ms | #81, `boot/report_test.go` |

## The host's copy of the guest's disk

With the overlay writeback, what the guest writes is also in the host's page cache. That copy
pays for itself: it is the second level of cache a small read falls through to. 2048 MB guest,
512 MB file, p50 of 5 boots (#57-#59).

| | writeback | overlay O_DIRECT |
|---|---|---|
| The overlay in the host's cache after the guest deletes the file | 657 MB | 2 MB |
| Writing 512 MB | 440 ms | 330 ms |
| Reading 512 MB again, sequentially | 90 ms | 90 ms |
| 16 384 files of 8 KiB, random order | 530 ms | 1490 ms |
| QEMU's RSS: idle, cache full, after `drop_caches` | 208 / 740 / 386 MB | 206 / 744 / 392 MB |

Firecracker, Lambda and Modal keep the host's cache on purpose for the same reason. What does
bound it is spin's `memory.max` per machine (the guest's ceiling plus 768 MiB): a
`memory.high` of 1280 MB held the overlay's share of the host's cache near 430 MB with small
reads unchanged (run 36803912357), which `memory.max` already does well enough that a second
limit was not added.

## Giving the guest's cache back

A guest keeps in its page cache what it read once, and QEMU's resident memory with it: free
page reporting hands the host only what the guest has freed. Something in the guest has to
free it. `memory.reclaim` on the guest's root cgroup does, and keeps the working set: a
working set (16 384 files of 8 KiB) read every 5 s for 40 s beside a 512 MB file read once,
then the reclaimer (run 36809694419, p50 of 5; repeated in 36813445460 and 36817108384).

| Reclaimer | QEMU's RSS | The guest's cache | The working set read again |
|---|---|---|---|
| Nothing | 905 → 892 MB | 673 MB | 120 ms |
| `memory.reclaim` 512M at the end | 910 → 394 MB | 161 MB | 150 ms |
| DAMON_RECLAIM (idle 10 s, default quotas) | 904 → 748 MB | 296 MB | 260 ms (310 on repeat) |
| MGLRU `lru_gen`: age every memcg, evict the older generations | 906 → 394 MB | 34 MB | 440 ms |

DAMON is slower and takes part of the working set; MGLRU's eviction, as driven here, takes
everything older than the last aging, working set included. A first `lru_gen` attempt did
nothing at all, because the cache is charged to the memcg of the service that read it and an
`lru_gen` command acts on one memcg, while `memory.reclaim` on the root walks the hierarchy.
spin's guest supervisor runs `memory.reclaim` (spin #214, `spin-reclaim.service`).

### What stays after the guest frees it

After the guest drops its cache, QEMU stays ~200 MB above where it booted. QEMU's own memory
is a constant ~50 MB (RSS minus the guest's RAM mapping); the rest is guest RAM the guest freed
and free page reporting has not handed back. Reporting returns free blocks of
`page_reporting_order` and up - 9, 2 MiB, by default - and what `memory.reclaim` frees is
scattered (runs 36879800428, 36881456066, 36931619739; p50 of 3).

| `page_reporting_order` | Guest RAM on the host a minute after the drop (idle: 158 MB) | QEMU CPU that minute | RAM backed by huge pages after the guest uses 768 MB again | Using 768 MB again |
|---|---|---|---|---|
| 9 (default) | 346 MB | +100 ms | 968 of 968 MB | 250 ms |
| 3 | 243 MB | +130 ms | 794 of 960 MB | 290 ms |
| 0 | 238 MB | +120 ms | 778 of 961 MB | 290 ms |

A lower order gives about 100 MB more back per VM, and only until the guest uses that memory
again; order 3 already gets all of it. In exchange about 180 MB of the guest's RAM ends up on
4 KiB pages on the host and stays there, and every later access to it pays for that. The
order stays at 9. Worth measuring again only if memory per host becomes the limit, and then
under a real workload: `boot:page-cache` with `SPIN_PAGE_CACHE_ONLY=^reclaim$` has the columns.

## Trimming: what a deleted file costs the host

`discard=unmap` reaches qcow2 end to end: a trim after a delete takes the overlay from 657 to
21 MB allocated and from 657 to 20 MB in the host's cache, in every variant (run 36815822140).
Two things made it look broken first:

- `fstrim` run right after `rm` trims nothing of the deleted file: ext4 has not committed the
  freed extents yet, and busy extents are skipped. A `sync` first is what the probe does.
- QEMU 11.1.1's virtio-blk never starts accounting for a DISCARD, so `query-blockstats`
  reported 0 unmap operations beside 1.1 GiB trimmed. Upstream fixed it in 331ce936
  (2026-09-11, in no tag yet); this tree carries it (#79), and the caches probe now fails when
  a trim goes uncounted. With it: 10-12 operations, 1123 MB (run 36884702758).

A guest trims when its `fstrim.timer` fires, which the image makes hourly (#76).

## First writes onto the base

With 64 KiB clusters, a guest's first 4 KiB write onto data in the base copies a whole cluster
into the overlay. 2000 random 4 KiB `O_DIRECT` writes into a 106 MB file of the base, p50 of 5
(run 36813023345):

| Overlay | Overlay growth, and its share of the host's cache | First pass | Second pass |
|---|---|---|---|
| 64 KiB clusters | 82 MB | 3120 ms | 3040 ms |
| 128 KiB clusters, 4 KiB subclusters (`extended_l2=on`) | 13 MB | 3090 ms | 3030 ms |

The time is one `dd` process per write and hides the copy's latency; the measured gain is space
and cache. spin makes every layer this way (spin #220).

## Device memory and the MTRRs

A guest maps device memory it asks for as write-back only where the MTRRs allow it. qboot left
them disabled ("MTRRs disabled by BIOS"), so Linux mapped such memory uncached-minus through
PAT, and every access to it went to memory uncached. Found through virtio-pmem, whose region
read at 8.3 MB/s whatever QEMU's mapping (runs 36811489671, 36814547075, 36816626039): the
region was RAM in a KVM memslot, the guest's PAT entry for it was `uncached-minus`
(run 36818224412), and with SeaBIOS - which programs the MTRRs - it read at 1.1 GB/s
(run 36818721809). It applies to any device memory a guest maps write-back (pmem, a virtio-fs
DAX window), not to RAM. qboot now programs them (#75): write-back by default, the 32-bit PCI
hole uncached; no boot cost measured (kernel and init 103.5 vs 103.1 ms over 20 boots).

## Turned down

| | What was measured | Why not |
|---|---|---|
| The base on virtio-pmem with DAX, shared by every VM | ~155 MB less private memory per VM with /usr read; one copy of the base in the host's cache; re-reading slower (270 vs 70 ms) | Pages shared between VMs are a side channel (Flush+Reload; Firecracker advises against it): where there is a risk of lateral movement, it is not done. #63 and #65 closed |
| The overlay `O_DIRECT` | The host keeps 2 MB instead of 657; a writer beside it 1.7 GB/s with p99 20 ms for its neighbour | Small reads 3x slower (1490 vs 530 ms) |
| DAMON_RECLAIM, MGLRU eviction | Above | Slower, or not selective |
| A lower `page_reporting_order` | Above | Huge pages lost for good, for a transient gain |
| KSM | - | Rowhammer-style page-deduplication attacks (Flip Feng Shui); Firecracker asks for it off; spin turns it off |
| Free page hinting | - | QEMU uses it for migration only; Firecracker warns of corruption |
| virtio-fs DAX | - | SHMEM_MAP is not merged in QEMU; Cloud Hypervisor deprecated it |
| virtio-mem together with an inflated balloon | - | Incompatible in one VM |
| Readahead in GB | - | Modal saw production latency from it |

## What others do

From a survey of other sandboxes' code and documentation (VS: verified in code, D: documented,
B: blog), before measuring:

- **Reclaim in the guest, then free page reporting** is what everyone does. E2B freezes the
  user cgroup and runs `fstrim`, `sync`, `drop_caches` and `compact_memory` before a pause
  ([reclaim.go](https://github.com/e2b-dev/infra/blob/23f7a0f89dc3/packages/orchestrator/pkg/sandbox/reclaim.go), VS);
  Meta's Senpai/TMO paces reclaim by PSI ([TMO](https://engineering.fb.com/2022/06/20/data-infrastructure/transparent-memory-offloading-more-memory-at-a-fraction-of-the-cost-and-power/), D);
  ChromeOS inflates the balloon by `guest cache - target` under host pressure
  ([balloon_policy.cc](https://cos.googlesource.com/third_party/platform2/+/refs/heads/release-R113/vm_tools/concierge/balloon_policy.cc), VS).
- **Keep the host's cache**: Firecracker and Lambda do buffered I/O
  ([device.rs](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/devices/virtio/block/virtio/device.rs), VS;
  [ATC23](https://www.usenix.org/system/files/atc23-brooker.pdf)), bounded by cgroups and rate
  limiters ([prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md), D);
  Modal dropped FUSE direct I/O because it turns the page cache off
  ([talk](https://modal.com/blog/jono-containers-talk), B).
- **Discard to holes**: Firecracker turns TRIM into a hole punch
  ([block-discard.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/block-discard.md), D).
- **Base on pmem/DAX**: Kata ([qemu_arch_base.go](https://raw.githubusercontent.com/kata-containers/kata-containers/main/src/runtime/virtcontainers/qemu_arch_base.go), VS),
  Firecracker 1.14 ([pmem.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/pmem.md), VS) and
  RunD ([ATC22](https://www.usenix.org/system/files/atc22-li-zijun-rund.pdf)) - and Firecracker's
  own warning about sharing the file between VMs is why it is turned down here.
- **The cold boot**: SnapStart, REAP and E2B record a boot's working set and prefetch it. Here a
  cold boot reads 47 MB of the 830 MB base and costs 32 ms over a warm one; prefetching those
  files with `WILLNEED` took 47 ms and won back 24 ms (run 36803912357). Not adopted: a host
  keeps the base warm for every machine after the first.

## Measuring it again

The probes are `go test` behind the `boot/` Task targets, run on construct by
`gh workflow run lab.yml -f experiment=boot:page-cache -f vars='REPS=3; SPIN_PAGE_CACHE_ONLY=<probe>'`
(caches, reclaim), optionally with `kernel_run`/`qemu_run` and
`variant=beside|instead` for a build of a branch. The cold-boot, `memory.high`, reclaimers and
first-write probes behind the other tables were deleted once answered; they are in
`boot/pagecache_test.go`'s history. What the lab taught about itself:

- **Only construct's numbers count.** A laptop has neither the Spin OS kernel nor a quiet host.
- **One run at a time.** construct has three runners on one machine; two lab runs side by side
  measure each other. Read results by run id, never by "the newest".
- **A probe that mounts must unmount in `t.Cleanup`**: a loop mount left behind broke every
  later checkout on that runner.
- **Unix socket paths are 108 bytes**: QMP sockets live under `/tmp`, not the lab's long
  `TMPDIR`.
- **A kernel without modules has nothing for `modprobe` to load**: ask for the device
  (`/dev/nbd0`) or the feature, never the module.
- **Three boots a row is noise at the 5% scale**; compare with 20, alternated.
