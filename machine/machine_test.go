// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"encoding/json"
	"fmt"
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

// nic is a NIC validate accepts, for a test about something else.
func nic() NIC { return NIC{TapFD: 3, MAC: "52:54:00:00:00:01"} }

// What Args puts on the command line, one machine per row. Each row is the whole
// line joined with spaces and a trailing one, so a want ending in a space pins the
// end of an argument.
//
// Every string here is something whose absence is invisible in a guest until it is
// slow or wrong — none of them fails loudly when it goes.
func TestArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*Spec)
		want   []string
		absent []string
	}{{
		// A machine given no display, no default devices and no way to spawn a helper
		// is most of what makes this machine what it is.
		name: "machine-wide flags",
		want: []string{"-nodefaults", "-sandbox on,obsolete=deny", "vmgenid,guid=auto"},
		// Anonymous RAM names no backend; the virtio-mem device is only for a ceiling.
		absent: []string{"memory-backend", "virtio-mem", "-incoming"},
	}, {
		// The slot map is the reason the kernel can be told pci=lastbus=0. A device
		// that moved off its slot is a device the guest may not find, with no error.
		name: "fixed slots",
		set: func(s *Spec) {
			s.VsockCID = 7
			s.Disks = []Disk{{Path: "/a.qcow2", Format: "qcow2"}, {Path: "/b.raw", Format: "raw"}}
			s.NICs = []NIC{nic()}
		},
		want: []string{
			"vhost-vsock-pci,guest-cid=7,disable-legacy=on,addr=0x2 ",
			"virtio-rng-pci,disable-legacy=on,addr=0x3 ",
			// The only way a running VM gives memory back. Without it the answer to
			// "how is memory reclaimed?" is "the VM exits".
			"virtio-balloon-pci,free-page-reporting=on,deflate-on-oom=on,disable-legacy=on,addr=0x4 ",
			"virtio-blk-pci,drive=blk0,disable-legacy=on,addr=0x5 ",
			"virtio-blk-pci,drive=blk1,disable-legacy=on,addr=0x6 ",
			"virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01,romfile=,disable-legacy=on,addr=0x10 ",
			// Without these QEMU hands every block request to a worker pool and copies
			// every packet through userspace, and nothing reports either.
			"aio=io_uring",
			"tap,id=net0,fd=3,vhost=on ",
		},
	}, {
		// host_mtu is how the guest is told what it may send, and the only way of
		// telling it that it cannot then ignore: virtnet_probe assigns the announced
		// value to both dev->mtu and dev->max_mtu. Root inside the machine can discard
		// a DHCP option or a kernel parameter; this it cannot.
		name: "an announced MTU",
		set: func(s *Spec) {
			n := nic()
			n.MTU = 1400
			s.NICs = []NIC{n}
		},
		want: []string{"virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01,romfile=,disable-legacy=on,addr=0x10,host_mtu=1400 "},
	}, {
		// And nothing to announce says nothing, rather than announcing zero — which
		// QEMU takes as "no MTU" but only after the guest has read a config field the
		// feature bit says is valid.
		name:   "no MTU",
		set:    func(s *Spec) { s.NICs = []NIC{nic()} },
		absent: []string{"host_mtu"},
	}, {
		// Each NIC on a slot of its own, counting up from the first.
		name: "two NICs",
		set: func(s *Spec) {
			second := nic()
			second.TapFD, second.MAC = 4, "52:54:00:00:00:02"
			s.NICs = []NIC{nic(), second}
		},
		want: []string{
			"netdev=net0,mac=52:54:00:00:00:01,romfile=,disable-legacy=on,addr=0x10 ",
			"netdev=net1,mac=52:54:00:00:00:02,romfile=,disable-legacy=on,addr=0x11 ",
		},
	}, {
		// chassis is a root port's identity to the guest's ACPI, and two ports with one
		// chassis are a guest that hotplugs into the wrong one or neither.
		name: "root ports",
		set:  func(s *Spec) { s.HotplugPorts = 2 },
		want: []string{
			"pcie-root-port,id=rp0,chassis=1,addr=0x1a ",
			"pcie-root-port,id=rp1,chassis=2,addr=0x1b ",
		},
	}, {
		// Growing a VM is virtio-mem, not ACPI DIMM slots: -m carries the ceiling and
		// no slots=, and the device holds the region between boot memory and ceiling
		// with nothing plugged until somebody asks.
		name: "a memory ceiling",
		set:  func(s *Spec) { s.Memory.SizeMB, s.Memory.MaxMB = 2048, 8192 },
		want: []string{
			"-m 2048,maxmem=8192M ",
			"memory-backend-ram,id=mem.growth,size=6144M ",
			"virtio-mem-pci,id=vmem0,memdev=mem.growth,requested-size=0,disable-legacy=on,addr=0x1e ",
		},
		absent: []string{"slots="},
	}, {
		name:   "fixed memory",
		set:    func(s *Spec) { s.Memory.SizeMB = 2048 },
		want:   []string{"-m 2048 "},
		absent: []string{"maxmem", "virtio-mem"},
	}, {
		// A template's source maps its file shared and writes it. File-backed RAM is
		// in the machine string too, which is why it is in the shape and not only in
		// an -object.
		name: "the template's source",
		set:  func(s *Spec) { s.Memory.File, s.Memory.Shared = "/tmp/pc.ram", true },
		want: []string{
			"memory-backend=" + MemoryBackendID,
			"memory-backend-file,id=" + MemoryBackendID + ",size=512M,mem-path=/tmp/pc.ram,share=on ",
		},
		absent: []string{"readonly=on"},
	}, {
		// A restore maps the same file private and opens it read-only, so no VM
		// restored from a template can change it for the next, and a QEMU that does
		// not own the file can still restore from it.
		name:   "a restore",
		set:    func(s *Spec) { s.Memory.File = "/tmp/pc.ram" },
		want:   []string{"mem-path=/tmp/pc.ram,share=off,readonly=on,rom=off "},
		absent: []string{"rom=on"},
	}, {
		// A device handed over as a descriptor is named by it on the command line, and
		// one that is not is left for QEMU to open.
		name: "devices QEMU opens",
		set: func(s *Spec) {
			s.VsockCID = 7
			s.NICs = []NIC{nic()}
		},
		want:   []string{"-accel kvm ", "vhost-vsock-pci,guest-cid=7,disable-legacy=on,addr=0x2 ", "tap,id=net0,fd=3,vhost=on "},
		absent: []string{"device=/dev/fdset", "vhostfd"},
	}, {
		// The accelerator is always its own -accel and never accel= in -machine: QEMU
		// refuses the two together, and only -accel can carry /dev/kvm's descriptor.
		name: "devices handed over",
		set: func(s *Spec) {
			s.VsockCID = 7
			s.NICs = []NIC{nic()}
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 4}}}}
			s.KVMFDSet, s.VsockFD, s.NICs[0].VhostFD = 1, 5, 6
		},
		want: []string{
			"-machine q35,kernel-irqchip=on,", "-accel kvm,device=/dev/fdset/1 ",
			"addr=0x2,vhostfd=5 ", "tap,id=net0,fd=3,vhost=on,vhostfd=6 ",
		},
		absent: []string{"accel=kvm"},
	}, {
		// A disk given by path turns drop-cache off on its own node and not on a child
		// the image may not have: naming backing.file makes QEMU open a backing file
		// whether the header has one or not, and it refuses when it does not. The
		// chain form is in TestEveryNodeOfAChain.
		name:   "a disk by path",
		set:    func(s *Spec) { s.Disks = []Disk{{Path: "/images/one.qcow2", Format: "qcow2"}} },
		want:   []string{"file.drop-cache=off"},
		absent: []string{"backing.file.drop-cache"},
	}, {
		// A value a caller supplies is a value, not more options. QEMU splits an option
		// string on a single comma, so "a,readonly=off" as a path was a path "a" and a
		// second option. Doubled, it is one value; TestQEMUAcceptsEveryArgument asks
		// QEMU to open such paths.
		name: "commas in values",
		set: func(s *Spec) {
			s.Disks = []Disk{{Path: "/img/a,readonly=off", Format: "raw", Serial: "x,addr=0x2"}}
			s.Memory.File = "/mem/a,share=on"
			s.Monitors = []Monitor{{Socket: "/run/q,wait=on"}}
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3, Opaque: "/o,p"}}}}
		},
		want: []string{
			"file=/img/a,,readonly=off,if=none,",
			",serial=x,,addr=0x2 ",
			"mem-path=/mem/a,,share=on,share=off,",
			"unix:/run/q,,wait=on,server=on,wait=off ",
			"opaque=/o,,p ",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t)
			if tc.set != nil {
				tc.set(&s)
			}
			args, err := s.Args()
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ") + " "
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in %s", want, joined)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(joined, absent) {
					t.Errorf("%q in %s", absent, joined)
				}
			}
		})
	}
}

