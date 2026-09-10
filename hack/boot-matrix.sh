#!/usr/bin/env bash
# What this machine costs to boot, under each of the ways it can be booted.
#
# The question it answers is not "is it fast" but "where does the time go, and what does each
# decision cost". Every row is a real boot of the real release under KVM, and the number is
# systemd's own: it prints `Startup finished in <kernel> + <userspace> = <total>` to the
# console when show_status is on, so nothing here has to log in to read it. That is what
# makes this measurable at all — the login is one of the things being measured.
#
# Two dimensions:
#
#   initrd   with, the debug initramfs mounts the API filesystems and hands over; without,
#            the kernel mounts root itself, which it can do because virtio-blk and ext4 are
#            built in and build.sh writes a partitionless filesystem.
#   udev     the image masks systemd-udevd, deliberately, to save boot time. Masks are
#            symlinks in /etc, so unmasking means writing to the overlay before the boot —
#            which is what --unmask-udev does, through qemu-nbd.
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
# moves nothing. It is systemd's own start-up before it logs a word, on a guest page cache
# that is empty by definition: every read of the binary, of libsystemd-core and
# libsystemd-shared, and of the 263 unit files is a virtio round trip into a qcow2. The host
# cache is already warm across these runs, so it is not host I/O either.
#
# Which is the thing worth knowing about it: this is what a *cold boot* costs, and a
# workspace never pays it. A machine restored from a template resumes memory in which all of
# this has already happened. The number matters when the template is built and for this
# debugging boot, and nowhere else.
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

# unmask_udev removes the three mask symlinks from an overlay, so the boot that follows
# discovers its hardware the way an ordinary Linux system does.
#
# Through qemu-nbd because the overlay is qcow2 and the masks are files: there is no way to
# ask a kernel command line to unmask a unit, and /etc is the highest-priority place a mask
# can live, so nothing in the boot itself can override one.
unmask_udev() {
	local overlay=$1 dev=/dev/nbd0 mnt
	mnt=$(mktemp -d)
	sudo modprobe nbd max_part=8
	sudo qemu-nbd --connect="$dev" -f qcow2 "$overlay"
	# The kernel needs a moment to read the partition table it will not find: this image is
	# partitionless, so the device itself is the filesystem.
	for _ in $(seq 20); do sudo blkid "$dev" >/dev/null 2>&1 && break; sleep 0.2; done
	sudo mount "$dev" "$mnt"
	# All four, and the trigger is the one that is easy to miss: without it udevd runs and
	# nothing ever asks the kernel to re-announce the devices that already existed, so no
	# .device unit appears and the boot behaves exactly as if udev were still masked. The
	# first version of this function removed three and produced a row that looked like
	# "udev changes nothing".
	sudo rm -f "$mnt/etc/systemd/system/systemd-udevd.service" \
		"$mnt/etc/systemd/system/systemd-udevd-control.socket" \
		"$mnt/etc/systemd/system/systemd-udevd-kernel.socket" \
		"$mnt/etc/systemd/system/systemd-udev-trigger.service"
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
	local label=$1 initrd=$2 udev=$3
	local dir overlay log
	dir=$(mktemp -d)
	overlay="$dir/overlay.qcow2"
	log="$dir/console.log"
	"$OUT/bin/qemu-img" create -f qcow2 -F qcow2 -b "$OUT/image/rootfs.qcow2" "$overlay" >/dev/null
	[ "$udev" = udev ] && unmask_udev "$overlay"
	inject_analysis "$overlay"

	local -a args=(boot --release "$OUT" --disk "$overlay" --memory 2048 --cpus 2 --console "file:$log")
	if [ "$initrd" = initrd ]; then
		args+=(--initrd "$OUT/debug-initramfs.cpio.gz"
			--append "spinmachine.root=/dev/vda spinmachine.init=/sbin/init spinmachine.getty=1")
	else
		args+=(--append "root=/dev/vda rw init=/sbin/init")
	fi

	echo "==================== $label ===================="
	# It powers itself off, so a run takes as long as the boot; hitting the cap means it
	# never reached the point where the question could be asked.
	timeout "$BOOT_TIMEOUT" "$OUT/bin/spin-machine" "${args[@]}" >/dev/null 2>&1 || true
	read_analysis "$overlay"
	echo
}

# run boots one configuration and records what it cost.
#   $1 label · $2 "initrd"|"noinitrd" · $3 "udev"|"noudev" · $4 extra kernel arguments
run() {
	local label=$1 initrd=$2 udev=$3 extra=${4:-}
	local dir overlay log
	dir=$(mktemp -d)
	overlay="$dir/overlay.qcow2"
	log="$dir/console.log"
	"$OUT/bin/qemu-img" create -f qcow2 -F qcow2 -b "$OUT/image/rootfs.qcow2" "$overlay" >/dev/null

	[ "$udev" = udev ] && unmask_udev "$overlay"

	local -a args=(boot --release "$OUT" --disk "$overlay" --memory 2048 --cpus 2 --console "file:$log")
	# log_target=console as well as show_status: the status lines alone do not carry the one
	# line these numbers come from. It costs a little of what it measures — every message is
	# a write to a serial port — but it costs the same in every row, so the comparison holds
	# and only the absolute is inflated.
	local append="systemd.show_status=true systemd.log_level=info systemd.log_target=console $extra"
	if [ "$initrd" = initrd ]; then
		args+=(--initrd "$OUT/debug-initramfs.cpio.gz")
		append="spinmachine.root=/dev/vda spinmachine.init=/sbin/init spinmachine.getty=1 $append"
	else
		append="root=/dev/vda rw init=/sbin/init $append"
	fi
	args+=(--append "$append")

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
run "initrd    · udev masked"   initrd   noudev
run "no initrd · udev masked"   noinitrd noudev
run "no initrd · no serial-getty" noinitrd noudev "systemd.mask=serial-getty@ttyS0.service"
run "no initrd · udev on"       noinitrd udev
run "initrd    · udev on"       initrd   udev

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

# And where the userspace time actually goes, for the two configurations that reach a
# console. A total is a number to compare; blame is a list of things to delete.
if [ "${ANALYZE:-1}" = 1 ]; then
	echo
	echo "==================== where the userspace time goes ===================="
	analyze "initrd · udev masked"   initrd   noudev
	analyze "no initrd · udev on"    noinitrd udev
fi
