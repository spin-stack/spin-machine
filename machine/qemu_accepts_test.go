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
	"testing"
	"time"
)

// The rest of this package's tests assert on the strings Args produces, which
// proves the package agrees with itself. This one asks the binary.
//
// Every device and property named in machine.go — free-page-reporting,
// deflate-on-oom, requested-size, romfile=, the ICH9-LPC globals, the four
// -sandbox restrictions — is a string agreed with a QEMU built from
// qemu/devices.mak, and the two halves move independently: a device dropped from
// the allowlist, a property renamed at the next version bump, or a typo added
// here all produce the same thing, a VM that does not start, found days later by
// whoever booted one and reading like a fault in whatever launched it.
//
// It starts the machine rather than parsing it, because "it parsed" is a weaker
// claim than "it started": -S stops it before the first instruction, and a
// process that answers on the QMP monitor has already created and realized every
// device on its command line. Nine machines start, answer and are killed in
// 0.23 s together (measured 2026-09-08) — less than one boot.
//
// It is not part of `task test`: it needs a QEMU binary, and a source checkout
// has none. `task verify:args` is the gate, and it refuses rather than skips
// when the binary is missing. A developer running `go test ./...` with an empty
// _output/ gets the skip below.

// tcgOnly names the three arguments that cannot mean on the TCG binary what they
// mean on the one a tenant's host runs, and is the whole of what this check
// changes about a command line before handing it over. Everything else is passed
// byte for byte as Args produced it.
//
// Each rewrite is reported and printed with -v, so a reader can see exactly how
// far the claim reaches — and each is here because the alternative is worse:
//
//   - accel=kvm -> accel=tcg. The KVM-only binary refuses to start without
//     /dev/kvm and CI has none, so the TCG build is the only one that can answer
//     the question at all. Nothing else in the machine string moves;
//     kernel-irqchip=on, hpet=off and acpi=on are accepted by both, which is
//     itself worth knowing.
//
//   - -cpu host -> max. "host" is a KVM-only value by construction — QEMU says
//     "CPU model 'host' requires KVM or HVF" and exits — and max is the other
//     model that derives its features from the silicon, so migratable=on, the
//     property that only exists on those two, stays under test.
//
//   - the tap netdev -> a hub port. A NIC's backend is a TAP file descriptor the
//     caller opened, and a test cannot open one without CAP_NET_ADMIN, so
//     `tap,id=netN,fd=N,vhost=on` is the one line here that is not checked. What
//     is checked is the device that references it, verbatim: virtio-net-pci with
//     romfile= (which is a NIC that cannot start if the option ROM is missing and
//     the property is misspelt), the MAC, disable-legacy and the slot.
func tcgOnly(args []string) ([]string, []string, error) {
	out := append([]string(nil), args...)
	var rewrites []string
	accel := false

	for i := 0; i+1 < len(out); i++ {
		value := out[i+1]
		switch out[i] {
		case "-machine":
			if !strings.Contains(value, "accel=kvm") {
				continue
			}
			out[i+1] = strings.Replace(value, "accel=kvm", "accel=tcg", 1)
			rewrites = append(rewrites, "-machine accel=kvm -> accel=tcg")
			accel = true
		case "-cpu":
			model, rest, _ := strings.Cut(value, ",")
			if model != "host" {
				continue
			}
			out[i+1] = strings.TrimSuffix("max,"+rest, ",")
			rewrites = append(rewrites, fmt.Sprintf("-cpu %s -> %s", value, out[i+1]))
		case "-netdev":
			if !strings.HasPrefix(value, "tap,") {
				continue
			}
			id := ""
			for _, field := range strings.Split(value, ",") {
				if v, ok := strings.CutPrefix(field, "id="); ok {
					id = v
				}
			}
			if id == "" {
				return nil, nil, fmt.Errorf("a tap netdev with no id: %q", value)
			}
			out[i+1] = fmt.Sprintf("hubport,id=%s,hubid=0", id)
			rewrites = append(rewrites, fmt.Sprintf("-netdev %s -> %s", value, out[i+1]))
		}
	}

	// Loudly, rather than by testing a machine nobody runs: if the machine string
	// stops saying accel=kvm, this check has been quietly answering a different
	// question.
	if !accel {
		return nil, nil, fmt.Errorf("no accel=kvm in the machine string, so there is nothing for the TCG binary to stand in for")
	}
	return out, rewrites, nil
}