// An unnamed CPU is the silicon under KVM and everything QEMU can emulate under TCG,
// which refuses "host" outright.
func TestAnUnnamedCPUFollowsTheAccelerator(t *testing.T) {
	for accel, want := range map[string]string{"kvm": "host,", "tcg": "max,"} {
		s := spec(t)
		s.Accel = accel
		if got := s.shape().CPU; !strings.HasPrefix(got, want) {
			t.Errorf("-accel %s: -cpu %q, want %q first", accel, got, want)
		}
	}
}

// The command line and the fingerprint read one Shape. If they read two, a VM could
// restore from a template of a machine it is not.
func TestArgsCarriesTheShape(t *testing.T) {
	s := spec(t)
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	sh := s.shape()
	for _, c := range []struct{ flag, want string }{
		{"-machine", sh.Machine},
		{"-accel", sh.Accel},
		{"-cpu", sh.CPU},
		{"-smp", sh.SMP},
		{"-m", sh.Memory},
	} {
		if got := argValue(args, c.flag); got != c.want {
			t.Errorf("%s = %q, want %q", c.flag, got, c.want)
		}
	}
}

// Every node of a disk handed over as a chain, and the layers below the top are the
// ones that matter.
//
// discard=unmap on every node: the format node is where a guest's discard arrives,
// and one opened without unmap drops it and answers the guest that it worked.
//
// drop-cache off on every file node: QEMU's default drops the host's page cache for a
// node when the machine is activated after an incoming migration — see the note in
// chainArgs. A sealed read-only layer cannot have a stale cache, and it is one file
// that every VM on the host reads through, so dropping it is paid by all of them. It
// comes back when a node is rewritten and shows up as a slow restore on a busy host
// rather than as an error anywhere.
func TestEveryNodeOfAChain(t *testing.T) {
	s := spec(t)
	s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 10}}}, {ID: 2, FDs: []FD{{Num: 11}}}}
	s.Disks = []Disk{{Chain: []Image{{FDSet: 1, Format: "qcow2"}, {FDSet: 2, Format: "qcow2"}}, Serial: "root"}}
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for i, a := range args {
		if a == "-blockdev" && i+1 < len(args) {
			nodes = append(nodes, args[i+1])
		}
	}

	for _, tc := range []struct {
		which string
		// of picks the nodes the row is about; count is how many a chain of two has.
		of    func(node string) bool
		count int
		want  string
	}{
		{"every node", func(string) bool { return true }, 4, `"discard":"unmap"`},
		{"every file node", func(n string) bool { return strings.Contains(n, `"driver":"file"`) }, 2, `"drop-cache":false`},
	} {
		t.Run(tc.which, func(t *testing.T) {
			n := 0
			for _, node := range nodes {
				if !tc.of(node) {
					continue
				}
				n++
				if !strings.Contains(node, tc.want) {
					t.Errorf("missing %s: %s", tc.want, node)
				}
			}
			if n != tc.count {
				t.Errorf("a chain of two images has %d of these nodes, want %d", n, tc.count)
			}
		})
	}
}

