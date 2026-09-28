// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSSHUnderLoad times SSH logins into a guest that is busy, and says whether a login still
// gets in.
//
// The guest runs testdata/ssh-load.sh once it is up: logins while idle, while a service
// burns every vCPU eight times over, while a login session does the same, and while tmpfs
// holds all but 64 and then 16 MiB of memory. The client runs with a weight nothing else in
// the guest has, so a slow login is sshd's. Each case also reports what the guest was under -
// runnable tasks, memory available, PSI - so a load that did not happen cannot pass for one
// sshd shrugged off.
//
// It exists because SSH connections to busy VMs were timing out and the obvious fix was to
// give sshd priority. Measured 2026-09-28, 7.2.8, 2 vCPUs, 1 GiB, 3 boots of 10 logins per
// case, p50 / max in ms, in two runs that each compared a drop-in on ssh.service against the
// image as built:
//
//	                  idle      cpu-service   cpu-session   memory-64   memory-16
//	as built          102/203   105/207       105/207       102/204     313/614
//	CPUWeight=1000,
//	IOWeight=1000     102/203   104/107       105/208       102/103     213/826
//
//	as built          102/203   104/208       104/208       102/103     212/430
//	MemoryLow=64M     102/208   105/205       104/206       102/103     211/354
//
// No login failed. CPU load, at 14-41% PSI cpu-some with 17-20 tasks runnable, does not slow
// one: cgroup v2's fair share already gives ssh.service its part of the machine whatever runs
// beside it, so a weight on it buys nothing. Memory at 16 MiB available doubles a login, and
// neither variant changes that. So the guest, loaded like this, is not where the timeouts
// come from, and nothing here is shipped for it. Not OOMScoreAdjust either: sshd sets its own
// oom_score_adj to -1000 and restores the inherited value in the processes it forks, so on
// the unit the -1000 would be what the users' sessions inherit.
//
//	SPIN_SSH_LOAD=1   run at all
//	REPS=<n>          boots (default 2), 10 logins per case in each
//
// Needs /dev/kvm and a built release; edits a private raw copy of the base image with the
// release's debugfs, so no sudo.
func TestSSHUnderLoad(t *testing.T) {
	if os.Getenv("SPIN_SSH_LOAD") == "" {
		t.Skip("set SPIN_SSH_LOAD=1: this boots the image and loads it")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 2)

	cases := []string{"idle", "cpu-service", "cpu-session", "memory-64", "memory-16"}
	got := map[string][]string{}
	var psi []string
	for range reps {
		logins, pressure := sshLoadRun(t, out)
		for c, ms := range logins {
			got[c] = append(got[c], ms...)
		}
		psi = append(psi, pressure...)
	}

	var r strings.Builder
	fmt.Fprintf(&r, "\nSSH login, ms: p50 / p95 / max, and failures, over %d boots\n\n", reps)
	for _, c := range cases {
		fmt.Fprintf(&r, "  %-12s %s\n", c, loginSummary(got[c]))
	}
	r.WriteString("\nwhat each case was under, per boot (tasks runnable, memory available, and PSI avg10 where the kernel has it):\n")
	for _, p := range psi {
		fmt.Fprintf(&r, "  %s\n", p)
	}
	t.Log(r.String())
}

var (
	sshLoadLine = regexp.MustCompile(`SSHLOAD (\S+)((?: (?:\d+|FAIL))+)`)
	sshLoadPSI  = regexp.MustCompile(`SSHLOADPSI (.*)`)
)

// sshLoadRun boots one guest with the load script in its root and returns each case's logins,
// and the pressure each case was measured under.
func sshLoadRun(t *testing.T, out string) (map[string][]string, []string) {
	t.Helper()
	dir := t.TempDir()
	raw := filepath.Join(dir, "rootfs.raw")
	mustRun(t, filepath.Join(out, "bin/qemu-img"), "convert", "-f", "qcow2", "-O", "raw",
		filepath.Join(out, "image/rootfs.qcow2"), raw)

	script, err := os.ReadFile(filepath.Join("testdata", "ssh-load.sh"))
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{
		"/sshload.sh": string(script),
		"/etc/systemd/system/sshload.service": "[Unit]\nAfter=multi-user.target\n[Service]\nType=oneshot\n" +
			"ExecStart=/bin/sh /sshload.sh\nStandardOutput=journal+console\nStandardError=journal+console\n",
		"/etc/systemd/system/sshload.timer": "[Timer]\nOnBootSec=3s\nAccuracySec=100ms\n",
	}
	debugfs := filepath.Join(out, "bin/debugfs")
	for p, c := range all {
		src := filepath.Join(dir, "input")
		if err := os.WriteFile(src, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		// mkdir fails on a directory that is there, which is not a failure here; the write is
		// what is checked.
		_ = exec.Command(debugfs, "-w", "-R", "mkdir "+filepath.Dir(p), raw).Run()
		mustRun(t, debugfs, "-w", "-R", "write "+src+" "+p, raw)
		// debugfs exits 0 whether or not the write happened: read it back.
		if b, err := exec.Command(debugfs, "-R", "cat "+p, raw).Output(); err != nil || string(b) != c {
			t.Fatalf("%s is not in the guest's root as written (%v)", p, err)
		}
	}
	mustRun(t, debugfs, "-w", "-R",
		"symlink /etc/systemd/system/timers.target.wants/sshload.timer /etc/systemd/system/sshload.timer", raw)

	console := filepath.Join(dir, "console")
	cmd := exec.Command(filepath.Join(out, "bin/spin-machine"), "boot", "--release", out,
		"--disk", raw, "--disk-format", "raw", "--memory", "1024", "--cpus", "2",
		"--console", "file:"+console, "--init", "/sbin/init")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Minute)
	for {
		b, _ := os.ReadFile(console)
		s := string(b)
		if strings.Contains(s, "SSHLOAD_DONE") {
			res := map[string][]string{}
			for _, m := range sshLoadLine.FindAllStringSubmatch(s, -1) {
				res[m[1]] = strings.Fields(m[2])
			}
			var pressure []string
			for _, m := range sshLoadPSI.FindAllStringSubmatch(s, -1) {
				pressure = append(pressure, strings.TrimSpace(m[1]))
			}
			return res, pressure
		}
		if strings.Contains(s, "SSHLOAD_FAILED") || time.Now().After(deadline) {
			t.Fatalf("the load script did not finish; the console ends:\n%s", tail(b, 3000))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// loginSummary is p50 / p95 / max of the logins that got in, and how many did not.
func loginSummary(v []string) string {
	var ms []float64
	failed := 0
	for _, s := range v {
		n, err := strconv.Atoi(s)
		if err != nil {
			failed++
			continue
		}
		ms = append(ms, float64(n))
	}
	if len(ms) == 0 {
		return fmt.Sprintf("all %d failed", failed)
	}
	sort.Float64s(ms)
	return fmt.Sprintf("%.0f / %.0f / %.0f, %d failed", pct(ms, 50), pct(ms, 95), ms[len(ms)-1], failed)
}
