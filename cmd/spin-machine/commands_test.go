// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
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
	asked  chan []qmpRequest
}

type qmpRequest struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments"`
}

func newMonitor(t *testing.T, replies ...string) *monitor {
	t.Helper()
	m := &monitor{socket: filepath.Join(t.TempDir(), "qmp.sock"), asked: make(chan []qmpRequest, 1)}
	l, err := net.Listen("unix", m.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		var asked []qmpRequest
		defer func() { m.asked <- asked }()
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if _, err := io.WriteString(c, `{"QMP": {"version": {}, "capabilities": []}}`+"\n"); err != nil {
			return
		}
		r := bufio.NewReader(c)
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			var req qmpRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				return
			}
			asked = append(asked, req)
			if _, err := io.WriteString(c, reply+"\n"); err != nil {
				return
			}
		}
	}()
	return m
}

// commands is what the command under test asked, once it has let the monitor go.
func (m *monitor) commands(t *testing.T) []qmpRequest {
	t.Helper()
	select {
	case asked := <-m.asked:
		return asked
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
	for _, f := range []string{"bin/qemu-system-x86_64", "bin/qemu-img", "kernel/vmlinux", "qemu/pvh.bin", "image/rootfs.qcow2"} {
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
			"--disk-direct-over-backing", "--memory", "1024", "--max-memory", "4096", "--cpu", "Skylake-Server-v4",
			"--cpus", "3", "--max-cpus", "8", "--memory-file", "/m", "--memory-share", "--hotplug-ports", "2",
			"--vsock-cid", "7", "--qmp", "/qmp", "--incoming", "file:/s", "--console", "", "--init", "/bin/sh",
			"--root", "/dev/vdb", "--profile", "--append", "a=1 b"}, every, false},
		{"incoming defer is a restore over QMP", []string{"--incoming", "defer"}, deferred, true},
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
