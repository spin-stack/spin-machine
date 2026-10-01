# The QEMU this machine runs on. One build, for every consumer, and statically linked.
#
# One and not several, and the reason is not disk. `machine.Spec.Fingerprint` hashes this
# binary's *contents* into the identity that decides whether a VM may resume from a
# checkpoint, so two binaries are two machines and a checkpoint taken on one is undefined on
# the other. A second build of "the same" QEMU with one flag moved is a second machine that
# looks identical in a file listing.
#
# ## Static, and why that is worth a different distribution
#
# A dynamically linked QEMU is not one artefact, it is an artefact plus the set of
# libraries it was compiled against. Extracted onto a machine that has different ones it
# dies at start-up — `liburing.so.2: cannot open shared object file` — so a release has to
# ship either a container to run it in or the loader and the libraries beside it, and a
# consumer has to know which. Static ends the question: the binary runs wherever the kernel
# does.
#
# That is why the builder is Alpine and musl rather than Ubuntu and glibc. glibc can be
# linked statically but does it badly — anything that resolves a user or a host name still
# wants to dlopen an NSS module at run time, and a static binary cannot. musl links
# statically as its normal mode.
#
# Measured before adopting it, on the same host, same kernel, same machine line: process
# start to a QMP `quit`, 44 ms glibc against 43 ms musl over five runs — no difference. A
# VM boots, `lspci` inside it shows the slot map this repository declares, io_uring, native
# aio and thread-pool aio all attach, and the accelerator split holds. What has *not* been
# measured is a long-running guest under load: musl's allocator and its 128 KiB default
# thread stack differ from glibc's, and if something surfaces it will surface there.
#
# ## The flags worth explaining
#
#   --static         see above. It requires every dependency as a .a, which is why the
#                    package list below asks for -static packages by name; a missing one
#                    fails at link time, twenty minutes in.
#   --enable-tools   ships qemu-img, which is not a convenience: a copy-on-write overlay
#                    over the base image is created by running it, so a caller with the
#                    emulator and no qemu-img cannot give a container somewhere to write.
#
#                    qemu-nbd comes out of the same flag and is deliberately not shipped:
#                    it exports a disk over NBD, which is a way of reaching an image
#                    nothing here uses — every qcow2 is opened by QEMU itself or by
#                    qemu-img.
#   TCG              built twice, below. A host serving tenants must refuse to run without
#                    /dev/kvm rather than emulate; a CI runner has no /dev/kvm and must.
#   git              not a build dependency of QEMU. meson wants to fetch dtc as a
#                    subproject when no libfdt is installed, and Alpine ships no static
#                    libfdt, so the subproject is how fdt gets built — and MICROVM depends
#                    on fdt (hw/i386/Kconfig). Without git: `Git program not found, cannot
#                    download dtc.wrap via git`, at configure.
#
# Two flags that look like obvious wins and are not:
#   --enable-vhost-user-blk-server   a device nothing asks for; a guest here gets an
#                                    ordinary virtio-blk-pci disk over a qcow2 QEMU opens.
#   --without-default-features       then every device q35 needs is ours to enumerate, and
#                                    a missing one is a link error or an unusable machine.

# The pins - the Alpine image by digest, QEMU's version and its tarball's sha256 - are
# versions.yaml's, handed in by qemu/Taskfile.yml. None has a default here: a build that was not
# handed one fails rather than building something the file does not say.
ARG ALPINE_IMAGE
# qboot's toolchain: see the qboot stage.
ARG DEBIAN_IMAGE

FROM ${ALPINE_IMAGE} AS builder

ARG QEMU_VERSION
# The tarball this build is made of, by content.
#
# https is a claim about who you are talking to, not about what they sent, and this
# download is the one input to the machine that comes from outside and is not read from a
# file in this repository. Without the check, an archive that served a different tarball —
# or a cache mount holding a truncated one from an interrupted build — is compiled and
# shipped, and the first sign is a fingerprint nobody can explain.
#
# It also makes usr/share/spin-stack/SOURCES in a release true rather than aspirational:
# what that file names is what was built.
ARG QEMU_SHA256
ARG JOBS=8
# Bump when the configure flags, the base image, or qemu/devices.mak change: the build
# directories live in cache mounts and configure only runs when one holds no config.status,
# so without this a change is silently applied to the previous configuration.
#
# "The base image" is in that list because it was left out once. Moving this build from
# Ubuntu to Alpine and adding --static without bumping the rev re-entered a build directory
# configured under the old base, skipped configure, relinked, and produced a dynamically
# linked binary from a Dockerfile that says --static on every other line. The assertion
# below is what caught it.
ARG QEMU_CONFIG_REV=6

