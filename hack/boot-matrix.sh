#!/usr/bin/env bash
# What this machine costs to boot, under each of the ways it can be booted.
#
# The question it answers is not "is it fast" but "where does the time go, and what does each
# decision cost". Every row is a real boot of the real release under KVM, and the number is
# systemd's own: it prints `Startup finished in <kernel> + <userspace> = <total>` to the
# console when show_status is on, so nothing here has to log in to read it. That is what
# makes this measurable at all — the login is one of the things being measured.
#
# One dimension, udev: the image runs systemd-udevd, and the rows that mask it again are
# what say whether masking was ever worth anything. Masks are symlinks in /etc, so a row
# writes to its throwaway overlay before the boot.
#
# It used to have a second, initrd, and losing it is the point rather than a reduction:
# every boot here now goes root=/dev/vda straight into the image, which the kernel can do
# because virtio-blk and ext4 are built in and build.sh writes a partitionless filesystem.
# The debug initramfs the other half of the matrix booted was a second init, and this
# repository does not ship an init.
#
# Every boot writes to a throwaway overlay over the base image, and the base's digest is
# checked afterwards: a run that modified it is a run whose numbers are worthless and whose
# damage is permanent.
#
# WHERE THE TIME IS, measured 2026-09-10, so the next person does not look where it is not.
#
# The kernel is 62ms and mounts the root filesystem at 56ms. Everything else is userspace,
# and dmesg locates it exactly:
#
#     [ 0.056141] EXT4-fs (vda): mounted filesystem r/w
#     [ 0.058798] Run /sbin/init as init process
#     [ 0.838756] systemd[1]: systemd 259.5 running in system mode
#
# 675ms between the kernel exec'ing systemd and systemd being ready to say so. After that
# line everything is fast: units start appearing 50ms later and arrive in single-digit
# milliseconds each, the heaviest service in `blame` is 37ms, and the whole of the rest of
# the boot is about 115ms.
#
# So it is not the units, not the 13 generators, and not the services — pruning any of them
# moves nothing. It was two timeouts. `systemd.log_level=debug systemd.log_target=kmsg`
# gives every one of systemd's own lines a kernel timestamp, and the gap has two stalls of
# 334ms each in it, both waiting for a serial port to answer a question:
#
#     [0.172] IPE support is disabled in the kernel, ignoring
#     [0.506] Failed to query /dev/console for terminfo: Operation not supported
#     [0.518] ProtectSystem=auto selected, but not running in an initrd, skipping
#     [0.852] systemd 259.5 running in system mode
#
# TERM=dumb on the kernel command line removes both — 687ms to 19ms — and the reasoning,
# the alternatives that did not work and what it costs are at the line that sets it, in
# machine/cmdline.go. Keep this harness pointed at the same gap: `Run /sbin/init` to the
# version banner is the measurement, and a change that claims to move it should move that.
#
# The guess this replaced, recorded because it was plausible and wrong: that it was a cold
# guest page cache faulting in the binary, libsystemd-shared and 263 unit files over virtio.
# Every part of that is true and none of it was the cost. Debug logging with timestamps
# found in one boot what a week of reasoning about I/O would not have.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=${OUT:-$(pwd)/_output}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-70}
RESULTS=$(mktemp -d)/results
: >"$RESULTS"

for f in "$OUT/bin/spin-machine" "$OUT/bin/qemu-img" "$OUT/kernel/vmlinux" "$OUT/image/rootfs.qcow2"; do
	test -f "$f" || { echo "missing $f — run: task build" >&2; exit 1; }
done
test -e /dev/kvm || { echo "no /dev/kvm: these numbers are only meaningful under KVM" >&2; exit 1; }

BASE_DIGEST=$(sha256sum "$OUT/image/rootfs.qcow2" | cut -d' ' -f1)

# mask_udev puts the mask symlinks into an overlay, so the boot that follows discovers no
# hardware — the configuration this image shipped for six months, kept here as the thing the
# current one is measured against.
#
# It masks rather than unmasking because the image now ships udev on. The first version of
# this harness unmasked, and once the image changed that was a no-op applied to every row:
# the run still printed "udev masked" against "udev on" and both rows had udev. The lesson is
# in which direction a harness should push — towards the configuration that is *not* shipped,
# so a row that stops doing anything stops being a comparison rather than quietly becoming
# one against itself.
#
# Through qemu-nbd because the overlay is qcow2 and the masks are files: there is no way to
# ask a kernel command line to mask a unit, and /etc is the highest-priority place a mask
# can live.
mask_udev() {
	local overlay=$1 dev=/dev/nbd0 mnt
	mnt=$(mktemp -d)
	sudo modprobe nbd max_part=8
	sudo qemu-nbd --connect="$dev" -f qcow2 "$overlay"
	# The kernel needs a moment to read the partition table it will not find: this image is
	# partitionless, so the device itself is the filesystem.
	for _ in $(seq 20); do sudo blkid "$dev" >/dev/null 2>&1 && break; sleep 0.2; done
	sudo mount "$dev" "$mnt"
	# All four, and the trigger is the one that is easy to miss: leave it out and udevd is
	# off but the kernel is still asked to re-announce its devices, so the row measures
	# neither configuration. Its mirror image cost this harness a run once.
	for u in systemd-udevd.service systemd-udevd-control.socket \
		systemd-udevd-kernel.socket systemd-udev-trigger.service; do
		sudo ln -sf /dev/null "$mnt/etc/systemd/system/$u"
	done
	sudo umount "$mnt"
	sudo qemu-nbd --disconnect "$dev" >/dev/null
	rmdir "$mnt"
}