// Where each node of a chain takes its backing from. Each image's backing is the node
// under it, and the last image's is null, so QEMU follows no name a header holds; a raw
// image has no backing option at all, and naming one is an error QEMU raises at open.
func TestAChainsBacking(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain []Image
		// want is each format node's "backing", top first; absent is the key missing.
		want []any
	}{
		{"qcow2 over qcow2", []Image{{FDSet: 1, Format: "qcow2"}, {FDSet: 2, Format: "qcow2"}}, []any{"blk0-1", nil}},
		{"qcow2 over raw", []Image{{FDSet: 1, Format: "qcow2"}, {FDSet: 2, Format: "raw"}}, []any{"blk0-1", "absent"}},
		{"raw alone", []Image{{FDSet: 1, Format: "raw"}}, []any{"absent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t)
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 10}}}, {ID: 2, FDs: []FD{{Num: 11}}}}
			s.Disks = []Disk{{Chain: tc.chain}}
			args, err := s.Args()
			if err != nil {
				t.Fatal(err)
			}
			backing := map[string]any{}
			for i, a := range args {
				if a != "-blockdev" {
					continue
				}
				var node map[string]any
				if err := json.Unmarshal([]byte(args[i+1]), &node); err != nil {
					t.Fatalf("a -blockdev that is not JSON: %s", args[i+1])
				}
				if node["driver"] == "file" {
					continue
				}
				b, ok := node["backing"]
				if !ok {
					b = "absent"
				}
				backing[node["node-name"].(string)] = b
			}
			for j, want := range tc.want {
				name := "blk0"
				if j > 0 {
					name = fmt.Sprintf("blk0-%d", j)
				}
				if got := backing[name]; got != want {
					t.Errorf("%s has backing %v, want %v", name, got, want)
				}
			}
		})
	}
}

