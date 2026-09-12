#!/bin/bash
# Build rootfs.qcow2 from the tree mkosi assembles. Runs inside the image/Dockerfile
# container; writes $OUT (default /out/image/rootfs.qcow2).
#
# What it produces: a partitionless ext4 filesystem holding the workload's userland,
# wrapped in a read-only qcow2 that every VM maps as a backing file.
#
# One filesystem and not a stack of layers, so copy-on-write happens in the block layer
# where QEMU does it rather than in a union filesystem inside the guest:
#
#     qemu-img create -f qcow2 -F qcow2 -b rootfs.qcow2 overlay.qcow2
#
# which is the shape a chain of images is made of: one base, many overlays.
#
# ext4 and not something denser: the guest kernel has EXT4_FS, EROFS_FS and OVERLAY_FS and
# explicitly not XFS_FS, BTRFS_FS or SQUASHFS (kernel/config-7.2.1-x86_64). Any other
# filesystem here starts with a kernel config change one directory over.
set -euo pipefail

OUT="${OUT:-/out/image/rootfs.qcow2}"

# The two texts that describe the image rather than being it. They go at the root of the
# tree and not beside the image, because that is where a release carries them: one
# licences.txt and one packages.txt for the whole machine, not one per part.
SHARE="${SHARE:-$(dirname "$(dirname "$OUT")")}"
# Normalizes timestamps in the tree, so the build's wall clock is not baked into every
# file. It is not on its own a claim that the image is bit-reproducible: the userland is
# assembled from a live archive, and the filesystem is sized from what came out of it, so
# two builds days apart differ for reasons this variable does not touch. What a given
# release actually contains is the checksum in machine.env.
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"

mkdir -p "$(dirname "$OUT")" "$SHARE" /work/out /cache
cd /work

echo "==> mkosi: assembling the userland"
mkosi --force build
tree=/work/out/rootfs
test -d "$tree" || { echo "ERROR: mkosi produced no tree at $tree" >&2; exit 1; }

# --- identity ---------------------------------------------------------------------------
#
# Done here rather than through mkosi's RemoveFiles because the two files below are not
# "remove it": /etc/machine-id has to exist and be empty. Empty is what systemd reads as
# "not provisioned yet", so each VM generates its own on its writable overlay; absent makes
# some tooling treat the image as broken, and populated makes every VM that ever boots from
# this base share one identity. mkosi also writes a machine-id of its own after RemoveFiles
# runs, which is how a build that looked clean shipped one (measured 2026-09-07).
: > "$tree/etc/machine-id"
rm -f "$tree/var/lib/dbus/machine-id"
rm -f "$tree/etc/hostname" "$tree/etc/resolv.conf"
rm -f "$tree"/etc/ssh/ssh_host_*
# The random seed is the same argument as machine-id and worse: a pool every VM starts from
# the same state is not a pool. The guest gets entropy from virtio-rng, which is why
# systemd-random-seed.service is masked in optimize-systemd.sh.
rm -f "$tree/var/lib/systemd/random-seed"

# The directories the guest mounts over. They have to exist — a mount over a missing
# directory fails — and the guest expects them empty.
for d in etc tmp proc sys dev run; do
    mkdir -p "$tree/$d"
done
chmod 1777 "$tree/tmp"

# --- the licences -----------------------------------------------------------------------
#
# Moved out of the tree, not generated here: the file was written by
# mkosi.postinst.chroot, which is the only moment the copyright files exist —
# optimize-systemd.sh deletes /usr/share/doc a few lines later, and it is a vendored script
# this repository does not rewrite.
#
# Beside the image rather than inside it. The notices have to accompany a copy of the
# software, and a release is what gets copied; a few hundred licence texts in every VM that
# boots are just bytes nobody reads.
harvested="$tree/var/lib/spin-machine-licenses.txt"
test -s "$harvested" || {
    echo "ERROR: no licence texts were collected — the image would be redistributed" >&2
    echo "       without the copyright notices its licences require. See" >&2
    echo "       image/mkosi.postinst.chroot." >&2
    exit 1; }
