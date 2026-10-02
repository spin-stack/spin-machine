# Moving a VM to another machine

A checkpoint is one state file — memory, device state and CPU state, written by a
`migrate` to a file — carried somewhere else and resumed there.

Stopping a VM here and resuming it there is a lifecycle, and this repository does not
implement one — it builds the machine that lifecycle runs on, and offers the arguments it
needs: `Incoming`, either a state to load at exec time (`-incoming file:…`) or `defer` (start
with no state and wait to be told where it is, over QMP, for a caller that starts the machine
before it has the state in hand).

`spin-machine save --qmp … --to state` is the first half by hand, and the second is either
`spin-machine boot --incoming file:state` or `boot --incoming defer` followed by
`spin-machine restore --qmp … --from state`.

## The CPU decides whether a VM can move at all

The default, `host`, shows the guest this host's own feature set through CPUID — which the
guest reads once and never questions. Resume that somewhere without AVX-512 and it executes
an instruction that is not there. So `Fingerprint` folds the host's CPU model in whenever
the model derives from the host, and a state saved here does not match a machine elsewhere.

Naming a model — the oldest microarchitecture in the fleet — makes every host show the same
CPU, and the host's own silicon drops out of the fingerprint. Measured, restoring a state
saved with `Broadwell-v4`:

| CPU on the second host | |
|---|---|
| `Broadwell-v4` | resumes |
| `Skylake-Client-v4` (richer) | resumes |
| `Nehalem` (poorer) | refuses: `Failed to set special registers` |

Save with the baseline, and any host that meets or exceeds it can take the VM.

**A named model carries `enforce=on`**, and without it naming one means nothing. QEMU's
default is to warn about features the host cannot provide and start anyway, having quietly
removed them — so the same model name gives a different guest CPU on different machines.
Measured: asking a Raptor Lake host for `Skylake-Server-v4` gives five warnings about
missing AVX-512 and exit 0. With `enforce=on` it is `Host doesn't support requested
features` and exit 1, before the VM exists.

## The devices have to match too

Down to whether there is a serial port — restoring without one says `Unknown section or
instance 'serial'`. That is why the device set is in the fingerprint
([machine.md](machine.md#the-fingerprint)). NICs are in it, by count and MTU: a NIC is on
the command line, so it is there when the state is loaded. Disks are not: they are the VM's
and not the machine's, and whoever resumes a checkpoint gives it the disks it was saved with.
