# The QEMU every spin-stack guest runs on. One build, for every consumer.
#
# spinbox and storage each had a Dockerfile.qemu pinning 11.1.1, building
# --target-list=x86_64-softmmu against a device allowlist that was byte-for-byte the same
# list. They differed in flags, and in nothing else. Two builds of the same emulator is not
# duplication that costs disk: spinbox decides whether a VM may be restored from a template
# by hashing the QEMU binary's *contents* (internal/host/vm/qemu/template.go), so two
# binaries are two machines, and a template built on one is undefined on the other.
#
# The flags here are the union of the two lists. Where they disagreed:
#
#   --enable-tools   storage needed qemu-img (its Agent shells out to it for every qcow2
#                    chain) and spinbox's build did not ship one, which is why
#                    `spinbox run` had to grow a --qemu-img flag pointing at storage's
#                    build. Enabled: qemu-img ships, and that flag can go.
#
#                    qemu-nbd comes out of the same --enable-tools and is deliberately
#                    not shipped: nothing in either project runs one. It exports a disk
#                    over NBD, which is a way of reaching an image this stack does not
#                    use — every qcow2 here is opened by QEMU itself or by qemu-img.
#   TCG              built twice, below. A host serving tenants must refuse to run without
#                    /dev/kvm rather than emulate; CI has no /dev/kvm and must emulate.
#   vmdk             storage disables it. spinbox cannot yet: containerd's erofs
#                    snapshotter hands it `merged.vmdk` and --disable-vmdk turns every VM
#                    into `Unknown driver 'vmdk'` at start-up. Kept until the qcow2 base
#                    image (image/) replaces that path — then add --disable-vmdk here, and
#                    know that doing so invalidates every template in the fleet.
#
# Two flags the briefing for this repository listed as storage's and that storage does not
# in fact pass, checked against storage/Dockerfile.qemu on 2026-09-07:
#   --enable-vhost-user-blk-server   storage disables it; the vhost-user data path went
#                                    with its local block engine.
#   --without-default-features       storage's own comment rejects it: every device q35
#                                    needs would then be ours to enumerate, and a missing
#                                    one is a link error or an unusable machine.
# Neither is enabled here.

FROM ubuntu:26.04 AS builder

ARG QEMU_VERSION=11.1.1
ARG JOBS=8
# Bump when the configure flags or qemu/devices.mak change: the build directories live in
# cache mounts and configure only runs when one holds no config.status, so without this a
# flag change would silently reuse the previous configuration.
ARG QEMU_CONFIG_REV=1
ARG DEBIAN_FRONTEND=noninteractive

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Keep .debs so the cache mounts below are useful.
RUN rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

RUN --mount=type=cache,sharing=locked,id=qemu-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=qemu-aptcache,target=/var/cache/apt \
    apt-get update && apt-get upgrade -y && \
    apt-get --no-install-recommends install -y \
        build-essential \
        meson \
        ninja-build \
        pkg-config \
        python3 \
        python3-venv \
        python3-tomli \
        bash \
        coreutils \
        curl \
        ca-certificates \
        xz-utils \
        libglib2.0-dev \
        libglib2.0-0 \
        zlib1g-dev \
        libpixman-1-dev \
        linux-libc-dev \
        libcap-ng-dev \
        libseccomp-dev \
        libseccomp2 \
        librdmacm-dev \
        acpica-tools \
        libbpf-dev \
        libpmem-dev \
        libaio-dev \
        liburing-dev \
        git \
        flex \
        bison \
        autoconf \
        automake \
        libtool \
        libffi-dev \
        libelf-dev \
        libdw-dev \
        patch && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Source tarball + extracted tree live in a cache mount, shared across builds.
RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    if [ ! -f "/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz" ]; then \
        curl -fsSL "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" \
             -o "/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz"; \
    fi && \
    if [ ! -d "/var/cache/qemu/qemu-${QEMU_VERSION}" ]; then \
        tar -xf "/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz" -C /var/cache/qemu; \
    fi

# The device set (see qemu/devices.mak) and the flags both builds share.
#
# ENV rather than a list repeated twice: the two builds below differ in exactly one thing,
# and a copied flag list is a place for them to drift into differing in two.
COPY qemu/devices.mak /build/spin-devices.mak

