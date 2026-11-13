// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"net"
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
// Two phases. First every cold boot, of every row and repetition interleaved: logged into over
// vsock, saved with `spin-machine save` (which ends it), and the state evicted from the host's
// page cache so it is read from the disk as a checkpoint kept for later is. Then every restore, in
// the same order: `boot --incoming defer` and `spin-machine restore` (QEMU up: its monitor
// answers; running: restore returned), logged into again, and killed before the next.
//
// The cold boots come first because on the bench fleet, whose hosts run KVM nested in a cloud
// VM, every cold boot that ran after a restored guest had run on the host was ~1.8x slower, all
// of its phases alike, whether or not that guest was still running; a laptop's KVM, not nested,
// showed nothing of the kind (2026-11-13). Interleaved boot-save-restore repetitions measured that
// instead of the machine: the cold login of every repetition but the first.
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
	proxy, key := scoreSSHTools(t)
	v := scoreVariant(key)
	tree := filepath.Join(t.TempDir(), "spin-machine")
	if b, err := exec.Command("go", "build", "-o", tree, "../cmd/spin-machine").CombinedOutput(); err != nil {
		t.Fatalf("building spin-machine: %v\n%s", err, b)
	}

	rows := []scoreRow{
		{"release", filepath.Join(out, "bin", "spin-machine"), out},
		{"this tree", tree, out},
	}
	if b := os.Getenv("SPIN_MACHINE_OUTPUT_B"); b != "" {
		rows = append(rows, scoreRow{"this tree, B", tree, b})
	}

	var saved []*scoreRun
	for i := range reps {
		for _, r := range rows {
			s := &scoreRun{scoreRow: r, v: v, proxy: proxy, key: key, dir: t.TempDir()}
			s.cold(t)
			t.Logf("%s, repetition %d: cold accepted %v, logged in %v, saved in %v", r.label, i+1,
				s.coldAccept.Round(time.Millisecond), s.coldLogin.Round(time.Millisecond), s.save.Round(time.Millisecond))
			saved = append(saved, s)
		}
	}
	for i, s := range saved {
		s.restore(t)
		t.Logf("%s, repetition %d: restored up %v, running %v, accepted %v, logged in %v", s.label, i/len(rows)+1,
			s.up.Round(time.Millisecond), s.running.Round(time.Millisecond),
			s.restoredAccept.Round(time.Millisecond), s.restoredLogin.Round(time.Millisecond))
	}

	pair := func(runs []*scoreRun, f func(*scoreRun) time.Duration) string {
		var ds []time.Duration
		for _, s := range runs {
			ds = append(ds, f(s))
		}
		p50, _ := boot.Percentile(ds, .5)
		p95, _ := boot.Percentile(ds, .95)
		return fmt.Sprintf("%6d/%-6d", p50.Milliseconds(), p95.Milliseconds())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\np50/p95 ms over %d of each, interleaved, %s vCPUs, %s MiB; each from the moment before QEMU is exec'd;\n"+
		"every cold boot before every restore\n\n", reps, v.cpus, v.memory)
	fmt.Fprintf(&b, "  %-14s %13s %13s   %13s   %13s %13s %13s %13s\n", "",
		"COLD: ACCEPT", "LOGIN", "SAVE", "RESTORE: UP", "RUNNING", "ACCEPT", "LOGIN")
	for _, r := range rows {
		var runs []*scoreRun
		for _, s := range saved {
			if s.label == r.label {
				runs = append(runs, s)
			}
		}
		fmt.Fprintf(&b, "  %-14s %s %s   %s   %s %s %s %s\n", r.label,
			pair(runs, func(s *scoreRun) time.Duration { return s.coldAccept }),
			pair(runs, func(s *scoreRun) time.Duration { return s.coldLogin }),
			pair(runs, func(s *scoreRun) time.Duration { return s.save }),
			pair(runs, func(s *scoreRun) time.Duration { return s.up }),
			pair(runs, func(s *scoreRun) time.Duration { return s.running }),
			pair(runs, func(s *scoreRun) time.Duration { return s.restoredAccept }),
			pair(runs, func(s *scoreRun) time.Duration { return s.restoredLogin }))
	}
	t.Log(b.String())
}

// scoreRow is a spin-machine and the release tree it boots.
type scoreRow struct{ label, cli, release string }

// scoreRun is one machine of a row: booted cold and saved, then restored from what it saved.
type scoreRun struct {
	scoreRow
	v          variant
	proxy, key string
	dir        string
	cid        uint32

	coldAccept, coldLogin, save                time.Duration
	up, running, restoredAccept, restoredLogin time.Duration
}

func (s *scoreRun) overlay() string { return filepath.Join(s.dir, "overlay.qcow2") }
func (s *scoreRun) state() string   { return filepath.Join(s.dir, "state") }

