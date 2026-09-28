# Checks and releases

## CI

Five workflows, and the split is about cost. `ci.yml` runs on every push and builds none of
the three artefacts — it is `task lint` and `task test`, which is fast and catches most
mistakes. It also *pulls* one: `task qemu:fetch` unpacks the published QEMU of the pinned
version in seconds, and `task verify:args` hands it the command line `machine.Spec.Args`
builds. The device set is decided in `qemu/devices.mak` and the device and property names
are written out by hand in `machine/machine.go`; nothing else connects the two, and a
device dropped from the allowlist reads exactly like a typo added here — a VM that does not
start, found by whoever boots one.

Each artefact has a path-triggered workflow of its own, because QEMU and the kernel are
tens of minutes each and the base image is ~1.5 GB of apt: `qemu.yml`, `kernel.yml`,
`image.yml`. Each ends in the verification that belongs to it, so a build that lost a
device, a kernel that lost its PVH notes, or an image that grew an identity fails in the
workflow that produced it.

CI calls the same targets a developer calls. A second definition of what the gate is, kept
in a workflow file, goes out of step with the first exactly when a check is added.

`verify:consumer` is the end-to-end check and is opt-in, because this repository ships no
runtime to boot with: pass `CONSUMER_RUNTIME` and `CONSUMER_INITRD` and it unpacks the
tarball and asks three questions of **the image inside it** — does a write reach the disk
and come back, is that write invisible to the next VM, and is the base byte-identical
afterwards. "It printed something" answers none of the three.

## Releases

`release.yml` builds all three and packs one tarball. Versions are **CalVer**,
`v20260909.01`: a release of this repository is the machine as it stood on a date. There is
no API here to promise compatibility about, and the one thing a version could promise —
that templates still match — is decided by the fingerprint of the artefacts, not by a
number anybody chose. Pushing a `v*` tag releases that version; running the workflow by
hand with no input generates the next sequence for today, tags the commit, and puts the
three checksums in the release notes.

## What a release owes

Apache-2.0 covers the recipes, not what they build. A release tarball is almost entirely
other people's software: QEMU and Linux are GPL-2.0, and the base image is an Ubuntu
userland under a dozen licences. Shipping their binaries carries obligations a LICENSE file
in a source tree does not discharge — GPL-2.0 section 3, and section 6 of the LGPL for the
libraries inside the statically linked QEMU.

So a release answers them:

- `SOURCES` names every upstream source by version, URL and SHA-256, says how it was built,
  and carries the written offer.
- `packages.txt` lists every package in the base image with its exact version, which is
  what `apt-get source <package>=<version>` needs.
- The upstream tarballs are pinned by SHA-256 in `versions.yaml` and **verified on every
  build**, not only after a download — the source lives in a cache mount that outlives the
  build that filled it. That is also what makes `SOURCES` true rather than aspirational:
  what it names is what was compiled.
