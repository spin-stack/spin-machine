#!/bin/sh
# The QEMU source tree at /var/cache/qemu/qemu-$QEMU_VERSION, from the upstream tarball pinned by
# $QEMU_SHA256 - downloaded if the cache mount does not have it, checked every time.
#
# Every step that reads the tree runs this first, and not only the step that used to fetch it.
# The tree lives in a cache mount, and a cache mount is not part of a layer: on a runner whose
# mount is empty, a fetch step restored from the layer cache does not run, and the build step
# after it - rerun because devices.mak changed - found no tree ("cp: cannot create regular file
# .../configs/devices/x86_64-softmmu/spin.mak: No such file or directory"). That is every PR that
# touches devices.mak on a fresh runner.
set -eu
tarball="/var/cache/qemu/qemu-${QEMU_VERSION}.tar.xz"
if [ ! -f "$tarball" ]; then
    curl -fsSL "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" -o "$tarball"
fi
# Checked on every build and not only after a download: the tarball lives in a cache mount, so
# the copy being compiled today may have arrived weeks ago.
echo "${QEMU_SHA256}  $tarball" | sha256sum -c -
if [ ! -d "/var/cache/qemu/qemu-${QEMU_VERSION}" ]; then
    tar -xf "$tarball" -C /var/cache/qemu
fi