# inject_analysis puts a timer in the overlay that writes systemd's own breakdown to a file
# and powers the machine off.
#
# It is how this gets `systemd-analyze blame` out of a machine nobody can log into, which is
# the whole difficulty: the console is what is being measured, so a harness that needs one to
# read the numbers cannot measure the configurations where there is none.
#
# A timer and not a unit ordered after multi-user.target, because a unit that is part of the
# boot transaction cannot ask whether the boot has finished — `systemd-analyze` answers
# "Bootup is not yet finished" while its own job is still queued, and
# `is-system-running --wait` deadlocks waiting for itself. OnBootSec puts it outside the
# transaction, which is the only place the question has an answer.
#
# To a file and not the console, because by then agetty owns the console and the output goes
# where nobody reads it. The harness mounts the overlay afterwards and reads it back.
inject_analysis() {
	local overlay=$1 dev=/dev/nbd0 mnt
	mnt=$(mktemp -d)
	sudo modprobe nbd max_part=8
	sudo qemu-nbd --connect="$dev" -f qcow2 "$overlay"
	for _ in $(seq 20); do sudo blkid "$dev" >/dev/null 2>&1 && break; sleep 0.2; done
	sudo mount "$dev" "$mnt"
	sudo tee "$mnt/etc/systemd/system/spin-boot-analysis.service" >/dev/null <<-'UNIT'
		[Unit]
		Description=Write the boot analysis and power off

		[Service]
		Type=oneshot
		ExecStart=/bin/sh -c '{ systemd-analyze; echo "=== BLAME ==="; systemd-analyze blame; echo "=== CHAIN ==="; systemd-analyze critical-chain; } > /boot-analysis.txt 2>&1; sync'
		ExecStart=/usr/bin/systemctl poweroff
	UNIT
	sudo tee "$mnt/etc/systemd/system/spin-boot-analysis.timer" >/dev/null <<-'TIMER'
		[Unit]
		Description=Analyse the boot once it is over

		[Timer]
		OnBootSec=6s
		AccuracySec=100ms

		[Install]
		WantedBy=timers.target
	TIMER
	sudo mkdir -p "$mnt/etc/systemd/system/timers.target.wants"
	sudo ln -sf /etc/systemd/system/spin-boot-analysis.timer \
		"$mnt/etc/systemd/system/timers.target.wants/spin-boot-analysis.timer"
	sudo umount "$mnt"
	sudo qemu-nbd --disconnect "$dev" >/dev/null
	rmdir "$mnt"
}

# read_analysis mounts an overlay a boot has finished with and prints what it wrote.
read_analysis() {
	local overlay=$1 dev=/dev/nbd0 mnt
	mnt=$(mktemp -d)
	sudo qemu-nbd --connect="$dev" -f qcow2 "$overlay"
	for _ in $(seq 20); do sudo blkid "$dev" >/dev/null 2>&1 && break; sleep 0.2; done
	sudo mount -o ro "$dev" "$mnt"
	sudo cat "$mnt/boot-analysis.txt" 2>/dev/null || echo "  (the machine never got far enough to write one)"
	sudo umount "$mnt"
	sudo qemu-nbd --disconnect "$dev" >/dev/null
	rmdir "$mnt"
}

# analyze boots one configuration with that timer in it and prints what it said.
analyze() {
	local label=$1 udev=$2
	local dir overlay log
	dir=$(mktemp -d)
	overlay="$dir/overlay.qcow2"
	log="$dir/console.log"
	"$OUT/bin/qemu-img" create -f qcow2 -F qcow2 -b "$OUT/image/rootfs.qcow2" "$overlay" >/dev/null
	[ "$udev" = noudev ] && mask_udev "$overlay"
	inject_analysis "$overlay"

	local -a args=(boot --release "$OUT" --disk "$overlay" --memory 2048 --cpus 2 --console "file:$log"
		--append "root=/dev/vda rw init=/sbin/init")

	echo "==================== $label ===================="
	# It powers itself off, so a run takes as long as the boot; hitting the cap means it
	# never reached the point where the question could be asked.
	timeout "$BOOT_TIMEOUT" "$OUT/bin/spin-machine" "${args[@]}" >/dev/null 2>&1 || true
	read_analysis "$overlay"
	echo
}

