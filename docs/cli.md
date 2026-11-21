# spin-machine-dev, the command

`spin-machine-dev` is what a person types to work on this machine, and GitHub Actions runs the same
verbs. Each verb's `--help` says what it needs, does, leaves and exits with. This page says how the
verbs fit together. `spin-machine-dev --help` lists them, and `go tool task bin` builds the command
into `_output/bin`.

It is not `spin-machine`. `cmd/spin-machine` is the machine's own tool: it boots a machine,
attaches a disk, saves and restores. The tarball ships none: spin builds the `machine` package of
the release it pins, and a person builds the tool with `spin-machine-dev build spin-machine`. `cmd/spin-machine-dev`
builds, measures and releases the machine, and is never shipped.

## Principles

They are spin-os's (spin-stack/ami, `docs/cli.md`), because the two commands solve the same problem:

1. **A verb is a goal, not a step.** `spin-machine-dev build image` builds the image and whatever
   it is made with that is missing or stale, such as e2fsprogs and the systemd it installs.
   `release` builds every part before it packs them. Nobody builds e2fsprogs first.
2. **State is files, never variables.** Every verb reads and writes under `_output/` (`--out`).
   Each part a build makes leaves `_output/manifest/<part>.json`, which names a digest of its
   inputs: the build arguments versions.yaml hands its Dockerfile, and every file under its
   directory. `build` skips a part whose inputs have not moved and whose output still verifies. A
   verb can run alone, again, or on another machine with the same `_output/`.
3. **Two outputs.** Progress and the builds' logs go to stderr, for a person. With `--json`, one
   JSON document goes to stdout, for a program.
4. **Exit status means something, the same everywhere.** 0 is done. 1 is done with something to
   act on (a verify that failed, a pin behind, a file not attested). 2 means the verb could not do
   it (no network, a missing input, a build that failed). A workflow branches on the status, never
   on text.
5. **Flags for choices, the environment for credentials and the runner.** `--jobs`,
   `--cache-registry`, `--cache-export` and `--version` are flags. The environment carries only
   what is secret or the runner's: `GH_TOKEN` or `GITHUB_TOKEN`, `GITHUB_REPOSITORY`,
   `SOURCE_DATE_EPOCH`. In a workflow, a verb whose result a later step needs also writes that
   result to `$GITHUB_OUTPUT` (`build`'s `qemu_version`, `release version`'s `version`, `release
   notes`' `notes`, `changed`'s `build`), and writes what it made to the run's summary. Both are
   copies, never the only place the result is.

## Nouns

| noun | is | where |
|---|---|---|
| part | what a build makes: `qemu` (with qboot and the firmware), `kernel`, `e2fsprogs`, `systemd`, `image`; and `spin-machine` | `_output/{bin,qemu,kernel,e2fsprogs,systemd,image}`, `_output/manifest/<part>.json` |
| release | every part, packed as one tarball with one version, its manifest, SOURCES and SBOM | `_output/release/` |
| pin | an entry of `versions.yaml`: what a part is built from, by version and hash | `versions.yaml` |

## Working on the machine

```
spin-machine-dev build [part...]      what is missing or stale, every part when none is named; each verified
spin-machine-dev fetch qemu           main's published QEMU of the pinned version, in seconds
spin-machine-dev verify [part|args]   assert what _output holds, building nothing; args asks QEMU for the machine's command lines
spin-machine-dev shell                boot it and get a console in the base image, which is checked unchanged after
spin-machine-dev vsock                make /dev/vhost-vsock usable, for the probes and verify args
spin-machine-dev menuconfig           edit the kernel's configuration in place
spin-machine-dev clean [--cache]      remove _output; with --cache, the BuildKit and mkosi caches too
```

A part's build first empties what it writes. `--output type=local` writes files and removes none,
so a file the build stopped producing would otherwise stay, indistinguishable from one it
produces. The build ends in its verification: the binaries run here and only the CI QEMU can
emulate, the kernel is a relocatable bzImage with the vmlinux it was made from, and the e2fsprogs
tools answer the pinned version. `verify --help` says each.

