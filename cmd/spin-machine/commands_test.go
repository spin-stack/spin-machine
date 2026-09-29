// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// monitor is a QMP socket that answers from a script and keeps what it was asked, so a test can
// hold a command to the conversation QEMU needs - which commands, in which order, with what -
// and not only to the replies it got back.
type monitor struct {
	socket string
	done   chan served
}

type served struct {
	asked []qmpRequest
	err   error
}

func newMonitor(t *testing.T, replies ...string) *monitor {
	t.Helper()
	m := &monitor{socket: filepath.Join(t.TempDir(), "qmp.sock"), done: make(chan served, 1)}
	l, err := net.Listen("unix", m.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// The goroutine sends what serve said instead of reporting it: a t.Error after the
	// test has already given up on it panics.
	go func() {
		c, err := l.Accept()
		if err != nil {
			m.done <- served{err: err}
			return
		}
		asked, err := serve(c, replies)
		m.done <- served{asked, err}
	}()
	return m
}

// commands is what the command under test asked, once it has let the monitor go.
func (m *monitor) commands(t *testing.T) []qmpRequest {
	t.Helper()
	select {
	case s := <-m.done:
		if s.err != nil {
			t.Error(s.err)
		}
		return s.asked
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor was never let go")
		return nil
	}
}

const ok = `{"return": {}}`

// save and restore, against a monitor that answers as QEMU does: what each sends, and what it
// makes of the answers. A real QEMU does the same in boot/restore_bench_test.go, under KVM.
func TestSaveAndRestoreSayWhatQEMUNeeds(t *testing.T) {
	state := filepath.Join(t.TempDir(), "vm.state")
	shared := `{"return": [{"id": "pc.ram", "share": true, "size": 536870912}]}`
	completed := `{"return": {"status": "completed"}}`
	active := `{"return": {"status": "active"}}`

	for _, tc := range []struct {
		name    string
		replies []string
		do      func(socket string) error
		// want is the commands, in order, after qmp_capabilities.
		want    []string
		wantErr string
	}{{
		name:    "a save migrates to the file and quits once it is complete",
		replies: []string{ok, ok, active, completed, ok},
		do:      func(s string) error { return save(saveFlags{qmp: s, to: state, timeout: time.Minute}) },
		want:    []string{"migrate", "query-migrate", "query-migrate", "quit"},
	}, {
		name:    "a template stops the VM with the capability set, before it migrates",
		replies: []string{ok, shared, ok, ok, ok, completed, ok},
		do: func(s string) error {
			return save(saveFlags{qmp: s, to: state, template: true, timeout: time.Minute})
		},
		want: []string{"query-memdev", "migrate-set-capabilities", "stop", "migrate", "query-migrate", "quit"},
	}, {
		name:    "a template of RAM that is not shared is refused before anything else",
		replies: []string{ok, `{"return": [{"id": "pc.ram", "share": false}]}`},
		do: func(s string) error {
			return save(saveFlags{qmp: s, to: state, template: true, timeout: time.Minute})
		},
		want:    []string{"query-memdev"},
		wantErr: "not a shared pc.ram",
	}, {
		name:    "a save QEMU could not finish says why",
		replies: []string{ok, ok, `{"return": {"status": "failed", "error-desc": "No space left on device"}}`},
		do:      func(s string) error { return save(saveFlags{qmp: s, to: state, timeout: time.Minute}) },
		want:    []string{"migrate", "query-migrate"},
		wantErr: "failed: No space left on device",
	}, {
		name:    "a save still running when its time is up is an error",
		replies: []string{ok, ok, active},
		do:      func(s string) error { return save(saveFlags{qmp: s, to: state}) },
		want:    []string{"migrate", "query-migrate"},
		wantErr: "not finished within",
	}, {
		name:    "a refused migrate is the save's error",
		replies: []string{ok, `{"error": {"class": "GenericError", "desc": "no"}}`},
		do:      func(s string) error { return save(saveFlags{qmp: s, to: state, timeout: time.Minute}) },
		want:    []string{"migrate"},
		wantErr: "starting the save",
	}, {
		name:    "a template restore sets the capability, loads, and continues a VM that arrives paused",
		replies: []string{ok, ok, ok, completed, `{"return": {"status": "paused"}}`, ok},
		do: func(s string) error {
			return restore(restoreFlags{qmp: s, from: state, template: true, timeout: time.Minute})
		},
		want: []string{"migrate-set-capabilities", "migrate-incoming", "query-migrate", "query-status", "cont"},
	}, {
		name:    "a VM that arrives running is not continued",
		replies: []string{ok, ok, completed, `{"return": {"status": "running"}}`},
		do:      func(s string) error { return restore(restoreFlags{qmp: s, from: state, timeout: time.Minute}) },
		want:    []string{"migrate-incoming", "query-migrate", "query-status"},
	}, {
		name:    "a load that fails is the restore's error",
		replies: []string{ok, ok, `{"return": {"status": "failed", "error-desc": "bad section"}}`},
		do:      func(s string) error { return restore(restoreFlags{qmp: s, from: state, timeout: time.Minute}) },
		want:    []string{"migrate-incoming", "query-migrate"},
		wantErr: "failed: bad section",
	}, {
		name: "a VM that will not continue is the restore's error",
		replies: []string{ok, ok, completed, `{"return": {"status": "paused"}}`,
			`{"error": {"class": "GenericError", "desc": "no"}}`},
		do:      func(s string) error { return restore(restoreFlags{qmp: s, from: state, timeout: time.Minute}) },
		want:    []string{"migrate-incoming", "query-migrate", "query-status", "cont"},
		wantErr: "starting the restored VM",
	}, {
		name:    "a refused migrate-incoming is the restore's error",
		replies: []string{ok, `{"error": {"class": "GenericError", "desc": "no"}}`},
		do:      func(s string) error { return restore(restoreFlags{qmp: s, from: state, timeout: time.Minute}) },
		want:    []string{"migrate-incoming"},
		wantErr: "loading",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor(t, tc.replies...)
			err := tc.do(m.socket)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
			asked := m.commands(t)
			var got []string
			for _, r := range asked[1:] {
				got = append(got, r.Execute)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("asked %v, want %v", got, tc.want)
			}
			// The file named is the one given, absolute, in QEMU's file: form; and the
			// capability is the one both sides of a template must agree on.
			for _, r := range asked {
				switch r.Execute {
				case "migrate", "migrate-incoming":
					if uri := r.Arguments["uri"]; uri != "file:"+state {
						t.Errorf("%s to %v, want file:%s", r.Execute, uri, state)
					}
				case "migrate-set-capabilities":
					if b, _ := json.Marshal(r.Arguments); !strings.Contains(string(b), `"capability":"x-ignore-shared","state":true`) {
						t.Errorf("capabilities %s, want x-ignore-shared on", b)
					}
				}
			}
		})
	}

	for _, err := range []error{save(saveFlags{to: state}), save(saveFlags{qmp: "q"}),
		restore(restoreFlags{from: state}), restore(restoreFlags{qmp: "q"})} {
		if err == nil || !strings.Contains(err.Error(), "are required") {
			t.Errorf("a command without --qmp or its file returned %v", err)
		}
	}
}

