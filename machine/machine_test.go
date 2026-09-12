// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spec returns a minimal valid spec pointing at files that exist, so
// Fingerprint has something to hash.
func spec(t *testing.T) Spec {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return Spec{
		QEMU:     write("qemu-system-x86_64", "qemu"),
		Kernel:   write("vmlinux", "kernel"),
		Firmware: dir,
		BootCPUs: 2,
		Memory:   Memory{SizeMB: 512},
	}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestArgsCarriesTheShape(t *testing.T) {
	s := spec(t)
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	sh := s.Shape()
	for flag, want := range map[string]string{
		"-machine": sh.Machine,
		"-cpu":     sh.CPU,
		"-smp":     sh.SMP,
		"-m":       sh.Memory,
	} {
		if got := argValue(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
}

// A machine given no display, no default devices and no way to spawn a helper is
// most of what makes this machine what it is, and none of it is visible in a
// guest until something is wrong.
func TestArgsHasTheMachineWideFlags(t *testing.T) {
	args, err := spec(t).Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-nodefaults", "-sandbox on,obsolete=deny", "vmgenid,guid=auto"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// The slot map is the reason the kernel can be told pci=lastbus=0. A device that
// moved off its slot is a device the guest may not find, with no error.
func TestDevicesSitOnTheirFixedSlots(t *testing.T) {
	s := spec(t)
	s.VsockCID = 7
	s.Disks = []Disk{{Path: "/a.qcow2", Format: "qcow2"}, {Path: "/b.raw", Format: "raw"}}
	s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}

	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"vhost-vsock-pci,guest-cid=7,disable-legacy=on,addr=0x2",
		"virtio-rng-pci,disable-legacy=on,addr=0x3",
		// The only way a running VM gives memory back. Without it the answer to
		// "how is memory reclaimed?" is "the VM exits".
		"virtio-balloon-pci,free-page-reporting=on,deflate-on-oom=on,disable-legacy=on,addr=0x4",
		"virtio-blk-pci,drive=blk0,disable-legacy=on,addr=0x5",
		"virtio-blk-pci,drive=blk1,disable-legacy=on,addr=0x6",
		"virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01,romfile=,disable-legacy=on,addr=0x10",
		// The two that are invisible in a guest until something is slow: without
		// them QEMU hands every block request to a worker pool and copies every
		// packet through userspace, and nothing reports either.
		"aio=io_uring",
		"tap,id=net0,fd=3,vhost=on",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// host_mtu is how the guest is told what it may send, and the only way of telling
// it that it cannot then ignore: virtnet_probe assigns the announced value to both
// dev->mtu and dev->max_mtu, so the guest boots with it and the kernel refuses to
// raise it. Root inside the machine can discard a DHCP option or a kernel
// parameter; this it cannot.
func TestTheGuestIsToldItsMTUOnTheDevice(t *testing.T) {
	s := spec(t)
	s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01", MTU: 1400}}

	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01,romfile=,disable-legacy=on,addr=0x10,host_mtu=1400") {
		t.Errorf("the NIC does not announce its MTU: %s", joined)
	}

	// And a machine with nothing to announce says nothing, rather than announcing
	// zero — which QEMU takes as "no MTU" but only after the guest has read a
	// config field the feature bit says is valid.
	s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}
	args, err = s.Args()
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "host_mtu") {
		t.Errorf("a NIC with no MTU still names one: %s", joined)
	}
}

// A disk's format is stated, never guessed: a wrong guess is a guest that boots
// and finds a disk full of nothing.
func TestDiskFormatIsRequired(t *testing.T) {
	s := spec(t)
	s.Disks = []Disk{{Path: "/a.qcow2"}}
	if _, err := s.Args(); err == nil {
		t.Fatal("a disk with no format was accepted")
	}
}

// Growing a VM is virtio-mem now, not ACPI DIMM slots. -m carries the ceiling and
// no slots=, the device appears, and nothing is plugged until somebody asks.
func TestMemoryCeilingGivesAVirtioMemDeviceAndNoSlots(t *testing.T) {
	s := spec(t)
	s.Memory.SizeMB = 2048
	s.Memory.MaxMB = 8192

	if got := s.Shape().Memory; got != "2048,maxmem=8192M" {
		t.Errorf("-m is %q; slots are ACPI DIMM sockets and this machine plugs none", got)
	}

	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		// The region between the boot memory and the ceiling, and nothing else.
		"memory-backend-ram,id=mem.growth,size=6144M",
		"virtio-mem-pci,id=vmem0,memdev=mem.growth,requested-size=0,disable-legacy=on,addr=0x1e",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}

	// A VM that cannot grow gets neither, and its -m says so.
	fixed := spec(t)
	fixed.Memory.SizeMB = 2048
	if got := fixed.Shape().Memory; got != "2048" {
		t.Errorf("-m is %q for a machine with no ceiling", got)
	}
	args, err = fixed.Args()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "virtio-mem") {
		t.Error("a machine with no memory ceiling was given a virtio-mem device")
	}
}