## Keeping the pins current

```
spin-machine-dev outdated             which pins are behind their upstream (exits 1 when any is)
spin-machine-dev bump <entry> [ver]   move one pin; a QEMU bump prints what newer machine types change
spin-machine-dev osv [--issues]       OSV against the newest release's SBOM; an issue per package with findings
spin-machine-dev cves [--local]       the kernel CNA's CVEs against the newest release's kernel, or _output's
```

`outdated` and `bump` run go-tools' `versions` (go.mod's tool directive), which every spin-stack
repository shares. `spin-machine-dev` adds what belongs to this repository: the QEMU machine types,
the builder images, OSV and the kernel's CVEs.

## Releasing

```
spin-machine-dev release [--version v]   build every part and spin-machine, then pack the parts; refuses a tree with anything missing
```

The release is one tarball. It holds QEMU and its firmware, the kernel, the base image, the
e2fsprogs tools, LICENSE and NOTICE, every patch each part was built with (a directory ships whole,
never as a list), `machine.env`, `SOURCES` and a CycloneDX SBOM. `machine.env` is the manifest:
versions and checksums, none of which decides whether checkpoints resume (the machine's record
does, `machine.CanResume`). The SBOM is inside the tarball and beside it.

`--help` hides the verbs that only workflows run. These are under `release`:

- `version`: the next vYYYYMMDD.NN.
- `notes`: what changed since the release before, and its manifest.
- `provenance`: SLSA.
- `sign`: cosign over every file, each bundle checked, then attestgate.
- `tag`: through the API, because GITHUB_TOKEN cannot push a ref over a workflow change.
- `pack`: pack what `_output` holds, building nothing.

The other hidden verbs are `publish qemu` (main's parts.yml), `changed` (whether a change can reach
a part), `needs` (whether every job a summary job needs passed) and `refresh` (refresh.yml's
proposals).

## How GitHub Actions composes it

A workflow is the verbs in order, a step each. A job that runs spin-machine-dev runs
`go tool task bin` first, which builds `_output/bin/spin-machine-dev` and puts it on the job's
PATH. Under `go run`, a step would exit 1 whatever the verb's status. taskgate
(`-command spin-machine-dev`, in `task lint`) checks that each step is one invocation or one task.

```
ci.yml       task lint ; task test ; task coverage ; task mutate ; task bin ;
             spin-machine-dev build spin-machine ; spin-machine-dev fetch qemu ;
             spin-machine-dev vsock ; spin-machine-dev verify args
parts.yml    changed: spin-machine-dev changed --base $BASE --pins ... <paths>, once a part
             qemu:    spin-machine-dev build qemu --cache-registry ; vsock ; verify args ;
                      publish qemu (main)
             kernel:  spin-machine-dev build kernel --cache-registry
             image:   spin-machine-dev build e2fsprogs ; spin-machine-dev build image
             parts:   spin-machine-dev needs
compat.yml   task boot:buildkit ; spin-machine-dev vsock ;
             spin-machine-dev build qemu kernel spin-machine ; task boot:here ; task boot:record ;
             spin-machine-dev lab compat ; task boot:lockdown
release.yml  compat.yml ; spin-machine-dev release version ; release ; release notes ;
             release provenance ; release sign ; release tag ; then the publish action ;
             report.yml ; task trace ; task ship-it:wake
refresh.yml  task git:app ; spin-machine-dev refresh kernel|systemd|builders
report.yml   task bin ; task vsock ; spin-machine-dev releases measure TAG... ; then in main's
             checkout: task bin ; spin-machine-dev releases record LADDER_JSON... ;
             task git:app ; spin-machine-dev releases propose
osv.yml      spin-machine-dev osv --issues ; spin-machine-dev cves
lab.yml      task vsock ; task lab:ci
pages.yml    task bin ; spin-machine-dev releases site --out _site
```

