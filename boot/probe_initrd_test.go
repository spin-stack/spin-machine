// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// probeInitrd is SPIN_PROBE_INITRD when it names one, and otherwise the initrd of probeinit,
// built here: the firmware and floor probes ran only where somebody had made a diagnostic
// initrd by hand, which the lab runner never has. The same one serves both sides of a
// comparison, so its own cost cancels out.
func probeInitrd(t *testing.T) string {
	t.Helper()
	if os.Getenv("SPIN_PROBE_INITRD") != "" {
		return envFile(t, "SPIN_PROBE_INITRD")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "init")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "./probeinit")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building probeinit: %v\n%s", err, out)
	}
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "probe.cpio")
	if err := os.WriteFile(path, newcArchive([]cpioEntry{
		{name: "dev", mode: 0o040755},
		// Without it the kernel starts init with no console, and nothing it prints arrives.
		{name: "dev/console", mode: 0o020600, rdevMajor: 5, rdevMinor: 1},
		{name: "proc", mode: 0o040755},
		{name: "init", mode: 0o100755, data: body},
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type cpioEntry struct {
	name                 string
	mode                 uint32
	rdevMajor, rdevMinor uint32
	data                 []byte
}

// newcArchive is the "new ASCII" cpio format the kernel unpacks an initramfs from: a 110-byte
// hex header per entry, the name and the data each padded to four bytes, and a TRAILER!!!.
func newcArchive(entries []cpioEntry) []byte {
	var b bytes.Buffer
	pad := func() {
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	write := func(ino int, e cpioEntry) {
		nlink := 1
		if e.mode&0o170000 == 0o040000 {
			nlink = 2
		}
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, e.mode, 0, 0, nlink, 0, len(e.data), 0, 0, e.rdevMajor, e.rdevMinor, len(e.name)+1, 0)
		b.WriteString(e.name)
		b.WriteByte(0)
		pad()
		b.Write(e.data)
		pad()
	}
	for i, e := range entries {
		write(i+1, e)
	}
	write(0, cpioEntry{name: "TRAILER!!!"})
	return b.Bytes()
}

func TestANewcArchiveIsWhatCpioReads(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("no cpio on this host to read the archive back")
	}
	a := newcArchive([]cpioEntry{
		{name: "dev", mode: 0o040755},
		{name: "init", mode: 0o100755, data: []byte("#!/bin/sh\n")},
	})
	list := exec.Command("cpio", "-itv", "--quiet")
	list.Stdin = bytes.NewReader(a)
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("cpio refused the archive: %v\n%s", err, out)
	}
	for _, want := range []string{"drwxr-xr-x", "dev", "-rwxr-xr-x", "init"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("cpio -itv does not list %q:\n%s", want, out)
		}
	}
}
