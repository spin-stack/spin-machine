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
	out, err := filepath.Abs("../_output")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := machine.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	base, err := rel.Rootfs()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "rootfs.raw")
	mustRun(t, filepath.Join(out, "bin/qemu-img"), "convert", "-f", "qcow2", "-O", "raw", base, raw)
	debugfs := func(command string) string {
		t.Helper()
		output, err := exec.Command("debugfs", "-w", "-R", command, raw).CombinedOutput()
		if err != nil {
			t.Fatalf("debugfs %s: %v\n%s", command, err, output)
		}
		return string(output)
	}
	write := func(path, content string) {
		t.Helper()
		src := filepath.Join(dir, "input")
		if err := os.WriteFile(src, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		// debugfs returns success even if the write failed. Read the guest file
		// back to ensure the experiment actually installed its input.
		debugfs("write " + src + " " + path)
		got, err := exec.Command("debugfs", "-R", "cat "+path, raw).Output()
		if err != nil || string(got) != content {
			t.Fatalf("guest file %s differs from its input: %v", path, err)
		}
	}
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
		path := "/etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf"
		debugfs("rm " + path)
		if strings.Contains(debugfs("stat "+path), "Inode:") {
			t.Fatal("the drop-in is still in the image; the experiment would prove nothing")
		}
	}
	for _, name := range []string{"logind-check.sh", "logind-session.sh"} {
		content, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write("/"+name, string(content))
	}
	write("/etc/systemd/system/logind-check.service", `[Unit]
Description=Check on-demand sessions in a disposable guest
After=multi-user.target
[Service]
Type=oneshot
Environment=LOGIND_EXPECT=`+expectedState+`
ExecStart=/bin/sh /logind-check.sh
StandardOutput=journal+console
StandardError=journal+console
`)
	write("/etc/systemd/system/logind-check.timer", `[Timer]
OnBootSec=3s
AccuracySec=100ms
`)
	debugfs("symlink /etc/systemd/system/timers.target.wants/logind-check.timer /etc/systemd/system/logind-check.timer")
	spec := rel.Spec()
	spec.BootCPUs = 2
	spec.Memory.SizeMB = 1024
	spec.Disks = []machine.Disk{{Path: raw, Format: "raw"}}
	spec.Serial = "stdio"
	c := machine.DefaultCmdline()
	c.Root = "/dev/vda"
	c.Init = "/sbin/init"
	spec.Cmdline = c.String()
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		spec.QEMU = filepath.Join(out, "bin/qemu-system-x86_64-tcg")
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], "accel=kvm", "accel=tcg")
			if args[i] == "host,migratable=on" {
				args[i] = "max,migratable=on"
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