A build in CI reads BuildKit's cache from GitHub's registry (`--cache-registry`,
`internal/build.CacheRegistry`) and writes it back only on main (`--cache-export`). A pull
request's, a release's and compat's builds read main's cache, and nobody reads what they compiled
after. The cache lives in the registry and not in the Actions cache, because the fleet's runners
cannot read the Actions cache, and the Actions cache evicted a lane's layers between a merge and
the release that needed them.

## The Taskfile

What a person types is spin-machine-dev. The Taskfile is go-tools' gates, the same in every
spin-stack repository (`test`, `lint`, `lint:workflows`, `mutate`, `coverage`), and CI's plumbing:
`bin`, `git:app` (git as the release app), `trace` and `ship-it:wake`. The probes' targets
(`boot:*`), `lab`, `lab:ci` and `vsock` run what the verbs built.

## Measuring

`spin-machine-dev lab` is how this machine is measured: here, where a laptop finds leads, and on the
bench fleet, where a number decides something (AGENTS.md, "The goal"). Its code is
`internal/lab`. Each verb's `--help` says what it takes.

| noun | is | where |
|---|---|---|
| tree | a whole machine laid out as a release: QEMU and its firmware, a kernel, the image and a spin-machine, with a `manifest.json` naming where each part came from and the edits made to every boot's overlay | `_output/trees/NAME/` |
| probe | a measurement of a tree: it boots the tree, records named numbers for each repetition, and says what it lost | `internal/lab`, one file each |
| experiment | a probe over trees, repetitions of each, interleaved, written as `result.json`: every sample, the options, each tree's manifest, the host | `_output/lab/results/`, or a lab run's artifact |

A tree starts from a release (`--from release`, `--from release:v20261119.01`) or from a directory
laid out as one (`--from _output`). It replaces what its flags ask for:

- `--kernel run:N`: a parts.yml run's bzImage, or a Kernel workflow run's from before parts.yml. A
  run from before 2026-11-19 has none, and the lab refuses it by name. `--kernel` also takes the
  path of a bzImage.
- `--qemu run:N`, or a directory with `bin/` and `qemu/`.
- `--spin-machine here`: built from this checkout.
- `--spin-machine release`: built from the commit the release's `machine.env` names, fetched by
  that commit from the checkout's `origin`, so neither the module path nor the repository's name
  matters. That is the machine the release defined, and the ladder boots each release with it.
- Without `--spin-machine`, the tree keeps the spin-machine of the tree it starts from. A release
  ships none, so a tree from a release always gets this checkout's, as the `boot:` targets'
  `:tools` dependency gave the probes before.
- `--append ARGS`: added to the kernel command line of every boot.
- `--boot FLAG`: a spin-machine boot flag for every boot, such as `--boot --cpu=Skylake-Server-v4`.
- `--cpu baseline`: the newest named CPU model the host making the tree can run
  (`machine.QEMU.CPUModels`, and `internal/lab` ranks them).
- `--edit lane`: the first login's lane as this checkout builds it, put into a published
  release's overlays.
- `--edit floor`: no systemd before sshd. It is a bound, used with
  `--append init=/usr/local/sbin/spin-floor`.
- `--edit pressure`: the pressure probe's guest sender, its unit and its enable, as this checkout
  builds them into the image, put into a published release's overlays.
- `--edit nolinger`: spin not lingering, as this checkout builds the image.
- `--edit forwarder-early`: `login=spin`'s forwarder, which stands for spin's supervisor, without
  systemd's default dependencies.
- `--edit carry-masks`, `carry-generators`, `carry-logging` and `carry-coldplug`: what
  optimize-systemd.sh and the coldplug filter do for boot time alone, taken back
  (docs/boot-time.md, "What the machine carries"). Each goes after `--edit lane`.
- `--systemd PATCH`: systemd built here with a patch from image/systemd/experiments, in every
  overlay.

Nothing is written through the tree a tree starts from. Parts are links, and a replaced part is a
copy. The lab refuses a tree whose manifest is missing, names no source for a part, or does not
match the file, before anything boots. A probe is handed trees, never a path in an environment
variable it may not read.

