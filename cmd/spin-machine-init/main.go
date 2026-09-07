// Command spin-machine-init brings a guest up far enough to hand it over.
//
// It does the part of a guest's bring-up that is the same whoever runs the guest:
// mount the filesystems a Linux userland cannot run without, find the root disk,
// move onto it, exec. That is deliberately where it stops. Everything past it —
// an RPC channel to the host, a container lifecycle, a task API — is the business
// of whatever software runs guests, and this repository builds a machine and does
// not know what that is.
//
// It exists because a machine booted with `init=/bin/bash` and nothing else has
// no /proc, so `df` warns, `ps` sees nothing and `poweroff` answers "Running in
// chroot, ignoring request" — which looks like a fault in the image and is not.
// In production that job belongs to the guest's own init; here it is this.
//
// It is not in a release. It is what `task shell` boots, so that this repository
// can start its own machine without borrowing a runtime, and so that the parts a
// real guest init also has to do are exercised by something rather than only
// described.
//
// Built static, put in a cpio, and passed as -initrd. Three kernel arguments
// steer it, all read from /proc/cmdline:
//
//	spinmachine.root=/dev/vda        the disk to move onto, by /dev node …
//	spinmachine.root=serial:sbxroot  … or by virtio-blk serial (see findDisk)
//	spinmachine.init=/sbin/init      what to exec there (default /sbin/init)
//	spinmachine.getty=ttyS0          enable a serial login on that port first
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

const newRoot = "/newroot"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nspin-machine-init: %v\n", err)
		// Sit still rather than exit. PID 1 exiting panics the kernel, and the
		// panic scrolls the actual error off a serial console — which is how the
		// first version of this hid its own message. Not `select {}`: with no
		// other goroutine the runtime calls that a deadlock and exits, which is
		// exactly the panic being avoided.
		for {
			time.Sleep(time.Hour)
		}
	}
}

func run() error {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		// /proc is not mounted yet, which is the whole problem this program
		// exists for, so mount it before reading how it was invoked.
		if err := os.MkdirAll("/proc", 0o755); err != nil {
			return fmt.Errorf("creating /proc: %w", err)
		}
		if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			return fmt.Errorf("mounting /proc: %w", err)
		}
		if cmdline, err = os.ReadFile("/proc/cmdline"); err != nil {
			return fmt.Errorf("reading /proc/cmdline: %w", err)
		}
	}

	// devtmpfs in the initramfs, before anything looks for a disk. The kernel
	// mounts it automatically for a *root* filesystem (CONFIG_DEVTMPFS_MOUNT)
	// and not for an initramfs, so /dev here is an empty directory and
	// /dev/vda — the device this program exists to mount — does not exist.
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return fmt.Errorf("creating /dev: %w", err)
	}
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, ""); err != nil {
		return fmt.Errorf("mounting /dev: %w", err)
	}

	rootArg := arg(string(cmdline), "spinmachine.root")
	init := arg(string(cmdline), "spinmachine.init")
	getty := arg(string(cmdline), "spinmachine.getty")
	if rootArg == "" {
		return fmt.Errorf("no spinmachine.root= on the kernel command line")
	}
	if init == "" {
		init = "/sbin/init"
	}

	root, err := findDisk(rootArg)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", newRoot, err)
	}
	// ext4 without qualification: it is the only filesystem this machine's
	// kernel can mount that anything here produces.
	if err := syscall.Mount(root, newRoot, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", root, newRoot, err)
	}

	// Mounted inside the new root before the switch, because after it this
	// program is gone and nothing else would.
	//
	// /proc is the one that is not a convenience: everything that reads process
	// state, from `ps` to `poweroff` deciding whether it is in a container,
	// reads it and misbehaves quietly without it.
	const noSuidNoExecNoDev = syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV
	for _, m := range []struct {
		target, fstype, source string
		flags                  uintptr
	}{
		{"proc", "proc", "proc", noSuidNoExecNoDev},
		{"sys", "sysfs", "sysfs", noSuidNoExecNoDev | syscall.MS_RDONLY},
		{"dev", "devtmpfs", "devtmpfs", syscall.MS_NOSUID},
		{"dev/pts", "devpts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC},
	} {
		target := newRoot + "/" + m.target
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", target, err)
		}
		if err := syscall.Mount(m.source, target, m.fstype, m.flags, ""); err != nil {
			// Only /proc is fatal. A machine with no devpts is still one to look
			// around in, and saying so beats refusing to boot.
			if m.target == "proc" {
				return fmt.Errorf("mounting /proc in the new root: %w", err)
			}
			fmt.Fprintf(os.Stderr, "spin-machine-init: mounting /%s: %v\n", m.target, err)
		}
	}

	// switch_root, by hand: move the new root over /, chroot into it, and exec.
	// The kernel refuses to unmount an initramfs, which is why this is a move
	// and not a pivot_root.
	//
	// The chdir comes first and is not optional. MS_MOVE and the chroot that
	// follows both act on the *current directory*, so without it the chroot
	// lands back in the initramfs — which has one file in it — and the exec
	// below fails with a bare ENOENT that names nothing.
	if err := os.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir to %s: %w", newRoot, err)
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("moving the new root over /: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after chroot: %w", err)
	}

	if getty != "" {
		if err := enableSerialGetty(getty); err == nil {
			fmt.Fprintf(os.Stderr, "spin-machine-init: enabled a serial login on %s\n", getty)
		} else {
			// Not fatal: a machine with no login prompt is still one to look at
			// through whatever else was asked for.
			fmt.Fprintf(os.Stderr, "spin-machine-init: enabling a login on %s: %v\n", getty, err)
		}
	}

	// Exec, not fork: PID 1 has to *be* the program that was asked for, so it
	// receives the signals and reaps the orphans that PID 1 is expected to.
	if err := syscall.Exec(init, []string{init}, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", init, err)
	}
	return nil
}

