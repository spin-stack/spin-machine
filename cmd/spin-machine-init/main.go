// Command spin-machine-init is a PID 1 that exists to hand the machine over to
// something else.
//
// It is a debugging tool and deliberately almost nothing: it mounts the four
// filesystems a Linux userland cannot run without, moves onto the real root, and
// execs whatever was asked for. It is not a container runtime, it supervises
// nothing, and it is not part of a release — the software that runs inside a
// guest belongs to whoever runs guests, not to the repository that builds the
// machine.
//
// It exists because a machine booted with `init=/bin/bash` and nothing else has
// no /proc, so `df` warns, `ps` sees nothing and `poweroff` answers "Running in
// chroot, ignoring request" — which looks like a fault in the image and is not.
// Somebody has to mount them, and in production that somebody is the guest's own
// init. For `task shell` it is this.
//
// Built static, put in a cpio, and passed as -initrd. Two kernel arguments steer
// it, both read from /proc/cmdline:
//
//	spinmachine.root=/dev/vda    the block device to move onto (required)
//	spinmachine.init=/bin/bash   what to exec there (default /sbin/init)
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

	root := arg(string(cmdline), "spinmachine.root")
	init := arg(string(cmdline), "spinmachine.init")
	if root == "" {
		return fmt.Errorf("no spinmachine.root= on the kernel command line")
	}
	if init == "" {
		init = "/sbin/init"
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

	// Exec, not fork: PID 1 has to *be* the program that was asked for, so it
	// receives the signals and reaps the orphans that PID 1 is expected to.
	if err := syscall.Exec(init, []string{init}, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", init, err)
	}
	return nil
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