// launch starts the machine with its monitor on qmp, and returns a channel closed when it has
// exited and a stop that kills it - its whole process group - and waits. The test's end stops it
// if nothing has.
func (s *scoreRun) launch(t *testing.T, qmp string, extra ...string) (exited <-chan struct{}, stop func()) {
	t.Helper()
	args := append([]string{"boot", "--release", s.release, "--disk", s.overlay(), "--memory", s.v.memory,
		"--cpus", s.v.cpus, "--vsock-cid", fmt.Sprint(s.cid), "--qmp", qmp, "--console", "file:" + qmp + ".console",
		"--append", "init=/sbin/init"}, extra...)
	cmd := exec.Command(s.cli, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching %s: %v", s.cli, err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop = func() {
		select {
		case <-done:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	t.Cleanup(stop)
	return done, stop
}

// login waits for sshd to accept on vsock and then logs in, returning when each happened. The
// port is probed with a short connect until it accepts, and only then ssh: vsockproxy's own
// connect waits out vsock's two-second default against a guest not yet listening, which made a
// cold login seconds; and probing again between logins starts a per-connection sshd each time,
// which can trip the socket's start limit (both 2026-11-13).
func (s *scoreRun) login(t *testing.T, t0 time.Time, what string) (accepted, loggedIn time.Duration) {
	t.Helper()
	deadline := t0.Add(60 * time.Second)
	for !vsockAccepts(s.cid, 22) {
		if time.Now().After(deadline) {
			t.Fatalf("%s, %s: sshd never accepted on vsock", s.label, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
	accepted = time.Since(t0)
	for time.Now().Before(deadline) {
		if exec.Command("ssh", scoreSSHArgs(s.proxy, s.key, s.cid)...).Run() == nil {
			return accepted, time.Since(t0)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s, %s: sshd accepted but was never logged into", s.label, what)
	return 0, 0
}

// cold boots a fresh overlay of the row's release, logs in, saves it - which ends it - and
// evicts the state from the host's page cache, which the save has just filled: a checkpoint kept
// for later is read from the disk, and twenty of them cached would be twenty fewer GiB for the
// cold boots that follow.
func (s *scoreRun) cold(t *testing.T) {
	t.Helper()
	mustRun(t, filepath.Join(s.release, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(s.release, "image", "rootfs.qcow2"), s.overlay())
	editOverlay(t, s.overlay(), s.v)
	s.cid = uint32(100000 + rand.IntN(1<<20))

	qmp := filepath.Join(s.dir, "cold.qmp")
	t0 := time.Now()
	exited, _ := s.launch(t, qmp)
	s.coldAccept, s.coldLogin = s.login(t, t0, "cold")

	t0 = time.Now()
	mustRun(t, s.cli, "save", "--qmp", qmp, "--to", s.state())
	s.save = time.Since(t0)
	select { // save quits QEMU, and the restore opens the same overlay
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: QEMU was still running 30s after its state was saved", s.label)
	}
	evict(t, s.state())
}

// restore resumes the state cold saved, logs in, and kills the machine: restored guests left
// running would be what the restores after them measured.
func (s *scoreRun) restore(t *testing.T) {
	t.Helper()
	qmp := filepath.Join(s.dir, "restored.qmp")
	t0 := time.Now()
	_, stop := s.launch(t, qmp, "--incoming", "defer")
	for {
		if c, err := net.Dial("unix", qmp); err == nil {
			_ = c.Close()
			break
		}
		if time.Since(t0) > 30*time.Second {
			t.Fatalf("%s: the restoring QEMU never opened its QMP socket", s.label)
		}
		time.Sleep(time.Millisecond)
	}
	s.up = time.Since(t0)
	mustRun(t, s.cli, "restore", "--qmp", qmp, "--from", s.state())
	s.running = time.Since(t0)
	s.restoredAccept, s.restoredLogin = s.login(t, t0, "restored")
	stop()
}

// evict drops path from the host's page cache.
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

// scoreSSHTools builds vsockproxy and makes a key to log in with.
func scoreSSHTools(t *testing.T) (proxy, key string) {
	t.Helper()
	dir := t.TempDir()
	proxy = filepath.Join(dir, "vsockproxy")
	if b, err := exec.Command("go", "build", "-trimpath", "-o", proxy, "./vsockproxy").CombinedOutput(); err != nil {
		t.Fatalf("building vsockproxy: %v\n%s", err, b)
	}
	key = filepath.Join(dir, "key")
	mustRun(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	return proxy, key
}

// scoreVariant is a boot that can be logged into from the host, as boot:ssh-first's rows are: the
// key in spin's own directory, owned by spin (sshd's StrictModes refuses one it cannot trust, and
// a refused key reads as a login that never comes), the echo getty, and sshd@.service ordered
// after the image's host key generation, which a release from before v20261113.01 does not order
// itself - its per-connection sshd finds no host keys.
func scoreVariant(key string) variant {
	files := gettyDropin(gettyEcho)
	files["/etc/systemd/system/sshd@.service.d/keygen.conf"] = "[Unit]\nWants=spin-machine-sshd-keygen.service\nAfter=spin-machine-sshd-keygen.service\n"
	setup := `ids=$(awk -F: '$1=="spin"{print $3":"$4}' "$MNT/etc/passwd"); test -n "$ids"
install -d -m 700 -o "${ids%:*}" -g "${ids#*:}" "$MNT/home/spin/.ssh"
install -m 600 -o "${ids%:*}" -g "${ids#*:}" ` + key + `.pub "$MNT/home/spin/.ssh/authorized_keys"
`
	return variant{label: "score", cpus: cmp.Or(os.Getenv("CPUS"), "2"), memory: "2048", files: files, setup: setup}
}

// scoreSSHArgs is `ssh spin@ true` into the guest with context id cid, through vsockproxy.
func scoreSSHArgs(proxy, key string, cid uint32) []string {
	return []string{"-F", "/dev/null", "-i", key, "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=error",
		"-o", "ConnectTimeout=10", "-o", fmt.Sprintf("ProxyCommand=%s %d 22", proxy, cid), "spin@vsock", "true"}
}
