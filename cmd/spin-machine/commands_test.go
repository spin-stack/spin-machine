// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
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
// makes of the answers.
func TestSaveAndRestoreSayWhatQEMUNeeds(t *testing.T) {
	state := filepath.Join(t.TempDir(), "vm.state")
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
		name:    "a restore loads, and continues a VM that arrives paused",
		replies: []string{ok, ok, completed, `{"return": {"status": "paused"}}`, ok},
		do:      func(s string) error { return restore(restoreFlags{qmp: s, from: state, timeout: time.Minute}) },
		want:    []string{"migrate-incoming", "query-migrate", "query-status", "cont"},
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
			// The file named is the one given, absolute, in QEMU's file: form.
			for _, r := range asked {
				if r.Execute != "migrate" && r.Execute != "migrate-incoming" {
					continue
				}
				if uri := r.Arguments["uri"]; uri != "file:"+state {
					t.Errorf("%s to %v, want file:%s", r.Execute, uri, state)
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
	for _, f := range []string{"bin/qemu-system-x86_64", "bin/qemu-system-x86_64-tcg", "bin/qemu-img", "kernel/vmlinux", "qemu/pvh.bin", "qemu/qboot.bin", "image/rootfs.qcow2"} {
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
	every.BIOS = "bios-256k.bin"
	every.CPU, every.BootCPUs, every.MaxCPUs = "Skylake-Server-v4", 3, 8
	every.Accel = "tcg"
	every.Memory = machine.Memory{SizeMB: 1024, MaxMB: 4096}
	every.HotplugDisks, every.VsockCID = 2, 7
	every.Monitors = []machine.Monitor{{Socket: "/qmp"}}
	every.Incoming = "file:/s"
	every.Serial = ""
	every.Disks = []machine.Disk{{Path: "/d", Format: "raw", Readonly: true, Serial: "ser",
		DirectOverBacking: true}}
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
		{"everything given", []string{"--qemu", "/q", "--kernel", "/k", "--initrd", "/i", "--firmware", "/f", "--bios", "bios-256k.bin",
			"--disk", "/d", "--disk-format", "raw", "--disk-readonly", "--disk-serial", "ser",
			"--disk-direct-over-backing", "--accel", "tcg", "--memory", "1024", "--max-memory", "4096", "--cpu", "Skylake-Server-v4",
			"--cpus", "3", "--max-cpus", "8", "--hotplug-disks", "2",
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
			err := attach(attachFlags{qmp: m.socket, target: 1, disk: disk, format: "raw", serial: "s"})
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
				if dev["bus"] != "scsi0.0" || dev["scsi-id"] != float64(1) || dev["drive"] != "hd1-drive" || dev["serial"] != "s" {
					t.Errorf("device_add %v, want the disk at target 1 over hd1-drive with its serial", dev)
				}
			}
		})
	}

	if err := attach(attachFlags{qmp: "q.sock"}); err == nil || !strings.Contains(err.Error(), "--disk") {
		t.Errorf("attach with no disk returned %v, want the flags it needs", err)
	}
}

// memory, against a monitor that answers as QEMU does: the size is asked of the virtio-mem device
// by its id, and the plugged size read back until it is that. A guest that stops short is an
// error that says how far it got, and so is a size QEMU refuses.
func TestMemoryAsksForASizeAndSaysWhatWasPlugged(t *testing.T) {
	const mib = 1 << 20
	size := func(n int) string { return fmt.Sprintf(`{"return": %d}`, n*mib) }
	refused := `{"error": {"class": "GenericError", "desc": "not a multiple of the block size"}}`
	for _, tc := range []struct {
		name    string
		sizeMB  int
		replies []string
		timeout time.Duration
		want    string
		wantErr string
	}{
		{name: "grown", sizeMB: 1024, replies: []string{ok, ok, size(512), size(1024)}, timeout: time.Minute,
			want: "plugged 1024 MiB in"},
		{name: "all given back", sizeMB: 0, replies: []string{ok, ok, size(0)}, timeout: time.Minute,
			want: "plugged 0 MiB in"},
		{name: "stopped short", sizeMB: 1024, replies: []string{ok, ok, size(4)},
			want: "plugged 4 MiB after", wantErr: "reached 4 MiB of the 1024"},
		{name: "refused", sizeMB: 1024, replies: []string{ok, refused}, wantErr: "asking for 1024 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor(t, tc.replies...)
			var out strings.Builder
			err := memory(memoryFlags{qmp: m.socket, sizeMB: tc.sizeMB, timeout: tc.timeout}, &out)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("memory printed %q, want %q", out.String(), tc.want)
			}
			asked := m.commands(t)
			set := asked[1]
			if set.Execute != "qom-set" || set.Arguments["path"] != "/machine/peripheral/"+machine.VirtioMemID ||
				set.Arguments["property"] != "requested-size" || set.Arguments["value"] != float64(tc.sizeMB*mib) {
				t.Errorf("asked %+v, want requested-size set to %d MiB on the virtio-mem device", set, tc.sizeMB)
			}
			for _, r := range asked[2:] {
				if r.Execute != "qom-get" || r.Arguments["property"] != "size" {
					t.Errorf("then asked %+v, want the plugged size", r)
				}
			}
		})
	}

	if err := memory(memoryFlags{qmp: "q.sock", sizeMB: -1}, io.Discard); err == nil || !strings.Contains(err.Error(), "--size") {
		t.Errorf("memory with no size returned %v, want the flags it needs", err)
	}
	if err := run([]string{"memory", "--nope"}); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("run memory with a flag it does not take returned %v", err)
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
	if err := run([]string{"compare", "--nope"}); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("run compare with a flag it does not take returned %v", err)
	}
}

