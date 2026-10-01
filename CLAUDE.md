# Working in this repository

## What this is

A virtual machine: QEMU, a guest kernel, a base image, and the definition of the machine
they make. Four artefacts and one version.

**A release does not carry software that owns a guest.** No container runtime, agent,
supervisor, RPC service, or init is part of the published machine. A release is not bootable
on its own: whoever runs a guest supplies its init.

Diagnostic input is different from release content. A test or experiment may use a caller
supplied initrd, a temporary guest helper, a disposable overlay, or a separately built
firmware image when it is isolated from `task build` and `task release`. State clearly what
is diagnostic, who supplies it, and what it is measuring. The qboot probe is the model:
it does not change a release tree and compares both variants with the same diagnostic initrd.

**Keep consumer-specific implementation out of this tree, but record real contracts.** Do
not copy consumer code, paths, or an ADR as a substitute for an explanation. It is correct to
name an external component when its protocol, provisioned file, lifecycle, or compatibility
contract affects this machine; describe the contract here and test the observable behaviour
where possible.

## The three rules that are load-bearing

These are release invariants, not defaults. Everything below them is a strong engineering
default unless it says otherwise; an experiment may depart from a default when it is isolated
and makes its scope and result clear.

1. **A release is one machine.** `machine.Spec.Fingerprint` hashes the QEMU binary, the
   kernel, the initrd and the firmware the guest runs (the BIOS, qboot.bin, and pvh.bin) by
   content, together with the five arguments that decide the machine's shape. A checkpoint
   resumes only onto a machine with the fingerprint of the one it was saved on; across two
   that differ a resume is undefined rather than an error. A content change to any of those
   files is a fleet-wide event: every checkpoint taken against the previous machine stops
   resuming. That is the design, not a bug to work around. A source or recipe change is a possible fleet-wide event; whether
   it is one is decided by the resulting artifact hashes and shape, not by the diff's apparent
   size. Verify them before promotion.

2. **The base image is never written to.** Every VM maps it read-only through a qcow2
   backing chain and many share one file. Anything that opens it for writing invalidates
   every overlay in existence — silently, because the overlays keep working until they read
   a cluster that moved. New images are written beside the old one and renamed over it.

3. **No identity in the base image.** No hostname, no machine-id with a value, no
   `/etc/resolv.conf`, no SSH host keys, no random seed. A VM is handed its identity after
   it is restored; anything baked in is shared by every VM that ever boots from that image.
   `image/build.sh` asserts each of these on the finished filesystem.

## Simplicity

Prefer the boring solution. The measure of a change here is not what it makes possible but
what it makes clear.

- **Build what is needed now.** Not what will probably be needed. A flag with one caller
  should be that caller's constant; an interface with one implementation should be the
  implementation; a configuration option nobody sets is a branch nobody tests. When a
  second case actually arrives, generalise then — the second case tells you what the
  abstraction should be, and guessing beforehand almost always produces the wrong one.
- **Deleting is a change like any other.** If something no longer earns its place, remove
  it rather than leaving it for a future that may not come. Commented-out code is deleted
  code with extra steps; git remembers.
- **A little duplication is cheaper than the wrong abstraction.** Two similar things that
  drift apart for good reasons are healthier than one thing with a mode flag.
- **Put the thing where the thing belongs.** Static configuration goes in the image, next
  to the other configuration. A decision about one boot goes in whatever made that boot. A
  systemd unit written by a Go program at run time was the wrong answer to a real problem;
  the unit belongs in `image/`, and only the symlink that enables it belongs in the boot.

### Experiments

Experiments are welcome when they make a production decision cheaper to evaluate. They must
not silently become production behaviour.

- Keep experiment outputs, overlays, initrds, firmware, and caches separate from release
  outputs. Do not modify a shared base image; use a disposable overlay or a new artifact.
- A probe may change one production default at a time, including a kernel option, unit,
  QEMU sandbox setting, CPU affinity, or diagnostic initrd. Production defaults remain the
  safe settings until the result is promoted deliberately.
