// SPDX-License-Identifier: Apache-2.0

// probeinit is PID 1 of the diagnostic initrd the firmware and floor probes boot when no
// SPIN_PROBE_INITRD is given (boot/probe_initrd_test.go builds it). It is what those probes
// document an initrd's /init must do: mount proc, print the kernel log lines that mention
// vmgenid, print SPIN-READY, and stay alive printing every new kernel log line - so a restored
// guest's "crng reseeded due to virtual machine fork" reaches the host.
//
// Diagnostic input, never part of a release: what runs as a machine's PID 1 is its caller's.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func main() {
	for _, m := range []struct{ src, dir, fs string }{
		{"proc", "/proc", "proc"},
		{"devtmpfs", "/dev", "devtmpfs"},
	} {
		_ = os.MkdirAll(m.dir, 0o755)
		_ = syscall.Mount(m.src, m.dir, m.fs, 0, "")
	}

	fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		fmt.Println("SPIN-PROBE: no /dev/kmsg:", err)
		fmt.Println("SPIN-READY")
		select {}
	}
	buf := make([]byte, 8192)
	// One record per read. What is already in the log first, without blocking, then the
	// marker, then whatever arrives after it.
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EAGAIN {
			break
		}
		if err != nil || n <= 0 {
			continue
		}
		if line := message(buf[:n]); strings.Contains(strings.ToLower(line), "vmgenid") {
			fmt.Println(line)
		}
	}
	fmt.Println("SPIN-READY")
	_ = syscall.SetNonblock(fd, false)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			continue
		}
		fmt.Println(message(buf[:n]))
	}
}

// message is a /dev/kmsg record's text: "6,339,5140900,-;text\n" is text.
func message(rec []byte) string {
	s := string(rec)
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimRight(s, "\n")
}