mv "$harvested" "$SHARE/licenses.txt"
echo "==> $(grep -c '^======== ' "$SHARE/licenses.txt" || true) licence texts, $(du -h "$SHARE/licenses.txt" | cut -f1)"

# --- the bill of materials --------------------------------------------------------------
#
# Every package in the tree, by name and exact version, written beside the image rather
# than into it.
#
# It is the only record of what this image is made of. The tree is thrown away, the qcow2
# is opaque, and `CleanPackageMetadata=no` keeps dpkg's database inside the image — but
# reading it back requires booting a VM. This file answers "which version of that library
# shipped in the release from March?" without one, and it is what the SOURCES file in a
# release points at for the userland half: an Ubuntu source package is fetched by name and
# version, and those are the two things here.
dpkg-query --admindir="$tree/var/lib/dpkg" -W -f='${Package} ${Version} ${Architecture}\n' \
    2>/dev/null | sort > "$SHARE/packages.txt"
echo "==> $(wc -l < "$SHARE/packages.txt") packages recorded"

# --- filesystem -------------------------------------------------------------------------
#
# Sized from the tree, not fixed: a hardcoded size is either wasted space or a build that
# fails the day docker-ce grows. The slack covers ext4's own overhead — inode tables and
# the directory structure are charged before a single file is written.
#
# mkfs.ext4 -d writes the tree in directly: no loop device and no mount, so the filesystem
# is built with no privilege beyond what mkosi already needed.
#
# -U clear: a null filesystem UUID, on purpose. It makes the build deterministic, and
# nothing in this stack mounts by UUID — the guest resolves disks by virtio-blk serial,
# because a /dev node depends on probe order. A random UUID would be one more thing that
# changes the image's checksum for no reason anyone could name.
#
# -O ^has_journal: this file is opened read-only by every VM for its whole life. A journal
# is megabytes describing writes that cannot happen. The container's writes go to its own
# qcow2 overlay, which is a different file with a different filesystem on it.
kb=$(du -sk "$tree" | cut -f1)
size_kb=$(( kb + kb / 10 + 262144 ))
echo "==> mkfs.ext4: ${size_kb} KiB from a ${kb} KiB tree"
mkfs.ext4 -q -F -L "" -U clear -O ^has_journal -E root_owner=0:0 \
    -d "$tree" /work/base.raw "${size_kb}"

# --- checks -----------------------------------------------------------------------------
#
# Asked of the finished filesystem rather than of the build that made it. A tree that
# failed to copy in produces a perfectly valid empty ext4, and the failure would then be a
# VM that boots and finds nothing to run.
echo "==> checking the filesystem"
e2fsck -fn /work/base.raw

# debugfs is asked and its *output* is read, not its exit status: `debugfs -R` exits 0
# whether or not the request succeeded, printing "File not found by ext2_lookup" to stderr
# and returning success anyway. A check written the obvious way therefore passes for every
# path, present or not — which is how this script first reported a missing /bin/sh as fine
# and a deleted /etc/hostname as baked in (2026-09-07). A successful stat starts its output
# with "Inode:"; nothing else does.
stat_in_image() { debugfs -R "stat $1" /work/base.raw 2>&1; }
in_image() { stat_in_image "$1" | grep -q '^Inode:'; }

# /sbin/init is what this image is: a userland whose first process is systemd, and what a
# boot with `init=/sbin/init` lands on. /bin/sh is what the smoke test in the README runs.
for f in /sbin/init /bin/sh /bin/bash /usr/bin/docker /usr/local/bin/task; do
    in_image "$f" || { echo "ERROR: the image has no $f" >&2; exit 1; }
done

# hwclock comes from util-linux-extra, not from util-linux and not from a package called
# hwclock — Ubuntu split the two. Asked of the finished image rather than of the package
# list because the package that provides it has moved once already, and the failure this
# catches is a rename that still installs cleanly.
in_image /usr/sbin/hwclock || {
    echo "ERROR: the image has no /usr/sbin/hwclock; check util-linux-extra" >&2
    exit 1; }

