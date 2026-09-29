// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
//   - -accel kvm -> tcg. The KVM-only binary refuses to start without /dev/kvm
//     and CI has none, so the TCG build is the only one that can answer the
//     question at all. The machine string does not move; kernel-irqchip=on,
//     hpet=off, acpi=on, sata=off and smbus=off are accepted by both, which is
//     itself worth knowing — every case below asks that of whichever machine
//     string Shape currently produces, so an option added there is under this
//     check without a case being written for it.
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
		case "-accel":
			switch value {
			case "tcg":
				// Already emulated, because the spec asked to be. Nothing to stand
				// in for, and the check below is satisfied: what it guards against
				// is running a KVM machine while believing this is emulation.
			case "kvm":
				out[i+1] = "tcg"
				rewrites = append(rewrites, "-accel kvm -> tcg")
			default:
				// kvm with a /dev/kvm descriptor, which TCG has no use for and this
				// check has no descriptor to hand over.
				return nil, nil, fmt.Errorf("-accel %s cannot be stood in for by the TCG binary", value)
			}
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

	// Loudly, rather than by testing a machine nobody runs: if the command line
	// stops saying -accel kvm, this check has been quietly answering a different
	// question.
	if !accel {
		return nil, nil, fmt.Errorf("the command line names neither -accel kvm nor -accel tcg, so this check cannot say which accelerator it just exercised")
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
	// A published template, which a restoring QEMU may read and not write.
	sealedMemFile := filepath.Join(t.TempDir(), "sealed-memory")
	if err := os.WriteFile(sealedMemFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sealedMemFile, 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealedMemFile, 0o444); err != nil {
		t.Fatal(err)
	}

	base := func() Spec {
		return Spec{
			QEMU:     qemu,
			Kernel:   kernel,
			Firmware: firmware,
			BootCPUs: 2,
			Memory:   Memory{SizeMB: 512},
			Cmdline:  DefaultCmdline(),
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
		// Restored from a file it may not write: a published template, which belongs to
		// the host and not to the VM. Root would open it anyway, so under root this case
		// says nothing; the gate runs as a user.
		name: "restore target, read-only template",
		spec: func(s Spec) Spec {
			s.Memory.File = sealedMemFile
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
				{Path: qcow2In(t, qemu, t.TempDir(), "overlay.qcow2", ""), Format: "qcow2",
					Serial: "overlay", Locking: true, Cache: "writeback"},
			}
			return s
		},
	}, {
		// A writable overlay opened O_DIRECT over a chain the host caches: the options
		// name a node that exists only because the image has a backing file, so it
		// is asked of a real one.
		name: "direct over backing",
		spec: func(s Spec) Spec {
			dir := diskDir(t)
			base := qcow2In(t, qemu, dir, "base.qcow2", "")
			s.Disks = []Disk{{Path: qcow2In(t, qemu, dir, "overlay.qcow2", base), Format: "qcow2",
				Serial: "overlay", Locking: true, DirectOverBacking: true}}
			return s
		},
	}, {
		// The same machine emulated: no /dev/kvm required, and the guest's
		// instructions executed by QEMU itself.
		//
		// Worth starting and not only rendering, because the two arguments it
		// changes are ones QEMU refuses rather than ignores. -accel tcg is absent
		// from the ordinary build — "invalid accelerator tcg" — which is why this
		// names the TCG one; and "host" is not a model an emulator can present,
		// so a shape that kept the KVM default would die with "CPU model 'host'
		// requires KVM or HVF". Everything else about the machine is unchanged,
		// which is the claim.
		name: "emulated",
		spec: func(s Spec) Spec {
			s.Accel = "tcg"
			return s
		},
	}, {
		// The hotplug controller, which is a machine that will be given disks while it
		// runs. Only that the controller is accepted and realized is asked here; that a
		// disk can then be added to it is TestADiskArrivesAtItsTargetOnTheHotplugController's.
		name: "hotplug disks",
		spec: func(s Spec) Spec {
			s.HotplugDisks = MaxHotplugDisks
			return s
		},
	}, {
		// Two monitors, which is a machine whose lifecycle and whose disk are
		// driven by different things. Both are required to answer: a socket that
		// greets and never replies would look like a working monitor to whoever
		// only dialled it, and the greeting is written by the chardev.
		name: "two monitors",
		spec: func(s Spec) Spec {
			s.Monitors = []Monitor{{Socket: qmpSocket(t)}}
			return s
		},
	}, {
		// Paths with commas in them, which QEMU splits an option string on. Started
		// and not only rendered: that a doubled comma reads back as one is QEMU's
		// parser's contract, so it is QEMU that is asked. Undoubled, the disk's name
		// is a readonly= QEMU cannot parse and the socket's is a wait= it obeys.
		name: "commas in paths",
		spec: func(s Spec) Spec {
			mem := filepath.Join(t.TempDir(), "memory,share=on")
			if err := os.WriteFile(mem, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(mem, 512<<20); err != nil {
				t.Fatal(err)
			}
			s.Memory.File, s.Memory.Shared = mem, true
			s.Disks = []Disk{{Path: rawDisk(t, "a,readonly=off.raw"), Format: "raw", Serial: "a,b"}}
			s.Monitors = []Monitor{{Socket: qmpSocket(t) + ",wait=on"}}
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
			// Every machine gets a first monitor, and a case that is about monitors adds
			// its own after it.
			s.Monitors = append([]Monitor{{Socket: qmpSocket(t)}}, s.Monitors...)
			var sockets []string
			for _, m := range s.Monitors {
				sockets = append(sockets, m.Socket)
			}

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
			vm := startQEMU(t, qemu, append(args, "-S"))

			// Every monitor, and not the process: a machine with two of them has to
			// serve both, because they exist so that two components can drive it
			// without relaying through each other, and a second socket that greets
			// but never replies would look like a working one to whoever only
			// dialled it.
			//
			// query-status after the handshake, and not the greeting alone: the
			// greeting is written by the chardev when the socket is accepted, whereas
			// a reply to a command comes from the main loop, which is only reached
			// once every device on the command line has been created and realized.
			// That is the claim this check makes.
			for _, socket := range sockets {
				status := dialQMP(t, socket, vm).do("query-status", nil)
				t.Logf("running, monitor %s answered query-status with %s", filepath.Base(socket), status)
			}
		})
	}
}

func rawDisk(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
