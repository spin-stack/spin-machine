# Working in this repository

## What this is

A virtual machine: QEMU, a guest kernel, a base image, and the definition of the machine
they make. Four artefacts and one version.

**What it is not: anything that runs inside a guest.** No container runtime, no agent, no
supervisor, no RPC. A release is not bootable on its own and that is deliberate — whoever
runs guests brings the init. `cmd/spin-machine-init` is the single exception, is a
debugging tool, and is not in a release.

**It does not know about the projects that consume it.** No repository names, no file paths
into other trees, no ADR numbers. If a rationale can only be stated by naming a consumer,
it is either the wrong rationale or the wrong repository for the code. Say what is true of
the machine.

## The three rules that are load-bearing

Everything else is style. These are correctness, and each fails silently.

1. **A release is one machine.** `machine.Spec.Fingerprint` hashes the QEMU binary, the
   kernel and the initrd by content, together with the four arguments that decide the
   machine's shape. Two machines with the same fingerprint may exchange templates; two
   without may not, and a restore across them is undefined rather than an error. Any change
   to any of those three files invalidates every template in existence. That is the design,
   not a bug to work around — but it means "I only changed a comment in `kernel/Dockerfile`"
   is a fleet-wide event, and it has already happened once in this repository's history.

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
  `Spec.Validate` exists because a disk with no format is a guest that boots and finds a
  disk full of nothing — that is worth a check at the boundary, not a sensible default.
- **Name things for what they are** to the reader, not for their type. `Shape`, `Spec`,
  `Fingerprint` are the words used when talking about this machine; use those.

## Comments

Comments here are longer than usual, on purpose. The rule is what makes them worth reading:

- **Say why, not what.** The code says what. If a comment restates it, delete the comment.
- **Carry the evidence.** When something is the way it is because of a measurement, put the
  number in: `87.6 ms to 51.1 ms, kernel to init`, `+356 ms and rejected`, `−3.7 ms of
  boot`. A claim without a number invites someone to undo it on a hunch.
- **Record what was tried and failed.** The comments that have paid for themselves most
  here are the ones naming a dead end: the drop-in that did not lift the device dependency,
  the flag that broke a link twenty minutes into a build, the check that passed for every
  input because a tool exits 0 on failure. Someone will otherwise try it again.
- **Date a fact that could go stale.** "measured 2026-09-07" tells a reader whether to
  re-check.
- **Do not name the projects that consume this.** See above.

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

## Taskfiles

Each part's targets live beside what they build — `qemu/Taskfile.yml`, `kernel/Taskfile.yml`,
`image/Taskfile.yml` — and the root file holds the vars they share and the targets that
cross all of them. CI calls the same targets a developer calls: a second definition of what
the gate is, kept in a workflow file, goes out of step with the first exactly when a check
is added.

## Versions

CalVer, `vYYYYMMDD.NN`. A release is the machine as it stood on a date. There is no API here
to promise compatibility about, and the one thing a version could promise — that templates
still match — is decided by the fingerprint, not by a number anybody chose.