// File-backed RAM changes the machine string, which is why it is in the shape
// and not only in an -object.
func TestMemoryFileChangesTheShape(t *testing.T) {
	s := spec(t)
	if strings.Contains(s.Shape().Machine, MemoryBackendID) {
		t.Fatal("anonymous RAM should not name a memory backend")
	}
	s.Memory.File = "/tmp/pc.ram"
	if !strings.Contains(s.Shape().Machine, "memory-backend="+MemoryBackendID) {
		t.Fatalf("file-backed RAM is not in the machine string: %s", s.Shape().Machine)
	}
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "memory-backend-file,id="+MemoryBackendID) {
		t.Error("no memory-backend-file object")
	}
}

// The whole point of the fingerprint: two machines may exchange templates only
// if the binary, the kernel, the initrd and the shape all agree. Each of these
// changes is one a caller could make without noticing.
func TestFingerprintChangesWithTheMachine(t *testing.T) {
	base := spec(t)
	want, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	same, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if same != want {
		t.Fatal("the same machine fingerprinted differently twice")
	}

	// Whether RAM is file-backed must NOT change it: a template is always taken
	// from a machine with a memory file, so the lookup that decides whether a VM
	// may restore has to give the same answer for a VM that has not been given
	// one yet.
	withFile := base
	withFile.Memory.File = "/tmp/pc.ram"
	got, err := withFile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("a memory file changed the fingerprint; a VM could never find its own template")
	}

	for name, mutate := range map[string]func(*Spec){
		"more memory":  func(s *Spec) { s.Memory.SizeMB = 1024 },
		"more vCPUs":   func(s *Spec) { s.BootCPUs = 4 },
		"a max memory": func(s *Spec) { s.Memory.MaxMB = 4096 },
		"a new kernel": func(s *Spec) {
			if err := os.WriteFile(s.Kernel, []byte("a different kernel"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a new QEMU": func(s *Spec) {
			if err := os.WriteFile(s.QEMU, []byte("a different qemu"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		// Emulation is a different machine, and this is the assertion that keeps the two
		// apart. A guest's state under TCG is not a guest's state under KVM, so a restore
		// across them is undefined — and a shared fingerprint is exactly how one would
		// happen, silently, on a host that has both binaries.
		//
		// The binary's own hash would separate them in practice, since TCG needs the build
		// that has it compiled in. This holds the shape to it as well, so the separation
		// does not rest on a caller remembering to change two things at once.
		"emulation": func(s *Spec) { s.Accel = "tcg" },
	} {
		changed := base
		mutate(&changed)
		got, err := changed.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("%s did not change the fingerprint", name)
		}
		// Put the files back for the next case.
		base = spec(t)
		if want, err = base.Fingerprint(); err != nil {
			t.Fatal(err)
		}
	}
}

// The device list is part of the machine, because a restore loads device state
// into one — but only the part of it that exists when the state is loaded.
//
// A vsock, a memory ceiling and a serial port are there on both sides of a
// restore. Disks and NICs are not: a template is built from an empty VM and each
// restored VM is given its own afterwards, so counting them here would mean a VM
// never finding the template it should restore from.
func TestFingerprintCoversTheDevicesPresentAtRestore(t *testing.T) {
	base := spec(t)
	want, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Spec){
		"a vsock":          func(s *Spec) { s.VsockCID = 7 },
		"a memory ceiling": func(s *Spec) { s.Memory.MaxMB = s.Memory.SizeMB * 2 },
	} {
		changed := base
		mutate(&changed)
		got, err := changed.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}

	// A NIC is on the command line, so it is present when state is loaded and a machine
	// with one cannot restore from a template frozen without one.
	//
	// This case said the opposite until 2026-09-09 — that a NIC must not change the
	// fingerprint, because "it is cold-plugged after a restore, so a VM would never find
	// its template". That was true of disks and asserted of both.
	withNIC := base
	withNIC.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}
	got, err := withNIC.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Error("a NIC did not change the fingerprint; a machine with one would restore from a template without one, into a device whose state is not in the stream")
	}

	// A disk does not, and that is the difference: it is added after the restore, so a
	// machine that will be given one looks exactly like the template it came from.
	withDisk := base
	withDisk.Disks = []Disk{{Path: "/a", Format: "raw"}}
	got, err = withDisk.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("a disk changed the fingerprint; it is attached after a restore, so a VM would never find its template")
	}
}

