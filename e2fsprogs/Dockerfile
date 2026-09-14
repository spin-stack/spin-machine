# e2fsprogs, statically linked: the tools that make and read the ext4 filesystems this
# machine's kernel mounts.
#
# Pinned for the same reason QEMU and the kernel are. A filesystem is made by one program and
# mounted by another, and the features it is made with — orphan_file, metadata_csum_seed, the
# block and inode sizes — are what mke2fs's version and its configuration file default to on
# the host that happened to run it. Two hosts with two distributions make two different
# filesystems from the same command line, and neither says so. Static, so a binary from a
# release runs the same on any Linux, the build container's included.
#
# The configuration file is part of the pin. mke2fs reads its defaults from mke2fs.conf, and a
# pinned binary reading the host's /etc/mke2fs.conf is only half pinned; the one built here is
# upstream's, from the same tarball, and callers point MKE2FS_CONFIG at it.
#
# libarchive linked in directly, which is what `mke2fs -d <tarball>` needs: it is how a
# filesystem is populated with every file owned by root without building it as root. Upstream
# would otherwise dlopen() libarchive.so at run time, and a static binary cannot.

ARG ALPINE_VERSION=3.24
FROM alpine:${ALPINE_VERSION} AS builder

ARG E2FSPROGS_VERSION=1.47.4
# From sha256sums.asc beside the tarball on kernel.org.
ARG E2FSPROGS_SHA256=fd5bf388cbdbe006a3d3b318d983b2948382440acc85a87f1e7d108653e8db0b
ARG JOBS=8

SHELL ["/bin/sh", "-o", "pipefail", "-c"]

RUN --mount=type=cache,sharing=locked,id=e2fsprogs-apk,target=/var/cache/apk \
    apk add --cache-dir /var/cache/apk \
        build-base linux-headers pkgconf curl xz binutils \
        libarchive-dev libarchive-static \
        acl-static zlib-static xz-static zstd-static bzip2-static lz4-static \
        expat-static openssl-libs-static

WORKDIR /build

RUN --mount=type=cache,sharing=locked,id=e2fsprogs-src,target=/var/cache/e2fsprogs \
    tarball="/var/cache/e2fsprogs/e2fsprogs-${E2FSPROGS_VERSION}.tar.xz" && \
    if [ ! -f "$tarball" ]; then \
        curl -fsSL "https://mirrors.edge.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v${E2FSPROGS_VERSION}/e2fsprogs-${E2FSPROGS_VERSION}.tar.xz" -o "$tarball"; \
    fi && \
    echo "${E2FSPROGS_SHA256}  $tarball" | sha256sum -c - && \
    tar -xf "$tarball" --strip-components=1

# The private uuid and blkid libraries, so nothing is linked from the build image; no NLS,
# fuse2fs, uuidd or the host-integration files, none of which a filesystem is made with.
RUN ./configure \
        --disable-nls \
        --disable-fuse2fs \
        --disable-uuidd \
        --disable-elf-shlibs \
        --enable-libuuid \
        --enable-libblkid \
        --with-libarchive=direct \
        --without-udev-rules-dir \
        --without-crond-dir \
        --without-systemd-unit-dir \
        LDFLAGS="-static -s" \
        PKG_CONFIG="pkg-config --static" && \
    make -j"${JOBS}" && \
    for b in misc/mke2fs e2fsck/e2fsck misc/dumpe2fs debugfs/debugfs; do \
        if readelf -l "$b" | grep -q "program interpreter"; then \
            echo "$b is dynamically linked" >&2; exit 1; \
        fi; \
    done && \
    ./misc/mke2fs -V 2>&1 | grep -q "mke2fs ${E2FSPROGS_VERSION}" && \
    grep -q "orphan_file" misc/mke2fs.conf

FROM scratch AS extract

# mke2fs as mkfs.ext4: it reads its own name to choose the filesystem type, so the name is
# what makes `mkfs.ext4 <device>` an ext4 with ext4's defaults.
COPY --from=builder /build/misc/mke2fs /bin/mkfs.ext4
COPY --from=builder /build/e2fsck/e2fsck /bin/e2fsck
COPY --from=builder /build/misc/dumpe2fs /bin/dumpe2fs
COPY --from=builder /build/debugfs/debugfs /bin/debugfs
COPY --from=builder /build/misc/mke2fs.conf /e2fsprogs/mke2fs.conf
