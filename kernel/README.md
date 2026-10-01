# Guest kernel

A version and a config file (`config-<version>-<arch>`). It is here because a kernel and the
machine that boots it are one release: changing either strands every checkpoint taken
against the previous pair. `task kernel:build` builds it; `task kernel:verify` reads the ELF
notes out of the kernel it just stripped.

## Memory that moves

A VM given a ceiling gets a `virtio-mem` device covering the gap between its boot size and
that ceiling, with nothing plugged. The host grows and shrinks it over QMP; there are no
ACPI DIMM slots, because a DIMM can be added and, in practice, not removed — unplugging one
needs the guest to offline a whole memory block and a single unmovable page in it makes
that fail. Measured on a 1 GiB VM with a 4 GiB ceiling: 0 → 3072 MiB and back to 2 MiB.

The kernel command line carries `memhp_default_state=online` for it. Without that the
growth stops at the boot size and says nothing: memory added and never onlined is memory
the guest cannot use but must still describe, so the driver declines to take more — asked
for 2048 and then 3072 MiB, it plugged 1024 and stayed there.

Separately, every VM gets a `virtio-balloon-pci` with `free-page-reporting=on`, which is
how a guest hands back memory it merely stopped using. The two answer different questions:
the balloon returns what is free, virtio-mem changes how much there is.

## BPF

The kernel carries what modern BPF development needs, and it did not before: `BPF_JIT`
(every program ran interpreted), `DEBUG_INFO_BTF` (without it there is no
`/sys/kernel/btf/vmlinux`, so no `vmlinux.h`, no `bpftool btf dump` and no CO-RE at all),
`FUNCTION_TRACER` for fentry/fexit attachment, `FTRACE_SYSCALLS`, and the networking program
types — `NET_CLS_BPF`, `NET_ACT_BPF`, `NET_SCH_INGRESS`, `XDP_SOCKETS`, `BPF_STREAM_PARSER`.
Verified in a guest: `/sys/kernel/btf/vmlinux` is 4,552,359 bytes and
`net.core.bpf_jit_enable` is 1.

It costs **+21.4 ms** of kernel boot, measured over five runs each against the same config
without those symbols (137.4 ms to 158.8 ms, kernel start to `Freeing unused kernel image`),
and 5.0 MB of image — 4.55 MB of which is the `.BTF` section, which `strip -s` keeps because
it is allocated and the guest reads it back out of its own image. `ftrace: allocating 41727
entries` is the new work that shows up in the log.

## Lockdown and the BPF LSM

The guest's root is not the kernel's. The kernel runs two LSMs, `lockdown` and `bpf`
(`CONFIG_LSM="lockdown,bpf"`):

- **Lockdown** is forced to *confidentiality* from the first instruction, and no write to
  `/sys/kernel/security/lockdown` lowers it. Root can then neither rewrite the running kernel
  (kexec, `/dev/port`, MSRs, ACPI table overrides) nor read it (kprobes, `bpf_probe_read_kernel`,
  perf on the kernel).
- **`/dev/mem` and `/proc/kcore`** are not built at all, rather than left to lockdown.
- **The BPF LSM** is there so a consumer can enforce policy in the guest with programs it loads
  before handing the machine to its tenant. Lockdown is what makes such policy hold: without
  it, root reads or rewrites the kernel those programs live in.

What enforcing the policy is for, and how, is the consumer's. That this kernel does both is
what `kernel/Dockerfile` checks after olddefconfig, and `task boot:lockdown` asks a booted guest.

Lockdown at confidentiality also takes away the tenant's kernel tracing: kprobes,
`bpf_probe_read_kernel` and perf on the kernel. User-space tracing and networking eBPF are
untouched. systemd notices at boot: it logs `use of bpf to read kernel RAM is restricted` and
carries on, and logins are unaffected (`task boot:logind`).

Measured 2026-10-01, `task boot:initcalls` with `SPIN_KERNEL_B` set to the kernel without these
options: no cost that can be told from noise. Over 20 interleaved boots each, kernel start to
`Freeing unused kernel image` had a p50 of 196.3 ms with them and 208.3 ms without, on a host at
load 6-10, where the p95 was 342 ms.

## Changing it

Every change to the config strands every checkpoint in the fleet, which is the design —
it should not be quiet. The kernel workflow runs `hack/fingerprint-diff` and puts the
answer in its summary.

`Dockerfile` builds with a pinned toolchain — Debian trixie by image digest, its packages from
snapshot.debian.org at a fixed date — because a different compiler is a different `vmlinux`,
which is a different fingerprint. Both are `versions.yaml` entries (`debian`,
`debian-snapshot`), and they move on purpose, not when Debian publishes a point release.

And the build is reproducible: two builds from the same inputs produce the same `vmlinux`,
byte for byte. That needs trixie's pahole 1.30, which is deterministic with `-j`. Bookworm's
1.24 encodes `.BTF` in whatever order make's `-j` threads finish, which gives every release a
new fingerprint whether or not the kernel changed.
