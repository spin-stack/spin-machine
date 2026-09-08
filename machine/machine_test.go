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
// into one. Each of these changes what a template may be restored into, and none
// of them touches a file the fingerprint hashes — so before the topology was
// included, every one of them was invisible.
func TestFingerprintCoversTheDeviceList(t *testing.T) {
	base := spec(t)
	want, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Spec){
		"a disk":  func(s *Spec) { s.Disks = []Disk{{Path: "/a", Format: "raw"}} },
		"a NIC":   func(s *Spec) { s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01"}} },
		"a vsock": func(s *Spec) { s.VsockCID = 7 },
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

	// A second disk is a different machine from one disk; a read-only disk is a
	// different machine from a writable one at the same slot.
	one := base
	one.Disks = []Disk{{Path: "/a", Format: "raw"}}
	two := base
	two.Disks = []Disk{{Path: "/a", Format: "raw"}, {Path: "/b", Format: "raw"}}
	ro := base
	ro.Disks = []Disk{{Path: "/a", Format: "raw", Readonly: true}}

	f1, _ := one.Fingerprint()
	f2, _ := two.Fingerprint()
	fro, _ := ro.Fingerprint()
	if f1 == f2 {
		t.Error("one disk and two disks fingerprinted the same")
	}
	if f1 == fro {
		t.Error("a writable disk and a read-only one fingerprinted the same")
	}
}

// The serial port has state, so a machine saved with one cannot be resumed
// without one — `Unknown section or instance 'serial'`. Where its bytes go is not
// part of the machine; whether it exists is.
func TestFingerprintCoversTheSerialPort(t *testing.T) {
	with := spec(t)
	with.Serial = "mon:stdio"
	without := spec(t)
	without.Serial = ""

	a, err := with.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := without.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("a machine with a serial port and one without fingerprinted the same")
	}

	elsewhere := with
	elsewhere.Serial = "file:/var/log/console.log"
	c, err := elsewhere.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Error("where the console's bytes go made it a different machine")
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

func TestCmdlineInitAndArgs(t *testing.T) {
	c := DefaultCmdline()
	c.Init = "/sbin/vminitd"
	c.InitArgs = []string{"-vsock-rpc-port=1025"}
	got := c.String()
	if !strings.HasSuffix(got, "init=/sbin/vminitd -- -vsock-rpc-port=1025") {
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
