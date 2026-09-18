// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
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
		BootCPUs: 1, Memory: Memory{SizeMB: 512}, Cmdline: DefaultCmdline().String(),
		QMPFD: 6,
		FDSets: []FDSet{
			{ID: 1, FDs: []FD{{Num: 3, Opaque: top}}},
			{ID: 2, FDs: []FD{{Num: 4, Opaque: base}}},
			{ID: 3, FDs: []FD{{Num: 5, Opaque: next}}},
		},
		Disks: []Disk{{Chain: []Image{{FDSet: 1, Format: "qcow2"}, {FDSet: 2, Format: "qcow2"}}, Serial: "root", Locking: true}},
	}
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	args, _, err = tcgOnly(args)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(qemu, append(args, "-S")...) // #nosec G204 -- the binary and arguments this test built
	var out strings.Builder
	cmd.Stderr, cmd.Stdout = &out, &out
	cmd.ExtraFiles = files
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", qemu, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	conn := dialQMP(t, socket, &out)
	defer func() { _ = conn.Close() }()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	var greeting struct {
		QMP *struct{} `json:"QMP"`
	}
	if err := dec.Decode(&greeting); err != nil || greeting.QMP == nil {
		t.Fatalf("no QMP greeting on the handed-over monitor: %v\n\nQEMU said:\n%s", err, out.String())
	}
	command := func(what string, req any) json.RawMessage {
		t.Helper()
		if err := enc.Encode(req); err != nil {
			t.Fatalf("sending %s: %v", what, err)
		}
		for {
			var reply struct {
				Return json.RawMessage `json:"return"`
				Event  string          `json:"event"`
				Error  *struct {
					Desc string `json:"desc"`
				} `json:"error"`
			}
			if err := dec.Decode(&reply); err != nil {
				t.Fatalf("reading the reply to %s: %v\n\nQEMU said:\n%s", what, err, out.String())
			}
			if reply.Event != "" {
				continue
			}
			if reply.Error != nil {
				t.Fatalf("%s was refused: %s\n\nQEMU said:\n%s", what, reply.Error.Desc, out.String())
			}
			return reply.Return
		}
	}
	command("qmp_capabilities", map[string]string{"execute": "qmp_capabilities"})

	command("blockdev-add next's file", map[string]any{"execute": "blockdev-add", "arguments": map[string]any{
		"driver": "file", "node-name": "next-file", "filename": "/dev/fdset/3", "aio": "io_uring", "locking": "on",
	}})
	command("blockdev-add next", map[string]any{"execute": "blockdev-add", "arguments": map[string]any{
		"driver": "qcow2", "node-name": "next", "file": "next-file", "backing": nil,
	}})
	command("blockdev-snapshot", map[string]any{"execute": "blockdev-snapshot", "arguments": map[string]any{
		"node": "blk0", "overlay": "next",
	}})

	var sets []struct {
		ID  int `json:"fdset-id"`
		FDs []struct {
			Opaque string `json:"opaque"`
		} `json:"fds"`
	}
	if err := json.Unmarshal(command("query-fdsets", map[string]string{"execute": "query-fdsets"}), &sets); err != nil {
		t.Fatal(err)
	}
	opaque := map[int]string{}
	for _, s := range sets {
		for _, fd := range s.FDs {
			opaque[s.ID] = fd.Opaque
		}
	}
	for id, want := range map[int]string{1: top, 2: base, 3: next} {
		if opaque[id] != want {
			t.Errorf("descriptor set %d says it is %q, want %q", id, opaque[id], want)
		}
	}
}