- Record the baseline, environment, sample count, and the measured result. A fast exploratory
  run is enough to decide what to investigate; a promotion needs a reproducible comparison
  and the functional check that could reveal a false win.
- If a promoted change alters what a restored guest sees, update the machine identity and
  prove that checkpoints do not cross the boundary. If it is host-only (for example launcher
  CPU affinity), document why it does not.
- Remove or quarantine an experiment once it no longer has an active question. Keep the
  conclusion and durable evidence, not a permanent switch in the release path.
- A measurement that decides something is taken on the KVM runner (construct), through
  `.github/workflows/lab.yml`, one run at a time: its three runners share one machine, and two
  lab runs side by side measure each other. A laptop has neither the Spin OS kernel nor a quiet
  host, so its numbers are leads, not findings. Compare with enough boots to beat the noise -
  three a row moved a p50 by 5-11% between runs of one release (#81) - and alternate the
  variants rather than running them in blocks.
- What the guest's memory and disk were measured to cost, and why each default is what it is,
  is `docs/memory-and-disk.md`: add a result there when it decides something, with its run.

## Go

- **Clarity beats cleverness.** This code is read far more often than it is written, and
  usually by someone trying to find out why a VM did not start.
- **Handle an error once.** Wrap it with what was being attempted — `fmt.Errorf("mounting
  %s at %s: %w", …)` — and return it. Do not log it and return it. An error message should
  say what failed, on what, so that the person reading a serial console at 2am does not
  have to guess. A bare `no such file or directory`, which this repository shipped once,
  names nothing.
- **Do not panic**, and do not exit from a library. `cmd/` decides what a failure means.
- **Accept interfaces, return structs**, and only introduce an interface when there is a
  second implementation or a test that genuinely needs one.
- **Make the zero value useful** where it is cheap to do so, and validate where it is not.
  `Spec.validate`, which `Args` runs, exists because a disk with no format is a guest that boots and finds a
  disk full of nothing — that is worth a check at the boundary, not a sensible default.
- **Name things for what they are** to the reader, not for their type. `Shape`, `Spec`,
  `Fingerprint` are the words used when talking about this machine; use those.

## Comments

Comments here are longer than usual, on purpose. The rule is what makes them worth reading:

- **Say why, not what.** The code says what. If a comment restates it, delete the comment.
- **Carry durable evidence.** When a production decision rests on a measurement, put a
  concise result and its context near the decision, or link to a maintained measurement
  record. Include a number when it is the reason for the decision; do not turn an exploratory
  observation into a permanent claim.
- **Record failed approaches selectively.** Keep a failed attempt near code only when it
  prevents a plausible, costly regression or repeats a non-obvious tool failure. Put raw
  samples and short-lived exploration in a measurement record, issue, or commit instead.
- **Date a fact that could go stale.** "measured 2026-09-07" tells a reader whether to
  re-check.
- **Do not write down that it works.** The README had a "Status" section listing what had
  been built and verified on a date. Every line of it was something CI answers
  continuously and can answer *red*, or something already said next to the decision it
  justified — so it could only rot, and it did. A claim about the state of the tree
  belongs in a workflow, a badge, or a commit message. Never in prose that nobody
  re-reads.
- **Name external contracts when relevant.** See above.

## Artefacts leave this machine

Host-executed binaries published in a release should be statically linked unless there is a
documented deployment reason not to be. This rule does not apply to guest userspace, build
tools, or diagnostic artifacts. A released host binary that depends on host libraries needs a
compatibility and packaging story, because its failure otherwise arrives on someone else's
machine at start-up.

## Verifying

- **Assert what came out, not what went in.** A build that produced an empty filesystem
  produces a perfectly valid empty filesystem. `image/build.sh` opens the finished image
  and looks for `/sbin/init`; the QEMU build asks the binary it just produced which devices
  it has; `kernel:verify` reads the ELF notes out of the kernel it just stripped.
- **Check the tool's output, not only its exit status.** `debugfs -R` exits 0 whether or
  not the file it was asked about exists, which made every filesystem check in this
  repository vacuously pass until someone looked.
- **Fail where the cause is.** A missing option ROM should fail in the build that did not
  ship it, not in a VM three weeks later that will not start. Every workflow ends in the
  verification that belongs to it.
- **A tarball with something missing is worse than no tarball**: it installs, and the
  missing piece surfaces somewhere else. `hack/release` refuses rather than warning.
  A list of files to ship is how one goes missing: `hack/release` named qboot's patches one
  by one, and v20261001.01 shipped a `qboot.bin` without the patch it was built with. Ship
  what a directory holds.
- **Ask the kernel for a feature, never for its module.** Spin OS's kernels have no loadable
  modules: what they have is built in, `modprobe` finds nothing, and a built-in driver without
  parameters has no `/sys/module` entry. Look for what the feature makes - `/dev/nbd0`,
  `/dev/vhost-vsock` - and only then load a module. Asking the module said no to features
  that were there, twice (#56, #82).

## Trying the machine

`spin-machine` (`cmd/spin-machine`, built by `task tools`) is how this machine is run by
hand. A one-off check goes through it: do not write a Python or shell harness that rebuilds
its command line, speaks QMP, or makes overlays itself. A harness like that tests itself and
not the definition, and it is thrown away with what it found. If the CLI cannot do what the
check needs, and the check is part of the machine's contract (a disk hotplugged, a save,
a restore), add it to the CLI. If the check should be repeated, it belongs in `boot/`
as a Task target.

- **Rebuild it first.** `_output/bin/spin-machine` is whatever `task tools` last built, so
  a change to `machine/` is invisible until the tool is rebuilt. `spin-machine args` shows
  the command line that will actually run.
- **The base is safe by default.** Without `--disk`, `boot` puts the base image under a
  throwaway overlay (QEMU's `-snapshot`), and root is `/dev/vda`. `--disk` opens the named
  file as given, so point it at an overlay and never at `rootfs.qcow2`.
- **Driving a guest.** `--init /bin/sh` gives a shell with nothing mounted; its console is
  on stdin/stdout, or on a socket with `--console unix:PATH,server=on,wait=off`. `--qmp
  PATH` opens the socket that `attach`, `detach` and `save` talk to.

```sh
spin-machine boot --hotplug-disks 2 --qmp /tmp/q.sock --init /bin/sh &
spin-machine attach --qmp /tmp/q.sock --target 0 --disk disk.raw   # sda, in the guest
spin-machine detach --qmp /tmp/q.sock --target 0
```

## Taskfiles

Each part's targets live beside what they build — `qemu/Taskfile.yml`, `kernel/Taskfile.yml`,
`image/Taskfile.yml` — and the root file holds the vars they share and the targets that
cross all of them. CI calls the same targets a developer calls: a second definition of what
the gate is, kept in a workflow file, goes out of step with the first exactly when a check
is added.

## Licence and third-party source

Apache-2.0. New Go files carry `// SPDX-License-Identifier: Apache-2.0`.

Anything vendored into this tree goes in `NOTICE`, with its licence and — if it was
changed — a statement that it was. Anything that ends up *inside a release* goes in the
`SOURCES` file that `hack/release` writes: what it is, which upstream tarball it was built
from, that tarball's SHA-256, and how to get it. The repository's licence covers the
recipes; a tarball of GPL binaries carries obligations of its own, and the release is where
they are answered.

An input downloaded from outside is pinned by hash and checked on every build, not only
after a download — the caches outlive the builds that filled them. The pin is written once, in
`versions.yaml`, and handed to the Dockerfile as a build argument with no default; a test fails
on a digest or a commit written anywhere else. `task bump NAME=...` moves one entry, and the
entry's note says what the bump is checked with.

## Versions

CalVer, `vYYYYMMDD.NN`. A release is the machine as it stood on a date. There is no API here
to promise compatibility about, and the one thing a version could promise — that checkpoints
still resume — is decided by the fingerprint, not by a number anybody chose.
