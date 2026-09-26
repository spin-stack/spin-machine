// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Firmware A/B, against a caller-supplied diagnostic initrd.
//
// The firmware is ~10 ms of vCPU 0's work in a boot that reaches a login in a few hundred,
// so this measures a small line item and says so: what it is for is deciding whether a
// firmware can run this machine at all, and only then what it saves.
//
// qboot cannot, as it stands. Its ACPI linker-loader implements ALLOCATE, ADD_POINTER and
// ADD_CHECKSUM and stops on anything else, and vmgenid needs WRITE_POINTER to tell QEMU
// where the GUID lives. The symptom is a vCPU halted inside the firmware with not one byte
// on the console, not even earlyprintk — which is why the stock variant is booted here
// deliberately and its timeout is the assertion, not a failure.
//
// The inputs are not release artefacts and nothing here builds them:
//
//	SPIN_FIRMWARE_PROBE=1        run at all
//	SPIN_PROBE_INITRD=<cpio>     a diagnostic initrd; see below
//	REPS=<n>                     boots per firmware (default 20)
//
// The two qboot binaries are read from _output/qboot/, where `task qemu:qboot` writes them,
// and SPIN_QBOOT_STOCK and SPIN_QBOOT_PATCHED override that. Defaulting rather than
// requiring them removes a trap worth naming: a relative path in either variable resolves
// against this package's directory and not the repository root, so `_output/qboot/...` on
// the command line looked right and pointed at boot/_output.
//
// The initrd is diagnostic input because what runs as PID 1 is not this repository's
// business. Its /init must mount proc, print any dmesg line matching vmgenid, print
// SPIN-READY, stay alive, and keep printing new dmesg output so a reseed after restore is
// visible. Its own CPU and memory work lands inside every number here, so the same initrd
// has to be used for both variants or the comparison is between two initrds.
func TestFirmwareCost(t *testing.T) {
	if os.Getenv("SPIN_FIRMWARE_PROBE") == "" {
		t.Skip("set SPIN_FIRMWARE_PROBE=1: this boots dozens of VMs and needs firmware built outside the release")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 20)

	bios := map[string]string{
		"seabios": filepath.Join(out, "qemu", "bios-256k.bin"),
		"stock":   firmwareFile(t, "SPIN_QBOOT_STOCK", filepath.Join(out, "qboot", "stock", "bios.bin")),
		"patched": firmwareFile(t, "SPIN_QBOOT_PATCHED", filepath.Join(out, "qboot", "patched", "bios.bin")),
	}
	p := &probe{
		out:      out,
		firmware: filepath.Join(out, "qemu"),
		kernel:   filepath.Join(out, "kernel", "vmlinux"),
		initrd:   envFile(t, "SPIN_PROBE_INITRD"),
		bios:     bios,
		dir:      t.TempDir(),
	}

	// One: the machine boots this firmware at all, with vmgenid off. A firmware that cannot
	// do this is not slow, it is broken, and separating the two is why there are three boots
	// before any timing.
	if ms := p.mustReach(t, "stock", noVMGenID, "SPIN-READY"); ms > 0 {
		t.Logf("stock qboot, no vmgenid: %.1f ms to SPIN-READY", ms)
	}

	// Two: the stock firmware is expected to hang once vmgenid is on. Asserted, because a
	// stock qboot that booted would mean the WRITE_POINTER story is wrong and every number
	// below is measuring something else.
	if ms, err := p.reach(t, "stock", withVMGenID, "SPIN-READY", 3*time.Second); err == nil {
		t.Fatalf("stock qboot booted with vmgenid in %.1f ms; it implements no WRITE_POINTER, "+
			"so either the firmware is not the one described or the machine no longer asks "+
			"for vmgenid", ms)
	}
	t.Log("stock qboot, vmgenid on: no console output, as expected")

	// Three: the patched firmware boots, publishes the table, and survives a restore with
	// the guest noticing. The reseed is the point of vmgenid; a firmware that boots and
	// loses it is worse than one that does not boot, because nothing says so.
	p.mustPublishVMGenID(t)

	// Only now, the timing. Interleaved for the reason TestBootCost is: a block schedule
	// hands one variant a slow minute and reads it as a difference.
	samples := map[string][]float64{}
	order := []string{"seabios", "patched"}
	for range reps {
		for _, v := range order {
			ms, err := p.reach(t, v, withVMGenID, "SPIN-READY", 8*time.Second)
			if err != nil {
				t.Fatalf("%s: %v", v, err)
			}
			samples[v] = append(samples[v], ms)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nmilliseconds from the moment before QEMU is exec'd, p50/p95 over %d boots\n\n", reps)
	fmt.Fprintf(&b, "%-10s %10s %10s\n", "FIRMWARE", "P50", "P95")
	for _, v := range order {
		s := append([]float64(nil), samples[v]...)
		sort.Float64s(s)
		fmt.Fprintf(&b, "%-10s %10.2f %10.2f\n", v, pct(s, 50), pct(s, 95))
	}
	// The difference, stated rather than left to the reader: it is the number the decision
	// turns on, and it is small enough that a reader who has to subtract will round it up.
	sb, pa := append([]float64(nil), samples["seabios"]...), append([]float64(nil), samples["patched"]...)
	sort.Float64s(sb)
	sort.Float64s(pa)
	fmt.Fprintf(&b, "\np50 reduction: %.2f ms\n", pct(sb, 50)-pct(pa, 50))
	t.Log(b.String())
}

const (
	withVMGenID = true
	noVMGenID   = false
)

type probe struct {
	out, firmware, kernel, initrd, dir string
	bios                               map[string]string
	n                                  int
}

// vm is one QEMU, and the socket paths are numbered because a probe starts dozens and a
// reused path is a QMP connection to the previous one's corpse.
type vm struct {
	cmd    *exec.Cmd
	stdout *os.File
	out    []byte
	t0     time.Time
	qmp    string
}

func (p *probe) start(t *testing.T, variant string, gen bool, incoming string) *vm {
	t.Helper()
	p.n++
	qmp := filepath.Join(p.dir, fmt.Sprintf("qmp-%d.sock", p.n))

	// The machine line is the probe's own, not machine.Spec's: this compares firmware, so
	// the disks, NICs and vsock a real machine carries are absent on purpose — every device
	// is work inside the interval, and work that is identical in both variants only adds
	// variance. It is also why these numbers are not a machine's boot time.
	args := []string{
		"-L", p.firmware,
		"-machine", "q35,sata=off,smbus=off",
		"-accel", "kvm", "-cpu", "host",
		"-m", "2048", "-smp", "2",
		"-nodefaults", "-display", "none", "-serial", "stdio", "-monitor", "none",
		"-qmp", "unix:" + qmp + ",server=on,wait=off",
		"-bios", p.bios[variant],
		"-kernel", p.kernel,
		"-initrd", p.initrd,
		"-append", "console=ttyS0 quiet loglevel=3 pci=lastbus=0 no_timer_check " +
			"tsc=reliable rcupdate.rcu_expedited=1 TERM=dumb rdinit=/init",
	}
	if gen {
		args = append(args, "-device", "vmgenid,guid=auto")
	}
	if incoming != "" {
		args = append(args, "-incoming", "file:"+incoming)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("console pipe: %v", err)
	}
	cmd := exec.Command(filepath.Join(p.out, "bin", "qemu-system-x86_64"), args...)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// t0 before Start, so exec'ing QEMU and the firmware are both inside the interval. They
	// are the two things this test exists to compare.
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching QEMU: %v", err)
	}
	_ = w.Close()
	return &vm{cmd: cmd, stdout: r, t0: t0, qmp: qmp}
}

// wait reads the console until marker appears, and returns when it did. A deadline on the
// descriptor rather than a goroutine and a channel: there is one reader, and it is this one.
func (v *vm) wait(marker string, timeout time.Duration) (float64, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 65536)
	for {
		if err := v.stdout.SetReadDeadline(deadline); err != nil {
			return 0, fmt.Errorf("setting the console deadline: %w", err)
		}
		n, err := v.stdout.Read(buf)
		v.out = append(v.out, buf[:n]...)
		if strings.Contains(string(v.out), marker) {
			return float64(time.Since(v.t0).Microseconds()) / 1000, nil
		}
		if err != nil {
			return 0, fmt.Errorf("never saw %q in %s; console was:\n%s",
				marker, timeout, tail(v.out, 800))
		}
	}
}