for d in /etc /tmp /proc /sys /dev /run; do
    in_image "$d" || { echo "ERROR: the image has no $d for the guest to mount over" >&2; exit 1; }
done

# The identity checks, asserted rather than trusted to the deletions above. An image
# carrying one of these hands the same identity to every VM that ever boots from it.
for f in /etc/hostname /etc/resolv.conf /var/lib/dbus/machine-id /var/lib/systemd/random-seed; do
    if in_image "$f"; then
        echo "ERROR: $f is baked into the base image; every VM would share it" >&2
        exit 1
    fi
done
if in_image /etc/ssh/ssh_host_ed25519_key; then
    # openssh-server's postinst generates a set at install time. Every VM booting from this
    # base would present the same host key, which is the identity trap with a fingerprint.
    echo "ERROR: an SSH host key is baked into the base image" >&2
    exit 1
fi

# machine-id is the exception: present, and empty.
in_image /etc/machine-id || { echo "ERROR: /etc/machine-id is missing" >&2; exit 1; }
mid_size=$(stat_in_image /etc/machine-id | sed -n 's/.*Size: \([0-9]\+\).*/\1/p' | head -1)
[ "${mid_size:-unset}" = "0" ] || {
    echo "ERROR: /etc/machine-id is ${mid_size} bytes; every VM would share that identity" >&2
    exit 1; }

# udev runs, and the assertion is that nothing masks it again. It was masked here for six
# months to save boot time it does not cost: 15 boots of each with `task boot:bench` on
# 2026-09-10, and with these four masked, fifteen out of fifteen never reached a login at
# all — the machine waits out the full ten-second dev-ttyS0.device timeout and then offers
# nothing. Masking is a one-line edit that looks like an optimization, so the result lives
# here too, where such an edit is caught.
for u in systemd-udevd.service systemd-udevd-kernel.socket systemd-udevd-control.socket \
         systemd-udev-trigger.service; do
    if stat_in_image "/etc/systemd/system/$u" | grep -q '/dev/null'; then
        echo "ERROR: $u is masked - it is free and it buys a getty and hot-plug events" >&2
        exit 1
    fi
done

# What udev is on for, other than the getty: an added CPU or memory block arrives as a
# kernel event and nothing else is listening for one. Without the rules file the events are
# delivered to no one, which is indistinguishable from udev being masked.
in_image /etc/udev/rules.d/40-spin-hotadd.rules || {
    echo "ERROR: the image has no 40-spin-hotadd.rules; a hot-plugged CPU stays offline" >&2
    exit 1; }

# The kernel command line carries TERM=dumb, to stop systemd waiting 334ms twice for a serial
# port to answer a terminfo query. agetty is invoked as `- ${TERM}` and its unit sets none,
# so without these drop-ins PID1's `dumb` is what a human's login shell gets: no colour, no
# clear, no cursor addressing. The two files are 687ms of boot and a usable terminal, and
# they only work together — which is why the absence of one is an error and not a warning.
for f in /etc/systemd/system/serial-getty@.service.d/term.conf \
         /etc/systemd/system/getty@.service.d/term.conf; do
    in_image "$f" || { echo "ERROR: the image has no $f; a login would inherit TERM=dumb" >&2; exit 1; }
done

# A boot optimization that is invisible in a file listing and expensive when it regresses:
# a masked unit is a symlink to /dev/null, and a package upgrade replacing one turns it
# back on. Two of the ones optimize-systemd.sh masked for measured reasons.
for u in systemd-random-seed.service tmp.mount; do
    stat_in_image "/etc/systemd/system/$u" | grep -q '/dev/null' || {
        echo "ERROR: $u is not masked - optimize-systemd.sh masked it and something put it back" >&2
        exit 1; }
done

