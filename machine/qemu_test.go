// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// outputDir is the release tree the QEMU tests ask. The default is the tree beside this
// package; the variable is what `task verify:args` passes, because OUTPUT_DIR can move and
// a check that looked at the old location would answer about a QEMU nobody is building any
// more.
func outputDir(t *testing.T) string {
	t.Helper()
	out, err := filepath.Abs(cmp.Or(os.Getenv("SPIN_MACHINE_OUTPUT"), "../_output"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// qemuVM is a QEMU this test started and kills when it ends.
type qemuVM struct {
	qemu string
	args []string
	out  *strings.Builder
	done chan struct{}
	exit error
}

// startQEMU runs qemu with files at descriptors 3 onwards.
//
// done is closed rather than sent to, so that both a caller and the cleanup can wait on
// it. Sent to, the first receive took the value and the second blocked for the whole test
// timeout — which is how a rejected argument presented as a hang instead of the error QEMU
// had already printed.
func startQEMU(t *testing.T, qemu string, args []string, files ...*os.File) *qemuVM {
	t.Helper()
	vm := &qemuVM{qemu: qemu, args: args, out: &strings.Builder{}, done: make(chan struct{})}
	cmd := exec.Command(qemu, args...) // #nosec G204 -- the binary and arguments this test built
	cmd.Stderr, cmd.Stdout = vm.out, vm.out
	cmd.ExtraFiles = files
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", qemu, err)
	}
	go func() { vm.exit = cmd.Wait(); close(vm.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-vm.done
	})
	return vm
}

// fail names the argument that was rejected, because a check whose failure says "exit
// status 1" costs more than it saves. QEMU names it itself — "-device
// virtio-balloon-pci,...: Property '...' not found" — so its output is reproduced whole,
// under the command line that produced it, one argument per line.
func (vm *qemuVM) fail(t *testing.T, what string, err error) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v\n\n%s", what, err, vm.qemu)
	for _, a := range vm.args {
		fmt.Fprintf(&b, " \\\n  %s", a)
	}
	if out := strings.TrimSpace(vm.out.String()); out != "" {
		fmt.Fprintf(&b, "\n\nQEMU said:\n%s", out)
	} else {
		b.WriteString("\n\nQEMU said nothing.")
	}
	t.Fatal(b.String())
}

// qmp is a monitor of a qemuVM, past the greeting and the capabilities handshake.
type qmp struct {
	t   *testing.T
	vm  *qemuVM
	enc *json.Encoder
	dec *json.Decoder
}

// dialQMP connects to one monitor of vm.
//
// The socket exists as soon as the chardev is created, which is before the CPU is created
// and long before any device is realized, so its existence proves nothing — a machine
// whose CPU model the accelerator refused left a listening socket behind and had already
// exited. An answer on it proves it, which is why the callers that ask whether a machine
// started send query-status and not only read the greeting.
func dialQMP(t *testing.T, socket string, vm *qemuVM) *qmp {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var conn net.Conn
	for {
		select {
		case <-vm.done:
			// The usual failure: QEMU rejected an argument and exited.
			vm.fail(t, "QEMU exited before answering on "+filepath.Base(socket), vm.exit)
		default:
		}
		c, err := net.Dial("unix", socket)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			vm.fail(t, "QEMU never answered on "+filepath.Base(socket), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	q := &qmp{t: t, vm: vm, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
	var greeting struct {
		QMP *struct{} `json:"QMP"`
	}
	if err := q.dec.Decode(&greeting); err != nil {
		vm.fail(t, "reading the QMP greeting", err)
	}
	if greeting.QMP == nil {
		vm.fail(t, "the monitor did not greet", fmt.Errorf("no QMP field"))
	}
	q.do("qmp_capabilities", nil)
	return q
}

// try sends one command and returns its reply, or QEMU's refusal as an error. Events in
// between are skipped.
func (q *qmp) try(command string, args any) (json.RawMessage, error) {
	q.t.Helper()
	req := map[string]any{"execute": command}
	if args != nil {
		req["arguments"] = args
	}
	if err := q.enc.Encode(req); err != nil {
		q.vm.fail(q.t, "sending "+command, err)
	}
	for {
		var reply struct {
			Return json.RawMessage `json:"return"`
			Event  string          `json:"event"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
		}
		if err := q.dec.Decode(&reply); err != nil {
			q.vm.fail(q.t, "reading the reply to "+command, err)
		}
		if reply.Event != "" {
			continue
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("%s", reply.Error.Desc)
		}
		return reply.Return, nil
	}
}

// do is try, failing the test on a refusal.
func (q *qmp) do(command string, args any) json.RawMessage {
	q.t.Helper()
	reply, err := q.try(command, args)
	if err != nil {
		q.vm.fail(q.t, command+" was refused", err)
	}
	return reply
}

// qemuTCG is the emulating build and the firmware beside it.
//
// The TCG binary and not the one a host runs: that one has no TCG compiled in
// (qemu:verify asserts it) and refuses to start without /dev/kvm, which is a
// device CI does not have and a check must not require.
//
// The two paths out of a release tree, and not machine.OpenRelease, because it asks
// for a whole machine and this needs half of one: the lane that runs this check
// on every push has a QEMU and deliberately no kernel and no base image, both of
// which are tens of minutes to build.
func qemuTCG(t *testing.T) (qemu, firmware string) {
	t.Helper()

	out := outputDir(t)
	qemu = filepath.Join(out, qemuTCGName)
	if _, err := os.Stat(qemu); err != nil {
		t.Skipf("no QEMU at %s: %v\n\t`task verify:args` is the gate — it fetches one"+
			" and refuses rather than skipping", qemu, err)
	}
	return qemu, filepath.Join(out, firmwareDir)
}

// pvhStub writes a kernel QEMU will load and never execute: an ELF with a PVH
// entry note, which is the only way into this machine.
//
// A stub and not the kernel from _output/kernel, deliberately. What is under
// test is the command line, and requiring the real kernel would tie this check
// to the kernel build — tens of minutes — so it could only ever run in a lane
// that had one, which is the lane a machine/*.go change does not reach. QEMU
// still loads it through pvh.bin out of the firmware directory, so -L and the
// option ROM that boot depends on are exercised either way.
//
// Without the note QEMU says "Error loading uncompressed kernel without PVH ELF
// Note" and exits, which is how this was checked.
func pvhStub(t *testing.T) string {
	t.Helper()

	const entry = 0x100000
	le := binary.LittleEndian

	// One Xen ELF note: XEN_ELFNOTE_PHYS32_ENTRY (18), the 32-bit entry point.
	note := make([]byte, 0, 20)
	note = le.AppendUint32(note, 4)  // namesz, "Xen\0"
	note = le.AppendUint32(note, 4)  // descsz, one address
	note = le.AppendUint32(note, 18) // XEN_ELFNOTE_PHYS32_ENTRY
	note = append(note, 'X', 'e', 'n', 0)
	note = le.AppendUint32(note, entry)

	const phoff, phentsize, phnum = 64, 56, 2
	noteOff := uint64(phoff + phentsize*phnum)
	textOff := noteOff + uint64(len(note))

	elf := make([]byte, 0, 256)
	elf = append(elf, 0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	elf = le.AppendUint16(elf, 2)  // ET_EXEC
	elf = le.AppendUint16(elf, 62) // EM_X86_64
	elf = le.AppendUint32(elf, 1)  // EV_CURRENT
	elf = le.AppendUint64(elf, entry)
	elf = le.AppendUint64(elf, phoff)
	elf = le.AppendUint64(elf, 0) // no sections
	elf = le.AppendUint32(elf, 0) // no flags
	elf = le.AppendUint16(elf, 64)
	elf = le.AppendUint16(elf, phentsize)
	elf = le.AppendUint16(elf, phnum)
	elf = le.AppendUint16(elf, 64)
	elf = le.AppendUint16(elf, 0)
	elf = le.AppendUint16(elf, 0)

	segment := func(typ, flags uint32, offset, addr, size uint64) {
		elf = le.AppendUint32(elf, typ)
		elf = le.AppendUint32(elf, flags)
		elf = le.AppendUint64(elf, offset)
		elf = le.AppendUint64(elf, addr) // vaddr
		elf = le.AppendUint64(elf, addr) // paddr
		elf = le.AppendUint64(elf, size) // filesz
		elf = le.AppendUint64(elf, size) // memsz
		elf = le.AppendUint64(elf, 4)    // align
	}
	segment(4, 4, noteOff, 0, uint64(len(note))) // PT_NOTE
	segment(1, 5, textOff, entry, 1)             // PT_LOAD, r-x
	elf = append(elf, note...)
	elf = append(elf, 0xf4) // hlt, which -S means is never reached

	path := filepath.Join(t.TempDir(), "vmlinux-stub")
	if err := os.WriteFile(path, elf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// diskDir is a directory that supports O_DIRECT. The test's temporary directory does
// not when it is on tmpfs, which it is on hosts that mount /tmp that way, and QEMU then
// refuses cache.direct=on with EINVAL — a failure about the host, not the options. So a
// tmpfs one is swapped for one under this module's _output, which is on disk.
func diskDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	const tmpfsMagic = 0x01021994
	if st.Type != tmpfsMagic {
		return dir
	}
	if err := os.MkdirAll(filepath.Join("..", "_output"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(filepath.Join("..", "_output"), "direct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// qcow2In creates a qcow2 in dir, over backing when it is not empty.
func qcow2In(t *testing.T, qemu, dir, name, backing string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	args := []string{"create", "-q", "-f", "qcow2"}
	if backing != "" {
		args = append(args, "-F", "qcow2", "-b", backing)
	}
	args = append(args, path, "16M")
	img := filepath.Join(filepath.Dir(qemu), "qemu-img")
	if out, err := exec.Command(img, args...).CombinedOutput(); err != nil { // #nosec G204 -- beside the binary under test
		t.Fatalf("%s %v: %v\n%s", img, args, err, out)
	}
	return path
}

// qmpSocket keeps the path short. A Unix socket address is 108 bytes including
// the terminator, and a test's temporary directory plus a socket name is close
// enough to it that QEMU has failed with "UNIX socket path is too long".
func qmpSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "qmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}
