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

Measured end to end, 15 restores, 2 GiB of guest memory, exec to the machine running:
**24 ms**, against about 227 ms for a cold boot to a login prompt. The device state is
**356 KB** per template, because the guest's RAM stays in the file that backs it.

Nothing below is faster because the VM does less. It is faster because the expensive parts
of starting a machine already happened once.

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

## What the caller has to hold to

- **The device set must be identical.** Down to whether there is a serial port. The failure
  is `Unknown section or instance 'serial' 0`, at load time.
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