// TestQEMUAcceptsEveryArgument starts each interesting Spec under the TCG binary
// and requires it to reach the monitor.
func TestQEMUAcceptsEveryArgument(t *testing.T) {
	qemu, firmware := qemuTCG(t)
	kernel := pvhStub(t)

	// A memory file, and one large enough: memory-backend-file takes the size
	// from the object and maps the file, so a short one is an error about the
	// file rather than about the command line.
	memFile := filepath.Join(t.TempDir(), "memory")
	if err := os.WriteFile(memFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(memFile, 512<<20); err != nil {
		t.Fatal(err)
	}

	base := func() Spec {
		return Spec{
			QEMU:     qemu,
			Kernel:   kernel,
			Firmware: firmware,
			BootCPUs: 2,
			Memory:   Memory{SizeMB: 512},
			Cmdline:  DefaultCmdline().String(),
		}
	}

	cases := []struct {
		name string
		// needsVsock is the one host resource this check cannot stand in for.
		needsVsock bool
		spec       func(s Spec) Spec
	}{{
		// The plainest machine there is: anonymous RAM, no growth, no vsock, no
		// disk, no NIC. Everything unconditional in Args is in this one.
		name: "plain",
		spec: func(s Spec) Spec { return s },
	}, {
		// The machine a template is taken from: RAM in a file, mapped shared so
		// the pages the guest dirties reach the file the restores will read.
		name: "template source",
		spec: func(s Spec) Spec {
			s.Memory.File, s.Memory.Shared = memFile, true
			return s
		},
	}, {
		// And the machine one is restored into: the same file mapped private,
		// with no machine state until QMP says where to load it from.
		name: "restore target",
		spec: func(s Spec) Spec {
			s.Memory.File = memFile
			s.IncomingDefer = true
			return s
		},
	}, {
		// A memory ceiling, which is what adds virtio-mem and its second memory
		// backend, and a vCPU ceiling, which is what adds maxcpus=.
		name: "ceilings",
		spec: func(s Spec) Spec {
			s.Memory.MaxMB = 2048
			s.MaxCPUs = 8
			return s
		},
	}, {
		name:       "vsock",
		needsVsock: true,
		spec: func(s Spec) Spec {
			s.VsockCID = 12345
			return s
		},
	}, {
		// Both disks a VM here gets: the read-only base, opened by every VM on
		// the host at once, and a writable overlay with a lock and a serial.
		name: "disks",
		spec: func(s Spec) Spec {
			s.Disks = []Disk{
				{Path: rawDisk(t, "base.raw"), Format: "raw", Readonly: true, Serial: "base"},
				{Path: qcow2Disk(t, qemu, "overlay.qcow2"), Format: "qcow2",
					Serial: "overlay", Locking: true, Cache: "writeback"},
			}
			return s
		},
	}, {
		name: "nic",
		spec: func(s Spec) Spec {
			s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:12:34:56"}}
			return s
		},
	}, {
		// The other half of Shape: a named model, which is enforce=on rather than
		// migratable=on.
		//
		// qemu64 and not a microarchitecture anybody would deploy, because
		// enforce=on is exactly what stops one from being checked here — it
		// refuses to start a model the accelerator cannot fully provide, which is
		// the whole reason it is on the command line, and TCG cannot provide any
		// of them. Skylake-Server-v4 was tried: ten warnings and then "TCG doesn't
		// support requested features", exit 1, which is the flag doing its job.
		// What is left to check is the package's own contribution — that enforce
		// is a property this binary has, on a model it knows — since which model
		// is named is the caller's.
		name: "named cpu",
		spec: func(s Spec) Spec {
			s.CPU = "qemu64"
			return s
		},
	}, {
		// The shape a real VM has, all at once: file-backed RAM, a ceiling, a
		// vsock, two disks, a NIC and a console.
		name:       "everything",
		needsVsock: true,
		spec: func(s Spec) Spec {
			s.Memory.File, s.Memory.Shared = memFile, true
			s.Memory.MaxMB, s.MaxCPUs = 2048, 8
			s.VsockCID = 12345
			s.Disks = []Disk{
				{Path: rawDisk(t, "everything.raw"), Format: "raw", Readonly: true, Serial: "base"},
			}
			s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:12:34:56"}}
			s.Serial = "file:" + filepath.Join(t.TempDir(), "console.log")
			return s
		},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.needsVsock {
				// The one device with a host dependency this check cannot stand
				// in for: vhost-vsock is a kernel module, and there is no
				// backend that keeps the device line and drops the requirement.
				// The workflows load it and open it up; a machine that cannot is
				// told what went unchecked rather than shown a pass.
				//
				// Opened and not stat'd, because the two ways this fails are
				// different and only one of them is "no module": a GitHub runner
				// has the device node and hands the user no access to it, and a
				// stat cannot tell the difference — measured 2026-09-08, where it
				// reached QEMU as "Could not open '/dev/vhost-vsock': Permission
				// denied" and read like a rejected argument.
				dev, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0)
				if err != nil {
					t.Skipf("vhost-vsock-pci went unchecked, because this host will not"+
						" give the device up: %v\n\t(modprobe vhost_vsock, and the node"+
						" has to be openable by whoever runs QEMU)", err)
				}
				_ = dev.Close()
			}

			s := c.spec(base())
			s.QMPSocket = qmpSocket(t)

			args, err := s.Args()
			if err != nil {
				t.Fatal(err)
			}
			args, rewrites, err := tcgOnly(args)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rewrites {
				t.Logf("rewritten for the TCG binary: %s", r)
			}

			// -S, and it is the only argument added: it stops the machine before
			// the first instruction, so what is measured is device creation and
			// not a guest booting under emulation.
			start(t, qemu, append(args, "-S"), s.QMPSocket)
		})
	}
}

