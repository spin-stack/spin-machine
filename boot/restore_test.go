// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/spin-machine/boot"
)

// What resuming a checkpoint costs, from the host: the path a workspace takes, which is handed
// its identity after it is restored rather than booted.
//
// One boot is logged into over vsock, so the guest is settled, and saved with `spin-machine
// save`. Then, from the moment before QEMU is exec'd: QEMU up with --incoming defer (its QMP
// socket answering), the state loaded and the VM running (`spin-machine restore` returned), and
// the first `ssh spin@ true` done. The state's file is evicted from the host's page cache
// before the restore, so it is read from the disk as a checkpoint kept for later would be.
//
// The rows are the state's layout, --channels on save and restore: QEMU's single stream, which
// every checkpoint is today, and mapped-ram written and read by that many multifd channels. A
// checkpoint is restored only in the layout it was saved in, so the second is a decision about
// every checkpoint a caller keeps; this is what it is worth.
//
//	SPIN_BOOT_RESTORE=1  run at all
//	REPS=<n>             saves and restores of each row (default 5)
//	CHANNELS=<n>         the mapped-ram row's channels (default 4)
//	CPUS=<n>             vCPUs (default 2)
func TestRestoreCost(t *testing.T) {
	if os.Getenv("SPIN_BOOT_RESTORE") == "" {
		t.Skip("set SPIN_BOOT_RESTORE=1: boots, saves and restores VMs with a vsock device")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a key into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	if _, err := os.Stat("/dev/vhost-vsock"); err != nil {
		t.Skip("no /dev/vhost-vsock: run task vsock")
	}
	reps := envInt(t, "REPS", 5)
	channels := envInt(t, "CHANNELS", 4)
	proxy, key := sshTools(t)
	v := sshVariant(key, "restore", "")
	// save and restore are this tree's, built here: --channels is newer than any release, and
	// the CLI is what is being measured. The machine - QEMU, kernel, image - is the release's.
	cli := filepath.Join(t.TempDir(), "spin-machine")
	if b, err := exec.Command("go", "build", "-o", cli, "../cmd/spin-machine").CombinedOutput(); err != nil {
		t.Fatalf("building spin-machine: %v\n%s", err, b)
	}

	rows := []struct {
		label    string
		channels int
	}{{"stream", 0}, {fmt.Sprintf("mapped-ram x%d", channels), channels}}
	type samples struct {
		save, up, loaded, login []time.Duration
		sizeMB, diskMB          []float64
	}
	got := map[string]*samples{}
	for range reps {
		for _, r := range rows {
			res := restoreOnce(t, out, cli, v, proxy, key, r.channels)
			s := got[r.label]
			if s == nil {
				s = &samples{}
				got[r.label] = s
			}
			s.save, s.up, s.loaded, s.login = append(s.save, res.save), append(s.up, res.up), append(s.loaded, res.loaded), append(s.login, res.login)
			s.sizeMB, s.diskMB = append(s.sizeMB, res.sizeMB), append(s.diskMB, res.diskMB)
		}
	}

	ms := func(ds []time.Duration, q float64) string {
		d, _ := boot.Percentile(ds, q)
		return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nsave and restore of a settled guest, p50/p95 ms over %d of each, interleaved, %s vCPUs, %s MiB\n"+
		"(restore columns from the moment before QEMU is exec'd; the state read cold from the disk)\n\n", reps, v.cpus, v.memory)
	fmt.Fprintf(&b, "  %-16s %13s %13s %13s %13s %16s\n", "LAYOUT", "SAVE", "QEMU UP", "RUNNING", "FIRST LOGIN", "STATE MiB/DISK")
	for _, r := range rows {
		s := got[r.label]
		fmt.Fprintf(&b, "  %-16s %6s/%-6s %6s/%-6s %6s/%-6s %6s/%-6s %8.0f/%-6.0f\n", r.label,
			ms(s.save, .5), ms(s.save, .95), ms(s.up, .5), ms(s.up, .95), ms(s.loaded, .5), ms(s.loaded, .95),
			ms(s.login, .5), ms(s.login, .95), pct(s.sizeMB, 50), pct(s.diskMB, 50))
	}
	t.Log(b.String())
}

type restoreRun struct {
	save, up, loaded, login time.Duration
	sizeMB, diskMB          float64 // the state's length, and the blocks it holds on the disk
}

// restoreOnce boots v, logs in, saves it with cli in the layout channels names, and restores it.
func restoreOnce(t *testing.T, out, cli string, v variant, proxy, key string, channels int) restoreRun {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)
	cid := uint32(100000 + rand.IntN(1<<20))
	state := filepath.Join(dir, "state")
	ch := strconv.Itoa(channels)

	launch := func(qmp string, extra ...string) *exec.Cmd {
		args := append([]string{"--disk", overlay, "--memory", v.memory, "--cpus", v.cpus,
			"--vsock-cid", fmt.Sprint(cid), "--qmp", qmp, "--console", "file:/dev/null",
			"--append", "init=/sbin/init"}, extra...)
		cmd := machineCommand(t, out, v, args...)
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
	login := func(deadline time.Time) bool {
		for time.Now().Before(deadline) {
			if vsockAccepts(cid, 22) && exec.Command("ssh", sshArgs(proxy, key, cid)...).Run() == nil {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}

	// The boot to save: logged into, so what the restore brings back is a guest that has
	// finished its boot and served a session.
	first := launch(filepath.Join(dir, "a.qmp"))
	if !login(time.Now().Add(60 * time.Second)) {
		t.Fatal("the boot to save was never logged into")
	}
	var r restoreRun
	t0 := time.Now()
	mustRun(t, cli, "save", "--qmp", filepath.Join(dir, "a.qmp"), "--to", state, "--channels", ch)
	r.save = time.Since(t0)
	_ = first.Wait() // save quits it

	st, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	var sys syscall.Stat_t
	if err := syscall.Stat(state, &sys); err != nil {
		t.Fatal(err)
	}
	r.sizeMB, r.diskMB = float64(st.Size())/(1<<20), float64(sys.Blocks*512)/(1<<20)
	evict(t, state)

	qmp := filepath.Join(dir, "b.qmp")
	t0 = time.Now()
	launch(qmp, "--incoming", "defer")
	for {
		if c, err := dialUnix(qmp); err == nil {
			c.Close()
			break
		}
		if time.Since(t0) > 30*time.Second {
			t.Fatal("the restoring QEMU never opened its QMP socket")
		}
		time.Sleep(time.Millisecond)
	}
	r.up = time.Since(t0)
	mustRun(t, cli, "restore", "--qmp", qmp, "--from", state, "--channels", ch)
	r.loaded = time.Since(t0)
	if !login(t0.Add(60 * time.Second)) {
		t.Fatal("the restored guest was never logged into")
	}
	r.login = time.Since(t0)
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
	defer f.Close()
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
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
