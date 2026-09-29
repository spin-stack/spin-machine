// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A disk handed over as descriptors is read without a path: the chain is opened after the
// directory that held it is gone, so a backing name in a header points nowhere and QEMU
// reads what it was given or nothing. Then the top is sealed under a new overlay handed over
// the same way, with one descriptor per image, and query-fdsets gives back what each
// descriptor was. The monitor is on a socket this test listens on and QEMU was handed, as a
// QEMU that may create no socket needs.
//
// Under the TCG binary, like TestQEMUAcceptsEveryArgument, and skipped where there is none.
func TestAChainOverDescriptorsIsReadWithoutAPathAndSealedUnderAnOverlay(t *testing.T) {
	qemu, firmware := qemuTCG(t)
	kernel := pvhStub(t)

	dir := diskDir(t)
	base := qcow2In(t, qemu, dir, "base.qcow2", "")
	top := qcow2In(t, qemu, dir, "top.qcow2", base)
	next := qcow2In(t, qemu, dir, "next.qcow2", top)

	open := func(path string, flag int) *os.File {
		t.Helper()
		f, err := os.OpenFile(path, flag, 0) // #nosec G304 -- a file this test made
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	// Descriptors 3 onwards, in this order.
	files := []*os.File{open(top, os.O_RDWR), open(base, os.O_RDONLY), open(next, os.O_RDWR)}
	// The console, a FIFO as a runner keeps one: its reader first, or opening the writer blocks.
	fifo := filepath.Join(t.TempDir(), "console")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = open(fifo, os.O_RDONLY|syscall.O_NONBLOCK)
	files = append(files, open(fifo, os.O_WRONLY))
	socket := qmpSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unix := listener.(*net.UnixListener) //nolint:forcetypeassert // Listen("unix") is one
	monitor, err := unix.File()
	if err != nil {
		t.Fatal(err)
	}
	// QEMU holds the listening socket from here, at the path this test dials: Go unlinks it
	// on Close unless told not to.
	unix.SetUnlinkOnClose(false)
	_ = listener.Close()
	files = append(files, monitor)

	// Nothing QEMU could open by name is left where the headers say.
	if err := os.Rename(dir, dir+".gone"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(dir+".gone", dir) })

	spec := Spec{
		QEMU: qemu, Kernel: kernel, Firmware: firmware,
		BootCPUs: 1, Memory: Memory{SizeMB: 512}, Cmdline: DefaultCmdline(),
		Monitors: []Monitor{{FD: 7}},
		FDSets: []FDSet{
			{ID: 1, FDs: []FD{{Num: 3, Opaque: top}}},
			{ID: 2, FDs: []FD{{Num: 4, Opaque: base}}},
			{ID: 3, FDs: []FD{{Num: 5, Opaque: next}}},
			{ID: 4, FDs: []FD{{Num: 6, Opaque: fifo}}},
		},
		SerialFDSet: 4,
		Disks:       []Disk{{Chain: []Image{{FDSet: 1, Format: "qcow2"}, {FDSet: 2, Format: "qcow2"}}, Serial: "root", Locking: true}},
	}
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	args, _, err = tcgOnly(args)
	if err != nil {
		t.Fatal(err)
	}

	vm := startQEMU(t, qemu, append(args, "-S"), files...)
	q := dialQMP(t, socket, vm)

	q.do("blockdev-add", map[string]any{
		"driver": "file", "node-name": "next-file", "filename": "/dev/fdset/3", "aio": "io_uring", "locking": "on",
	})
	q.do("blockdev-add", map[string]any{
		"driver": "qcow2", "node-name": "next", "file": "next-file", "backing": nil,
	})
	q.do("blockdev-snapshot", map[string]any{"node": "blk0", "overlay": "next"})

	var sets []struct {
		ID  int `json:"fdset-id"`
		FDs []struct {
			Opaque string `json:"opaque"`
		} `json:"fds"`
	}
	if err := json.Unmarshal(q.do("query-fdsets", nil), &sets); err != nil {
		t.Fatal(err)
	}
	opaque := map[int]string{}
	for _, s := range sets {
		for _, fd := range s.FDs {
			opaque[s.ID] = fd.Opaque
		}
	}
	for id, want := range map[int]string{1: top, 2: base, 3: next, 4: fifo} {
		if opaque[id] != want {
			t.Errorf("descriptor set %d says it is %q, want %q", id, opaque[id], want)
		}
	}
}
