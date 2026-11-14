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

// checkOnConsole boots a disposable 1 GiB guest of rel on root, whose unit prints ok or failed on
// the serial console, and fails the test unless ok comes first. It returns what the console said.
// Under TCG where there is no /dev/kvm: what it checks is behaviour, not time.
func checkOnConsole(t *testing.T, rel *machine.Release, root *rawRoot, ok, failed string) string {
	t.Helper()
	return checkGuest(t, rel, root, ok, failed, func(*machine.Spec) {}, func(string) {})
}

// checkGuest is checkOnConsole for a check the host takes part in: shape changes the machine
// before it starts, and said is given the console so far each time the guest has written to it.
func checkGuest(t *testing.T, rel *machine.Release, root *rawRoot, ok, failed string, shape func(*machine.Spec), said func(console string)) string {
	t.Helper()
	spec := rel.Spec()
	spec.BootCPUs = 2
	spec.Memory.SizeMB = 1024
	spec.Disks = []machine.Disk{{Path: root.path, Format: "raw"}}
	spec.Serial = "stdio"
	c := machine.DefaultCmdline()
	c.Root = "/dev/vda"
	c.Init = "/sbin/init"
	spec.Cmdline = c
	shape(&spec)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		tcg, err := rel.QEMUTCG()
		if err != nil {
			t.Fatal(err)
		}
		spec.QEMU, spec.Accel = tcg, "tcg"
		t.Log("TCG: checking functionality only")
	}
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
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
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()
	var console bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		console.Write(buf[:n])
		if strings.Contains(console.String(), ok) {
			return console.String()
		}
		if err != nil || strings.Contains(console.String(), failed) {
			t.Fatalf("guest check failed: %v\n%s\n%s", err, &console, &stderr)
		}
		said(console.String())
	}
}

// oneshotAtBoot has root run script as a oneshot unit three seconds after boot, its output on
// the console.
func oneshotAtBoot(t *testing.T, root *rawRoot, name, script, env string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	root.write("/"+script, string(content))
	root.write("/etc/systemd/system/"+name+".service", `[Unit]
Description=A check in a disposable guest
After=multi-user.target
[Service]
Type=oneshot
`+env+`
ExecStart=/bin/sh /`+script+`
StandardOutput=journal+console
StandardError=journal+console
`)
	root.write("/etc/systemd/system/"+name+".timer", `[Timer]
OnBootSec=3s
AccuracySec=100ms
`)
	root.link("/etc/systemd/system/timers.target.wants/"+name+".timer", "/etc/systemd/system/"+name+".timer")
}