// The fingerprint, one pair of machines per row: a is spec() changed by a, b is a
// changed by b, and the row says whether the two may exchange templates.
//
// Two machines may exchange templates only if the binary, the kernel, the initrd,
// the shape and the devices present when state is loaded all agree — and must be
// able to when all that differs is what is behind the devices, or a host keeps one
// template per VM.
func TestFingerprint(t *testing.T) {
	const intel, amd = "13th Gen Intel(R) Core(TM) i9-13900HK", "AMD EPYC 9634 84-Core Processor"
	rewrite := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withMTU := func(mtu int) func(*testing.T, *Spec) {
		return func(_ *testing.T, s *Spec) {
			n := nic()
			n.MTU = mtu
			s.NICs = []NIC{n}
		}
	}

	for _, tc := range []struct {
		name string
		a, b func(*testing.T, *Spec)
		// hostB is the host CPU b is fingerprinted on; a is always on intel.
		hostB string
		same  bool
	}{
		{name: "the same machine twice", same: true},

		// A path is not the machine: the same contents under another name is one.
		{name: "the same files at other paths", same: true, b: func(t *testing.T, s *Spec) {
			other := spec(t)
			s.QEMU, s.Kernel = other.QEMU, other.Kernel
		}},

		// Whether RAM is file-backed must not change it: a template is always taken
		// from a machine with a memory file, so the lookup that decides whether a VM
		// may restore has to give the same answer for a VM that has not been given
		// one yet.
		{name: "a memory file", same: true, b: func(_ *testing.T, s *Spec) { s.Memory.File = "/tmp/pc.ram" }},

		// A disk is added after the restore, so a machine that will be given one
		// looks exactly like the template it came from.
		{name: "a disk", same: true, b: func(_ *testing.T, s *Spec) { s.Disks = []Disk{{Path: "/a", Format: "raw"}} }},

		// A descriptor number is which file the backend reads, the way a disk's path
		// is, and a MAC is a property of the device and not of the bus. A caller that
		// wants distinct MACs should give each machine a segment of its own, not pay
		// for it in templates.
		{name: "another TAP and MAC", same: true,
			a: func(_ *testing.T, s *Spec) { s.NICs = []NIC{nic()} },
			b: func(_ *testing.T, s *Spec) { s.NICs = []NIC{{TapFD: 9, MAC: "52:54:00:ff:ff:ff"}} }},

		// What is behind a device is not the machine: two VMs with one disk each are
		// the same machine whether that disk holds a database or a scratch overlay,
		// and whether it arrives by path or as a descriptor.
		{name: "everything handed over as descriptors", same: true,
			a: func(_ *testing.T, s *Spec) {
				s.Disks = []Disk{{Path: "/one.qcow2", Format: "qcow2", Serial: "aaa"}}
				s.NICs = []NIC{nic()}
				s.VsockCID = 7
				s.Serial = "file:/var/log/console"
			},
			b: func(_ *testing.T, s *Spec) {
				s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 10}}}, {ID: 2, FDs: []FD{{Num: 11}}}, {ID: 3, FDs: []FD{{Num: 13}}}}
				s.Disks = []Disk{{Chain: []Image{{FDSet: 1, Format: "qcow2"}}, Serial: "bbb"}}
				s.NICs = []NIC{{TapFD: 9, MAC: "52:54:00:00:00:02", VhostFD: 15}}
				s.VsockCID = 42
				s.Serial, s.SerialFDSet = "", 2
				s.Monitors = []Monitor{{FD: 12}}
				s.KVMFDSet, s.VsockFD = 3, 14
			}},

		// Naming a model is how a fleet gets VMs that move: every host shows the guest
		// the same CPU, so the host's own silicon drops out.
		{name: "a named CPU on another host", same: true, hostB: amd,
			a: func(_ *testing.T, s *Spec) { s.CPU = "Skylake-Server-v4" }},

		{name: "more memory", b: func(_ *testing.T, s *Spec) { s.Memory.SizeMB = 1024 }},
		{name: "more vCPUs", b: func(_ *testing.T, s *Spec) { s.BootCPUs = 4 }},
		{name: "a new kernel", b: func(t *testing.T, s *Spec) { rewrite(t, s.Kernel, "a different kernel") }},
		{name: "a new QEMU", b: func(t *testing.T, s *Spec) { rewrite(t, s.QEMU, "a different qemu") }},
		// A guest's state under TCG is not a guest's state under KVM. The binary's
		// own hash separates them in practice; this holds the shape to it as well, so
		// the separation does not rest on a caller changing two things at once.
		{name: "emulation", b: func(_ *testing.T, s *Spec) { s.Accel = "tcg" }},
		// A vsock and a memory ceiling are there on both sides of a restore.
		{name: "a vsock", b: func(_ *testing.T, s *Spec) { s.VsockCID = 7 }},
		{name: "a memory ceiling", b: func(_ *testing.T, s *Spec) { s.Memory.MaxMB = s.Memory.SizeMB * 2 }},
		// A NIC is on the command line, so it is present when state is loaded. "It is
		// cold-plugged after a restore" is true of disks and does not carry across.
		{name: "a NIC", b: func(_ *testing.T, s *Spec) { s.NICs = []NIC{nic()} }},
		// The MTU is VIRTIO_NET_F_MTU, negotiated at probe and written into the
		// migration stream: an operator changing it must cost a template build, not
		// every workspace on the node. Announcing nothing is a third machine.
		{name: "an MTU announced", a: withMTU(0), b: withMTU(1500)},
		{name: "another MTU", a: withMTU(1500), b: withMTU(1400)},
		// Under model host the guest is told through CPUID exactly which instructions
		// this silicon has, and a template taken here describes a CPU the next host
		// may not have.
		{name: "another host under CPU host", hostB: amd},
		{name: "a named CPU instead of host", b: func(_ *testing.T, s *Spec) { s.CPU = "Skylake-Server-v4" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := func(model string) func() (string, error) {
				return func() (string, error) { return model, nil }
			}
			a := spec(t)
			if tc.a != nil {
				tc.a(t, &a)
			}
			fa, err := a.fingerprint(host(intel))
			if err != nil {
				t.Fatal(err)
			}
			b := a
			if tc.b != nil {
				tc.b(t, &b)
			}
			hostB := intel
			if tc.hostB != "" {
				hostB = tc.hostB
			}
			fb, err := b.fingerprint(host(hostB))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.same && fa != fb:
				t.Error("two machines that may exchange templates have different fingerprints")
			case !tc.same && fa == fb:
				t.Error("two machines that may not exchange templates share a fingerprint")
			}
		})
	}
}

