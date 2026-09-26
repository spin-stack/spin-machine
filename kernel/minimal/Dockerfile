# The floor: the smallest kernel that still boots this machine, to measure what a kernel
# costs before anything is configured into it.
#
# ## Why a floor and not another removal
#
# Removing one subsystem at a time from the release config has now found zero four times in
# a row — CONFIG_VT, nineteen udev rule files, two device-count boot parameters, smaller TCP
# hash tables — and each of those cost a build or a bench run to learn. A floor says how much
# of the kernel phase is even available before anybody hunts for it: if the release kernel
# reaches the same marker in about what this one does, the config is not where the time is
# and the search should move. If the gap is large, the gap is the budget.
#
# ## Not a release kernel, and it could not be one
#
# Isolated from `task build` and `task release`, like qemu/qboot. It has no BTF, no BPF, no
# ftrace, no netfilter, no cgroups, no overlayfs, no ext4 and no virtio beyond nothing at
# all — so it cannot mount this machine's base image and cannot run systemd. That is the
# point of a floor, and it is why the comparison is made against a caller-supplied
# diagnostic initrd rather than against a login prompt: the marker has to be something both
# kernels can reach.
#
# Built from tinyconfig rather than by subtracting from the release config, because
# subtracting leaves whatever nobody thought to name — the same reason qemu/devices.mak is
# an allowlist.
ARG DEBIAN_IMAGE=debian:trixie@sha256:f324c7ff54321e8d9c588493a20244965938ce0aa50bbd1022d38010e9ffc4b1
ARG DEBIAN_SNAPSHOT=20260914T000000Z

FROM ${DEBIAN_IMAGE} AS builder

ARG DEBIAN_SNAPSHOT
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN rm -f /etc/apt/sources.list.d/debian.sources && cat > /etc/apt/sources.list.d/snapshot.sources <<EOF
Types: deb
URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
Check-Valid-Until: no
EOF

RUN --mount=type=cache,sharing=locked,id=kmin-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=kmin-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install -y --no-install-recommends \
        bc bison flex libelf-dev libssl-dev make gcc binutils xz-utils curl ca-certificates \
        pahole cpio

# Passed in rather than pinned again: kernel/Taskfile.yml reads both out of kernel/Dockerfile,
# which is the one copy. A second pin here would be a second thing to bump.
ARG KERNEL_VERSION
ARG KERNEL_SHA256
ARG KERNEL_ARCH=x86_64
ARG KERNEL_NPROC=8

WORKDIR /build
RUN curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" || \
    curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-${KERNEL_VERSION}.tar.xz"
RUN echo "${KERNEL_SHA256}  linux-${KERNEL_VERSION}.tar.xz" | sha256sum -c - && \
    tar -xf "linux-${KERNEL_VERSION}.tar.xz" && mv "linux-${KERNEL_VERSION}" linux

# tinyconfig, then back on exactly what a PVH guest needs to reach an initramfs and print.
#
# Every one of these is here because without it the guest produces no output and no error:
# PVH is the only entry QEMU has into this kernel, the 8250 is the only console, and an
# initramfs it cannot unpack is a panic nobody sees.
#
# BINFMT_SCRIPT is separate from BINFMT_ELF and tinyconfig leaves it off, which is the one
# that does not look like a missing kernel option. Without it the kernel cannot exec a file
# with a shebang, so `rdinit=/init` fails, the kernel falls through its default inits, finds
# /bin/sh in the initramfs and starts an interactive shell. What that looks like is a BusyBox
# banner and a prompt on the console and no sign of the init that was asked for.
#
# RD_LZ4 as well as RD_GZIP: lz4 is the format kernel/Dockerfile requires for a release,
# because unpacking the archive is one of the largest single items in kernel boot and lz4 is
# among the fastest to decompress. A floor kernel that could only read gzip would put a
# decompression this machine does not do inside every measurement.
RUN cd linux && \
    make ARCH=${KERNEL_ARCH} tinyconfig && \
    ./scripts/config \
        -e 64BIT -e SMP \
        -e PRINTK -e PRINTK_TIME \
        -e TTY -e SERIAL_8250 -e SERIAL_8250_CONSOLE \
        -e HYPERVISOR_GUEST -e PARAVIRT -e KVM_GUEST -e PVH \
        -e BLK_DEV_INITRD -e RD_GZIP -e RD_LZ4 \
        -e DEVTMPFS -e DEVTMPFS_MOUNT \
        -e PROC_FS -e SYSFS \
        -e BINFMT_ELF -e BINFMT_SCRIPT -e MULTIUSER \
        -e EPOLL -e SIGNALFD -e TIMERFD -e EVENTFD -e FUTEX && \
    make ARCH=${KERNEL_ARCH} olddefconfig

# Asserted before the compile, not after: a missing PVH note is twenty minutes of build and
# then a guest that halts with nothing on the console, which is the same symptom as half a
# dozen unrelated faults.
RUN cd linux && set -eux; \
    for sym in 64BIT SMP PRINTK TTY SERIAL_8250_CONSOLE PVH KVM_GUEST BLK_DEV_INITRD \
               RD_GZIP RD_LZ4 DEVTMPFS PROC_FS SYSFS BINFMT_ELF BINFMT_SCRIPT; do \
        grep -q "^CONFIG_${sym}=y" .config || { \
            echo "CONFIG_${sym} did not survive olddefconfig; this kernel cannot boot" >&2; \
            grep -n "CONFIG_${sym}" .config || true; exit 1; }; \
    done; \
    echo "symbols on: $(grep -c '=y$' .config), modules: $(grep -c '=m$' .config)"

RUN cd linux && make ARCH=${KERNEL_ARCH} -j${KERNEL_NPROC} vmlinux && \
    cp .config /build/kernel-config && \
    strip --strip-debug -o /build/vmlinux vmlinux

# What came out: the Xen PVH notes QEMU enters through, read from the stripped binary that
# will actually be booted rather than from the one before the strip.
RUN cd /build && \
    readelf -n vmlinux | grep -q "Xen" || { \
        echo "no Xen notes in the stripped vmlinux: QEMU has no entry point" >&2; exit 1; } && \
    ls -l vmlinux && readelf -n vmlinux | grep -A1 Xen | head -6

FROM scratch AS extract
COPY --from=builder /build/vmlinux /kernel-minimal/vmlinux
COPY --from=builder /build/kernel-config /kernel-minimal/kernel-config