// start runs QEMU and requires it to answer on its monitor.
//
// Every failure here has to name the argument that was rejected, because a check
// whose failure says "exit status 1" costs more than it saves. QEMU names it
// itself — "-device virtio-balloon-pci,...: Property '...' not found" — so its
// stderr is reproduced whole, under the command line that produced it, one
// argument per line.
func start(t *testing.T, qemu string, args []string, socket string) {
	t.Helper()

	var stderr strings.Builder
	cmd := exec.Command(qemu, args...) // #nosec G204 -- the binary and arguments this test built
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", qemu, err)
	}

	// Closed rather than sent to, so that both the loop below and the deferred
	// kill can wait on it. Sent to, the first receive took the value and the
	// second blocked for the whole test timeout — which is how a rejected
	// argument presented as a hang instead of the error QEMU had already printed.
	var exit error
	done := make(chan struct{})
	go func() { exit = cmd.Wait(); close(done) }()

	fail := func(what string, err error) {
		t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %v\n\n%s", what, err, qemu)
		for _, a := range args {
			fmt.Fprintf(&b, " \\\n  %s", a)
		}
		if out := strings.TrimSpace(stderr.String()); out != "" {
			fmt.Fprintf(&b, "\n\nQEMU said:\n%s", out)
		} else {
			b.WriteString("\n\nQEMU said nothing.")
		}
		t.Fatal(b.String())
	}

	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	// The socket exists as soon as the chardev is created, which is before the
	// CPU is created and long before any device is realized, so its existence
	// proves nothing — a machine whose CPU model the accelerator refused left a
	// listening socket behind and had already exited. An answer on it proves it.
	deadline := time.Now().Add(20 * time.Second)
	var conn net.Conn
	for {
		select {
		case <-done:
			// The usual failure: QEMU rejected an argument and exited.
			fail("QEMU exited before answering on the monitor", exit)
		default:
		}
		c, err := net.Dial("unix", socket)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			fail("QEMU never answered on the monitor", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)

	var greeting struct {
		QMP *struct{} `json:"QMP"`
	}
	if err := dec.Decode(&greeting); err != nil {
		fail("reading the QMP greeting", err)
	}
	if greeting.QMP == nil {
		fail("the monitor did not greet", fmt.Errorf("no QMP field"))
	}

	// query-status after the handshake, and not the greeting alone: the greeting
	// is written by the chardev when the socket is accepted, whereas a reply to a
	// command comes from the main loop, which is only reached once every device
	// on the command line has been created and realized. That is the claim this
	// check makes.
	for _, command := range []string{"qmp_capabilities", "query-status"} {
		if err := json.NewEncoder(conn).Encode(map[string]string{"execute": command}); err != nil {
			fail("sending "+command, err)
		}
		var reply struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
		}
		if err := dec.Decode(&reply); err != nil {
			fail("reading the reply to "+command, err)
		}
		if reply.Error != nil {
			fail(command+" failed", fmt.Errorf("%s", reply.Error.Desc))
		}
		if command == "query-status" {
			t.Logf("running, monitor answered query-status with %s", reply.Return)
		}
	}
}

// qemuTCG is the emulating build and the firmware beside it.
//
// The TCG binary and not the one a host runs: that one has no TCG compiled in
// (qemu:verify asserts it) and refuses to start without /dev/kvm, which is a
// device CI does not have and a check must not require.
//
// The two paths out of a release tree, and not machine.Open, because Open asks
// for a whole machine and this needs half of one: the lane that runs this check
// on every push has a QEMU and deliberately no kernel and no base image, both of
// which are tens of minutes to build.
func qemuTCG(t *testing.T) (qemu, firmware string) {
	t.Helper()

	// The default is the tree beside this package; the variable is what `task
	// verify:args` passes, because OUTPUT_DIR can move and a check that looked at
	// the old location would answer about a QEMU nobody is building any more.
	out, err := filepath.Abs(cmp.Or(os.Getenv("SPIN_MACHINE_OUTPUT"), "../_output"))
	if err != nil {
		t.Fatal(err)
	}
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

func rawDisk(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// qcow2Disk creates one with qemu-img, which is the tool a caller creates a
// VM's overlay with and sits beside the emulator in every release.
func qcow2Disk(t *testing.T, qemu, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	img := filepath.Join(filepath.Dir(qemu), "qemu-img")
	cmd := exec.Command(img, "create", "-f", "qcow2", path, "16M") // #nosec G204 -- beside the binary under test
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s create: %v\n%s", img, err, out)
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