// release is a tree OpenRelease accepts, of empty files: spec never runs what it names.
func release(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"bin/qemu-system-x86_64", "bin/qemu-system-x86_64-tcg", "bin/qemu-img", "kernel/vmlinux", "qemu/pvh.bin", "image/rootfs.qcow2"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The flags are the machine: each one lands in the field of the Spec it names, and a flag not
// given is the default a person running a machine by hand expects.
func TestFlagsAreTheSpec(t *testing.T) {
	dir := release(t)
	rel, err := machine.OpenRelease(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := rel.Spec()
	rootfs := filepath.Join(dir, "image/rootfs.qcow2")

	defaults := base
	defaults.BootCPUs = 2
	defaults.Memory = machine.Memory{SizeMB: 2048}
	defaults.Serial = "mon:stdio"
	defaults.Disks = []machine.Disk{{Path: rootfs, Format: "qcow2"}}
	defaults.Cmdline = machine.DefaultCmdline()
	defaults.Cmdline.Root = "/dev/vda"

	every := base
	every.QEMU, every.Kernel, every.Initrd, every.Firmware = "/q", "/k", "/i", "/f"
	every.CPU, every.BootCPUs, every.MaxCPUs = "Skylake-Server-v4", 3, 8
	every.Accel = "tcg"
	every.Memory = machine.Memory{SizeMB: 1024, MaxMB: 4096, File: "/m", Shared: true}
	every.HotplugPorts, every.VsockCID = 2, 7
	every.Monitors = []machine.Monitor{{Socket: "/qmp"}}
	every.Incoming = "file:/s"
	every.Serial = ""
	every.Disks = []machine.Disk{{Path: "/d", Format: "raw", Readonly: true, Serial: "ser",
		Cache: "none", DirectOverBacking: true}}
	every.Cmdline = machine.DefaultCmdline()
	every.Cmdline.Init, every.Cmdline.Root, every.Cmdline.RootReadonly = "/bin/sh", "/dev/vdb", true
	every.Cmdline = every.Cmdline.Profiling()
	every.Cmdline.Extra = append(every.Cmdline.Extra, "a=1", "b")
	every.Cmdline.Console = ""

	deferred := defaults
	deferred.IncomingDefer = true

	emulated := defaults
	emulated.Accel, emulated.QEMU = "tcg", filepath.Join(dir, "bin/qemu-system-x86_64-tcg")

	noDisk := defaults
	noDisk.Disks = nil
	noDisk.Cmdline = machine.DefaultCmdline()

	for _, tc := range []struct {
		name    string
		flags   []string
		want    machine.Spec
		scratch bool
	}{
		{"nothing given", nil, defaults, true},
		{"everything given", []string{"--qemu", "/q", "--kernel", "/k", "--initrd", "/i", "--firmware", "/f",
			"--disk", "/d", "--disk-format", "raw", "--disk-readonly", "--disk-serial", "ser", "--disk-cache", "none",
			"--disk-direct-over-backing", "--accel", "tcg", "--memory", "1024", "--max-memory", "4096", "--cpu", "Skylake-Server-v4",
			"--cpus", "3", "--max-cpus", "8", "--memory-file", "/m", "--memory-share", "--hotplug-ports", "2",
			"--vsock-cid", "7", "--qmp", "/qmp", "--incoming", "file:/s", "--console", "", "--init", "/bin/sh",
			"--root", "/dev/vdb", "--profile", "--append", "a=1 b"}, every, false},
		{"incoming defer is a restore over QMP", []string{"--incoming", "defer"}, deferred, true},
		{"tcg runs the release's TCG build", []string{"--accel", "tcg"}, emulated, true},
		{"no disk mounts no root", []string{"--disk", "-"}, noDisk, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o machineFlags
			if err := parse("boot", append([]string{"--release", dir}, tc.flags...), o.register); err != nil {
				t.Fatal(err)
			}
			got, err := o.spec()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("spec\n got %+v\nwant %+v", got, tc.want)
			}
			if o.scratch() != tc.scratch {
				t.Errorf("scratch %v, want %v", o.scratch(), tc.scratch)
			}
		})
	}
}