SHELL ["/bin/sh", "-o", "pipefail", "-c"]

# Every -static package is load-bearing: with --static, a dependency present only as a .so
# fails at link time after the whole tree has compiled. liburing and libaio ship their .a
# inside the -dev package rather than a separate one, which is why they look inconsistent.
RUN --mount=type=cache,sharing=locked,id=qemu-apk,target=/var/cache/apk \
    apk add --cache-dir /var/cache/apk \
        build-base meson ninja pkgconf python3 py3-setuptools bash coreutils curl xz \
        linux-headers bison flex perl git acpica \
        glib-dev glib-static pcre2-dev pcre2-static libffi-dev \
        pixman-dev pixman-static zlib-dev zlib-static \
        libseccomp-dev libseccomp-static libcap-ng-dev libcap-ng-static \
        liburing-dev libaio-dev gettext-dev gettext-static

WORKDIR /build

# Source tarball + extracted tree live in a cache mount, shared across builds. Each step that
# reads the tree runs qemu/source.sh first and builds the tree it prints, which says why. The
# patches this QEMU carries are applied there; each says what it changes and what it was
# measured to be worth, and the release ships them beside the binary (hack/release).
COPY qemu/source.sh /build/qemu-source.sh
COPY qemu/patches/ /build/qemu-patches/
RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    sh /build/qemu-source.sh

# The device set (see qemu/devices.mak) and the flags both builds share.
#
# ENV rather than a list repeated twice: the two builds below differ in exactly one thing,
# and a copied flag list is a place for them to drift into differing in two.
COPY qemu/devices.mak /build/spin-devices.mak

ENV QEMU_FLAGS="\
    --static \
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
    --enable-vhost-net \
    --enable-vhost-kernel \
    --enable-vhost-user \
    --disable-qom-cast-debug \
    --enable-lto \
    --enable-strip \
    --disable-containers \
    --disable-docs \
    --disable-debug-info \
    --disable-werror \
    --disable-plugins \
    --disable-vmdk \
    --without-default-devices \
    --disable-xen \
    --disable-xen-pci-passthrough \
    --disable-whpx \
    --disable-nitro \
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

# The vhost three are asked for rather than left alone, and that is the point: meson
# defaults them to `auto`, so they are compiled in when the build image happens to carry
# linux/vhost.h and silently absent when it does not. Absent, `-netdev tap,vhost=on` fails
# at start-up and, worse, a machine built without it and run without the flag copies every
# packet through QEMU — a performance regression with no error anywhere. Named here, a base
# image that stops providing the headers fails the build instead.
#
# qom_cast_debug defaults to *true* in meson_options.txt. It checks the type of every QOM
# object cast at run time, which is a debugging aid this repository has no use for on a
# host running tenants' VMs.
#
#
# What is *not* in that list, and why, because the tempting ones fail late:
#   pixman, glib, gnutls/nettle  — q35's display and qcow2's crypto still want them.
#   fdt                          — MICROVM depends on it (hw/i386/Kconfig).
#   CONFIG_CXL                   — not offered as optional by upstream; turning it off
#                                  broke the 11.0.2 link (pxb_cxl_realize, acpi_build).