// The kernel command line, one Cmdline per row.
func TestCmdline(t *testing.T) {
	withInit := DefaultCmdline()
	withInit.Init = "/sbin/custom-init"

	for _, tc := range []struct {
		name   string
		c      Cmdline
		want   []string
		absent []string
		suffix string
	}{{
		name: "default",
		c:    DefaultCmdline(),
		want: []string{
			// Without it every boot pays 8192 config reads looking for peer host bridges.
			"pci=lastbus=0",
			// Memory that arrives at runtime is no use to a guest that does not online
			// it, and the failure is silent: growth stops at the boot size.
			"memhp_default_state=online",
			// Two thirds of this machine's boot was systemd asking a serial port nobody
			// listens to two questions and waiting 334 ms for each answer. It looks like
			// a terminal preference, so it is what gets dropped while tidying; nothing
			// fails if it goes, the machine just takes five times as long.
			"TERM=dumb",
		},
	}, {
		name:   "init",
		c:      withInit,
		suffix: "init=/sbin/custom-init",
	}, {
		// A profiling boot goes silent, not verbose: registering a console replays the
		// whole ring into it inside an initcall, and the profile then measures itself.
		name:   "profiling",
		c:      DefaultCmdline().Profiling(),
		want:   []string{"loglevel=0", "initcall_debug", "log_buf_len=4M"},
		absent: []string{"quiet"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q: %s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("%q in %s", absent, got)
				}
			}
			if !strings.HasSuffix(got, tc.suffix) {
				t.Errorf("init must come last, with its args after --: %s", got)
			}
		})
	}
}

