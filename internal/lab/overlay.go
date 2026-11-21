// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Edit is a change to one boot's own overlay: /etc is the highest-priority place a mask or a
// drop-in can live, and no kernel parameter reaches it here.
type Edit struct {
	// Mask is units masked by a link to /dev/null in /etc/systemd/system. Never systemd.mask=:
	// systemd-debug-generator implements it, the image links that generator to /dev/null, and the
	// parameter parses, reaches /proc/cmdline and masks nothing (2026-09-10).
	Mask []string `json:"mask,omitempty"`
	// Files are written into the overlay: a path under / and its content.
	Files map[string]string `json:"files,omitempty"`
	// Links are made in the overlay: a path under / and its target.
	Links map[string]string `json:"links,omitempty"`
	// Setup is a shell script run as root with the overlay mounted at $MNT, under `sh -eu`.
	Setup string `json:"setup,omitempty"`
}

// NewOverlay makes path a fresh qcow2 overlay over the tree's base image. The base is its
// backing file and is only ever read.
// mutate-lane: TestEveryProbeMeasuresATree
func NewOverlay(t *Tree, path string) error {
	return run(t.QEMUImg(), "create", "-f", "qcow2", "-F", "qcow2", "-b", t.Rootfs(), path)
}

// EditOverlay makes the edits in the overlay at path, in order, through an NBD device and a mount.
// mutate-lane: TestEveryProbeMeasuresATree
func EditOverlay(path string, edits ...Edit) (err error) {
	dev, err := connectNBD(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, run("sudo", "qemu-nbd", "--disconnect", dev)) }()

	// The device's size arrives after qemu-nbd returns: a mount before it reads a device of zero
	// bytes and fails with "can't read superblock" (a runner reconnecting right after the previous
	// boot's disconnect did). blkid -p, because without it blkid answers from its cache, and said
	// ext4 about a device that was still empty.
	ready := false
	for range 20 {
		size, err := exec.Command("sudo", "blockdev", "--getsize64", dev).Output()
		if err == nil && strings.TrimSpace(string(size)) != "0" && exec.Command("sudo", "blkid", "-p", dev).Run() == nil {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("%s (overlay %s) never showed a filesystem", dev, path)
	}
	mnt, err := os.MkdirTemp("", "overlay-mnt")
	if err != nil {
		return fmt.Errorf("making a mount point for %s: %w", path, err)
	}
	defer func() { _ = os.Remove(mnt) }()
	if err := run("sudo", "mount", dev, mnt); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, run("sudo", "umount", mnt)) }()
	for _, e := range edits {
		if err := e.apply(mnt); err != nil {
			return fmt.Errorf("editing overlay %s: %w", path, err)
		}
	}
	return nil
}

// mutate-lane: TestEveryProbeMeasuresATree
func (e Edit) apply(mnt string) error {
	for _, u := range e.Mask {
		if err := run("sudo", "ln", "-sf", "/dev/null", filepath.Join(mnt, "etc/systemd/system", u)); err != nil {
			return err
		}
	}
	for _, path := range slices.Sorted(maps.Keys(e.Files)) {
		dst := filepath.Join(mnt, path)
		if err := run("sudo", "mkdir", "-p", filepath.Dir(dst)); err != nil {
			return err
		}
		// Through `sudo tee`: the mount is root's, and an edit that silently wrote nothing would
		// read as "the drop-in changed nothing".
		cmd := exec.Command("sudo", "tee", dst)
		cmd.Stdin, cmd.Stdout = strings.NewReader(e.Files[path]), nil
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("writing %s: %w: %s", path, err, out)
		}
	}
	if e.Setup != "" {
		// MNT through sudo's own assignment: sudo resets the environment, and under -u a setup
		// that lost MNT fails, where without it one wrote 300 units into the host's /etc.
		if out, err := exec.Command("sudo", "MNT="+mnt, "sh", "-euc", e.Setup).CombinedOutput(); err != nil {
			return fmt.Errorf("running the setup: %w: %s", err, out)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(e.Links)) {
		target := e.Links[path]
		// A mask of a udev rule only masks something when a rule of that name is there to shadow:
		// one that names nothing changes no timing and reads as a measurement.
		if target == "/dev/null" && strings.HasPrefix(path, "/etc/udev/rules.d/") {
			shadowed := filepath.Join(mnt, "usr/lib/udev/rules.d", filepath.Base(path))
			if exec.Command("sudo", "test", "-e", shadowed).Run() != nil {
				return fmt.Errorf("%s masks nothing: there is no %s in this image", path, filepath.Base(path))
			}
		}
		dst := filepath.Join(mnt, path)
		if err := run("sudo", "mkdir", "-p", filepath.Dir(dst)); err != nil {
			return err
		}
		if err := run("sudo", "ln", "-sfn", target, dst); err != nil {
			return err
		}
	}
	return nil
}