// Two machines whose NICs differ only in the descriptor they read and the address they
// answer to are one machine, and share a template.
//
// It is what makes a network possible at all under one template per host: a descriptor
// number is which file the backend reads, the way a disk's path is, and a MAC is a property
// of the device and not of the bus. A caller that wants distinct MACs pays for it in
// templates; the answer is to give each machine a segment of its own instead, which is what
// a namespace per machine is.
func TestFingerprintIgnoresWhichTAPANICReads(t *testing.T) {
	a := spec(t)
	a.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}

	b := a
	b.NICs = []NIC{{TapFD: 9, MAC: "52:54:00:ff:ff:ff"}}

	fa, err := a.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Errorf("two machines differing only in a descriptor and a MAC have different fingerprints (%s, %s); a host would keep one template per VM", fa, fb)
	}
}

// The MTU is the exception to the rule above: it is not a setting on the backend
// but VIRTIO_NET_F_MTU, negotiated at probe and written into the migration stream.
// Two machines announcing different MTUs are two machines, and if they shared a
// fingerprint a host would restore one into the other and QEMU would refuse the
// feature set — an operator changing the uplink's MTU would break every workspace
// on the node instead of costing one template build.
func TestFingerprintSeparatesMachinesByTheMTUTheyAnnounce(t *testing.T) {
	a := spec(t)
	a.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01", MTU: 1500}}

	b := a
	b.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01", MTU: 1400}}

	// And announcing nothing is a third machine: without the feature bit the guest
	// picks its own default, which is a different negotiation from being told 1500.
	none := a
	none.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}

	seen := map[string]string{}
	for name, s := range map[string]Spec{"1500": a, "1400": b, "unannounced": none} {
		f, err := s.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[f]; dup {
			t.Errorf("a machine announcing %s and one announcing %s share fingerprint %s; "+
				"a host would load one's template into the other", name, other, f)
		}
		seen[f] = name
	}
}

// What is behind a device is not the machine. Two VMs with one disk each are the
// same machine whether that disk holds a database or a scratch overlay — which is
// the whole reason a template is worth having.
func TestFingerprintIgnoresWhatIsBehindTheDevices(t *testing.T) {
	a := spec(t)
	a.Disks = []Disk{{Path: "/one.qcow2", Format: "qcow2", Serial: "aaa"}}
	a.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}}
	a.VsockCID = 7

	b := a
	b.Disks = []Disk{{Path: "/another.qcow2", Format: "qcow2", Serial: "bbb"}}
	b.NICs = []NIC{{TapFD: 9, MAC: "52:54:00:00:00:02"}}
	b.VsockCID = 42

	fa, err := a.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Error("different paths, MACs and context ids made it a different machine")
	}
}