# Two guards, and the second was learned the hard way.
#
# The first is config.status and not Makefile, and the directory is emptied before a
# configure runs. configure writes the Makefile early and config.status only once it has
# finished, so a configure that failed leaves a build directory that looks configured and a
# `make` that dies at `./config.status: No such file or directory` — in a cache mount, on
# every later build, until somebody thinks to prune it.
#
# The second guard is the compiler. A cache mount outlives the base image it was filled
# from, and `alpine:3.22` is a moving tag: the day its gcc went from 14 to 15.2 the cached
# object files stopped being usable by the compiler now present, and the build died in the
# linker with
#
#   lto1: fatal error: bytecode stream in file '…link-test.c.o' generated with LTO
#   version 14.0 instead of the expected 15.1
#
# which names neither the cache nor the base image. QEMU_CONFIG_REV does not help: it
# guards the *configuration*, and nothing about the configuration changed. So the compiler
# version is recorded in the build directory and the directory is thrown away when it
# stops matching — the build heals instead of waiting for somebody to recognise a linker
# error as a stale cache.
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
    tree="$(sh /build/qemu-source.sh)" && \
    cp /build/spin-devices.mak "$tree/configs/devices/x86_64-softmmu/spin.mak" && \
    key="${tree##*/}" && \
    mkdir -p qemu-build-kvm && find qemu-build-kvm -mindepth 1 -maxdepth 1 ! -name "$key" -exec rm -rf {} + && \
    mkdir -p "qemu-build-kvm/$key" && cd "qemu-build-kvm/$key" && \
    cc_now="$(cc -dumpfullversion 2>/dev/null || cc -dumpversion)" && \
    if [ -f config.status ] && [ "$(cat spin-cc-version 2>/dev/null)" != "$cc_now" ]; then \
        echo "the cached build directory was made by cc $(cat spin-cc-version 2>/dev/null || echo unknown), this is $cc_now — starting over"; \
        rm -rf ./*; \
    fi && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        "$tree/configure" \
            --prefix=/opt/qemu \
            --disable-tcg \
            ${QEMU_FLAGS} && \
        echo "$cc_now" > spin-cc-version; \
    fi && \
    make -j${JOBS} && \
    make install

RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    --mount=type=cache,sharing=locked,id=qemu-build-tcg-${QEMU_VERSION}-${QEMU_CONFIG_REV},target=/build/qemu-build-tcg \
    tree="$(sh /build/qemu-source.sh)" && \
    cp /build/spin-devices.mak "$tree/configs/devices/x86_64-softmmu/spin.mak" && \
    key="${tree##*/}" && \
    mkdir -p qemu-build-tcg && find qemu-build-tcg -mindepth 1 -maxdepth 1 ! -name "$key" -exec rm -rf {} + && \
    mkdir -p "qemu-build-tcg/$key" && cd "qemu-build-tcg/$key" && \
    cc_now="$(cc -dumpfullversion 2>/dev/null || cc -dumpversion)" && \
    if [ -f config.status ] && [ "$(cat spin-cc-version 2>/dev/null)" != "$cc_now" ]; then \
        echo "the cached build directory was made by cc $(cat spin-cc-version 2>/dev/null || echo unknown), this is $cc_now — starting over"; \
        rm -rf ./*; \
    fi && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        "$tree/configure" \
            --prefix=/opt/qemu-tcg \
            --enable-tcg \
            ${QEMU_FLAGS} && \
        echo "$cc_now" > spin-cc-version; \
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
    tree="$(sh /build/qemu-source.sh)" && \
    mkdir -p /build/pc-bios && \
    for f in bios.bin bios-256k.bin pvh.bin kvmvapic.bin efi-virtio.rom; do \
        install -m 0644 "$tree/pc-bios/$f" "/build/pc-bios/$f"; \
    done

# Fail the build here, not in a lane three phases later, if this configuration did not
# produce the devices, the tools and the two accelerators this release ships.
#
# The last two lines are the split itself, and neither half is visible in a file listing:
# the binary a tenant's host runs must refuse to emulate, and CI's must be able to. Getting
# them backwards is a fleet quietly running at a tenth of the speed, or a CI lane that
# cannot boot at all.
RUN Q=/opt/qemu/bin/qemu-system-x86_64 && \
    $Q -device help 2>&1 | grep -q 'virtio-blk-pci' && \
    $Q -device help 2>&1 | grep -q 'virtio-net-pci' && \
    $Q -device help 2>&1 | grep -q 'vhost-vsock-pci' && \
    $Q -device help 2>&1 | grep -q 'virtio-mem-pci' && \
    $Q -device help 2>&1 | grep -q 'virtio-scsi-pci' && \
    $Q -device help 2>&1 | grep -q 'scsi-hd' && \
    $Q -device help 2>&1 | grep -q 'vmgenid' && \
    ! $Q -device help 2>&1 | grep -qE '^name "(e1000e?|rtl8139|pcnet|vmxnet3|igb|ne2k_pci|tulip)"' && \
    ! $Q -device help 2>&1 | grep -qE '^name "(usb-kbd|usb-hub|VGA|virtio-vga|sb16|isa-fdc|isa-parallel|vfio-pci|intel-iommu|amd-iommu)"' && \
    $Q -machine help | grep -q '^q35 ' && \
    ! $Q -machine help | grep -q '^pc-i440fx' && \
    test -x /opt/qemu/bin/qemu-img && \
    /opt/qemu/bin/qemu-img --version && \
    $Q --version && \
    /opt/qemu-tcg/bin/qemu-system-x86_64 --version && \
    ! $Q -accel help | grep -qx tcg && \
    /opt/qemu-tcg/bin/qemu-system-x86_64 -accel help | grep -qx tcg