# logind is deferred rather than masked, and the two halves of that only work together.
# The drop-in makes pam_systemd's guard — access("/run/systemd/seats/") — true before
# logind has run, so the first login activates it over Varlink instead of being handed a
# session with no XDG_RUNTIME_DIR and no user manager. The overridden want is what keeps
# the 24 ms. Either half alone is a silent failure: without the drop-in every login comes
# up session-less and nothing logs an error, and a re-enabled want costs the boot time back
# while everything still works, so neither shows up anywhere but here.
in_image /etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf || {
    echo "ERROR: the image has no logind seats drop-in; every login would lose its session" >&2
    exit 1; }
# The want is overridden, not deleted: the package ships it in /usr, so the /etc path must
# hold a symlink to /dev/null. Asserting its *absence* is the version of this check that
# passed while the image still started logind at boot, because the /etc path was never
# there to remove (2026-09-12).
if ! stat_in_image /etc/systemd/system/multi-user.target.wants/systemd-logind.service |
        grep -q '/dev/null'; then
    echo "ERROR: the want for systemd-logind is not overridden; the package ships one in" \
         "/usr and logind is back in the boot transaction" >&2
    exit 1
fi
if stat_in_image /etc/systemd/system/systemd-logind.service | grep -q '/dev/null'; then
    echo "ERROR: systemd-logind.service is masked - deferring it means it still starts" \
         "on demand, and masked means it never starts at all" >&2
    exit 1
fi

# The distribution's background maintenance, masked by configure-system.sh for reasons
# that are written there. Asserted separately from the three above because the cause is a
# different script: these come back if a package upgrade re-runs a unit's [Install], and
# what they cost is an apt lock taken under a workload that is doing its own install.
for u in apt-daily.timer apt-daily-upgrade.timer motd-news.timer dpkg-db-backup.timer; do
    stat_in_image "/etc/systemd/system/$u" | grep -q '/dev/null' || {
        echo "ERROR: $u is not masked - configure-system.sh masked it and something put it back" >&2
        exit 1; }
done

# The login banner is this image's own, and pam_motd is enabled for ssh to print it — so
# whatever is in /etc/update-motd.d now runs on every login. That is the reason to assert
# both halves rather than only the one that was added: the distribution's scripts reach the
# network (50-motd-news), print an advertisement, and were deleted, and a package upgrade
# that restores one restores it into a path that is now read.
in_image /etc/update-motd.d/00-spin-boot || {
    echo "ERROR: the image has no /etc/update-motd.d/00-spin-boot to print the boot time" >&2
    exit 1; }
for f in /etc/update-motd.d/10-help-text /etc/update-motd.d/50-motd-news; do
    if in_image "$f"; then
        echo "ERROR: $f is back, and pam_motd now runs it at every login" >&2
        exit 1
    fi
done

# --- qcow2 ------------------------------------------------------------------------------
#
# No compression: the base is read by every VM on every boot and a compressed cluster is
# decompressed on each read. The file is written once and read forever.
# Written beside the target and renamed over it, never into it.
#
# The obvious version — `qemu-img convert -O qcow2 … "$OUT"` — opens the existing base for
# writing, and a VM running against that base holds an image lock on it, so the build dies
# with `Failed to get "write" lock. Is another process using the image?`. Which is the
# right refusal for the wrong reason: the problem is not the lock, it is that a base image
# is never edited. Every VM maps it read-only through a backing chain and many share one,
# so anything that writes into that file invalidates every overlay in existence — silently,
# because the overlays keep working until they read a cluster that moved.
#
# A rename replaces the directory entry and leaves the old inode alone, so a VM that is
# running right now keeps the image it booted from, and the next one gets the new file.
echo "==> qemu-img: wrapping it"
rm -f "$OUT.new"
qemu-img convert -f raw -O qcow2 /work/base.raw "$OUT.new"
qemu-img info --output=json "$OUT.new"
chmod 0444 "$OUT.new"
mv -f "$OUT.new" "$OUT"
echo "==> $OUT"
echo "    sha256 $(sha256sum "$OUT" | cut -d' ' -f1)"
echo "    size   $(du -h "$OUT" | cut -f1)"