// Profiling returns a new Cmdline, and the one it was made from stays the caller's.
// Extra is a slice: appending the profiling flags into spare capacity the caller's
// Extra had wrote them into the caller's array, so the caller's next append
// overwrote the profile's first flag.
func TestProfilingLeavesTheCallersCmdlineAlone(t *testing.T) {
	base := DefaultCmdline()
	base.Extra = make([]string, 0, 8)
	prof := base.Profiling()
	base.Extra = append(base.Extra, "caller's own")

	if got := prof.Extra[0]; got != "initcall_debug" {
		t.Errorf("the caller's append reached the profiling command line: Extra[0] = %q", got)
	}
}

// validate is the boundary, and every case in it is something that otherwise
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
			s.Disks = make([]Disk, maxDisks+1)
			for i := range s.Disks {
				s.Disks[i] = Disk{Path: "/a.qcow2", Format: "qcow2"}
			}
		}},
		{"more NICs than the slot range holds", func(s *Spec) {
			s.NICs = make([]NIC, maxNICs+1)
			for i := range s.NICs {
				s.NICs[i] = nic()
			}
		}},
		{"more root ports than the slot range holds", func(s *Spec) { s.HotplugPorts = MaxHotplugPorts + 1 }},
		{"a negative number of root ports", func(s *Spec) { s.HotplugPorts = -1 }},
		{"a disk with no path", func(s *Spec) { s.Disks = []Disk{{Format: "qcow2"}} }},
		// A wrong guess is a guest that boots and finds a disk full of nothing.
		{"a disk with no format", func(s *Spec) { s.Disks = []Disk{{Path: "/a.qcow2"}} }},
		{"a disk with a path and a chain", func(s *Spec) {
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}}
			s.Disks = []Disk{{Path: "/a.qcow2", Format: "qcow2", Chain: []Image{{FDSet: 1, Format: "qcow2"}}}}
		}},
		{"a chain image in a set nobody gave", func(s *Spec) {
			s.Disks = []Disk{{Chain: []Image{{FDSet: 9, Format: "qcow2"}}}}
		}},
		{"a chain image with no format", func(s *Spec) {
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}}
			s.Disks = []Disk{{Chain: []Image{{FDSet: 1}}}}
		}},
		{"a raw image with images under it", func(s *Spec) {
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}, {ID: 2, FDs: []FD{{Num: 4}}}}
			s.Disks = []Disk{{Chain: []Image{{FDSet: 1, Format: "raw"}, {FDSet: 2, Format: "qcow2"}}}}
		}},
		{"a descriptor set given twice", func(s *Spec) {
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}, {ID: 1, FDs: []FD{{Num: 4}}}}
		}},
		{"a descriptor set holding stdin", func(s *Spec) { s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 0}}}} }},
		{"a monitor given a path and a descriptor", func(s *Spec) { s.Monitors = []Monitor{{Socket: "/qmp", FD: 3}} }},
		{"a monitor given neither", func(s *Spec) { s.Monitors = []Monitor{{Socket: "/qmp"}, {}} }},
		{"a monitor on stderr", func(s *Spec) { s.Monitors = []Monitor{{FD: 2}} }},
		// A value from a fixed set is refused, not escaped: an escaped "qcow2,,x" is
		// still not a format, and an unescaped one is a second option.
		{"a disk format that is not one", func(s *Spec) { s.Disks = []Disk{{Path: "/a", Format: "qcow2,readonly=off"}} }},
		{"a chain image format that is not one", func(s *Spec) {
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}}
			s.Disks = []Disk{{Chain: []Image{{FDSet: 1, Format: "vmdk"}}}}
		}},
		{"a cache mode that is not one", func(s *Spec) { s.Disks = []Disk{{Path: "/a", Format: "raw", Cache: "none,aio=threads"}} }},
		{"a cache mode and O_DIRECT over backing", func(s *Spec) {
			s.Disks = []Disk{{Path: "/a", Format: "qcow2", Cache: "none", DirectOverBacking: true}}
		}},
		{"a CPU model with options", func(s *Spec) { s.CPU = "Skylake-Server-v4,enforce=off" }},
		{"an accelerator this machine does not run under", func(s *Spec) { s.Accel = "xen" }},
		// The kernel command line: each of these boots a guest that is told something
		// other than what the caller wrote.
		{"a root device with a space", func(s *Spec) { s.Cmdline.Root = "/dev/vda quiet" }},
		{"an extra parameter that ends the kernel's part", func(s *Spec) { s.Cmdline.Extra = []string{"quiet -- x"} }},
		{"a command line the kernel would truncate", func(s *Spec) {
			s.Cmdline.Extra = []string{strings.Repeat("x", maxCmdline)}
		}},
		{"/dev/kvm in a set nobody gave", func(s *Spec) { s.KVMFDSet = 1 }},
		{"/dev/kvm for an emulator", func(s *Spec) {
			s.Accel, s.KVMFDSet = "tcg", 1
			s.FDSets = []FDSet{{ID: 1, FDs: []FD{{Num: 3}}}}
		}},
		{"/dev/vhost-vsock with no vsock", func(s *Spec) { s.VsockCID, s.VsockFD = 0, 3 }},
		{"/dev/vhost-vsock on stdout", func(s *Spec) { s.VsockCID, s.VsockFD = 3, 1 }},
		{"/dev/vhost-net on stderr", func(s *Spec) {
			n := nic()
			n.VhostFD = 2
			s.NICs = []NIC{n}
		}},
		// A NIC is its TAP, and the zero value of a descriptor is stdin: NIC{} was a
		// machine whose network backend read the launcher's terminal.
		{"a NIC with no TAP", func(s *Spec) { s.NICs = []NIC{{MAC: "52:54:00:00:00:01"}} }},
		// The MAC goes into an option string: empty is "mac=,", and a comma in it
		// would be another option.
		{"a NIC with no MAC", func(s *Spec) { s.NICs = []NIC{{TapFD: 3}} }},
		{"a NIC with a MAC that is not one", func(s *Spec) { s.NICs = []NIC{{TapFD: 3, MAC: "52:54:00:00:00:01,romfile=x"}} }},
		{"a NIC with a 64-bit MAC", func(s *Spec) { s.NICs = []NIC{{TapFD: 3, MAC: "02:00:5e:10:00:00:00:01"}} }},
		// Both forms of -incoming: QEMU takes the flag once, and which source a VM
		// restores from is not something to guess at on the caller's behalf.
		{"both forms of -incoming", func(s *Spec) { s.IncomingDefer, s.Incoming = true, "file:/state" }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := spec(t)
			tc.breaks(&s)
			if err := s.validate(); err == nil {
				t.Fatalf("validate accepted %s", tc.what)
			}
			if _, err := s.Args(); err == nil {
				t.Fatalf("Args built a command line for %s", tc.what)
			}
		})
	}

	// And the specs the cases above are made from have to pass, or every one of them
	// would pass for the wrong reason.
	s := spec(t)
	if err := s.validate(); err != nil {
		t.Fatalf("the minimal spec does not validate: %v", err)
	}
	s.NICs = []NIC{nic()}
	if err := s.validate(); err != nil {
		t.Fatalf("the minimal spec with a NIC does not validate: %v", err)
	}

	// The least each bound allows: one megabyte, and the first descriptor that is not
	// stdin, stdout or stderr.
	s = spec(t)
	s.Memory.SizeMB = 1
	s.VsockCID, s.VsockFD = 3, 3
	if err := s.validate(); err != nil {
		t.Fatalf("the least each bound allows does not validate: %v", err)
	}
}