// Moving a VM between hosts, which is what model "host" quietly forbids.
//
// Under it the guest is told through CPUID exactly which instructions this
// silicon has and never asks again, so a template taken on one machine describes
// a CPU the next may not have. Nothing else in the fingerprint differs between
// two hosts — same binaries, same shape — so without the host CPU folded in, each
// machine accepts the other's templates and the guest resumes onto an instruction
// that is not there.
func TestFingerprintSeparatesHostsUnderCPUHost(t *testing.T) {
	base := spec(t)

	old := readHostCPU
	t.Cleanup(func() { readHostCPU = old })

	readHostCPU = func() (string, error) { return "13th Gen Intel(R) Core(TM) i9-13900HK", nil }
	intel, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	readHostCPU = func() (string, error) { return "AMD EPYC 9634 84-Core Processor", nil }
	amd, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if intel == amd {
		t.Fatal("two different host CPUs produced one fingerprint; each host would accept the other's templates")
	}
}

// And the other half: naming a model is how a fleet gets VMs that move. Every
// host then shows the guest the same CPU, so the host's own silicon must drop out
// of the fingerprint — otherwise templates are partitioned per machine for
// exactly the reason that no longer applies.
func TestFingerprintIgnoresTheHostUnderANamedCPU(t *testing.T) {
	named := spec(t)
	named.CPU = "Skylake-Server-v4"

	old := readHostCPU
	t.Cleanup(func() { readHostCPU = old })

	readHostCPU = func() (string, error) { return "13th Gen Intel(R) Core(TM) i9-13900HK", nil }
	a, err := named.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	readHostCPU = func() (string, error) { return "AMD EPYC 9634 84-Core Processor", nil }
	b, err := named.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("a named CPU model still partitioned templates by host; VMs could not move")
	}

	// And it is a different machine from the default, because the guest sees a
	// different CPU.
	def, err := spec(t).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if def == a {
		t.Error("a named CPU model fingerprinted the same as model host")
	}
}

// A path is not the machine: the same binary under two names is one machine, and
// two different binaries at one path are two.
func TestFingerprintIsContentNotPath(t *testing.T) {
	a := spec(t)
	b := spec(t)
	fa, err := a.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Error("identical contents at different paths fingerprinted differently")
	}
}

func TestCmdlineStopsThePCIScanAtBusZero(t *testing.T) {
	got := DefaultCmdline().String()
	if !strings.Contains(got, "pci=lastbus=0") {
		t.Errorf("pci=lastbus=0 is missing, and every boot pays 8192 config reads for it: %s", got)
	}
}

// Memory that arrives at runtime is no use to a guest that does not online it,
// and the failure is silent: growth stops at the boot size with no error.
func TestCmdlineOnlinesMemoryAsItArrives(t *testing.T) {
	if got := DefaultCmdline().String(); !strings.Contains(got, "memhp_default_state=online") {
		t.Errorf("virtio-mem growth would stall at the boot size: %s", got)
	}
}

// Two thirds of this machine's boot was systemd asking a serial port that nobody was
// listening to two questions and waiting 334ms for each answer. TERM=dumb is what makes it
// not ask: 687ms to 19ms between the kernel exec'ing init and systemd's first log line.
//
// Worth a test because it does not look like a boot flag. It looks like a terminal
// preference, and the one thing it must not be mistaken for is decoration somebody can drop
// while tidying — nothing fails if it goes, the machine just takes five times as long.
func TestCmdlineTellsSystemdTheConsoleWillNotAnswer(t *testing.T) {
	if got := DefaultCmdline().String(); !strings.Contains(got, "TERM=dumb") {
		t.Errorf("systemd would spend 668ms waiting out two terminal queries: %s", got)
	}
}

func TestCmdlineInitAndArgs(t *testing.T) {
	c := DefaultCmdline()
	c.Init = "/sbin/custom-init"
	c.InitArgs = []string{"-vsock-rpc-port=1025"}
	got := c.String()
	if !strings.HasSuffix(got, "init=/sbin/custom-init -- -vsock-rpc-port=1025") {
		t.Errorf("init must come last, with its args after --: %s", got)
	}
}

// A profiling boot goes silent, not verbose: registering a console replays the
// whole ring into it inside an initcall, and the profile then measures itself.
func TestProfilingSilencesTheConsole(t *testing.T) {
	got := DefaultCmdline().Profiling().String()
	if !strings.Contains(got, "loglevel=0") || strings.Contains(got, "quiet") {
		t.Errorf("a profiling boot should set loglevel=0 and not quiet: %s", got)
	}
	if !strings.Contains(got, "initcall_debug") || !strings.Contains(got, "log_buf_len=4M") {
		t.Errorf("missing the profiling flags: %s", got)
	}
}