# qcow2 is the base image's format and the overlay's, and raw is the extras disk's.
# Both are configure flags with no device to hold them up,
# so a build that dropped one produces a VM that dies at start-up with
# `Unknown driver` — after the machine is created, in a lane a long way from here.
#
# The whole help text is flattened before it is searched, and not grepped line by line:
# qemu-img wraps the format list across several lines, so a `grep` for one format
# matches only if that format happens to land on the first line. vmdk did not, and this
# check failed a build whose every other assertion had passed (2026-09-07).
RUN help=$(/opt/qemu/bin/qemu-img --help 2>&1 | tr '\n' ' ') && \
    for f in qcow2 raw; do \
        case " $help " in \
            *" $f "*) ;; \
            *) echo "ERROR: this qemu-img cannot open $f images"; exit 1 ;; \
        esac; \
    done && \
    echo "OK: qemu-img opens qcow2 and raw"

# Static, asserted rather than assumed. `--static` is a request, and a build that silently
# fell back to dynamic linking produces a binary that works perfectly here and dies on the
# first host that has a different liburing — which is the entire failure this build exists
# to remove, arriving later and somewhere else.
#
# The check reads the ELF rather than running `ldd`: musl's ldd is the loader itself and
# prints its own path even for a static binary, which reads like a dependency and is not
# one. Two things are asked instead — a NEEDED entry, which names a shared library this
# binary cannot start without, and an INTERP segment, which names the loader that would
# have to find it.
RUN for b in /opt/qemu/bin/qemu-system-x86_64 /opt/qemu/bin/qemu-img \
             /opt/qemu-tcg/bin/qemu-system-x86_64; do \
        if readelf -lW "$b" | grep -q INTERP; then \
            echo "ERROR: $b wants a dynamic loader; --static did not take"; exit 1; \
        fi; \
        if readelf -dW "$b" | grep -q NEEDED; then \
            echo "ERROR: $b has shared library dependencies"; exit 1; \
        fi; \
    done && \
    echo "OK: every binary is statically linked"

# ============================================
# Runtime image
# ============================================
#
# Two binaries and five files on a base that exists only so a human can open a shell in it
# and look around. There are no libraries to install: that is the whole difference static
# linking makes here. An Ubuntu base with a hand-written apt list would have to be kept in
# step with what the builder links against, and the way that is found to be wrong is a CI
# run.
# ============================================
# qboot: the BIOS this machine boots with
# ============================================
#
# QEMU's SeaBIOS is built for every guest a PC can run; this machine boots one Linux kernel
# through PVH and nothing else, and qboot is a firmware for exactly that. Measured on the lab
# runner, 20 boots interleaved (2026-09-29): 121.9 -> 114.8 ms p50 to a diagnostic init, and
# report's whole matrix boots on it. Stock qboot cannot run this machine: its ACPI linker
# loader stops on WRITE_POINTER, which vmgenid needs, and the guest halts with nothing on the
# console. qboot/write-pointer.patch adds the command, qboot/pam.patch cuts the memory map
# rebuilds its PAM setup costs QEMU; qboot/README.md says what each does.
#
# Built here and not by a Dockerfile of its own, because it ships beside the SeaBIOS blobs it
# replaces: everything that reads this build's qemu/ directory - a release, `task qemu:fetch`,
# CI - has the firmware the machine boots.
#
# Debian and not the Alpine above: the compiler is part of the firmware. The toolchain once
# moved its measured saving from 6.3 to 3.9 ms (qboot/README.md), so it is pinned the way the
# kernel's is - the same Debian by digest and its archive by snapshot date - and it needs
# gcc -m32 -ffreestanding, which is 32-bit code generation compiled into the compiler.
FROM ${DEBIAN_IMAGE} AS qboot

ARG DEBIAN_SNAPSHOT
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# The archive as it stood on DEBIAN_SNAPSHOT, so the compiler is the same one next month.
# Valid-Until is off because a snapshot's Release files expire by design; the signatures are
# still checked, and they are what makes plain http enough - the image carries no CA
# certificates until ca-certificates is installed from this very archive.
RUN rm -f /etc/apt/sources.list.d/debian.sources && printf '%s\n' \
        'Types: deb' \
        "URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}" \
        'Suites: trixie trixie-updates' \
        'Components: main' \
        'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
        'Check-Valid-Until: no' \
        > /etc/apt/sources.list.d/snapshot.sources