// What run does with the commands that build a machine: args prints the line boot would exec,
// and a release that is not whole, or a machine that cannot be, is an error before anything
// runs.
func TestRunBuildsTheMachineOrSaysWhyNot(t *testing.T) {
	dir := release(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = run([]string{"args", "--release", dir})
	os.Stdout = stdout
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(printed), filepath.Join(dir, "bin/qemu-system-x86_64")) ||
		!strings.Contains(string(printed), "-snapshot") {
		t.Errorf("args printed %q, want the release's QEMU and -snapshot over its base image", printed)
	}

	// fingerprint prints the number together with the shape that went into it.
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = run([]string{"fingerprint", "--release", dir})
	os.Stdout = stdout
	_ = w.Close()
	printed, _ = io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var fp struct {
		Fingerprint string
		Shape       struct{ Machine string }
	}
	if err := json.Unmarshal(printed, &fp); err != nil || len(fp.Fingerprint) != 64 || fp.Shape.Machine == "" {
		t.Errorf("fingerprint printed %q (%v), want a sha256 and the shape it hashed", printed, err)
	}

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no command", nil, "no command given"},
		{"a release that is not whole", []string{"args", "--release", t.TempDir()}, "is not a whole machine"},
		{"a machine that cannot be", []string{"args", "--release", dir, "--memory", "0"}, "memory is 0"},
		{"a flag boot does not take", []string{"fingerprint", "--nope"}, "flag provided but not defined"},
		{"restore without its socket", []string{"restore", "--from", "x"}, "are required"},
		{"detach without its socket", []string{"detach"}, "--qmp is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.argv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run(%q) returned %v, want an error containing %q", tc.argv, err, tc.want)
			}
		})
	}
}