// What the fingerprint says is present when a template's state is loaded. Two machines
// that differ in one of these differ on the bus, so each has to be in the string when the
// device is there and out of it when it is not; a check that is only "the two
// fingerprints differ" holds none of them, because an inverted test still differs.
func TestTopologyNamesTheDevicesPresentAtRestore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*Spec)
		want   string
		absent string
	}{
		{name: "no vsock", set: func(*Spec) {}, absent: "vhost-vsock-pci"},
		{name: "a vsock", set: func(s *Spec) { s.VsockCID = 7 }, want: ";vhost-vsock-pci@0x2"},
		{name: "no memory ceiling", set: func(s *Spec) { s.Memory.MaxMB = s.Memory.SizeMB }, absent: "virtio-mem-pci"},
		{name: "a memory ceiling", set: func(s *Spec) { s.Memory.MaxMB = s.Memory.SizeMB + 1 }, want: ";virtio-mem-pci@0x1e"},
		{name: "no console", set: func(*Spec) {}, absent: "isa-serial"},
		{name: "a console by path", set: func(s *Spec) { s.Serial = "file:/log" }, want: ";isa-serial"},
		{name: "a console by descriptor", set: func(s *Spec) { s.SerialFDSet = 1 }, want: ";isa-serial"},
		{name: "no root ports", set: func(*Spec) {}, absent: "pcie-root-port"},
		{name: "one root port", set: func(s *Spec) { s.HotplugPorts = 1 }, want: ";pcie-root-port@0x1a*1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t)
			tc.set(&s)
			got := s.topology()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("topology %q, want it to hold %q", got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("topology %q, want no %q", got, tc.absent)
			}
		})
	}
}