func (v *vm) close() {
	if pgid, err := syscall.Getpgid(v.cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = v.cmd.Wait()
	_ = v.stdout.Close()
}

func (p *probe) reach(t *testing.T, variant string, gen bool, marker string, timeout time.Duration) (float64, error) {
	t.Helper()
	v := p.start(t, variant, gen, "")
	defer v.close()
	return v.wait(marker, timeout)
}

func (p *probe) mustReach(t *testing.T, variant string, gen bool, marker string) float64 {
	t.Helper()
	ms, err := p.reach(t, variant, gen, marker, 8*time.Second)
	if err != nil {
		t.Fatalf("%s firmware (vmgenid=%v): %v", variant, gen, err)
	}
	return ms
}

// mustPublishVMGenID boots the patched firmware, saves the machine, restores it into a new
// QEMU with a fresh GUID, and requires the guest to say it noticed.
//
// The reseed is the whole reason the device is on the machine's command line: two guests
// restored from one template share the template's memory, and that memory contains the
// state of the random pool. A firmware that boots and quietly fails to publish the table
// produces two VMs generating the same "random" bytes, and nothing on either of them
// reports a fault.
func (p *probe) mustPublishVMGenID(t *testing.T) {
	t.Helper()
	v := p.start(t, "patched", withVMGenID, "")
	ms, err := v.wait("SPIN-READY", 8*time.Second)
	if err != nil {
		v.close()
		t.Fatalf("patched qboot: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(v.out)), "vmgenid") {
		v.close()
		t.Fatalf("patched qboot booted in %.1f ms but the guest saw no VMGENID table; "+
			"console was:\n%s", ms, tail(v.out, 800))
	}
	t.Logf("patched qboot, vmgenid on: %.1f ms to SPIN-READY, table present", ms)

	state := filepath.Join(p.dir, "state")
	q := dial(t, v.qmp, v)
	q.do(t, "stop", nil)
	q.do(t, "migrate", map[string]any{"uri": "file:" + state})
	q.until(t, "query-migrate", "completed", 20*time.Second)
	q.close()
	v.close()

	r := p.start(t, "patched", withVMGenID, state)
	defer r.close()
	q = dial(t, r.qmp, r)
	defer q.close()
	// cont only once the incoming migration is done: a cont into a machine still loading
	// state is a race that passes most of the time.
	q.untilNot(t, "query-status", "inmigrate", 20*time.Second)
	q.do(t, "cont", nil)
	if _, err := r.wait("crng reseeded due to virtual machine fork", 20*time.Second); err != nil {
		t.Fatalf("the restored guest never reseeded, so the GUID did not reach it "+
			"— every VM restored from one template would share its random pool: %v", err)
	}
	t.Log("restored guest reseeded: the WRITE_POINTER path carries the GUID")
}

// A QMP client small enough to read: connect, swallow the greeting, negotiate, and send
// commands one at a time. No pipelining, because every call here is followed by a decision.
type qmp struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

func dial(t *testing.T, socket string, v *vm) *qmp {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			q := &qmp{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
			var greeting struct {
				QMP *struct{} `json:"QMP"`
			}
			if err := q.dec.Decode(&greeting); err != nil {
				t.Fatalf("reading the QMP greeting: %v\n\nQEMU said:\n%s", err, tail(v.out, 800))
			}
			q.do(t, "qmp_capabilities", nil)
			return q
		}
		if time.Now().After(deadline) {
			t.Fatalf("QEMU never answered on %s: %v\n\nQEMU said:\n%s",
				filepath.Base(socket), err, tail(v.out, 800))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (q *qmp) do(t *testing.T, cmd string, args map[string]any) json.RawMessage {
	t.Helper()
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	if err := q.enc.Encode(req); err != nil {
		t.Fatalf("sending %s: %v", cmd, err)
	}
	for {
		var reply struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
			Event string `json:"event"`
		}
		if err := q.dec.Decode(&reply); err != nil {
			t.Fatalf("reading the reply to %s: %v", cmd, err)
		}
		if reply.Error != nil {
			t.Fatalf("%s was refused: %s", cmd, reply.Error.Desc)
		}
		// Events arrive interleaved with replies, and a migration emits several.
		if reply.Event != "" {
			continue
		}
		return reply.Return
	}
}

// until polls cmd until its "status" field reads want, and fails on "failed" rather than
// waiting out the deadline: a migration that has already given up has nothing to say later.
func (q *qmp) until(t *testing.T, cmd, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var s struct{ Status string }
		if err := json.Unmarshal(q.do(t, cmd, nil), &s); err != nil {
			t.Fatalf("reading %s: %v", cmd, err)
		}
		if s.Status == want {
			return
		}
		if s.Status == "failed" {
			t.Fatalf("%s reported failed while waiting for %s", cmd, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed %q for %s, waiting for %q", cmd, s.Status, timeout, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (q *qmp) untilNot(t *testing.T, cmd, unwanted string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var s struct{ Status string }
		if err := json.Unmarshal(q.do(t, cmd, nil), &s); err != nil {
			t.Fatalf("reading %s: %v", cmd, err)
		}
		if s.Status != unwanted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed %q for %s", cmd, unwanted, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (q *qmp) close() { _ = q.conn.Close() }

// firmwareFile takes the override if there is one and the built firmware otherwise, and
// skips rather than failing when neither is there: this compares firmware no release carries,
// so a checkout without it is not a broken checkout.
func firmwareFile(t *testing.T, name, dflt string) string {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		return envFile(t, name)
	}
	if _, err := os.Stat(dflt); err != nil {
		t.Skipf("no %s — run: task qemu:qboot (or set %s)", dflt, name)
	}
	return dflt
}

// envFile fails rather than skipping: a path given explicitly and not there is a mistake in
// the invocation, and skipping would hide it.
func envFile(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is unset; this probe compares firmware that no release carries", name)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		t.Fatalf("resolving %s=%q: %v", name, v, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("%s=%s: %v\n(a relative path here resolves against %s, not the repository root)",
			name, abs, err, mustCwd(t))
	}
	return abs
}

func mustCwd(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return d
}

// pct is nearest-rank on an already sorted slice, matching boot.Percentile so the two
// harnesses cannot disagree about what a p95 is.
func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := (p*len(sorted) + 99) / 100
	if i < 1 {
		i = 1
	}
	return sorted[i-1]
}
