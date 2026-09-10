# The toolchain that builds rootfs.qcow2. It is not the image build itself: that is
# image/build.sh, run inside a container from this Dockerfile.
#
# Why the split, since everything else here is a `docker buildx build --target extract`:
# mkosi assembles a root filesystem by unsharing a user and mount namespace, and BuildKit
# forbids that — `mkosi was forbidden to unshare namespaces`, PermissionError on unshare(2),
# measured 2026-09-07. The escape hatches are RUN --security=insecure, which needs a
# buildkitd started with an entitlement flag (so `task build:image` would fail on any
# machine whose builder was created the ordinary way), or a container run with the
# privileges mkosi needs. The second is one flag on one `docker run`.
#
# So: this file pins the tools, image/build.sh is the build, Taskfile.yml joins them.
#
# ubuntu:26.04 and not debian, because the image being built is Ubuntu 26.04 (resolute) and
# this is where mkosi's keyring and apt come from. Debian's mkosi can build an Ubuntu image
# too, with ubuntu-keyring installed by hand; using the same distribution removes the
# question.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive

RUN rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# What mkosi itself runs: apt and dpkg to assemble the tree, ubuntu-keyring for the archive
# it comes from (gpgv, which checks the signatures, arrives with apt). curl fetches mkosi
# itself, below. e2fsprogs turns the tree into a filesystem with no loop device
# (mkfs.ext4 -d) and reads it back to check it (debugfs, e2fsck); qemu-utils wraps it.
#
# qemu-utils and not this repository's own qemu-img: the image build would otherwise depend
# on the QEMU build, which is tens of minutes of compiling to write one qcow2 header. The
# format is not version-sensitive in the direction that matters — an image written by
# Ubuntu's qemu-img is read by ours — and `task verify:image` proves it by booting one.
RUN --mount=type=cache,sharing=locked,id=image-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=image-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install --no-install-recommends -y \
        ubuntu-keyring \
        curl \
        apt \
        dpkg \
        ca-certificates \
        e2fsprogs \
        qemu-utils \
        python3 \
        xz-utils \
        zstd && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# mkosi from its release tarball and not from Ubuntu's `mkosi` package, which is whatever
# the distribution happens to carry — 26-3 in resolute, and a different version the day the
# archive moves. Which version assembles the tree decides what ends up in it, and the base
# image is hashed into machine.env, so "apt gave us a newer mkosi" is a change to the
# release nobody wrote down.
#
# mkosi is pure Python and runs from its source tree: bin/mkosi puts the checkout on
# PYTHONPATH and execs it. Nothing is installed into the system Python, so there is no
# second copy to go stale, and `apt-get install mkosi`'s 30-package dependency closure —
# systemd-boot, tpm2-tools, pesign, cryptsetup, for outputs this build does not produce —
# stays out of the container.
#
# By content, like the kernel and QEMU tarballs: https says who sent it, not what they sent.
ARG MKOSI_VERSION=27
ARG MKOSI_SHA256=f85d81da7bea87f98eabd5753935fbf81501ca83d8c40f5f90dca2d29bbf2827
RUN --mount=type=cache,sharing=locked,id=image-mkosi-src,target=/var/cache/mkosi-src \
    tarball="/var/cache/mkosi-src/mkosi-${MKOSI_VERSION}.tar.gz" && \
    if [ ! -f "$tarball" ]; then \
        curl -fsSL -o "$tarball" "https://github.com/systemd/mkosi/archive/refs/tags/v${MKOSI_VERSION}.tar.gz"; \
    fi && \
    # Every build, not only after a download: the cache mount outlives the build that
    # filled it.
    echo "${MKOSI_SHA256}  $tarball" | sha256sum -c - && \
    mkdir -p /opt/mkosi && \
    tar -xf "$tarball" -C /opt/mkosi --strip-components=1 && \
    ln -s /opt/mkosi/bin/mkosi /usr/local/bin/mkosi && \
    # Asks the binary just installed which version it is, rather than trusting the tag.
    test "$(mkosi --version)" = "mkosi ${MKOSI_VERSION}"

COPY image/mkosi.conf image/mkosi.postinst.chroot /work/
COPY image/mkosi.extra /work/mkosi.extra
COPY image/build.sh /usr/local/bin/build-base-image
RUN chmod +x /usr/local/bin/build-base-image /work/mkosi.postinst.chroot \
             /work/mkosi.extra/usr/local/lib/spin-base/*.sh

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/build-base-image"]