RUN --mount=type=cache,sharing=locked,id=qboot-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=qboot-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install -y --no-install-recommends \
        gcc binutils patch git ca-certificates

# The commit and the SHA-256 of the tar `git archive` writes for it: versions.yaml's `qboot`.
# git verifies the tree is the one the commit names; the checksum verifies the bytes handed to
# the compiler are the ones measured, and is stable only because this image's git is pinned.
ARG QBOOT_VERSION
ARG QBOOT_COMMIT
ARG QBOOT_ARCHIVE_SHA256

WORKDIR /build
RUN git clone --quiet --branch "${QBOOT_VERSION}" https://github.com/bonzini/qboot.git upstream && \
    git -C upstream archive "${QBOOT_COMMIT}" > upstream.tar && \
    echo "${QBOOT_ARCHIVE_SHA256}  upstream.tar" | sha256sum -c -

COPY qemu/qboot/write-pointer.patch qemu/qboot/pam.patch qemu/qboot/mtrr.patch /build/

# Upstream's meson.build flags, with -Os fixed rather than taken from a build type.
RUN set -eux; \
    mkdir -p src out; \
    tar -xf upstream.tar -C src; \
    patch -p1 --fuzz=0 -d src < write-pointer.patch; \
    patch -p1 --fuzz=0 -d src < pam.patch; \
    patch -p1 --fuzz=0 -d src < mtrr.patch; \
    cd src; \
    for f in *.c *.S; do \
        gcc -Os -m32 -march=i386 -mregparm=3 -fno-stack-protector \
            -fno-delete-null-pointer-checks -ffreestanding \
            -mstringop-strategy=rep_byte -minline-all-stringops -fno-pic \
            -Iinclude -c "$f" -o "/build/out/${f%.*}.o"; \
    done; \
    gcc -nostdlib -m32 -no-pie -Wl,--build-id=none -Wl,-Tflat.lds \
        /build/out/*.o -o /build/out/qboot.elf; \
    objcopy -O binary /build/out/qboot.elf /build/qboot.bin

# Asserted on what came out. A firmware that is not the size the ROM window expects does not
# fail here - it fails as a guest that prints nothing - so the size is checked; and the patches,
# because a patch that applied to nothing still leaves a tree that compiles: without the first
# the firmware hangs every machine that has vmgenid, without the second it is slower and says
# nothing, and without the third every device memory a guest maps reads uncached.
RUN set -eux; \
    size=$(stat -c%s /build/qboot.bin); \
    test "$size" -eq 65536 || { echo "qboot.bin is $size bytes, not 65536" >&2; exit 1; }; \
    grep -q 'CMD_WRITE_PTR' src/tables.c && grep -q 'fw_cfg_write_file' src/fw_cfg.c || { echo "the WRITE_POINTER patch is not in the tree that was compiled" >&2; exit 1; }; \
    grep -q '0x33333330' src/hwsetup.c || { echo "the PAM patch is not in the tree that was compiled" >&2; exit 1; }; \
    grep -q 'setup_mtrr();' src/main.c || { echo "the MTRR patch is not in the tree that was compiled" >&2; exit 1; }; \
    sha256sum /build/qboot.bin

FROM ${ALPINE_IMAGE} AS runtime

COPY --from=builder /opt/qemu/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64
COPY --from=builder /opt/qemu-tcg/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64-tcg
COPY --from=builder /opt/qemu/bin/qemu-img /usr/local/bin/qemu-img
COPY --from=builder /build/pc-bios/ /usr/share/spin-stack/qemu/
COPY --from=qboot /build/qboot.bin /usr/share/spin-stack/qemu/qboot.bin

# The binaries are static, so this proves rather more than it looks: it is the same check a
# consumer's host performs when it runs them, on a base that shares nothing with the one
# they were built on.
RUN /usr/local/bin/qemu-system-x86_64 --version && /usr/local/bin/qemu-img --version

ENTRYPOINT ["/usr/local/bin/qemu-system-x86_64"]

# ============================================
# Extract stage: the tree onto the host (_output/)
# ============================================
FROM scratch AS extract

COPY --from=builder /opt/qemu/bin/qemu-system-x86_64 /bin/qemu-system-x86_64
COPY --from=builder /opt/qemu-tcg/bin/qemu-system-x86_64 /bin/qemu-system-x86_64-tcg
COPY --from=builder /opt/qemu/bin/qemu-img /bin/qemu-img
COPY --from=builder /build/pc-bios/ /qemu/
COPY --from=qboot /build/qboot.bin /qemu/qboot.bin
