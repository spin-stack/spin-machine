// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
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
// `true` run as spin over SSH is `login`. The client is golang.org/x/crypto/ssh offering only
// mlkem768x25519-sha256, the one key exchange the image's sshd accepts: the runners' OpenSSH
// (9.6 on 2026-11-13) has no ML-KEM and is refused. The console's prompt is timed in the
// same boot, with the getty an echo as boot:bench's baseline has it, so the two read side by
// side.
//
// What `login` holds beyond `listen` is the first login's own chain: sshd's host keys, the key
// exchange, PAM, logind, the user's manager.
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

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key+".pub", xssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}

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
	// nothing: with default dependencies the keygen starts after basic.target, no sooner than the
	// first connection's sshd@ starts it. Each row's edit was checked in its setup, so a row that
	// matched nothing failed rather than measuring the image under another name.
	//
	//
	// Then three image changes, one row each and the three together, 20 boots of each, lab run
	// 43483372488 (2026-11-13), p50/p95 ms of the first login and of accept to login: as built
	// 489/506, 177/194; the host keys generated from the boot's first dispatch 468/494, 154/168;
	// sshd-socket-generator and systemd-run-generator masked 489/501, 180/192; the eleven units
	// of the initial transaction whose condition fails on every boot here masked (the pcr*
	// units, kmod-static-nodes, apparmor, the distribution's sshd-keygen, getty-static,
	// getty@tty1) 484/501, 178/191; all three 469/490, 156/173. Only the keys moved the login,
	// so the image makes them early and the other two stay out. What those two were after:
	// sshd-socket-generator loads libcrypto to find sshd_config at its defaults and is the
	// slowest generator, 3.5-23.5 ms in a guest against ~1.5 for each of systemd's, but the
	// generators run side by side and masking both took their phase from 5.3 to 3.7 ms p50 (a
	// laptop, 8 boots of each); the units are parsed in the gap boot:waits shows after the unit
	// load, which is PID 1 building the initial transaction (see waits_probe_test.go).
	//
	// The row below is written so it holds on a release from before this branch as well as
	// after it, because the lab boots published releases: it puts the image in the state this
	// branch builds - logind in the boot, spin lingering, the host keys made from the boot's
	// first dispatch - whether or not it already was.
	const asThisBranch = `rm -f "$MNT/etc/systemd/system/multi-user.target.wants/systemd-logind.service"
test -e "$MNT/usr/lib/systemd/system/multi-user.target.wants/systemd-logind.service"
install -d "$MNT/var/lib/systemd/linger" && touch "$MNT/var/lib/systemd/linger/spin"
`
	// Lab run 43463472762 (2026-11-13), 20 boots of each, p50 ms from sshd's accept to the
	// first login: as built 177; the host keys generated from the boot's first dispatch rather
	// than by the first connection's sshd@ 155; that and the login's files read into the page
	// cache early 149, with sshd accepting 18 ms later. On a laptop, the vsock socket and sshd@
	// without default dependencies accepted no sooner per login and dropped 7 of 22 first
	// connections. Then lab run 43479854381, 20 of each, first login / a second: the runner's
	// OpenSSH 9.6, which took sntrup761x25519, 489/104; x/crypto/ssh with mlkem768x25519
	// 461/40, and with curve25519 461/39.
	asBuilt := row("as this branch", asThisBranch)
	asBuilt.files["/etc/systemd/system/spin-machine-sshd-keygen.service.d/early.conf"] =
		"[Unit]\nDefaultDependencies=no\nConflicts=shutdown.target\nBefore=shutdown.target\n"
	asBuilt.links = map[string]string{
		"/etc/systemd/system/sysinit.target.wants/spin-machine-sshd-keygen.service": "/usr/lib/systemd/system/spin-machine-sshd-keygen.service",
	}
	rows := []variant{asBuilt}

	type samples struct{ listen, login, accepted, again, prompt []time.Duration }
	got := map[string]*samples{}
	failed, classical := map[string]int{}, map[string]int{}
	for range reps {
		for _, v := range rows {
			r := firstSSH(t, out, v, signer)
			if got[v.label] == nil {
				got[v.label] = &samples{}
			}
			if r.listen == 0 || r.login == 0 {
				failed[v.label]++
				t.Logf("%s: a boot with no login: listen %v, login %v; %s", v.label, r.listen, r.login, r.why)
				continue
			}
			if r.classical {
				classical[v.label]++
			}
			g := got[v.label]
			g.listen, g.login, g.again, g.prompt = append(g.listen, r.listen), append(g.login, r.login), append(g.again, r.again), append(g.prompt, r.prompt)
			g.accepted = append(g.accepted, r.login-r.listen)
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
	fmt.Fprintf(&b, "  %-16s %15s %15s %15s %15s %15s %9s %14s\n", "", "SSHD ACCEPTS", "FIRST LOGIN", "ACCEPT TO LOGIN", "A SECOND TOOK", "CONSOLE PROMPT", "NO LOGIN", "CURVE25519 IN")
	for _, v := range rows {
		g := got[v.label]
		if g == nil {
			g = &samples{}
		}
		fmt.Fprintf(&b, "  %-16s %7s/%-7s %7s/%-7s %7s/%-7s %7s/%-7s %7s/%-7s %9d %14d\n", v.label,
			ms(g.listen, .5), ms(g.listen, .95), ms(g.login, .5), ms(g.login, .95), ms(g.accepted, .5), ms(g.accepted, .95),
			ms(g.again, .5), ms(g.again, .95),
			ms(g.prompt, .5), ms(g.prompt, .95), failed[v.label], classical[v.label])
	}
	t.Log(b.String())
}

type firstSSHRun struct {
	listen, login, prompt time.Duration
	again                 time.Duration // a second login, right after the first: what any login costs
	classical             bool          // a login offering only curve25519 got in
	why                   string
}

func firstSSH(t *testing.T, out string, v variant, signer xssh.Signer) firstSSHRun {
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
	for time.Now().Before(deadline) {
		if _, err := sshLogin(cid, signer, xssh.KeyExchangeMLKEM768X25519); err != nil {
			r.why = err.Error()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		r.login = time.Since(t0)
		if d, err := sshLogin(cid, signer, xssh.KeyExchangeMLKEM768X25519); err == nil {
			r.again = d
		}
		// Whether sshd turns away a client with no ML-KEM, as the image's drop-in has it do. A
		// release from before it lets one in, so this is counted rather than failed.
		_, err := sshLogin(cid, signer, xssh.KeyExchangeCurve25519)
		r.classical = err == nil
		break
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

// sshLogin runs `true` as spin over SSH on cid's port 22, offering only the key exchange kex,
// and says how long it took from the connect.
func sshLogin(cid uint32, signer xssh.Signer, kex string) (time.Duration, error) {
	t0 := time.Now()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("opening a vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: 22}); err != nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("connecting to vsock %d:22: %w", cid, err)
	}
	conn := vsockConn{os.NewFile(uintptr(fd), "vsock")}
	defer conn.Close()
	c, chans, reqs, err := xssh.NewClientConn(conn, "vsock", &xssh.ClientConfig{
		User:            "spin",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), // a fresh VM's keys, made in its boot
		Config:          xssh.Config{KeyExchanges: []string{kex}},
	})
	if err != nil {
		return 0, fmt.Errorf("ssh to vsock %d:22: %w", cid, err)
	}
	client := xssh.NewClient(c, chans, reqs)
	defer client.Close()
	s, err := client.NewSession()
	if err != nil {
		return 0, fmt.Errorf("opening a session on vsock %d:22: %w", cid, err)
	}
	defer s.Close()
	if err := s.Run("true"); err != nil {
		return 0, fmt.Errorf("running true on vsock %d:22: %w", cid, err)
	}
	return time.Since(t0), nil
}

// vsockConn is a connected vsock socket as the net.Conn x/crypto/ssh takes: net.FileConn
// knows no AF_VSOCK address, and nothing here asks for one.
type vsockConn struct{ *os.File }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

func (vsockConn) LocalAddr() net.Addr  { return vsockAddr{} }
func (vsockConn) RemoteAddr() net.Addr { return vsockAddr{} }
