# The guest kernel.
#
# It is here, next to the QEMU binary and the base image, because a kernel and the machine
# that boots it are one release: `machine.Spec.Fingerprint` hashes this image by content
# alongside the emulator, so changing either invalidates every template taken against the
# previous pair. They are coupled by what they are, and storing them apart only hides it.
#
# The build depends on no source but its own: a version and a config file.
#
# What is NOT here: an initrd. What runs inside a guest belongs to whoever runs guests —
# this repository builds the machine and boots it far enough to prove it works.
#
# The config is not stable furniture. Symbols get added and removed for measured reasons —
# a device a restored VM needs to reseed from, a crypto driver worth milliseconds of boot —
# and every one of those changes invalidates every template in the fleet. That is the
# design, and it makes a change here a fleet-wide event rather than a tidy-up.
#
# Base image: debian bookworm, and deliberately not something newer. A different compiler
# is a different vmlinux, which is a different machine fingerprint, which is every template
# invalidated. That may be a change worth making; it is not one to make by accident.

FROM debian:bookworm AS kernel-build-base

ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

RUN --mount=type=cache,sharing=locked,id=kernel-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=kernel-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential libncurses-dev flex bison libssl-dev libelf-dev bc cpio git \
        wget xz-utils curl lz4 ca-certificates file \
        # The four that CONFIG_DEBUG_INFO_BTF brings with it, and none of them is obvious
        # from the symbol's name:
        #
        #   dwarves      provides pahole, which turns the DWARF the compiler emits into the
        #                .BTF section. Missing, the build fails near the end with
        #                `BTF: .tmp_vmlinux.btf: pahole (pahole) is not available`.
        #   python3      BTF makes the kernel build tools/bpf/resolve_btfids, which builds
        #                a copy of libbpf, which generates bpf_helper_defs.h with a python
        #                script. Missing, that fails as exit 127 — a "command not found"
        #                three Makefiles deep, naming a header.
        #   zlib1g-dev,
        #   pkg-config   what that same libbpf links and finds itself with.
        dwarves python3 zlib1g-dev pkg-config \
        # apparmor and procps are not kernel build dependencies: they are what
        # check-docker-config.sh below shells out to. Without apparmor_parser it reports
        # "apparmor: enabled, but apparmor_parser missing" and exits non-zero, failing a
        # configuration that is correct; without sysctl it prints three "sysctl: not found"
        # lines while inspecting the bridge driver.
        apparmor procps

ARG KERNEL_VERSION="7.2.1"
ARG KERNEL_ARCH="x86_64"
ARG KERNEL_NPROC="16"
# The tarball this kernel is made of, by content. See the same argument in qemu/Dockerfile:
# https says who sent it, not what they sent, and this is the one input that comes from
# outside the repository. It is also what makes the SOURCES file in a release true.
ARG KERNEL_SHA256=4158098c8a28ea8fbe068c855d3aa6341e03d06282add3cb9049f3a24e4557b3

# moby's check-config.sh, with the SELinux and AppArmor checks removed: they are host
# policy, not kernel features this guest needs, and they fail a config that is correct.
RUN curl -o /usr/local/bin/check-docker-config.sh -fsSL https://raw.githubusercontent.com/moby/moby/master/contrib/check-config.sh \
  && chmod +x /usr/local/bin/check-docker-config.sh \
  && sed -i '/IP_VS_RR/s/\\$//; /SECURITY_SELINUX/d; /SECURITY_APPARMOR/d' /usr/local/bin/check-docker-config.sh

WORKDIR /usr/src

RUN --mount=type=cache,sharing=locked,id=kernel-src-${KERNEL_VERSION},target=/var/cache/kernel \
    KERNEL_MAJOR="$(echo "${KERNEL_VERSION}" | cut -d. -f1)" && \
    tarball="/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" && \
    if [ ! -f "$tarball" ]; then \
        wget -O "$tarball" "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/linux-${KERNEL_VERSION}.tar.xz"; \
    fi && \
    # Every build, not only after a download: the cache mount outlives the build that
    # filled it.
    echo "${KERNEL_SHA256}  $tarball" | sha256sum -c - && \
    tar -xf "$tarball" -C /usr/src && \
    mv linux-${KERNEL_VERSION} linux

COPY kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH} /usr/src/linux/.config

