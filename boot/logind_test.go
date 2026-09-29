// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// A correctness test, including under TCG: none of its durations are boot
// performance measurements. It edits a private raw copy, without mounts or NBD.
func TestLogindSessions(t *testing.T) {
	if os.Getenv("SPIN_LOGIND_TEST") != "1" {
		t.Skip("set SPIN_LOGIND_TEST=1 to check first SSH login in a built image")
	}
	out := releaseTree(t)
	rel, err := machine.OpenRelease(out)
	if err != nil {
		t.Fatal(err)
	}
	base, err := rel.Rootfs()
	if err != nil {
		t.Fatal(err)
	}
	root := newRawRoot(t, out, base)
	// The shipped image does not start logind at boot: the first login activates it over
	// the Varlink socket, which is worth 18 ms (see the 10-seats.conf drop-in). So the
	// state this asserts before any login is `inactive`, and a machine that answers
	// `active` has put logind back into the boot transaction.
	//
	// SPIN_LOGIND_NO_SEATS=1 removes the drop-in and nothing else. It is the experiment
	// that says what the drop-in does, and it is expected to fail: without
	// /run/systemd/seats, pam_systemd never asks logind for a session and never activates
	// it, so every login — SSH and console alike — comes up with XDG_RUNTIME_DIR unset and
	// no error anywhere (2026-09-12).
	expectedState := "inactive"
	if os.Getenv("SPIN_LOGIND_NO_SEATS") == "1" {
		root.remove("/etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf")
	}
	for _, name := range []string{"logind-check.sh", "logind-session.sh"} {
		content, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		root.write("/"+name, string(content))
	}
	root.write("/etc/systemd/system/logind-check.service", `[Unit]
Description=Check on-demand sessions in a disposable guest
After=multi-user.target
[Service]
Type=oneshot
Environment=LOGIND_EXPECT=`+expectedState+`
ExecStart=/bin/sh /logind-check.sh
StandardOutput=journal+console
StandardError=journal+console
`)
	root.write("/etc/systemd/system/logind-check.timer", `[Timer]
OnBootSec=3s
AccuracySec=100ms
`)
	root.link("/etc/systemd/system/timers.target.wants/logind-check.timer", "/etc/systemd/system/logind-check.timer")
	spec := rel.Spec()
	spec.BootCPUs = 2
	spec.Memory.SizeMB = 1024
	spec.Disks = []machine.Disk{{Path: root.path, Format: "raw"}}
	spec.Serial = "stdio"
	c := machine.DefaultCmdline()
	c.Root = "/dev/vda"
	c.Init = "/sbin/init"
	spec.Cmdline = c
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		spec.QEMU = filepath.Join(out, "bin/qemu-system-x86_64-tcg")
		for i := range args {
			if args[i] == "-accel" {
				args[i+1] = "tcg"
			}
			if strings.HasPrefix(args[i], "host,migratable=on") {
				args[i] = "max" + strings.TrimPrefix(args[i], "host")
			}
		}
		t.Log("TCG: checking functionality only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.QEMU, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	stop := func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
			waited = true
		}
	}
	defer stop()
	var console bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		console.Write(buf[:n])
		if strings.Contains(console.String(), "LOGIND_CHECK_OK") {
			t.Log(console.String())
			return
		}
		if err != nil || strings.Contains(console.String(), "LOGIND_CHECK_FAILED") {
			stop()
			t.Fatalf("guest session check failed: %v\n%s\n%s", err, &console, &stderr)
		}
	}
}