// Under model host the fingerprint carries this host's CPU. Every host this runs on has a
// model name in /proc/cpuinfo.
func TestHostCPUModelReadsThisHost(t *testing.T) {
	model, err := hostCPUModel()
	if err != nil {
		t.Fatal(err)
	}
	if model == "" {
		t.Fatal("an empty CPU model name")
	}
}

// Resuming a VM rather than booting one. The URI form was declared in Spec and
// never emitted, so `boot -incoming file:/path/state` started a fresh guest and
// reported success — a restore that silently is not one.
func TestIncomingNamesTheSourceOnTheCommandLine(t *testing.T) {
	for _, tc := range []struct {
		what string
		set  func(*Spec)
		// want is -incoming's value, and empty for no -incoming at all: an -incoming
		// with an empty value is a QEMU that waits for a migration nobody will send.
		want string
	}{
		{"a URI at exec time", func(s *Spec) { s.Incoming = "file:/state" }, "file:/state"},
		{"deferred to QMP", func(s *Spec) { s.IncomingDefer = true }, "defer"},
		{"a boot", func(*Spec) {}, ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := spec(t)
			tc.set(&s)
			args, err := s.Args()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, a := range args {
				if a == "-incoming" {
					n++
				}
			}
			switch {
			case tc.want == "" && n != 0:
				t.Errorf("a machine asked to boot carries -incoming %q", argValue(args, "-incoming"))
			case tc.want != "" && (n != 1 || argValue(args, "-incoming") != tc.want):
				t.Errorf("-incoming %d times, value %q, want once with %q", n, argValue(args, "-incoming"), tc.want)
			}
		})
	}
}