# Every check below is a symbol that olddefconfig can silently drop and whose absence
# presents as something other than a missing kernel option.
RUN <<EOT
    set -e
    cd /usr/src/linux

    make ARCH=${KERNEL_ARCH} olddefconfig

    echo "Verifying critical kernel configs after olddefconfig..."
    grep -q "CONFIG_VIRTIO_NET=y" .config || (echo "ERROR: CONFIG_VIRTIO_NET not enabled after olddefconfig!" ; grep VIRTIO_NET .config ; exit 1)
    grep -q "CONFIG_VIRTIO_PCI=y" .config || (echo "ERROR: CONFIG_VIRTIO_PCI not enabled!" ; exit 1)
    grep -q "CONFIG_NET_CLS_ACT=y" .config || (echo "ERROR: CONFIG_NET_CLS_ACT not enabled!" ; exit 1)

    # Memory hotplug. Without these three the host can add a DIMM, QEMU will accept it,
    # query-memory-devices will show it, and /sys/devices/system/memory/memory<N>/online —
    # the file something in the guest has to write to bring it online — will not exist,
    # so the guest never sees the memory.
    grep -q "CONFIG_MEMORY_HOTPLUG=y" .config || (echo "ERROR: CONFIG_MEMORY_HOTPLUG not enabled (memory hotplug cannot reach the guest)!" ; exit 1)
    grep -q "CONFIG_MEMORY_HOTREMOVE=y" .config || (echo "ERROR: CONFIG_MEMORY_HOTREMOVE not enabled (unplug cannot work)!" ; exit 1)
    grep -q "CONFIG_ACPI_HOTPLUG_MEMORY=y" .config || (echo "ERROR: CONFIG_ACPI_HOTPLUG_MEMORY not enabled (a pc-dimm is never noticed)!" ; exit 1)

    # An lz4 initramfs. lz4 is among the fastest formats to decompress and unpacking the
    # archive is one of the largest single items in kernel boot; without this the kernel
    # does not recognise such an archive at all and the VM does not boot.
    grep -q "CONFIG_RD_LZ4=y" .config || (echo "ERROR: CONFIG_RD_LZ4 not enabled (the lz4 initramfs cannot be unpacked)!" ; exit 1)

    # The filesystems the base image and the guest's overlay are made of. ext4 is what
    # image/ produces and what a writable layer is formatted as; erofs is the read-only
    # half of the containerd path that qcow2 replaces; overlayfs stacks them. A base image
    # in a filesystem this kernel cannot mount is a VM that boots and finds no root, so
    # changing image/mkosi.conf to xfs or squashfs starts here, not there.
    grep -q "CONFIG_EXT4_FS=y" .config || (echo "ERROR: CONFIG_EXT4_FS not enabled (the base image cannot be mounted)!" ; exit 1)
    grep -q "CONFIG_EROFS_FS=y" .config || (echo "ERROR: CONFIG_EROFS_FS not enabled!" ; exit 1)
    grep -q "CONFIG_OVERLAY_FS=y" .config || (echo "ERROR: CONFIG_OVERLAY_FS not enabled!" ; exit 1)

    # PVH is the only boot path: there is no bootloader and no UEFI, and QEMU enters this
    # kernel through pvh.bin using the Xen notes CONFIG_PVH puts in the program headers.
    # Without it -kernel has nothing to enter and the VM does not start.
    grep -q "CONFIG_PVH=y" .config || (echo "ERROR: CONFIG_PVH not enabled (QEMU has no entry point into this kernel)!" ; exit 1)

    # BPF. Each of these is something whose absence looks like a broken program rather than
    # a missing kernel option, which is why they are checked rather than trusted.
    #
    #   BPF_JIT         without it every program runs in the interpreter — an order of
    #                   magnitude slower, and `bpf_jit_enable` is simply not there.
    #   DEBUG_INFO_BTF  the one that matters most. No BTF means no /sys/kernel/btf/vmlinux,
    #                   so no `bpftool btf dump`, no vmlinux.h to generate, and no CO-RE
    #                   relocations: a program built against one kernel will not load on
    #                   another, which is the entire point of modern BPF.
    #   FUNCTION_TRACER with the architecture's direct-call support it is what makes
    #                   fentry/fexit attachment possible; without it those programs fail to
    #                   attach with a message about the target.
    grep -q "CONFIG_BPF_JIT=y" .config || (echo "ERROR: CONFIG_BPF_JIT not enabled (every BPF program would run interpreted)!" ; exit 1)
    grep -q "CONFIG_DEBUG_INFO_BTF=y" .config || (echo "ERROR: CONFIG_DEBUG_INFO_BTF not enabled (no BTF means no CO-RE)!" ; exit 1)
    grep -q "CONFIG_FUNCTION_TRACER=y" .config || (echo "ERROR: CONFIG_FUNCTION_TRACER not enabled (fentry/fexit cannot attach)!" ; exit 1)
    # BPF_LSM is deliberately NOT enabled, and this is where the decision is written down
    # so it is not made again by accident. It depends on CONFIG_SECURITY, which is off here
    # and would put LSM hooks on paths this kernel is tuned for boot time on — and what it
    # buys is the ability to enforce access policy *inside* a VM that holds one workload,
    # which is a boundary drawn inside the boundary this machine already is. Somebody who
    # needs to develop BPF LSM programs turns on CONFIG_SECURITY and this, deliberately,
    # with a measurement, and accepts that it is a different machine.

    # Boot performance: the RAID6 PQ benchmark probes all SIMD implementations at boot to
    # pick the fastest. It must stay disabled. (RAID6_PQ itself is not selected today, so
    # the symbol is normally absent.)
    ! grep -q "CONFIG_RAID6_PQ_BENCHMARK=y" .config || (echo "ERROR: CONFIG_RAID6_PQ_BENCHMARK must not be enabled (boot perf)!" ; exit 1)

    echo "Virtio configuration:"
    grep "CONFIG_VIRTIO" .config | grep -v "^#" || echo "No VIRTIO options enabled!"

    echo "Verifying kernel config for Docker support..."
    /usr/local/bin/check-docker-config.sh /usr/src/linux/.config || (echo "Kernel config verification failed!" ; exit 1)

    echo "Using kernel config from kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH}"
