// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMemoryUnplug asks how much of what a VM grew by it can give back, and what that costs the
// guest, for each way the guest can online the memory virtio-mem plugs.
//
// The VM grows to its ceiling, and a script in the guest fills most of it: a large tmpfs file,
// which it then deletes, and 200 000 small files it keeps, whose inodes and dentries are kernel
// memory that cannot be moved. The VM is then asked to give everything back. What was plugged
// when it stopped, how long that took and whether the kernel killed anything for memory while
// it tried is the answer, per variant:
//
//	online                  memhp_default_state=online as the machine sets it: the zone the
//	                        kernel picks, ZONE_NORMAL for memory beside the boot memory
//	auto-movable            memory_hotplug.online_policy=auto-movable: ZONE_MOVABLE while it
//	                        stays under auto_movable_ratio (301%) of the kernel's memory
//	auto-movable, 1500%     the ratio a 512 MiB machine growing to 8 GiB needs for all of it
//
// Memory in ZONE_MOVABLE can always be offlined; memory in ZONE_NORMAL only if nothing the kernel
// cannot move landed in it. The price of ZONE_MOVABLE is that the kernel's own allocations -
// page tables, slab, the memmap of the plugged memory itself - come out of the boot memory alone.
//
//	SPIN_MEMORY_UNPLUG=1   run at all
//	REPS=<n>               boots per variant (default 3)
//	FLAGS=<flags>          the machine (default: 512 MiB growing to 8 GiB, 2 vCPUs)
//
// Needs /dev/kvm and a built release; no sudo.
func TestMemoryUnplug(t *testing.T) {
	if os.Getenv("SPIN_MEMORY_UNPLUG") == "" {
		t.Skip("set SPIN_MEMORY_UNPLUG=1: this fills guests' memory and takes it back")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 3)
	flags := strings.Fields(orElse("--memory 512 --max-memory 8192 --cpus 2", os.Getenv("FLAGS")))
	grow := unplugGrowth(t, flags)

	variants := []struct{ name, append string }{
		{"online", ""},
		{"auto-movable", "memory_hotplug.online_policy=auto-movable"},
		{"auto-movable, 1500%", "memory_hotplug.online_policy=auto-movable memory_hotplug.auto_movable_ratio=1500"},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\ngrown by %d MiB, filled, then asked to give it all back; %d boots per variant\n\n", grow, reps)
	fmt.Fprintf(&b, "%-22s %14s %10s %10s %8s\n", "VARIANT", "LEFT (MiB)", "MS", "OOM KILLS", "ALIVE")
	for range reps {
		for _, v := range variants {
			r := unplugOnce(t, out, flags, v.append, grow)
			fmt.Fprintf(&b, "%-22s %14d %10d %10d %8v\n", v.name, r.left, r.ms, r.oomKills, r.alive)
		}
	}
	t.Log(b.String())
}

// unplugGrowth is what the machine can grow by: its ceiling less its boot memory.
func unplugGrowth(t *testing.T, flags []string) int {
	t.Helper()
	get := func(name string) int {
		for i, f := range flags {
			if f == name && i+1 < len(flags) {
				n, err := strconv.Atoi(flags[i+1])
				if err != nil {
					t.Fatalf("%s %s: %v", name, flags[i+1], err)
				}
				return n
			}
		}
		t.Fatalf("FLAGS has no %s: this measures a machine that can grow", name)
		return 0
	}
	return get("--max-memory") - get("--memory")
}

type unplugResult struct {
	left, ms, oomKills int
	alive              bool
}

var (
	unplugDone  = regexp.MustCompile(`plugged (\d+) MiB (?:in|after) (\d+) ms`)
	unplugAlive = regexp.MustCompile(`UNPLUG-ALIVE oom=(\d+)`)
)

// unplugScript waits for the plugged memory to be online, fills it, keeps what the kernel cannot
// move, and after the host has taken memory back says whether it still runs and what the kernel
// killed.
const unplugScript = `#!/bin/sh
want=$(( $1 * 1024 ))
until [ "$(awk '/MemTotal/ {print $2}' /proc/meminfo)" -ge "$want" ]; do sleep 0.2; done
mkdir -p /run/unplug
mount -t tmpfs -o size=100% tmpfs /run/unplug
mkdir /run/unplug/files
free=$(awk '/MemAvailable/ {print int($2 / 1024)}' /proc/meminfo)
dd if=/dev/zero of=/run/unplug/big bs=1M count=$(( free * 6 / 10 )) status=none
i=0
while [ $i -lt 200000 ]; do : > /run/unplug/files/$i; i=$((i + 1)); done
rm -f /run/unplug/big
sync
echo UNPLUG-READY > /dev/console
sleep 45
echo "UNPLUG-ALIVE oom=$(dmesg | grep -c 'Out of memory')" > /dev/console
`

// unplugOnce boots one VM, grows it by grow MiB, lets the guest fill it and asks for it all back.
func unplugOnce(t *testing.T, out string, flags []string, extra string, grow int) unplugResult {
	t.Helper()
	root := newRawRoot(t, out, filepath.Join(out, "image/rootfs.qcow2"))
	root.write("/unplug.sh", unplugScript)
	root.write("/etc/systemd/system/unplug.service", fmt.Sprintf("[Unit]\nAfter=multi-user.target\n[Service]\n"+
		"Type=oneshot\nExecStart=/bin/sh /unplug.sh %d\nStandardOutput=journal+console\nStandardError=journal+console\n",
		grow))
	root.link("/etc/systemd/system/multi-user.target.wants/unplug.service", "/etc/systemd/system/unplug.service")

	dir := shortDir(t)
	console, qmp := filepath.Join(dir, "console"), filepath.Join(dir, "qmp")
	args := append([]string{"boot", "--release", out, "--disk", root.path, "--disk-format", "raw",
		"--console", "file:" + console, "--qmp", qmp}, flags...)
	if extra != "" {
		args = append(args, "--append", extra)
	}
	cli := filepath.Join(out, "bin/spin-machine")
	cmd := exec.Command(cli, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	waitConsole(t, console, "login:", 60*time.Second)
	if b, err := exec.Command(cli, "memory", "--qmp", qmp, "--size", strconv.Itoa(grow)).CombinedOutput(); err != nil {
		t.Fatalf("growing by %d MiB: %v\n%s", grow, err, b)
	}
	waitConsole(t, console, "UNPLUG-READY", 3*time.Minute)
	// Its error is the result: a guest that stops short is what this measures.
	b, _ := exec.Command(cli, "memory", "--qmp", qmp, "--size", "0", "--timeout", "30s").CombinedOutput()
	m := unplugDone.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("spin-machine memory said nothing of what was plugged:\n%s", b)
	}
	r := unplugResult{}
	r.left, _ = strconv.Atoi(m[1])
	r.ms, _ = strconv.Atoi(m[2])
	s := waitConsoleMatch(console, unplugAlive, 90*time.Second)
	if s != nil {
		r.alive = true
		r.oomKills, _ = strconv.Atoi(s[1])
	}
	return r
}

// shortDir is a directory whose paths fit a unix socket's 108 bytes, which a test's own
// temporary directory, under a long module path, does not.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "unplug")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// waitConsole waits for the console to say want, and fails the test with its tail if it does not.
func waitConsole(t *testing.T, console, want string, timeout time.Duration) {
	t.Helper()
	end := time.Now().Add(timeout)
	for {
		b, _ := os.ReadFile(console)
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("no %q on the console within %s:\n%s", want, timeout, tail(b, 3000))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitConsoleMatch is the first match of re on the console within timeout, or nil.
func waitConsoleMatch(console string, re *regexp.Regexp, timeout time.Duration) []string {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		b, _ := os.ReadFile(console)
		if m := re.FindStringSubmatch(string(b)); m != nil {
			return m
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}