```
spin-machine-dev lab tree NAME [tree flags]              make _output/trees/NAME
spin-machine-dev lab run PROBE [TREE...] [--tree NAME=SPEC]... [--reps N] [--opt K=V]... [--out DIR]
spin-machine-dev lab show RUN_ID|DIR                     a lab run's result.json, downloaded, as its table
spin-machine-dev lab dispatch PROBE --tree NAME=SPEC... [--reps N] [--opt K=V]... [--host bench|bench-metal]
spin-machine-dev lab check TARGET [--tree NAME=SPEC]     boot:TARGET on one tree; a second tree, firmware-stages boots beside it
spin-machine-dev lab ladder [--releases N] [--newest vX] the newest releases measured together: ladder.json
spin-machine-dev lab compat [--here DIR] [--releases N]  a checkpoint of every release whose machine is this one, resumed
```

`lab ladder` compares releases, and two runs taken apart do not. The ladder boots the `--releases`
newest releases (five unless told) up to `--newest`, each a tree with `--spin-machine release`,
interleaved on one host. It runs every goal probe over the releases whose spin-machine can run it.
A probe says which commands and flags it needs, and v20261119.01's spin-machine has no
`boot --restore`. The ladder's table is the goal's two numbers release by release, each with its
Δ against the release above, and the newest against 100 ms. Beside them are the prestarted
numbers, save, state, the kernel and QEMU's RSS as diagnostics, and the shape every release booted.

### What a Δ says

Every comparison the lab prints is the Δ p50 of two sets of boots taken in one run, interleaved,
with its 95% interval. That covers `lab run`'s and `lab show`'s table of trees against the first,
a ladder's table, `releases compare`, and the page's Δ column. The interval is the bootstrap's:
each side drawn again with replacement 2000 times from a fixed seed, so the same samples always
print the same interval (`lab.DeltaP50`). The cold first login of v20261119.01 against
v20261114.02 reads `-54.2 [-62.9, -49.7]`, and v20261119.02's against it
`+3.0 [-0.8, +6.6] within noise` (lab 44641240861).

One rule decides: **a change is a gain only when the interval of its Δ excludes 0**. For a number
measured in time, the interval lies wholly below 0. An interval that includes 0 is within noise,
whatever the Δ's size, and no threshold sits beside the rule. A Δ between two runs has no interval
and is not read, because the bench moves between runs by as much as a release does. A side with
one boot has no interval either, and the lab prints its Δ bare, saying so.

### The shape