// connectNBD attaches overlay to an NBD device nobody holds. Not /dev/nbd0 alone: two runners on
// one host each took nbd0 and the second failed (2026-09-29). A device is held while its
// /sys/block/nbdN/pid exists; one taken between that look and the connect fails, and the next is
// tried.
// mutate-lane: TestEveryProbeMeasuresATree
func connectNBD(overlay string) (string, error) {
	if err := LoadNBD(); err != nil {
		return "", err
	}
	devs, err := filepath.Glob("/sys/block/nbd*")
	if err != nil {
		return "", fmt.Errorf("listing NBD devices: %w", err)
	}
	var last string
	for _, d := range devs {
		if _, err := os.Stat(filepath.Join(d, "pid")); err == nil {
			continue
		}
		dev := "/dev/" + filepath.Base(d)
		out, err := exec.Command("sudo", "qemu-nbd", "--connect="+dev, "-f", "qcow2", overlay).CombinedOutput()
		if err == nil {
			return dev, nil
		}
		last = fmt.Sprintf("%s: %v: %s", dev, err, out)
	}
	return "", fmt.Errorf("no free NBD device among %d for %s; the last tried: %s", len(devs), overlay, last)
}

// LoadNBD makes the NBD devices be there. On a kernel with NBD built in - Spin OS's has no modules
// - they are there from boot, and modprobe is only asked where they are not.
// mutate-lane: TestEveryProbeMeasuresATree
func LoadNBD() error {
	if _, err := os.Stat("/dev/nbd0"); err == nil {
		return nil
	}
	if out, err := exec.Command("sudo", "-n", "modprobe", "nbd", "max_part=8").CombinedOutput(); err != nil {
		return fmt.Errorf("sudo modprobe nbd max_part=8: %w: %s", err, out)
	}
	// modprobe returns when the module is loaded, and the nodes come after it, from udev.
	for range 50 {
		if _, err := os.Stat("/dev/nbd0"); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("/dev/nbd0 did not appear after modprobe nbd")
}

// CanSudo is whether sudo runs without asking, which editing an overlay needs.
// mutate-lane: TestEveryProbeMeasuresATree
func CanSudo() bool { return exec.Command("sudo", "-n", "true").Run() == nil }

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// namedEdit is the edit a tree asks for by name, read from this checkout when the tree is made,
// for the tree at dir.
func namedEdit(name, checkout, dir string) (Edit, error) {
	switch name {
	case "lane":
		return laneEdit(checkout)
	case "pressure":
		return pressureEdit(checkout, dir)
	case "floor":
		return floorEdit, nil
	case "nolinger":
		return noLingerEdit, nil
	case "forwarder-early":
		return forwarderEarlyEdit, nil
	case "carry-masks":
		return carryMasksEdit, nil
	case "carry-generators":
		return carryGeneratorsEdit, nil
	case "carry-logging":
		return carryLoggingEdit, nil
	case "carry-coldplug":
		return carryColdplugEdit, nil
	}
	return Edit{}, fmt.Errorf("no edit named %q; the edits are: lane, pressure, floor, nolinger, forwarder-early, carry-masks, carry-generators, carry-logging, carry-coldplug", name)
}

// The carry-* edits take back, from a release's overlay, what the image does to systemd for boot
// time alone and was never measured on a goal number in spin's shape (docs/boot-time.md, "What the
// machine carries"), one group each, as image/mkosi.extra/usr/local/lib/spin-base/
// optimize-systemd.sh and the coldplug filter do it. What the script does for a function (the time
// sync, the first boot's prompt, the machine-id commit, journald's storage, the timeouts, docker's
// socket activation) is not in any of them, and neither are the masks of the five units of
// ConditionNeedsUpdate (ldconfig, sysusers, hwdb-update, journal-catalog-update, update-done): the
// image has no /etc/.updated or /var/.updated, so unmasked they rebuild hwdb.bin, the linker cache
// and the catalog in every boot, the first login +260 ms and QEMU's RSS +90 MiB on a laptop (2
// boots, 2026-11-21). Each step checks what it undoes, so a release that no longer carries one
// fails the tree rather than measuring it twice.

// carryMasksEdit unmasks the units optimize-systemd.sh masks for boot time that the image has.
var carryMasksEdit = Edit{
	Setup: `for u in systemd-journal-flush.service systemd-fsck-root.service systemd-modules-load.service \
	systemd-hibernate-clear.service systemd-pstore.service systemd-random-seed.service \
	modprobe@configfs.service modprobe@drm.service modprobe@fuse.service modprobe@efi_pstore.service \
	sys-kernel-debug.mount sys-kernel-tracing.mount \
	systemd-tmpfiles-setup-dev.service systemd-tmpfiles-setup-dev-early.service \
	dev-hugepages.mount dev-mqueue.mount; do
	test "$(readlink "$MNT/etc/systemd/system/$u")" = /dev/null
	rm "$MNT/etc/systemd/system/$u"
done
`,
}

// carryGeneratorsEdit enables the generators and tmpfiles rules optimize-systemd.sh disables.
var carryGeneratorsEdit = Edit{
	Setup: `for g in systemd-debug-generator systemd-factory-reset-generator systemd-fstab-generator \
	systemd-gpt-auto-generator systemd-hibernate-resume-generator systemd-rc-local-generator \
	systemd-system-update-generator systemd-sysv-generator; do
	test "$(readlink "$MNT/etc/systemd/system-generators/$g")" = /dev/null
	test -e "$MNT/usr/lib/systemd/system-generators/$g"
	rm "$MNT/etc/systemd/system-generators/$g"
done
for c in systemd-pstore.conf systemd-network.conf x11.conf; do
	test "$(readlink "$MNT/etc/tmpfiles.d/$c")" = /dev/null
	rm "$MNT/etc/tmpfiles.d/$c"
done
`,
}

// carryLoggingEdit is the manager's logging and status as systemd's defaults, and the printk sysctl
// gone; the timeouts in fast-boot.conf stay.
var carryLoggingEdit = Edit{
	Setup: `f="$MNT/etc/systemd/system.conf.d/fast-boot.conf"
grep -qx 'LogLevel=err' "$f" && grep -qx 'ShowStatus=no' "$f"
sed -i -e '/^LogLevel=/d' -e '/^LogTarget=/d' -e '/^ShowStatus=/d' -e '/^StatusUnitFormat=/d' "$f"
rm "$MNT/etc/sysctl.d/99-quiet-boot.conf"
`,
}

// carryColdplugEdit is udev's coldplug unfiltered, as the distribution runs it: the release's
// systemd-udev-trigger.service.d/10-coldplug.conf removed. It was kept on the lane in
// 2 vCPUs (lab 43518915618, 436 -> 402 ms); spin's path and one vCPU have not measured it.
var carryColdplugEdit = Edit{
	Setup: `f="$MNT/etc/systemd/system/systemd-udev-trigger.service.d/10-coldplug.conf"
grep -q -- '--subsystem-match=block' "$f"
rm "$f"
`,
}

// forwarderEarlyEdit starts login=spin's forwarder, which stands for spin's supervisor, without
// systemd's default dependencies, as the image starts ssh.service: what spin would get if its
// supervisor were ordered so (docs/boot-time.md, "Open experiments", the forwarder). It refuses a
// tree booted without the forwarder.
var forwarderEarlyEdit = Edit{
	Files: map[string]string{
		"/etc/systemd/system/spin-lab-forward.service.d/zz-early.conf": "[Unit]\nDefaultDependencies=no\nConflicts=shutdown.target\nBefore=shutdown.target\n",
	},
	Setup: `test -e "$MNT/etc/systemd/system/spin-lab-forward.service" || { echo "forwarder-early edits spin's path: boot it with login=spin" >&2; exit 1; }
`,
}

// noLingerEdit is a published release's image as this checkout builds it: spin no longer lingers
// (configure-system.sh), so logind starts no user@1000 in the boot. A release from before has the
// linger file, and the edit fails on one without it rather than measuring the release twice.
var noLingerEdit = Edit{
	Setup: `test -e "$MNT/var/lib/systemd/linger/spin"
rm "$MNT/var/lib/systemd/linger/spin"
`,
}

// floorEdit is the first login with no systemd before it, a bound on what systemd costs and never
// a candidate (systemd stays: what runs in a workspace expects it). Its init mounts what sshd
// needs, makes a host key and hands each vsock connection to `sshd -i`; it prints the getty echo's
// prompt, so the probe's console column is the moment it starts listening. A tree boots it with
// --append init=/usr/local/sbin/spin-floor, the kernel taking the last init= it is given.
//
// It is 11-13's floor (branch floor-kernel, 652fb3f). On v20261120.02, bench fleet, 2026-11-20
// (lab 44750322384): first login 219 ms against the release's 322 in the probes' shape (2 vCPUs,
// 2 GiB), and 240 against 445 in spin's (1 vCPU, 128 MiB, ceilings 16 and 8 GiB, 4 hotplug disks).
var floorEdit = Edit{
	Files: map[string]string{"/usr/local/sbin/spin-floor": `#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null
mkdir -p /dev/pts && mount -t devpts devpts /dev/pts
mount -t tmpfs run /run
mkdir -p -m 0755 /run/sshd
ssh-keygen -q -t ed25519 -N "" -f /run/ssh_host_ed25519_key
echo SPIN-READY-login: > /dev/ttyS0
exec socat VSOCK-LISTEN:22,fork,reuseaddr EXEC:"/usr/sbin/sshd -i -h /run/ssh_host_ed25519_key"
`},
	Setup: `chmod 0755 "$MNT/usr/local/sbin/spin-floor"
test -x "$MNT/usr/bin/socat" && test -x "$MNT/usr/sbin/sshd"
`,
}

// laneEdit is the first login's lane as this checkout's image builds it, put into a published
// release's overlay: the lab boots releases, and a change to the image is measured as the release
// with the change in it, interleaved with the release as it was published.
//
// It holds on a release from before the lane as well as after: logind in the boot, the host keys
// made from the boot's first dispatch, the lane's drop-ins read from
// image/mkosi.extra, and configure-system.sh's edits done the same way. Each step checks what it
// edits, so an edit that matched nothing fails rather than measuring the image under another name.
//
// What each part was worth, measured as rows of the probe this replaced (boot/ssh_first_test.go,
// lab runs 43308825692, 43312419007, 43463472762, 43483372488, 43479854381): logind in the boot
// and spin lingering 570 → 510 ms to the first login; the host keys from the boot's first
// dispatch 489 → 468; the masked generators and units, nothing, so they are not here.
func laneEdit(checkout string) (Edit, error) {
	e := Edit{
		Files: map[string]string{
			"/etc/systemd/system/spin-machine-sshd-keygen.service.d/early.conf": "[Unit]\nDefaultDependencies=no\nConflicts=shutdown.target\nBefore=shutdown.target\n",
		},
		Links: map[string]string{
			"/etc/systemd/system/sysinit.target.wants/spin-machine-sshd-keygen.service": "/usr/lib/systemd/system/spin-machine-sshd-keygen.service",
		},
		Setup: `rm -f "$MNT/etc/systemd/system/multi-user.target.wants/systemd-logind.service"
test -e "$MNT/usr/lib/systemd/system/multi-user.target.wants/systemd-logind.service"
grep -qx 'After=sysinit.target' "$MNT/usr/lib/systemd/system/dbus.socket"
sed -e '/^Wants=sysinit.target$/d' -e '/^After=sysinit.target$/d' "$MNT/usr/lib/systemd/system/dbus.socket" > "$MNT/etc/systemd/system/dbus.socket"
ln -sf /dev/null "$MNT/etc/tmpfiles.d/systemd-nologin.conf"
grep -q 'pam_systemd.so.*class=user-early-light' "$MNT/etc/pam.d/common-session" ||
	sed -i 's/^\(session.*pam_systemd\.so\)\(.*\)$/\1\2 class=user-early-light/' "$MNT/etc/pam.d/common-session"
grep -q 'pam_systemd.so.*class=user-early-light' "$MNT/etc/pam.d/common-session"
`,
	}
	rootfs := filepath.Join(checkout, "image", "mkosi.extra", "usr", "local", "lib", "spin-base", "rootfs")
	for _, d := range []string{"sshd-vsock.socket.d/10-lane.conf", "sshd@.service.d/10-lane.conf",
		"systemd-logind.service.d/10-lane.conf", "systemd-logind-varlink.socket.d/20-lane.conf",
		"user-runtime-dir@.service.d/10-lane.conf",
		"user@.service.d/10-lane.conf", "systemd-udev-trigger.service.d/10-coldplug.conf",
		"ssh.socket.d/10-lane.conf", "ssh.service.d/10-lane.conf", "sshd-keygen.service.d/10-lane.conf",
		"systemd-logind.service.d/30-privatetmp.conf", "dbus-broker.service.d/30-privatetmp.conf",
		"systemd-udevd.service.d/20-startup.conf", "systemd-udev-trigger.service.d/20-startup.conf"} {
		b, err := os.ReadFile(filepath.Join(rootfs, "etc", "systemd", "system", d))
		if err != nil {
			return Edit{}, fmt.Errorf("reading the lane's %s from this checkout: %w", d, err)
		}
		e.Files["/etc/systemd/system/"+d] = string(b)
	}
	return e, nil
}

// pressureEdit is the pressure probe's guest half as this checkout builds it into the image, put
// into a published release's overlays: the sender built into the tree at dir, its unit, and the
// link that enables it. A release that has it already is refused, rather than measured twice.
func pressureEdit(checkout, dir string) (Edit, error) {
	bin := filepath.Join(dir, "guest", "spin-machine-pressure")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/spin-machine-pressure")
	cmd.Dir, cmd.Env = checkout, append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return Edit{}, fmt.Errorf("building this checkout's pressure sender: %w: %s", err, out)
	}
	const unit = "/usr/lib/systemd/system/spin-machine-pressure.service"
	b, err := os.ReadFile(filepath.Join(checkout, "image", "mkosi.extra", "usr", "local", "lib", "spin-base", "rootfs", unit))
	if err != nil {
		return Edit{}, fmt.Errorf("reading the pressure sender's unit from this checkout: %w", err)
	}
	return Edit{
		Files: map[string]string{unit: string(b)},
		Links: map[string]string{"/etc/systemd/system/multi-user.target.wants/spin-machine-pressure.service": unit},
		Setup: `test ! -e "$MNT/usr/libexec/spin-machine-pressure"
install -D -m 0755 ` + bin + ` "$MNT/usr/libexec/spin-machine-pressure"
`,
	}, nil
}

