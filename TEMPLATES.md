# Templates: many VMs from one frozen machine

This repository does not implement a template lifecycle. It builds the machine one runs on
and offers the arguments that lifecycle needs. This describes the contract: what to hold to,
what the guest sees afterwards, and why the machine is shaped the way it is.

For moving a VM from one host to another, see **Moving a VM to another machine** in the
README. That is a different operation with a different tradeoff, and the difference between
them is one migration capability.

## Why a template rather than a boot

A cold boot spends most of its launch on work that exists only because the machine has no
memory yet. Profiled over twelve 85 ms launches, 2026-09-26: `kernel_init_pages` — the host
zeroing pages as guest RAM is faulted in — is 18.8% of the launch, and QEMU's own code is
3.67%. On a restore the zeroing is 3.0%, because the guest's memory arrives as mapped file
pages instead of fresh anonymous ones.

The copy of the kernel goes the same way, and QEMU says so in its own source rather than
leaving it to be measured: `rom_reset` in `hw/core/loader.c` skips every ROM when the machine
is starting under `RUN_STATE_INMIGRATE`, "because we'll fill the data in during the next
incoming migration in all cases". That is ~6 ms of copying 36 MB that a restore does not do.

Measured end to end, 2 GiB of guest memory, exec to the machine running, 2026-09-26:

| | |
|---|---|
| no disks | 24 ms |
| one qcow2 overlay over the 830 MiB base image | 29 ms |
| a cold boot to a login prompt, for scale | ~227 ms |

The device state is **400 KB** per template either way, because the guest's RAM stays in the
file that backs it.

The first row is the one to be careful with, and it was the only one this document carried at
first. A machine with no disks has nothing to activate when its vCPUs start, so it skips a
cost that a real one pays — see **What activation costs** below. Measure a template with the
disks the machine will actually have.

Nothing here is faster because the VM does less. It is faster because the expensive parts of
starting a machine already happened once.

## Freezing

Boot the machine with its RAM in a file, mapped shared:

    spec.Memory.File   = "/path/to/template.mem"
    spec.Memory.Shared = true

`Shared` is what lets the freeze leave the memory where it is. Then, over QMP, in this order:

    migrate-set-capabilities  {"capabilities": [{"capability": "x-ignore-shared", "state": true}]}
    stop
    migrate                   {"uri": "file:/path/to/template.state"}
    query-migrate             → poll until "completed"

`stop` comes before `migrate` and not after. A VM that keeps running once its state has been
captured has already written to its disk, and the state no longer describes it.

`x-ignore-shared` is what makes this a template rather than a portable save: it tells QEMU to
leave RAM in the file backing it instead of writing it into the stream. Without it the state
file carries the guest's memory — 92 MB rather than 356 KB for the same 2 GiB guest — which is
the right thing when one file has to be copied to another host, and the wrong thing here.
`spin-machine save` does it without the capability for exactly that reason.

## Restoring

Start the machine with the same memory file, mapped **private**, and no state:

    spec.Memory.File    = "/path/to/template.mem"
    spec.Memory.Shared  = false          // MAP_PRIVATE, opened read-only
    spec.IncomingDefer  = true           // -incoming defer

Then, over QMP:

    migrate-set-capabilities  {"capabilities": [{"capability": "x-ignore-shared", "state": true}]}
    migrate-incoming          {"uri": "file:/path/to/template.state"}
    query-status              → poll until the status is no longer "inmigrate"
    cont

`IncomingDefer` rather than a URI on the command line, and this is the reason the field
exists: the capability has to be agreed before the first byte of state is read, and there is
no way to pass a migration capability to `exec`.

`Shared = false` is not an optimisation. Mapped private and read-only, each VM sees the
template's memory and keeps its own writes; mapped shared, every VM writes the template that
the next one restores from. Checked: after eight restores through a private mapping the
template's SHA-256 is unchanged. Restore latency is the same either way — 23.98 ms private
against 24.64 shared — so there is nothing to trade.

## What activation costs

Starting the vCPUs after an incoming migration activates the block devices, and QEMU's default
is to drop the host's page cache for every image it opened while doing it:
`block/file-posix.c`'s `raw_co_invalidate_cache` flushes the node and calls
`posix_fadvise(fd, 0, 0, POSIX_FADV_DONTNEED)`. That is right for a live migration onto a
second host over shared storage, where the source may have written the image; it is wrong
here twice over. A layer below the top is sealed and read-only, so nothing can have made its
cache stale — and it is one file that every VM on the host reads through, so dropping it is
paid by all of them and by none of the one that restored.