EOT

FROM kernel-build-base AS kernel-build

ARG KERNEL_ARCH
ARG KERNEL_NPROC

# No cache mount: kernel compilation is not incremental between builds, so Docker's layer
# cache is the effective one.
RUN cd linux && make ARCH=${KERNEL_ARCH} -j${KERNEL_NPROC} all

RUN <<EOT
    set -e
    cd linux
    mkdir /build
    cp .config /build/kernel-config

    if [ "${KERNEL_ARCH}" != "x86_64" ]; then
        echo "ERROR: Only x86_64 architecture is supported, got: ${KERNEL_ARCH}"
        exit 1
    fi

    # Strip the ELF symbol table before handing the image to QEMU. QEMU's load_elf() reads
    # and qsort()s the symbol table of every kernel it loads — 42k entries here — and
    # nothing in this stack ever asks it for a symbol: the guest resolves its own names
    # through CONFIG_KALLSYMS, which lives in the loaded image data and is untouched by
    # this.
    #
    # PT_LOAD is byte-identical either way (30.68 MB), so the guest boots the same bits;
    # what goes away is 5.1 MB of file and the parse and sort of it. The PVH ELF notes this
    # machine boots from are in the program headers and survive the strip.
    #
    # Measured: qemu_launch 36.1 ms -> 24.3 ms, guest boot unchanged.
    cp vmlinux /build/vmlinux
    strip -s /build/vmlinux

    # The strip is the one step here that could plausibly remove what QEMU needs, and it is
    # asserted rather than reasoned about: no PVH note, no entry point, and the failure is
    # a VM that does not start.
    readelf -n /build/vmlinux | grep -qi 'XenVersion\|Xen' || {
        echo "ERROR: the stripped vmlinux carries no Xen PVH notes — QEMU cannot enter it"; exit 1; }

    # And .BTF, for the same reason and with a longer fuse. `strip -s` removes the symbol
    # table and the DWARF the compiler emitted — which is the point, since neither is read
    # at run time — but .BTF is an allocated section the *guest* reads back out of its own
    # image and publishes at /sys/kernel/btf/vmlinux. Losing it here would not break the
    # build or the boot; it would break every CO-RE program somebody writes next month.
    readelf -SW /build/vmlinux | grep -q '\.BTF' || {
        echo "ERROR: the stripped vmlinux has no .BTF section — CO-RE would not work in the guest"; exit 1; }
    echo "BTF: $(readelf -SW /build/vmlinux | awk '$2 == ".BTF" {print strtonum("0x" $6)}') bytes"
EOT

# ============================================
# Output stage
# ============================================
#
# Named vmlinux, which is what the ELF is. hack/release can add a second name beside it
# for a consumer that resolves the kernel differently; see COMPAT_KERNEL_NAME there.
FROM scratch AS kernel
COPY --from=kernel-build /build/vmlinux /vmlinux
COPY --from=kernel-build /build/kernel-config /kernel-config