// detach, against a monitor that answers as QEMU does: the device is asked for, QEMU says it
// went, and then its node is closed - in that order, since the node holds the image. Each way it
// can fail is an error that says which step it was.
func TestDetachTakesTheDiskBackThenClosesIt(t *testing.T) {
	deleted := `{"event": "DEVICE_DELETED", "data": {"device": "hd2"}}` + "\n" + ok
	refused := `{"error": {"class": "GenericError", "desc": "no"}}`
	for _, tc := range []struct {
		name    string
		replies []string
		want    []string
		wantErr string
	}{
		{name: "detached", replies: []string{ok, deleted, ok}, want: []string{"device_del", "blockdev-del"}},
		{name: "a device QEMU will not remove", replies: []string{ok, refused},
			want: []string{"device_del"}, wantErr: "removing the disk at target 2"},
		{name: "a device QEMU never reports gone", replies: []string{ok, ok},
			want: []string{"device_del"}, wantErr: "did not report the disk at target 2 gone"},
		{name: "a node QEMU will not close", replies: []string{ok, deleted, refused},
			want: []string{"device_del", "blockdev-del"}, wantErr: "closing the disk at target 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor(t, tc.replies...)
			err := detach(detachFlags{qmp: m.socket, target: 2, timeout: 200 * time.Millisecond})
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
			if asked[1].Arguments["id"] != "hd2" {
				t.Errorf("device_del %v, want hd2", asked[1].Arguments)
			}
			if len(asked) > 2 && asked[2].Arguments["node-name"] != "hd2-drive" {
				t.Errorf("blockdev-del %v, want hd2-drive", asked[2].Arguments)
			}
		})
	}
}

// A target is 0 when none is named, and one no controller addresses is refused before a monitor
// is dialled, at either end of the range.
func TestATargetIsOneTheControllerAddresses(t *testing.T) {
	var a attachFlags
	if err := parse("attach", []string{"--qmp", "q", "--disk", "d"}, a.register); err != nil {
		t.Fatal(err)
	}
	var d detachFlags
	if err := parse("detach", []string{"--qmp", "q"}, d.register); err != nil {
		t.Fatal(err)
	}
	if a.target != 0 || d.target != 0 {
		t.Errorf("targets %d and %d named by nobody, want 0", a.target, d.target)
	}

	for _, target := range []int{0, machine.MaxHotplugDisks - 1} {
		if err := checkTarget(target); err != nil {
			t.Errorf("target %d refused: %v", target, err)
		}
	}
	for target, want := range map[int]string{machine.MaxHotplugDisks: "targets 0 to 255", -1: "target -1"} {
		if err := checkTarget(target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("target %d: %v, want an error containing %q", target, err, want)
		}
	}
}
