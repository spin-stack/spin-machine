// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/spin-machine/boot"
)

// When a workspace can first be logged into, from the host, over SSH - the thing a workspace
// waits for, where boot:bench's `usable` is the serial console's prompt, which nothing a
// workspace does waits for.
//
// The machine gets a vsock device (--vsock-cid), and the image's systemd-ssh-generator binds
// sshd to AF_VSOCK port 22 when it finds one. From the moment before QEMU is exec'd the host
// tries to connect every few milliseconds; the first connection accepted is `listen`, and then
// one `ssh spin@ true` through boot/vsockproxy is `login`. The console's prompt is timed in the
// same boot, with the getty an echo as boot:bench's baseline has it, so the two read side by
// side.
//
// What `login` holds beyond `listen` is the first login's own chain, which boot:jobs records
// from inside: sshd's host keys generated, the key exchange, PAM, logind, the user's manager.
//
//	SPIN_SSH_FIRST=1  run at all
//	REPS=<n>          boots of each row (default 10)
//	CPUS=<n>          vCPUs (default 2)
func TestFirstSSH(t *testing.T) {
	if os.Getenv("SPIN_SSH_FIRST") == "" {
		t.Skip("set SPIN_SSH_FIRST=1: boots VMs with a vsock device and logs into them")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a key into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	if _, err := os.Stat("/dev/vhost-vsock"); err != nil {
		t.Skip("no /dev/vhost-vsock: run task vsock")
	}
	reps := envInt(t, "REPS", 10)

	dir := t.TempDir()
	proxy := filepath.Join(dir, "vsockproxy")
	build := exec.Command("go", "build", "-trimpath", "-o", proxy, "./vsockproxy")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building vsockproxy: %v\n%s", err, b)
	}
	key := filepath.Join(dir, "key")
	mustRun(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)

	// Every row has the key, the echo getty, and sshd@.service ordered after the image's host
	// key generation. The image carries that drop-in itself since 2026-11-12; it is written
	// here as well because the lab boots published releases, and one from before it cannot be
	// logged into over vsock at all - the per-connection sshd finds no host keys.
	keySetup := `ids=$(awk -F: '$1=="spin"{print $3":"$4}' "$MNT/etc/passwd"); test -n "$ids"
install -d -m 700 -o "${ids%:*}" -g "${ids#*:}" "$MNT/home/spin/.ssh"
install -m 600 -o "${ids%:*}" -g "${ids#*:}" ` + key + `.pub "$MNT/home/spin/.ssh/authorized_keys"
`
	row := func(label, setup string) variant {
		files := gettyDropin(gettyEcho)
		files["/etc/systemd/system/sshd@.service.d/keygen.conf"] = "[Unit]\nWants=spin-machine-sshd-keygen.service\nAfter=spin-machine-sshd-keygen.service\n"
		// The key in spin's own directory, owned by spin: sshd's StrictModes refuses one it
		// cannot trust, and a refused key reads as a login that never comes.
		return variant{label: label, cpus: cmp.Or(os.Getenv("CPUS"), "2"), memory: "2048", files: files, setup: keySetup + setup}
	}
	// Rows measured against "as built" and dropped once they had answered, on the bench fleet,
	// 10 boots of each interleaved, lab run 43308825692 (2026-11-12), p50/p95 ms of the first
	// login: as built then 570/626; pam_motd off 558/588; logind back in the boot 558/593;
	// logind in the boot and spin lingering 510/525, with sshd accepting no later. The image
	// now has no motd and lingers. Then, lab run 43312419007, 15 boots of each: the host keys
	// generated in the boot (the keygen wanted by multi-user.target) 518/542 against 514/531 -
	// nothing, so the keys stay with the first sshd that needs them. Each row's edit was checked in its setup, so a row that
	// matched nothing failed rather than measuring the image under another name.
	//
	// The rows below are written so they hold on a release from before this branch as well as
	// after it, because the lab boots published releases: each puts the image in the state this
	// branch builds - logind in the boot, spin lingering - whether or not it already was.
	const asThisBranch = `rm -f "$MNT/etc/systemd/system/multi-user.target.wants/systemd-logind.service"
test -e "$MNT/usr/lib/systemd/system/multi-user.target.wants/systemd-logind.service"
install -d "$MNT/var/lib/systemd/linger" && touch "$MNT/var/lib/systemd/linger/spin"
`
	rows := []variant{
		row("as this branch", asThisBranch),
	}

	type samples struct{ listen, login, again, prompt []time.Duration }
	got := map[string]*samples{}
	failed := map[string]int{}
	for range reps {
		for _, v := range rows {
			r := firstSSH(t, out, v, proxy, key)
			if got[v.label] == nil {
				got[v.label] = &samples{}
			}
			if r.listen == 0 || r.login == 0 {
				failed[v.label]++
				t.Logf("%s: a boot with no login: listen %v, login %v; %s", v.label, r.listen, r.login, r.why)
				continue
			}
			g := got[v.label]
			g.listen, g.login, g.again, g.prompt = append(g.listen, r.listen), append(g.login, r.login), append(g.again, r.again), append(g.prompt, r.prompt)
		}
	}
	ms := func(ds []time.Duration, q float64) string {
		d, ok := boot.Percentile(ds, q)
		if !ok {
			return "—"
		}
		return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nms from the moment before QEMU is exec'd, p50/p95 over %d boots of each, interleaved, %s vCPUs\n\n", reps, rows[0].cpus)
	fmt.Fprintf(&b, "  %-16s %15s %15s %15s %15s %9s\n", "", "SSHD ACCEPTS", "FIRST LOGIN", "A SECOND TOOK", "CONSOLE PROMPT", "NO LOGIN")
	for _, v := range rows {
		g := got[v.label]
		if g == nil {
			g = &samples{}
		}
		fmt.Fprintf(&b, "  %-16s %7s/%-7s %7s/%-7s %7s/%-7s %7s/%-7s %9d\n", v.label,
			ms(g.listen, .5), ms(g.listen, .95), ms(g.login, .5), ms(g.login, .95), ms(g.again, .5), ms(g.again, .95),
			ms(g.prompt, .5), ms(g.prompt, .95), failed[v.label])
	}
	t.Log(b.String())
}

type firstSSHRun struct {
	listen, login, prompt time.Duration
	again                 time.Duration // a second login, right after the first: what any login costs
	why                   string
}

func firstSSH(t *testing.T, out string, v variant, proxy, key string) firstSSHRun {
	t.Helper()
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	mustRun(t, filepath.Join(out, "bin", "qemu-img"), "create", "-f", "qcow2",
		"-F", "qcow2", "-b", filepath.Join(out, "image", "rootfs.qcow2"), overlay)
	editOverlay(t, overlay, v)

	// Context ids are the host's: two machines with one fail to start, so this one's is drawn
	// from a range nothing else here uses.
	cid := uint32(100000 + rand.IntN(1<<20))
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), "boot", "--release", out,
		"--disk", overlay, "--memory", v.memory, "--cpus", v.cpus,
		"--vsock-cid", fmt.Sprint(cid),
		"--console", "file:/dev/stdout", "--append", "init=/sbin/init")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	defer func() {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()

	var mu sync.Mutex
	var r firstSSHRun
	go func() {
		run, _ := boot.Watch(stdout, t0, time.Now, boot.Usable)
		mu.Lock()
		r.prompt = run.At[boot.Usable]
		mu.Unlock()
	}()

	deadline := t0.Add(60 * time.Second)
	for time.Now().Before(deadline) && r.listen == 0 {
		if vsockAccepts(cid, 22) {
			r.listen = time.Since(t0)
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if r.listen == 0 {
		r.why = "sshd never accepted on vsock; QEMU: " + strings.TrimSpace(stderr.String())
		return r
	}
	ssh := []string{"-F", "/dev/null", "-i", key, "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=error",
		"-o", "ConnectTimeout=10", "-o", fmt.Sprintf("ProxyCommand=%s %d 22", proxy, cid), "spin@vsock", "true"}
	for time.Now().Before(deadline) {
		b, err := exec.Command("ssh", ssh...).CombinedOutput()
		if err == nil {
			r.login = time.Since(t0)
			break
		}
		r.why = fmt.Sprintf("ssh: %v: %s", err, strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	if r.login != 0 {
		t1 := time.Now()
		if exec.Command("ssh", ssh...).Run() == nil {
			r.again = time.Since(t1)
		}
	}
	// The prompt is usually there by now; it is waited for so the row is not empty.
	for time.Now().Before(deadline) {
		mu.Lock()
		p := r.prompt
		mu.Unlock()
		if p != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return r
}

// vsockAccepts says whether something accepts a connection on cid:port now. The connect
// timeout is short because a machine not yet started is the common answer, and the default
// is two seconds of not knowing.
func vsockAccepts(cid, port uint32) bool {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	tv := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_CONNECT_TIMEOUT, &tv)
	return unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}) == nil
}