ENV QEMU_FLAGS="\
    --target-list=x86_64-softmmu \
    --with-devices-x86_64=spin \
    --enable-system \
    --disable-user \
    --enable-tools \
    --enable-kvm \
    --enable-linux-aio \
    --enable-linux-io-uring \
    --enable-cap-ng \
    --enable-seccomp \
    --enable-lto \
    --enable-strip \
    --disable-containers \
    --disable-docs \
    --disable-debug-info \
    --disable-werror \
    --disable-plugins \
    --disable-xen \
    --disable-xen-pci-passthrough \
    --disable-whpx \
    --disable-hvf \
    --disable-nvmm \
    --disable-vhost-user-blk-server \
    --disable-sdl \
    --disable-gtk \
    --disable-curses \
    --disable-opengl \
    --disable-virglrenderer \
    --disable-spice \
    --disable-vnc \
    --disable-png \
    --disable-vte \
    --disable-xkbcommon \
    --disable-brlapi \
    --disable-alsa \
    --disable-coreaudio \
    --disable-oss \
    --disable-pa \
    --disable-jack \
    --disable-sndio \
    --disable-pipewire \
    --disable-slirp \
    --disable-passt \
    --disable-vde \
    --disable-netmap \
    --disable-gio \
    --disable-curl \
    --disable-libssh \
    --disable-libnfs \
    --disable-libiscsi \
    --disable-rbd \
    --disable-vvfat \
    --disable-vdi \
    --disable-qed \
    --disable-qcow1 \
    --disable-bochs \
    --disable-cloop \
    --disable-dmg \
    --disable-parallels \
    --disable-vhdx \
    --disable-vpc \
    --disable-tpm \
    --disable-libusb \
    --disable-usb-redir \
    --disable-smartcard \
    --disable-u2f \
    --disable-canokey \
    --disable-guest-agent \
    --disable-guest-agent-msi \
    --disable-capstone \
    --disable-fuse \
    --disable-fuse-lseek \
    --disable-multiprocess \
    --disable-replication \
    --disable-rdma \
    --disable-libdw \
    --disable-gettext \
    --disable-virtfs \
    --disable-hv-balloon"

# What is *not* in that list, and why, because the tempting ones fail late:
#   pixman, glib, gnutls/nettle  — q35's display and qcow2's crypto still want them.
#   fdt                          — MICROVM depends on it (hw/i386/Kconfig).
#   CONFIG_CXL                   — not offered as optional by upstream; turning it off
#                                  broke the 11.0.2 link (pxb_cxl_realize, acpi_build).
#   --disable-vmdk               — see the header. spinbox still opens merged.vmdk.

# The guard is config.status and not Makefile, and the directory is emptied before a
# configure runs. configure writes the Makefile early and config.status only once it has
# finished, so a configure that failed leaves a build directory that looks configured and a
# `make` that dies at `./config.status: No such file or directory` — in a cache mount, on
# every later build, until somebody thinks to prune it.
#
# Two builds, differing in one flag: whether TCG is compiled in.
#
# /opt/qemu is what a host serving tenants runs, and it cannot emulate. A machine with no
# /dev/kvm gets an error at start-up instead of a guest at a tenth of the speed producing
# timings that read like measurements.
#
# /opt/qemu-tcg is for CI, where the runner has no /dev/kvm. Only its system binary is
# kept: qemu-img links no accelerator at all, so there is one of it and it comes from the
# build above.
RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    --mount=type=cache,sharing=locked,id=qemu-build-kvm-${QEMU_VERSION}-${QEMU_CONFIG_REV},target=/build/qemu-build-kvm \
    cp /build/spin-devices.mak \
       "/var/cache/qemu/qemu-${QEMU_VERSION}/configs/devices/x86_64-softmmu/spin.mak" && \
    mkdir -p qemu-build-kvm && cd qemu-build-kvm && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        /var/cache/qemu/qemu-${QEMU_VERSION}/configure \
            --prefix=/opt/qemu \
            --disable-tcg \
            ${QEMU_FLAGS}; \
    fi && \
    make -j${JOBS} && \
    make install

RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    --mount=type=cache,sharing=locked,id=qemu-build-tcg-${QEMU_VERSION}-${QEMU_CONFIG_REV},target=/build/qemu-build-tcg \
    cp /build/spin-devices.mak \
       "/var/cache/qemu/qemu-${QEMU_VERSION}/configs/devices/x86_64-softmmu/spin.mak" && \
    mkdir -p qemu-build-tcg && cd qemu-build-tcg && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        /var/cache/qemu/qemu-${QEMU_VERSION}/configure \
            --prefix=/opt/qemu-tcg \
            --enable-tcg \
            ${QEMU_FLAGS}; \
    fi && \
    make -j${JOBS} && \
    make install

# The firmware this machine boots with, and not `pc-bios/*`: that directory is 331 MB, of
# which 317 MB is edk2 for aarch64, arm, riscv and loongarch, in a build whose target list
# is x86_64-softmmu — and it was being copied into the image and out again on every build.
#
# Five files, and each is here because something stops without it:
#   pvh.bin        the guest kernel is an ELF with Xen PVH notes and QEMU enters it through
#                  this ROM. No real mode, no bzImage protocol, no bootloader, no UEFI.
#                  Missing, it fails as a rom-open error that reads like a backend bug.
#   kvmvapic.bin   loaded by the KVM accelerator on a q35.
#   bios.bin       the SeaBIOS payload the machine is built around; bios-256k.bin is what
#                  q35 actually loads.
#   efi-virtio.rom the option ROM of the one NIC this build keeps. A device model without
#                  its ROM does not slow a VM down, it stops it starting — `failed to find
#                  romfile`, at realize, before any guest code runs.
#
# Not here: vgabios-stdvga.bin (no display adapter is created — every machine line is
# -nodefaults -nographic) and linuxboot_dma.bin (the option ROM for a bzImage boot, which
# nothing here does). A caller that adds a device back has to add its ROM back with it.
RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    mkdir -p /build/pc-bios && \
    for f in bios.bin bios-256k.bin pvh.bin kvmvapic.bin efi-virtio.rom; do \
        install -m 0644 "/var/cache/qemu/qemu-${QEMU_VERSION}/pc-bios/$f" "/build/pc-bios/$f"; \
    done