// attach, against a monitor that answers as QEMU does: the node first and then the device on
// it, and a device QEMU refuses takes its node away again, because the node holds the image
// and its lock.
func TestAttachOpensTheDiskThenPlugsIt(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "d.raw")
	refused := `{"error": {"class": "GenericError", "desc": "no"}}`
	for _, tc := range []struct {
		name    string
		replies []string
		want    []string
		wantErr string
	}{
		{name: "attached", replies: []string{ok, ok, ok}, want: []string{"blockdev-add", "device_add"}},
		{name: "a node QEMU refuses", replies: []string{ok, refused},
			want: []string{"blockdev-add"}, wantErr: "opening"},
		{name: "a device QEMU refuses", replies: []string{ok, ok, refused, ok},
			want: []string{"blockdev-add", "device_add", "blockdev-del"}, wantErr: "adding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor(t, tc.replies...)
			err := attach(attachFlags{qmp: m.socket, port: 1, disk: disk, format: "raw", serial: "s"})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
			asked := m.commands(t)
			var got []string
			for _, r := range asked[1:] {
				got = append(got, r.Execute)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("asked %v, want %v", got, tc.want)
			}
			if len(asked) > 2 {
				dev := asked[2].Arguments
				if dev["bus"] != "rp1" || dev["drive"] != "rp1-drive" || dev["serial"] != "s" {
					t.Errorf("device_add %v, want the disk on rp1 over rp1-drive with its serial", dev)
				}
			}
		})
	}

	if err := attach(attachFlags{qmp: "q.sock"}); err == nil || !strings.Contains(err.Error(), "--disk") {
		t.Errorf("attach with no disk returned %v, want the flags it needs", err)
	}
}

// compare reads two reports and says what moved; a report it cannot read is an error that
// names the file, and not an empty comparison.
func TestCompareSaysWhatMoved(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("old.json", `{"release": "v1", "specs": [{"id": "a", "fingerprint": "x"}]}`)
	nw := write("new.json", `{"release": "v2", "specs": [{"id": "a", "fingerprint": "y"}]}`)
	var b strings.Builder
	if err := compare(compareFlags{old: old, new: nw, threshold: 0.05}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "## v2 against v1") || !strings.Contains(b.String(), "| a | fingerprint |") {
		t.Errorf("compare wrote:\n%s", b.String())
	}

	for _, tc := range []struct {
		name string
		o    compareFlags
		want string
	}{
		{"no new report", compareFlags{old: old}, "--new are required"},
		{"no old report", compareFlags{new: nw}, "--new are required"},
		{"a report that is not there", compareFlags{old: filepath.Join(dir, "none"), new: nw}, "reading the report"},
		{"a report that is not JSON", compareFlags{old: write("bad.json", "{"), new: nw}, "bad.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := compare(tc.o, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("compare returned %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := run([]string{"compare", "--old", old, "--new", nw}); err != nil {
		t.Errorf("run compare: %v", err)
	}
}