# run boots one configuration and records what it cost.
#   $1 label · $2 "udev"|"noudev" · $3 extra kernel arguments
run() {
	local label=$1 udev=$2 extra=${3:-}
	local dir overlay log
	dir=$(mktemp -d)
	overlay="$dir/overlay.qcow2"
	log="$dir/console.log"
	"$OUT/bin/qemu-img" create -f qcow2 -F qcow2 -b "$OUT/image/rootfs.qcow2" "$overlay" >/dev/null

	[ "$udev" = noudev ] && mask_udev "$overlay"

	local -a args=(boot --release "$OUT" --disk "$overlay" --memory 2048 --cpus 2 --console "file:$log")
	# log_target=console as well as show_status: the status lines alone do not carry the one
	# line these numbers come from. It costs a little of what it measures — every message is
	# a write to a serial port — but it costs the same in every row, so the comparison holds
	# and only the absolute is inflated.
	args+=(--append "root=/dev/vda rw init=/sbin/init \
systemd.show_status=true systemd.log_level=info systemd.log_target=console $extra")

	timeout "$BOOT_TIMEOUT" "$OUT/bin/spin-machine" "${args[@]}" >/dev/null 2>&1 || true

	# systemd's own number, and whether anything ever offered a login.
	local plain kernel user total login
	plain=$(tr -d '\000' <"$log" | tr '\r' '\n' | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g')
	# `|| true` on every one of these, and it is not defensive noise: a configuration that
	# never finished starting has no number to extract, grep exits 1, and under `set -e` the
	# harness would die on the first interesting row instead of recording it as the failure
	# it is.
	kernel=$(grep -aoE "Startup finished in [0-9.]+m?s \(kernel\)" <<<"$plain" | grep -oE "[0-9.]+m?s" | head -1 || true)
	user=$(grep -aoE "\+ [0-9.]+m?s \(userspace\)" <<<"$plain" | grep -oE "[0-9.]+m?s" | head -1 || true)
	total=$(grep -aoE "= [0-9.]+m?s" <<<"$plain" | grep -oE "[0-9.]+m?s" | head -1 || true)
	login=no
	grep -qaE "login:|root@" <<<"$plain" && login=yes || true

	printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$label" "${kernel:-—}" "${user:-—}" "${total:-—}" "$login" "$dir" >>"$RESULTS"
	printf '  %-34s kernel %-8s userspace %-9s total %-9s login %s\n' \
		"$label" "${kernel:-—}" "${user:-—}" "${total:-—}" "$login"
}

echo "==> booting each configuration under KVM (${BOOT_TIMEOUT}s cap each)"
run "as shipped"                udev
run "udev masked"               noudev
# The third row asks whether the ten seconds the second one spends are the getty's to give
# back, and the answer is no: masking serial-getty@ttyS0 leaves the boot waiting out
# `Job dev-ttyS0.device/start running (10s)` exactly as before — 10.300s against 10.299s,
# measured 2026-09-10. So the dependency on that device is not only the getty's, and the
# cheap-looking fix for a masked-udev boot is not a fix. Kept as a row because it is the
# obvious thing to try.
run "udev masked, no getty"     noudev "systemd.mask=serial-getty@ttyS0.service"

# There is no row for TERM, and it cannot be one: --append lands in Cmdline.Extra, which is
# rendered after the TERM=dumb the machine package puts there, so both reach init's
# environment and getenv answers with the first. TestCmdlineTellsSystemdTheConsoleWillNot-
# Answer guards that decision instead, and the measurement is at the line that makes it.

echo
if [ "$(sha256sum "$OUT/image/rootfs.qcow2" | cut -d' ' -f1)" != "$BASE_DIGEST" ]; then
	echo "👹 the base image changed during this run; every number above is suspect" >&2
	exit 1
fi
echo "the base image is untouched"
echo
printf '%-34s %-9s %-11s %-10s %s\n' CONFIGURATION KERNEL USERSPACE TOTAL LOGIN
awk -F'\t' '{printf "%-34s %-9s %-11s %-10s %s\n", $1, $2, $3, $4, $5}' "$RESULTS"
echo
echo "console logs are under the directories in $RESULTS"

# And where the userspace time actually goes, for the configuration that ships. A total is a
# number to compare; blame is a list of things to delete.
if [ "${ANALYZE:-1}" = 1 ]; then
	echo
	echo "==================== where the userspace time goes ===================="
	analyze "as shipped" udev
fi