# Fail the build here, not in a lane three phases later, if this configuration did not
# produce the devices, the tools and the two accelerators this release ships. The union of
# what spinbox's and storage's builds each asserted, because the artefact is now one.
#
# The last two lines are the split itself, and neither half is visible in a file listing:
# the binary a tenant's host runs must refuse to emulate, and CI's must be able to. Getting
# them backwards is a fleet quietly running at a tenth of the speed, or a CI lane that
# cannot boot at all.
RUN Q=/opt/qemu/bin/qemu-system-x86_64 && \
    $Q -device help 2>&1 | grep -q 'virtio-blk-pci' && \
    $Q -device help 2>&1 | grep -q 'virtio-net-pci' && \
    $Q -device help 2>&1 | grep -q 'vhost-vsock-pci' && \
    $Q -device help 2>&1 | grep -q 'virtconsole' && \
    ! $Q -device help 2>&1 | grep -qE '^name "(e1000e?|rtl8139|pcnet|vmxnet3|igb|ne2k_pci|tulip)"' && \
    $Q -machine help | grep -q '^q35 ' && \
    ! $Q -machine help | grep -q '^pc-i440fx' && \
    test -x /opt/qemu/bin/qemu-img && \
    /opt/qemu/bin/qemu-img --version && \
    $Q --version && \
    /opt/qemu-tcg/bin/qemu-system-x86_64 --version && \
    ! $Q -accel help | grep -qx tcg && \
    /opt/qemu-tcg/bin/qemu-system-x86_64 -accel help | grep -qx tcg

# qcow2 is the base image's format and the overlay's, and vmdk is the format the erofs
# snapshotter still hands spinbox. Both are configure flags with no device to hold them up,
# so a build that dropped one produces a VM that dies at start-up with
# `Unknown driver` — after the machine is created, in a lane a long way from here.
#
# The whole help text is flattened before it is searched, and not grepped line by line:
# qemu-img wraps the format list across several lines, so `grep 'Supported formats:.*vmdk'`
# matches only if vmdk happens to land on the first one. It did not, and this check failed
# a build whose every other assertion had passed (2026-09-07).
RUN help=$(/opt/qemu/bin/qemu-img --help 2>&1 | tr '\n' ' ') && \
    for f in qcow2 vmdk raw; do \
        case " $help " in \
            *" $f "*) ;; \
            *) echo "ERROR: this qemu-img cannot open $f images"; exit 1 ;; \
        esac; \
    done && \
    echo "OK: qemu-img opens qcow2, vmdk and raw" 

RUN file /opt/qemu/bin/qemu-system-x86_64 && \
    echo "Dynamic library dependencies:" && \
    ldd /opt/qemu/bin/qemu-system-x86_64

# ============================================
# Runtime image
# ============================================
#
# The binaries are dynamically linked against this build image's libraries, so an extracted
# tree does not run on a bare runner (`liburing.so.2: cannot open shared object file`).
# This stage is the answer to that for consumers who can run a container; the extract stage
# below is for those who cannot, and storage's `task qemu:tools` bridges the two.
FROM ubuntu:26.04 AS runtime

ARG DEBIAN_FRONTEND=noninteractive

RUN rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# libaio1t64 and not libaio1: Ubuntu's 64-bit time_t transition renamed the runtime
# package, so the old name resolves to nothing and `apt-get install` exits 100. The builder
# stage takes the -dev packages, whose names did not change, which is why a build can
# succeed while this stage cannot be assembled at all.
RUN --mount=type=cache,sharing=locked,id=qemu-runtime-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=qemu-runtime-aptcache,target=/var/cache/apt \
    apt-get update && \
    apt-get --no-install-recommends install -y \
        libglib2.0-0 \
        libpixman-1-0 \
        libcap-ng0 \
        libseccomp2 \
        libaio1t64 \
        liburing2 \
        libpmem1 \
        librdmacm1t64 \
        zlib1g && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/qemu/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64
COPY --from=builder /opt/qemu-tcg/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64-tcg
COPY --from=builder /opt/qemu/bin/qemu-img /usr/local/bin/qemu-img
COPY --from=builder /build/pc-bios/ /usr/share/spin-stack/qemu/

ENTRYPOINT ["/usr/local/bin/qemu-system-x86_64"]

# ============================================
# Extract stage: the tree onto the host (_output/)
# ============================================
FROM scratch AS extract

COPY --from=builder /opt/qemu/bin/qemu-system-x86_64 /bin/qemu-system-x86_64
COPY --from=builder /opt/qemu-tcg/bin/qemu-system-x86_64 /bin/qemu-system-x86_64-tcg
COPY --from=builder /opt/qemu/bin/qemu-img /bin/qemu-img
COPY --from=builder /build/pc-bios/ /share/spin-stack/qemu/