// findDisk resolves what to mount as root.
//
// A /dev node is taken as given. "serial:<name>" is looked up through
// /sys/block/*/serial, and that indirection is the point rather than a
// convenience: a virtio-blk device's /dev node depends on the order the guest
// probed the bus in, so a VM given two disks can find them swapped between boots,
// while a serial is set by whoever built the machine and does not move. Any guest
// init worth the name resolves disks this way; this one does it so that the
// machine's `serial=` option is exercised by something.
func findDisk(spec string) (string, error) {
	name, ok := strings.CutPrefix(spec, "serial:")
	if !ok {
		return spec, nil
	}

	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return "", fmt.Errorf("listing block devices: %w", err)
	}
	for _, e := range entries {
		serial, err := os.ReadFile("/sys/block/" + e.Name() + "/serial")
		if err != nil {
			// Most block devices have no serial attribute at all; that is not an
			// error, it is how they are told apart from the ones that do.
			continue
		}
		if strings.TrimSpace(string(serial)) == name {
			return "/dev/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no block device has serial %q", name)
}

// enableSerialGetty gives the guest a login prompt on a serial port.
//
// It is done here, into the root filesystem this VM is about to enter, and not
// baked into an image: the image is shared read-only by every VM that boots from
// it, this VM's root is a throwaway overlay over it, and a login prompt on a
// serial port is a debugging convenience production has no use for — a guest with
// no serial console would start an agetty on a port that is not there and retry
// until systemd gave up.
//
// It is needed at all because systemd starts getty@tty1 — a virtual console on a
// machine whose QEMU has no display adapter compiled in — and does not start one
// on the serial port, even though /sys/class/tty/console/active says ttyS0.
//
// The unit is written here rather than enabling the distribution's
// serial-getty@.service, and that is the part that took three boots to find. That
// unit carries `BindsTo=dev-%i.device`, and a .device unit exists only if udev
// announced it — but this image masks systemd-udevd, because a VM's hardware is
// fixed and known and udev is boot time spent discovering it. Enabling it gets:
//
//	[DEPEND] Dependency failed for serial-getty@ttyS0.service.
//
// A drop-in clearing BindsTo did not lift it either. Ten lines of unit with no
// device dependency at all does, and it says exactly what it wants: agetty, on
// this port, at getty.target.
func enableSerialGetty(port string) error {
	const unit = "spin-machine-console.service"

	if _, err := os.Stat("/usr/sbin/agetty"); err != nil {
		return fmt.Errorf("this guest has no agetty: %w", err)
	}

	// --noclear keeps the boot messages on screen, --keep-baud leaves the port as
	// QEMU set it, and the leading "-" on ExecStart stops a failure to open the
	// port from being reported as a crash on a machine that simply has no console.
	body := "[Unit]\n" +
		"Description=Serial console login (spin-machine debug boot)\n" +
		"After=systemd-user-sessions.service\n" +
		"Before=getty.target\n" +
		"IgnoreOnIsolate=yes\n" +
		"\n[Service]\n" +
		"ExecStart=-/usr/sbin/agetty --noreset --noclear --keep-baud 115200 " + port + " $TERM\n" +
		"Type=idle\n" +
		"Restart=always\n" +
		"RestartSec=1\n" +
		"TTYPath=/dev/" + port + "\n" +
		"TTYReset=yes\n" +
		"TTYVHangup=yes\n" +
		"StandardInput=tty\n" +
		"StandardOutput=tty\n" +
		"IgnoreSIGPIPE=no\n" +
		"SendSIGHUP=yes\n"

	path := "/etc/systemd/system/" + unit
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { // #nosec G306 -- a unit file
		return err
	}

	dir := "/etc/systemd/system/getty.target.wants"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	link := dir + "/" + unit
	_ = os.Remove(link)
	return os.Symlink(path, link)
}

// arg reads one key=value from a kernel command line.
func arg(cmdline, key string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}
