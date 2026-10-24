# Checks and releases

## CI

The workflows are split by cost. `ci.yml` runs on every push and builds none of
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

`task report` is the end-to-end check, and `report.yml` runs it on every release: every
combination of the machine's features, booted from the published tarball to a login.

## Releases

`release.yml` builds all three and packs one tarball. Versions are **CalVer**,
`v20260909.01`: a release of this repository is the machine as it stood on a date. There is
no API here to promise compatibility about, and the one thing a version could promise —
that checkpoints still resume — is decided by the fingerprint, the machine's contract, not by a
number anybody chose: a release that keeps it resumes every checkpoint the one before it saved
(`task boot:compat`), whatever QEMU, kernel or firmware it ships. A release is the workflow dispatched on main, and nothing else - a pushed
tag starts nothing: given a version it makes that one, with no input it generates the next
sequence for today; either way it tags the commit once its files are signed, and writes the
notes: what changed since the release before (`hack/releasenotes`: the commits that reached
the machine by the part they changed, the pins that moved, the kernel options turned on, off
or changed, and the patches added, changed or removed), whether checkpoints carry over
(`hack/fingerprint-diff`), and the checksums.

Each file the release offers - the tarball, its `.sha256` and `machine.env` - carries two
Sigstore bundles beside it: `<file>.sigstore.json`, the signature of the workflow that built it,
and `<file>.provenance.sigstore.json`, its SLSA provenance (`go tool provenance`: this workflow at
its commit, and every pin in `versions.yaml`). Whoever takes one checks both against
`release.yml` at the ref it ran on:

```sh
id=https://github.com/spin-stack/spin-machine/.github/workflows/release.yml@refs/heads/main
cosign verify-blob --bundle f.sigstore.json --certificate-identity "$id" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com f
cosign verify-blob-attestation --type slsaprovenance1 --bundle f.provenance.sigstore.json \
  --digest "$(sha256sum f | cut -d' ' -f1)" --digestAlg sha256 \
  --certificate-identity "$id" --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The attestation is checked by the file's digest: given the file, `verify-blob-attestation` reads it
whole and refuses one over 128 MiB (`COSIGN_MAX_ATTACHMENT_SIZE`), which the tarball is.

Every release ran on `refs/heads/main`, and that is the ref its certificate names; which release
a file is, its provenance says (`buildDefinition.externalParameters.version`), and whoever takes it
holds it to the version they asked for.

What the release is made of is its CycloneDX SBOM, written by `hack/release` from what the builds
recorded - the apk databases QEMU and e2fsprogs were statically linked in (each build's
`/lib/apk/db/installed`, extracted to `_output/sbom/`), the image's `packages.txt`, the pins in
`versions.yaml`, and each shipped file by its hash - and checked with `go tool sbom check`: a
component with no version fails the release. It is inside the tarball at
`usr/share/spin-stack/sbom/machine.cdx.json`, beside it as `<tarball>.cdx.json`, and attested
over the tarball as `<tarball>.cdx.sigstore.json` (`cosign verify-blob-attestation --type
cyclonedx`, with the identity above). QEMU, the kernel and qboot carry their patches as pedigree:
each patch file by name and hash, so the SBOM says the binary is upstream's source plus these.

The builders' packages are a commit's, not the mirror's on the day: `qemu/apk.list` and
`e2fsprogs/apk.list` say what each installs, and `qemu/apk.lock` and `e2fsprogs/apk.lock` pin every
package installed, dependencies included, at a version. A pin Alpine has dropped from its mirror
fails a cold build at `apk add`; `task apk:lock` writes both locks again from a fresh install on the
pinned Alpine, and the diff is the review.
`workflows/workflows_test.go` holds every workflow to attesting before it publishes.

## The feature matrix

A release also says what it costs. After it publishes, the `report` job boots the tarball it
just published, on the bench fleet (spin-stack/ci: one instance type with `/dev/kvm`), through every combination of the
machine's features under KVM (memory fixed or with a virtio-mem ceiling; vsock; a hotplug
controller) and every boot variant of the image. It writes `report.json` beside the tarball: per row, the command
line, the shape and fingerprint, whether QEMU ran it, and p50/p95 of each boot phase. It then
appends `spin-machine compare` against the newest earlier release that has a report to the
release notes. The axes are the `axes` table in `boot/report_test.go`; a new axis is measured
against every other without anyone choosing which pairs matter.

The same report is how an experiment is judged. Build the tree with the change, or keep the
release and pass the change to every boot, and compare:

```sh
task report OUT=base.json
task report OUT=exp.json FLAGS='--append mitigations=off'   # or --kernel /path/vmlinux
_output/bin/spin-machine compare --old base.json --new exp.json
```

`ONLY=<regexp>` runs the rows whose id matches, and `REPS=` sets the boots per row (default 20,
after one unmeasured boot that warms the page cache). Twenty, because at three the comparison
of v20261001.01 with v20260930.02 flagged every KVM row 5-11% slower, and twenty boots of each,
alternated on one host, put them within 5 ms of each other (2026-10-01). Times compare only
between reports taken on the same kind of host; `compare` says so when they were not.

`report.yml` is the job, and it runs by hand too: `gh workflow run report.yml -f
version=<release>` measures any published release and keeps `report.json` and the comparison as
an artifact without touching the release - how the runner is checked, and how two releases are
compared after the fact (`-f publish=true` does what a release does).

The runner needs `/dev/kvm` and `/dev/vhost-vsock` readable and writable by its user, `gh`, and
`sudo -n` for `modprobe nbd` and `qemu-nbd`: the image variants write into a throwaway overlay.
Without `/dev/vhost-vsock` the vsock rows are listed as skipped, and without sudo the variants
that edit the image are skipped too, and the getty is agetty rather than an echo.

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