`machine.Spec` turns it off, on every image of a disk given as a `Disk.Chain`. Traced with
`strace -y`, a restore then issues the call against no image at all, and `cont` goes from
8.9 ms to 2.8.

Two things a caller needs to know about that:

- **Give a restored disk its chain, not a path.** A `Disk.Path` names one image, and whatever
  backing file that image's own header names is opened by QEMU implicitly. `Spec` cannot turn
  the option off for a child it cannot know exists — naming it makes QEMU open a backing file
  whether there is one or not, and it refuses when there is not. So a disk given as a path
  spares its top image and still invalidates the base image underneath it.
- **The cost is not constant, so a measurement can miss it.** `POSIX_FADV_DONTNEED` is
  best-effort: `invalidate_mapping_pages` declines to free pages that were recently touched.
  Reading the base image just before restoring — which a benchmark is likely to do — leaves it
  costing 4.8 ms and freeing nothing. Against a colder cache it does the work, and it has been
  measured at 251 ms for an 870 MB base image, which is also 870 MB taken away from every
  other VM on the host.

## Announce each disk's own size

A restored guest keeps the capacity of the *template's* disk. virtio-blk reads capacity once
and then only on a configuration-change notification, so a VM restored onto a larger image
sees the smaller one and a VM restored onto a smaller image can be told to write past the end
of it.

So after `cont`, resize each writable disk to its own size over QMP, which is what raises that
notification:

    block_resize  {"node-name": "<node>", "size": <bytes>}

`query-named-block-nodes` reports what each node actually is, which is where the size to
announce comes from. A template and the machines restored from it deliberately do not have to
agree on how big their disks are — only on how many.

## What the caller has to hold to

- **The device topology must match, and the fingerprint does not check all of it.** The number
  and kind of devices has to be the same on both sides, down to whether there is a serial
  port, because that is what migration loads state into. The failure is
  `Unknown section or instance 'serial' 0`, at load time.

  `Fingerprint` covers that topology except the disks and the NICs, which it leaves out on
  purpose — see `TestFingerprintCoversTheDevicesPresentAtRestore`. So it will not tell a
  caller that it is about to restore a one-disk template into a machine with two. Nothing is
  silent about it, which is why the exclusion is tolerable: the load refuses and names the
  section. Holding to it is the caller's, because the caller is what decides how many disks a
  machine gets.

  That exclusion was written for a template frozen from a machine with no disks at all, which
  were then hot-plugged onto each VM afterwards. A template frozen with the disk shape its
  machines will have does not need it, and the argument for leaving disks out of the identity
  is weaker in that case than the comment there claims. Changing it would invalidate every
  template in existence, so it is worth deciding deliberately rather than drifting into.
- **The capability must be set on both sides.** Freezing with it and restoring without gives
  `Capability x-ignore-shared is off, but received capability is on`, and QEMU exits.
- **The fingerprints must match.** `machine.Spec.Fingerprint` hashes the QEMU binary, the
  kernel and the initrd by content together with the machine's shape. A restore across two
  different fingerprints is undefined rather than an error. See the README on `Spec.CPU`,
  which is what decides whether a machine can be restored on another host at all.
- **The disk has to match the memory.** The frozen guest's page cache refers to blocks on
  its disk, so a restored VM needs that disk as it was when the state was captured.
- **The template file is never written.** It is the same invariant the base image has, for
  the same reason: many VMs map one file, and a write to it is silently wrong for all of
  them.

## What the guest sees

It does not know. It continues from the instruction it stopped at, with the memory, the
devices and the clock it had. Which has consequences worth planning for:

- **`dmesg` is the original boot, verbatim.** The ring buffer is guest memory, so a restored
  VM reports a boot that happened once, possibly weeks earlier, on another VM.
- **Exactly one line is new**, and it is the only signal the kernel gives:

      random: crng reseeded due to virtual machine fork

  That is the `vmgenid` device. QEMU randomises its value for every VM it starts, the kernel
  compares it against the one in the memory it woke up with, and reseeds when they differ.
  Without it, every VM restored from one template would produce the same "random" bytes and
  nothing would report a fault — which is why the device is on this machine's command line
  and not optional.
- **The clock continues from the freeze**, it does not jump to wall time. `/proc/uptime` in a
  restored VM carries on from where the template was frozen.
- **Anything provisioned before the freeze is shared by every VM restored from it.** That is
  the whole reason the base image carries no hostname, no machine-id with a value, no
  `/etc/resolv.conf`, no SSH host keys and no random seed: identity is handed to a VM after
  it is restored, never baked into what it restores from.

So a consumer deciding "is this a fresh machine or a restored one" cannot ask the guest's
uptime or its logs. The reseed line is the one marker the kernel provides for free.
