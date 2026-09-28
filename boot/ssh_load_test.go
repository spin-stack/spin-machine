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
// burns every vCPU eight times over, while a login session does the same, and with /tmp full.
// The client runs with a weight nothing else in
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
// come from, and nothing here is shipped for it. (The memory columns were a tmpfs of the test's
// own filled to 64 and 16 MiB left; since MGLRU's min_ttl_ms is set, 16 MiB left is an OOM kill
// of whatever the kernel finds, so the case is /tmp filling to its cap instead.) Not
// OOMScoreAdjust either: sshd sets its own
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

	cases := []string{"idle", "cpu-service", "cpu-session", "tmp-full"}
	got := map[string][]string{}
	var psi []string
	for range reps {
		r := sshLoadRun(t, out, sshLoadGuest{}, 10*time.Minute)
		if !r.finished {
			t.Fatalf("the load script did not finish; the console ends:\n%s", r.console)
		}
		for c, ms := range r.logins {
			got[c] = append(got[c], ms...)
		}
		psi = append(psi, r.notes...)
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
	sshLoadNote = regexp.MustCompile(`SSHLOAD(?:PSI|EXHAUST) (.*)`)
)

// sshLoadGuest is what one boot of the load script changes in its guest: files and symlinks
// written into its root, files removed from it, and which of the script's cases run (its own
// list by default).
type sshLoadGuest struct {
	files  map[string]string
	links  map[string]string
	remove []string
	cases  string
}

// sshLoadResult is one boot: each case's logins, the lines saying what the guest was under,
// and whether the script got to the end - a guest that stopped answering is a result here.
type sshLoadResult struct {
	logins   map[string][]string
	notes    []string
	finished bool
	console  string
}

// sshLoadRun boots one guest with the load script in its root, and gives it until the deadline
// to finish.
func sshLoadRun(t *testing.T, out string, g sshLoadGuest, deadline time.Duration) sshLoadResult {
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
			"Environment=SSHLOAD_CASES=\"" + g.cases + "\"\n" +
			"ExecStart=/bin/sh /sshload.sh\nStandardOutput=journal+console\nStandardError=journal+console\n",
		"/etc/systemd/system/sshload.timer": "[Timer]\nOnBootSec=3s\nAccuracySec=100ms\n",
	}
	for p, c := range g.files {
		all[p] = c
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
	for _, p := range g.remove {
		// Present first, so that a variant removing a path the image no longer has fails here
		// rather than measuring the image as built under another name.
		if out, _ := exec.Command(debugfs, "-R", "stat "+p, raw).CombinedOutput(); !strings.Contains(string(out), "Inode:") {
			t.Fatalf("%s is not in the image, so removing it tests nothing", p)
		}
		mustRun(t, debugfs, "-w", "-R", "rm "+p, raw)
		if out, _ := exec.Command(debugfs, "-R", "stat "+p, raw).CombinedOutput(); strings.Contains(string(out), "Inode:") {
			t.Fatalf("%s is still in the guest's root", p)
		}
	}
	links := map[string]string{"/etc/systemd/system/timers.target.wants/sshload.timer": "/etc/systemd/system/sshload.timer"}
	for l, target := range g.links {
		links[l] = target
	}
	for l, target := range links {
		_ = exec.Command(debugfs, "-w", "-R", "mkdir "+filepath.Dir(l), raw).Run()
		mustRun(t, debugfs, "-w", "-R", "symlink "+l+" "+target, raw)
	}

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

	end := time.Now().Add(deadline)
	for {
		b, _ := os.ReadFile(console)
		s := string(b)
		done := strings.Contains(s, "SSHLOAD_DONE")
		if done || strings.Contains(s, "SSHLOAD_FAILED") || time.Now().After(end) {
			r := sshLoadResult{logins: map[string][]string{}, finished: done, console: tail(b, 3000)}
			for _, m := range sshLoadLine.FindAllStringSubmatch(s, -1) {
				r.logins[m[1]] = strings.Fields(m[2])
			}
			for _, m := range sshLoadNote.FindAllStringSubmatch(s, -1) {
				r.notes = append(r.notes, strings.TrimSpace(m[1]))
			}
			return r
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
