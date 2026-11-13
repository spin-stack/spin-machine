// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/spin-machine/boot"
)

// The two numbers a change to this machine is held to, side by side: a guest booted cold and a
// guest restored from a checkpoint, each to its first SSH login, from the moment before QEMU is
// exec'd. A machine is both - booted to make a checkpoint and restored from one - and a change
// that speeds one phase and not either of these has not sped the machine up.
//
// Each row is a spin-machine and a release tree. The release's own spin-machine is the machine
// as published; this tree's, built here, is the machine this change defines - the same QEMU,
// kernel and image under another command line, so a change to machine/ or cmd/ is a row of its
// own. SPIN_MACHINE_OUTPUT_B adds a third, this tree's spin-machine over another release tree:
// another kernel, image or QEMU.
//
// One boot per row and repetition: logged into over vsock (cold), saved with `spin-machine
// save`, the state evicted from the host's page cache so it is read from the disk as a
// checkpoint kept for later is, and restored with `boot --incoming defer` and `spin-machine
// restore` (QEMU up: its monitor answers; running: restore returned), then logged into again.
//
//	SPIN_BOOT_SCORE=1          run at all
//	REPS=<n>                   repetitions of each row (default 5), interleaved
//	SPIN_MACHINE_OUTPUT_B=<d>  a third row over another release tree
//	CPUS=<n>                   vCPUs (default 2)
func TestScore(t *testing.T) {
	if os.Getenv("SPIN_BOOT_SCORE") == "" {
		t.Skip("set SPIN_BOOT_SCORE=1: boots, saves and restores VMs with a vsock device")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a key into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	if _, err := os.Stat("/dev/vhost-vsock"); err != nil {
		t.Skip("no /dev/vhost-vsock: run task vsock")
	}
	reps := envInt(t, "REPS", 5)
	proxy, key := sshTools(t)
	v := sshVariant(key, "score", "")
	tree := filepath.Join(t.TempDir(), "spin-machine")
	if b, err := exec.Command("go", "build", "-o", tree, "../cmd/spin-machine").CombinedOutput(); err != nil {
		t.Fatalf("building spin-machine: %v\n%s", err, b)
	}

	type row struct{ label, cli, release string }
	rows := []row{
		{"release", filepath.Join(out, "bin", "spin-machine"), out},
		{"this tree", tree, out},
	}
	if b := os.Getenv("SPIN_MACHINE_OUTPUT_B"); b != "" {
		rows = append(rows, row{"this tree, B", tree, b})
	}
	type samples struct{ cold, save, up, running, restored []time.Duration }
	got := map[string]*samples{}
	for range reps {
		for _, r := range rows {
			res := scoreOnce(t, r.cli, r.release, v, proxy, key)
			s := got[r.label]
			if s == nil {
				s = &samples{}
				got[r.label] = s
			}
			s.cold, s.save = append(s.cold, res.cold), append(s.save, res.save)
			s.up, s.running, s.restored = append(s.up, res.up), append(s.running, res.running), append(s.restored, res.restored)
		}
	}

	ms := func(ds []time.Duration, q float64) string {
		d, _ := boot.Percentile(ds, q)
		return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\np50/p95 ms over %d of each, interleaved, %s vCPUs, %s MiB; each from the moment before QEMU is exec'd\n\n",
		reps, v.cpus, v.memory)
	fmt.Fprintf(&b, "  %-14s %13s   %13s   %13s %13s %13s\n", "", "COLD: LOGIN", "SAVE", "RESTORE: UP", "RUNNING", "LOGIN")
	for _, r := range rows {
		s := got[r.label]
		fmt.Fprintf(&b, "  %-14s %6s/%-6s   %6s/%-6s   %6s/%-6s %6s/%-6s %6s/%-6s\n", r.label,
			ms(s.cold, .5), ms(s.cold, .95), ms(s.save, .5), ms(s.save, .95), ms(s.up, .5), ms(s.up, .95),
			ms(s.running, .5), ms(s.running, .95), ms(s.restored, .5), ms(s.restored, .95))
	}
	t.Log(b.String())
}

type scoreRun struct {
	cold, save, up, running, restored time.Duration
}

// scoreOnce boots v with cli over release, logs in, saves it, and restores it.
func scoreOnce(t *testing.T, cli, release string, v variant, proxy, key string) scoreRun {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.qcow2")
	mustRun(t, filepath.Join(release, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(release, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)
	cid := uint32(100000 + rand.IntN(1<<20))
	state := filepath.Join(dir, "state")

	launch := func(qmp string, extra ...string) *exec.Cmd {
		args := append([]string{"boot", "--release", release, "--disk", overlay, "--memory", v.memory,
			"--cpus", v.cpus, "--vsock-cid", fmt.Sprint(cid), "--qmp", qmp, "--console", "file:/dev/null",
			"--append", "init=/sbin/init"}, extra...)
		cmd := exec.Command(cli, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("launching the machine: %v", err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
		return cmd
	}
	// The port probed with a short connect until it accepts, and only then ssh. vsockproxy's own
	// connect waits out vsock's two-second default against a guest not yet listening, which made
	// a cold login seconds; and probing again between logins starts a per-connection sshd each
	// time, which can trip the socket's start limit (both 2026-11-13).
	login := func(deadline time.Time) bool {
		for !vsockAccepts(cid, 22) {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(2 * time.Millisecond)
		}
		for time.Now().Before(deadline) {
			if exec.Command("ssh", sshArgs(proxy, key, cid)...).Run() == nil {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}

	var r scoreRun
	t0 := time.Now()
	first := launch(filepath.Join(dir, "a.qmp"))
	if !login(t0.Add(60 * time.Second)) {
		t.Fatal("the cold boot was never logged into")
	}
	r.cold = time.Since(t0)

	t0 = time.Now()
	mustRun(t, cli, "save", "--qmp", filepath.Join(dir, "a.qmp"), "--to", state)
	r.save = time.Since(t0)
	_ = first.Wait() // save quits it
	evict(t, state)

	qmp := filepath.Join(dir, "b.qmp")
	t0 = time.Now()
	launch(qmp, "--incoming", "defer")
	for {
		if c, err := dialUnix(qmp); err == nil {
			_ = c.Close()
			break
		}
		if time.Since(t0) > 30*time.Second {
			t.Fatal("the restoring QEMU never opened its QMP socket")
		}
		time.Sleep(time.Millisecond)
	}
	r.up = time.Since(t0)
	mustRun(t, cli, "restore", "--qmp", qmp, "--from", state)
	r.running = time.Since(t0)
	if !login(t0.Add(60 * time.Second)) {
		t.Fatal("the restored guest was never logged into")
	}
	r.restored = time.Since(t0)
	return r
}

// evict drops the state from the host's page cache, which the save has just filled: a
// checkpoint kept for later is read from the disk.
func evict(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Fsync(int(f.Fd())); err != nil {
		t.Fatalf("syncing %s: %v", path, err)
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatalf("evicting %s from the page cache: %v", path, err)
	}
}

// dialUnix connects to a unix socket: whether QEMU's monitor is there yet.
func dialUnix(path string) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
