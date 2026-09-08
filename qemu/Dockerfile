# The QEMU this machine runs on. One build, for every consumer, and statically linked.
#
# One and not several, and the reason is not disk. `machine.Spec.Fingerprint` hashes this
# binary's *contents* into the identity that decides whether a VM may restore from a
# template, so two binaries are two machines and a template taken on one is undefined on
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
#   vmdk             kept, and it is the one format held open for a consumer that has not
#                    moved to the qcow2 base image: a caller still handing this binary a
#                    .vmdk gets `Unknown driver 'vmdk'` at start-up without it. Dropping it
#                    is a one-line change here, and it invalidates every template in
#                    existence — as any change to this binary does.
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

ARG ALPINE_VERSION=3.24

FROM alpine:${ALPINE_VERSION} AS builder

ARG QEMU_VERSION=11.1.1
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
ARG QEMU_SHA256=079ffbff8a7111bbc89022107cbabf3bbfd614d5fc9d7cc675991196aca12482
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
ARG QEMU_CONFIG_REV=3

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

# Source tarball + extracted tree live in a cache mount, shared across builds.
RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    tarball="/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz" && \
    if [ ! -f "$tarball" ]; then \
        curl -fsSL "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" -o "$tarball"; \
    fi && \
    # Checked on every build and not only after a download: the tarball lives in a cache
    # mount, so the copy being compiled today may have arrived weeks ago.
    echo "${QEMU_SHA256}  $tarball" | sha256sum -c - && \
    if [ ! -d "/var/cache/qemu/qemu-${QEMU_VERSION}" ]; then \
        tar -xf "$tarball" -C /var/cache/qemu; \
    fi

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
#   --disable-vmdk               — see the header; a consumer may still hand this a .vmdk.

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
    cp /build/spin-devices.mak \
       "/var/cache/qemu/qemu-${QEMU_VERSION}/configs/devices/x86_64-softmmu/spin.mak" && \
    mkdir -p qemu-build-kvm && cd qemu-build-kvm && \
    cc_now="$(cc -dumpfullversion 2>/dev/null || cc -dumpversion)" && \
    if [ -f config.status ] && [ "$(cat spin-cc-version 2>/dev/null)" != "$cc_now" ]; then \
        echo "the cached build directory was made by cc $(cat spin-cc-version 2>/dev/null || echo unknown), this is $cc_now — starting over"; \
        rm -rf ./*; \
    fi && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        /var/cache/qemu/qemu-${QEMU_VERSION}/configure \
            --prefix=/opt/qemu \
            --disable-tcg \
            ${QEMU_FLAGS} && \
        echo "$cc_now" > spin-cc-version; \
    fi && \
    make -j${JOBS} && \
    make install

RUN --mount=type=cache,sharing=locked,id=qemu-src-${QEMU_VERSION},target=/var/cache/qemu \
    --mount=type=cache,sharing=locked,id=qemu-build-tcg-${QEMU_VERSION}-${QEMU_CONFIG_REV},target=/build/qemu-build-tcg \
    cp /build/spin-devices.mak \
       "/var/cache/qemu/qemu-${QEMU_VERSION}/configs/devices/x86_64-softmmu/spin.mak" && \
    mkdir -p qemu-build-tcg && cd qemu-build-tcg && \
    cc_now="$(cc -dumpfullversion 2>/dev/null || cc -dumpversion)" && \
    if [ -f config.status ] && [ "$(cat spin-cc-version 2>/dev/null)" != "$cc_now" ]; then \
        echo "the cached build directory was made by cc $(cat spin-cc-version 2>/dev/null || echo unknown), this is $cc_now — starting over"; \
        rm -rf ./*; \
    fi && \
    if [ ! -f config.status ]; then \
        rm -rf ./* && \
        /var/cache/qemu/qemu-${QEMU_VERSION}/configure \
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
    mkdir -p /build/pc-bios && \
    for f in bios.bin bios-256k.bin pvh.bin kvmvapic.bin efi-virtio.rom; do \
        install -m 0644 "/var/cache/qemu/qemu-${QEMU_VERSION}/pc-bios/$f" "/build/pc-bios/$f"; \
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

# qcow2 is the base image's format and the overlay's, and vmdk is the format a consumer
# may still hand this. Both are configure flags with no device to hold them up,
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
# linking makes here. This stage used to be an Ubuntu with a hand-written apt list that had
# to be kept in step with what the builder linked against, and the way it was found to be
# wrong was a CI run.
FROM alpine:${ALPINE_VERSION} AS runtime

COPY --from=builder /opt/qemu/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64
COPY --from=builder /opt/qemu-tcg/bin/qemu-system-x86_64 /usr/local/bin/qemu-system-x86_64-tcg
COPY --from=builder /opt/qemu/bin/qemu-img /usr/local/bin/qemu-img
COPY --from=builder /build/pc-bios/ /usr/share/spin-stack/qemu/

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