// Validate is the boundary, and every case in it is something that otherwise
// fails as a guest that boots and finds the world subtly wrong. There was one
// test for one of them; a case added without a test is a check nobody notices
// stopped firing.
func TestValidateRefuses(t *testing.T) {
	for _, tc := range []struct {
		what   string
		breaks func(*Spec)
	}{
		{"no QEMU", func(s *Spec) { s.QEMU = "" }},
		{"no kernel", func(s *Spec) { s.Kernel = "" }},
		{"no firmware", func(s *Spec) { s.Firmware = "" }},
		{"no boot CPUs", func(s *Spec) { s.BootCPUs = 0 }},
		{"a vCPU ceiling below the boot count", func(s *Spec) { s.BootCPUs, s.MaxCPUs = 4, 2 }},
		{"no memory", func(s *Spec) { s.Memory.SizeMB = 0 }},
		// A ceiling below the boot size: memoryArg drops it, so without this the
		// caller gets a machine that cannot grow having asked for one that can.
		{"a memory ceiling below the boot size", func(s *Spec) { s.Memory.SizeMB, s.Memory.MaxMB = 2048, 512 }},
		{"shared memory with no file to share", func(s *Spec) { s.Memory.Shared = true }},
		{"more disks than the slot range holds", func(s *Spec) {
			s.Disks = make([]Disk, MaxDisks+1)
			for i := range s.Disks {
				s.Disks[i] = Disk{Path: "/a.qcow2", Format: "qcow2"}
			}
		}},
		{"more NICs than the slot range holds", func(s *Spec) { s.NICs = make([]NIC, MaxNICs+1) }},
		{"more root ports than the slot range holds", func(s *Spec) { s.HotplugPorts = MaxHotplugPorts + 1 }},
		{"a negative number of root ports", func(s *Spec) { s.HotplugPorts = -1 }},
		{"a disk with no path", func(s *Spec) { s.Disks = []Disk{{Format: "qcow2"}} }},
		{"a disk with no format", func(s *Spec) { s.Disks = []Disk{{Path: "/a.qcow2"}} }},
		// Both forms of -incoming: QEMU takes the flag once, and which source a VM
		// restores from is not something to guess at on the caller's behalf.
		{"both forms of -incoming", func(s *Spec) { s.IncomingDefer, s.Incoming = true, "file:/state" }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := spec(t)
			tc.breaks(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %s", tc.what)
			}
			if _, err := s.Args(); err == nil {
				t.Fatalf("Args built a command line for %s", tc.what)
			}
		})
	}

	// And the spec the cases above are made from has to pass, or every one of them
	// would pass for the wrong reason.
	if err := spec(t).Validate(); err != nil {
		t.Fatalf("the minimal spec does not validate: %v", err)
	}
}

// Resuming a VM rather than booting one. The URI form was declared in Spec and
// never emitted, so `boot -incoming file:/path/state` started a fresh guest and
// reported success — a restore that silently is not one.
func TestIncomingNamesTheSourceOnTheCommandLine(t *testing.T) {
	for _, tc := range []struct {
		what string
		set  func(*Spec)
		want string
	}{
		{"a URI at exec time", func(s *Spec) { s.Incoming = "file:/state" }, "file:/state"},
		{"deferred to QMP", func(s *Spec) { s.IncomingDefer = true }, "defer"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := spec(t)
			tc.set(&s)
			args, err := s.Args()
			if err != nil {
				t.Fatal(err)
			}
			if got := argValue(args, "-incoming"); got != tc.want {
				t.Errorf("-incoming = %q, want %q", got, tc.want)
			}
		})
	}

	// A machine that was asked for neither must not carry the flag at all: an
	// -incoming with an empty value is a QEMU that waits for a migration nobody
	// is going to send.
	args, err := spec(t).Args()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if a == "-incoming" {
			t.Fatal("a machine asked to boot carries -incoming")
		}
	}
}