Every probe boots the machine spin boots a workspace in, unless a run asks for another: one vCPU
of a ceiling of 16, 128 MiB of a ceiling of 8 GiB through virtio-mem, and 16 targets on the
hotplug controller, 4 of them for disks (spin-stack/spin's `internal/runner/vmm/vmm.go` and `internal/runner/qemu/spec.go`). The options
are `cpus`, `max-cpus`, `memory`, `max-memory`, `hotplug-disks` and `flags` (more spin-machine boot
flags). `--opt cpus=2 --opt memory=2048 --opt max-cpus=0 --opt max-memory=0 --opt hotplug-disks=0
--opt plugged=0 --opt login=lane` is the shape the probes booted before they booted spin's. Every
table and record says the shape it measured. A record that names none booted that older shape.

Two options make a boot spin's after it starts as well. `plugged=MIB` (default 2048) sizes the
guest through virtio-mem right after its start. spin sizes every workspace the same way before
handing it its filesystem, to 2048 MiB unless the workspace asks for more. Left at 128 MiB
(`plugged=0`), the guest's OOM killer runs through the boot and the logins (lab 44784196287).
`login=spin`, the default, logs in as spin does: as root, through a forwarder from vsock to
`ssh.service` on 127.0.0.1:22 (`internal/lab/login.go`, `spinsPath`). `login=lane` logs in as the
user spin through systemd's vsock socket and one `sshd@` for each connection. That is the path the
probes took until 2026-11-20, and the one the `floor` edit's sshd serves, so a floor run passes
`login=lane`.

Each release's own numbers are kept in the repository, one record for each release in
`docs/releases/`. `docs/releases.md` is the page written from them, with its chart,
`docs/releases.svg` (docs/releasing.md, "The record"). The same page is published at
https://spin-stack.github.io/spin-machine/:

```
spin-machine-dev releases measure [TAG...] [--reps N]            each release with the one before it, here: a ladder.json each
spin-machine-dev releases record LADDER_JSON... [--run URL]       the record of each ladder's newest release, and the page
spin-machine-dev releases missing                                 the published releases with no record, oldest first
spin-machine-dev releases show RELEASE                            a record: its numbers, where, when, in what shape and against what
spin-machine-dev releases compare A B                             two records side by side: B's Δ when it was measured against A
spin-machine-dev releases doc [--check]                           docs/releases.md and .svg from the records; --check exits 1 when either differs
spin-machine-dev releases site --out OUT                          the same as a web page, OUT/index.html, each record beside it in OUT/releases
spin-machine-dev releases propose                                 the records this checkout wrote, as a pull request against main
```

`measure` runs a ladder of two for each release: the published release before it, and the release.
report.yml does the same when a release is published. `measure` keeps each ladder.json in
`_output/lab/measured/TAG` (`lab show` reads that directory), and `record` writes the record from
it. They are two verbs because report.yml runs them at two commits: it measures at the release's
tag and records with main's code, and main's test holds the records to main's page. The record is
the release's, with an `against` block: the release before it, its numbers from the same run, and
the Δ of each number with its interval. The page's Δ comes only from that block. It is blank for a
record measured alone until that record is measured again, and every record was measured alone
before records were measured in pairs.

`measure` over many tags, oldest first, backfills: it measures each tag with its own predecessor.
`compare A B` prints the pair from B's run when B was measured against A. Otherwise it prints both
records, each from its own run, with no Δ, and the `lab ladder` that measures them together.
`record` keeps only each ladder's newest release, against the release before it in that ladder, so
a ladder measured some other way records the same. `propose` commits what `git status` shows
changed under `docs/releases`, with the page and its chart, on a branch named for those releases.
A branch already on origin is a set proposed once, and `propose` does not propose it again. `site`
writes one HTML file that fetches nothing, from the same rows as the page's tables, with the chart
inline so its points' tooltips show. `pages.yml` runs `site` on every merge to main that changes a
record, and deploys the file to GitHub Pages. Every verb takes `--dir` for the records' directory
(default `docs/releases`), with the page and its chart beside it as `DIR.md` and `DIR.svg`.

`lab compat` resumes a checkpoint of each release here (`lab.ResumeHere`), newest first, and `task boot:compat` is
`lab compat` over a built release. It checks that a host that skipped releases still resumes their
checkpoints. It checks each release it fetches against its sha256 and its signature, as the lab
checks every release a tree is made from. Each release's checkpoint is taken at spin's shape with
this tree's `spin-machine save`, so its record is what that release's QEMU reports. It stops at the
first release whose checkpoint this machine refuses (`machine.CanResume`), and that row names every
difference. On the way, it holds `CanResume` to QEMU's one-way promise against each release's
QEMU.

`ssh-first` and `restore-first` are the goal's two numbers. `prestart` is a QEMU started ahead
against today's, and `initcalls` is the kernel's boot by initcall. Every probe takes the shape's
options (above) and its own: `restore-first` takes `fast=true` and `channels`, `prestart` takes
`channels`, and `initcalls` takes `top=N`. The
lab refuses an option the probe does not take. A probe that restores (`restore-first`, `prestart`,
`restore-together`) hands every restore its tree's machine record (`spin-machine restore
--target`). The record is read once per tree, before anything is measured, from a machine of the
tree held for a checkpoint in the probe's shape, as spin reads its host's once. A tree whose
spin-machine has no `target` command, every release before the record, restores as it always did,
and its restore asks the machine on its clock. `restore-first`'s cached row reads only the data of
the saved state into the page cache (`SEEK_DATA`). A mapped-ram state is a file as long as the RAM
ceiling, and its data is all QEMU loads from it. The row's `listening` is the restoring QEMU's
monitor accepting, not its machine made.

`restore-together` measures whether a checkpoint's channels hold up when a host restores several
workspaces at once. It has a row for each number in `together` (default `1 4 8`) and each count in
`channels` (default `1 8 16`). An empty `channels` means the tree's spin-machine's own count. A
measurement saves that many checkpoints, each of its own overlay, and starts a QEMU ahead for each
with `--restore`. It evicts every state from the page cache, as `restore-first`'s evicted row does,
and lets every `spin-machine restore` go at once. The table's `login` is each measurement's median,
and `slowest` its slowest. Under the table, the probe pools every restore's first login, one sample
each, and prints each row's p50 against the same number at once with the first count. Restores let
go together are not independent, so that interval is narrower than the truth. A lost restore loses
its measurement.

`states=cached` reads every state into the page cache instead, which takes the disk out of the
burst. `states=evicted cached` runs both, and `evicted` is the default. `defrag` names the host's
THP defrag policies, each the rows over again. The probe sets
`/sys/kernel/mm/transparent_hugepage/defrag` through sudo for each burst and puts it back after.
Under the pooled logins, the table prints for each row what the host counted during its bursts:
the memory free and cached going in, CPU busy, MiB read, the PSI stall time of CPU, I/O and memory,
and the page allocations that waited on reclaim or compaction or went without a huge page. It also
prints where on the host's CPUs the bursts ran: the busiest CPU and its share of the busy time, CPU
0's busy time against the others' mean, the CPU taking most interrupts with its share and its
busiest lines, a histogram of the CPUs QEMU's running threads were found on (sampled every 5 ms),
the QEMUs' and the job's `Cpus_allowed_list`, and whether irqbalance runs. `result.json` holds each
burst's counts, with every restore's `running`.

`density` is a workspace's first login with others already running on the host. It has a row for
each number in `crowds` (default `0 8 16 32`). The running workspaces are idle, or keep their vCPUs
busy with `load=cpu`, and settle for `settle` seconds (default 10) before the measured boot.

`transport` says how much of each goal number QMP and vsock are. It boots and restores as
`restore-first` does, with QEMU traced (QMP and the run state, in host time). From the trace it
reads which commands each path waits on: `cont` for a cold boot, and `migrate-incoming` and what
follows it for a restore. It also reads what each command took, and how long QEMU waited for
spin-machine's next command. After the cold login it times `echoes` round trips (default 200) of
query-status on the monitor, of a one-byte echo over vsock, and of the same echo through a hop like
spin's forwarder. It also counts a warm login's round trips. The detail under its table lists each
path's commands by place. A traced cold boot runs a shell more than `ssh-first`, so its
`cold_login` compares only with another traced boot.

`lab dispatch` starts lab.yml once no other lab run is queued or running, waits for it, and prints
its result. The bench fleet measures one run at a time, because two runs side by side measure each
other. `lab dispatch` is the way to compare. Use `--host bench` (nested, the default) unless the
question is nested against metal. `--ref` names the branch whose lab.yml and probes run (default
`main`). `lab run` exits 1 when a measurement was lost (a machine that never answered, counted in
the table's LOST) and 2 when the experiment could not run.

What is not a probe is a check, pass or fail, and stays a Go test with its `boot:` target:
`firmware`, `record`, `compat` (every release whose machine is this one, through `lab compat`),
`hotadd`, `logind` and `lockdown`. Two probes not yet ported stay Go tests too: `firmware-stages` and `waits`.
`lab check TARGET` runs one on any tree here. lab.yml's `check:TARGET` runs `firmware`,
`firmware-stages`, `waits` and `record`, the targets its `probe` input lists.

lab.yml's experiment is one step, `task lab:ci`, after `task vsock`. The step makes the trees its
`trees` input names (`NAME=SPEC; NAME=SPEC`), runs the probe, puts the table on the run's summary,
and keeps `result.json` as the artifact `lab-RUN_ID`, which `lab show RUN_ID` reads.
