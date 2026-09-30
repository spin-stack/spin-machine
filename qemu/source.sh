#!/bin/sh
# Prints the QEMU source tree to build: /var/cache/qemu/qemu-$QEMU_VERSION-<key>, the upstream
# tarball pinned by $QEMU_SHA256 - downloaded if the cache mount does not have it, checked every
# time - with qemu/patches applied, and <key> the hash of those patches.
#
# Every step that reads the tree runs this first, and not only the step that used to fetch it.
# The tree lives in a cache mount, and a cache mount is not part of a layer: on a runner whose
# mount is empty, a fetch step restored from the layer cache does not run, and the build step
# after it - rerun because devices.mak changed - found no tree ("cp: cannot create regular file
# .../configs/devices/x86_64-softmmu/spin.mak: No such file or directory"). That is every PR that
# touches devices.mak on a fresh runner.
#
# The tree is named by its patches because it outlives the build that patched it. Patched in
# place, a patch removed from the repository would stay in the tree, and a build directory made
# from one set of patches would relink objects compiled from another: make compares times, and a
# file put back to its upstream text is older than the object built from the patched one. So a
# change to the patches is a new tree, and the build directories take the same key (its last
# path component) for a directory of their own.
set -eu
tarball="/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz"
if [ ! -f "$tarball" ]; then
    curl -fsSL "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" -o "$tarball"
fi
# Checked on every build and not only after a download: the tarball lives in a cache mount, so
# the copy being compiled today may have arrived weeks ago.
echo "${QEMU_SHA256}  $tarball" | sha256sum -c - >&2

key=$(cat /dev/null /build/qemu-patches/*.patch 2>/dev/null | sha256sum | cut -c1-16)
tree="/var/cache/qemu/qemu-${QEMU_VERSION}-${key}"
# .spin-patched is written last: a tree without it is one an interrupted build left half made.
if [ ! -f "$tree/.spin-patched" ]; then
    rm -rf "$tree"
    mkdir -p "$tree"
    tar -xf "$tarball" -C "$tree" --strip-components=1
    for p in /build/qemu-patches/*.patch; do
        [ -f "$p" ] || continue
        patch -d "$tree" -p1 --forward --fuzz=0 < "$p" >&2 ||
            { echo "ERROR: $p does not apply to QEMU ${QEMU_VERSION}" >&2; exit 1; }
    done
    touch "$tree/.spin-patched"
fi
# Every other tree is a patch set this build no longer has.
for old in /var/cache/qemu/qemu-"${QEMU_VERSION}"*; do
    [ -d "$old" ] && [ "$old" != "$tree" ] && rm -rf "$old"
done
echo "$tree"