// systemdPackages is the edit that replaces the overlay's systemd with the packages in debs: each
// package's files that the new one no longer has are removed first, /etc is the image's, and the
// overlay is checked to hold the rebuilt systemd afterwards.
func systemdPackages(debs string) Edit {
	return Edit{Setup: `debs=` + debs + `
replaced=0
for deb in "$debs"/*.deb; do
	pkg=$(dpkg-deb -f "$deb" Package)
	list="$MNT/var/lib/dpkg/info/$pkg.list"
	[ -f "$list" ] || list="$MNT/var/lib/dpkg/info/$pkg:amd64.list"
	[ -f "$list" ] || continue
	new=$(mktemp)
	dpkg-deb --fsys-tarfile "$deb" | tar -t | sed -e 's|^\./|/|' -e 's|/$||' | sort > "$new"
	sort "$list" | comm -23 - "$new" | grep -v '^/etc/' | while read -r f; do
		if [ -L "$MNT$f" ] || [ -f "$MNT$f" ]; then rm -f "$MNT$f"; fi
	done
	rm -f "$new"
	dpkg-deb --fsys-tarfile "$deb" | tar -x -C "$MNT" --exclude=./etc
	replaced=$((replaced + 1))
done
test "$replaced" -gt 0
grep -q 'spin1' "$MNT/usr/lib/systemd/libsystemd-shared-"*.so 2>/dev/null || strings "$MNT/usr/lib/systemd/systemd" | grep -q '+spin1' || { echo "the overlay's systemd is not the rebuilt one" >&2; exit 1; }
`}
}
