// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
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

// TestRestoreFirstRequest measures what a restored template takes to do its first piece of
// work, and not only to start: a restore that reports running in ~25 ms has mapped the
// template's memory and read none of it, so every page the guest then touches is a fault on
// the host — cheap while the template is in the host's page cache, a read from the disk once
// it is not.
//
// Three ways a machine meets its memory, each a fresh restore from one template:
//
//   - warm: the template file is in the host's page cache, which is where a template that
//     restores often lives. The first request still faults every page it touches into this
//     QEMU's mappings; those are minor faults.
//   - cold: the template file was evicted before the restore, which is a template nobody has
//     restored for a while on a host with memory pressure.
//   - reclaimed: restored cold, one request, then the VM's own cgroup reclaimed while it sat
//     idle — the case of a host that overcommits memory and a guest whose requests are
//     sparse. The request after the reclaim is the number to read.
//
// What the guest runs is diagnostic input, supplied here and never shipped: the kernel starts
// /bin/sh on the console, the host types into it, and the "request" is `cat` over a working
// set the guest wrote into tmpfs before it was frozen. So the request's pages are guest RAM,
// which on the host is the template file mapped private. Each result line carries the guest's
// own measure of the work next to the host's of the whole round trip, and the host counts the
// major faults QEMU took while it ran.
//
// Read the guest's column as the cat alone, not as where the disk time went. Cold, the
// template's residency goes from 18 to 78 MiB across the first request, in ~11 major faults:
// readahead brings the working set in with whichever fault comes first — the shell waking,
// the fork of date — so the cat mostly finds it cached (measured 2026-09-28, 512 MiB guest,
// 64 MiB working set, NVMe).
//
// Guest writes after the restore are private copies, anonymous memory, and only swap can
// reclaim them. They are not what this measures: the working set is only read.
//
//	SPIN_RESTORE_BENCH=1   run at all
//	REPS=<n>               restores per case (default 10)
//	WS_MIB=<n>             the guest's working set (default 64)
//	MEMORY_MIB=<n>         guest RAM, and the template's size (default 1024)
//
// Needs /dev/kvm, a built release, a systemd user manager that delegates the memory
// controller (for the reclaim), and a disk under _output that is not tmpfs: a page of tmpfs
// has nowhere to be evicted to.
func TestRestoreFirstRequest(t *testing.T) {
	if os.Getenv("SPIN_RESTORE_BENCH") == "" {
		t.Skip("set SPIN_RESTORE_BENCH=1: this restores dozens of VMs and evicts a file from the host's page cache")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 10)
	wsMiB := envInt(t, "WS_MIB", 64)
	memMiB := envInt(t, "MEMORY_MIB", 1024)
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "true").Run(); err != nil {
		t.Skipf("systemd-run --user --scope: %v; the reclaimed case needs a cgroup of the VM's own", err)
	}

	work, err := os.MkdirTemp(out, "restorebench-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	var fs syscall.Statfs_t
	if err := syscall.Statfs(work, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == 0x01021994 { // TMPFS_MAGIC
		t.Skipf("%s is tmpfs: a template there cannot be evicted, so there is no cold case", work)
	}

	b := &restoreBench{out: out, dir: work, memMiB: memMiB}
	b.freeze(t, wsMiB)

	cases := []string{"warm", "cold", "reclaimed"}
	res := map[string]*restoreSamples{}
	for _, c := range cases {
		res[c] = &restoreSamples{}
	}
	// Interleaved, as TestBootCost is: a block schedule hands one case a slow minute and
	// reports it as a difference.
	for range reps {
		for _, c := range cases {
			b.run(t, c, res[c])
		}
	}

	var r strings.Builder
	fmt.Fprintf(&r, "\n%d MiB guest, %d MiB working set, %d restores per case; p50 / p95 in ms, host round trip (guest's own measure) [QEMU major faults, p50]\n\n",
		memMiB, wsMiB, reps)
	fmt.Fprintf(&r, "%-10s %-16s %-30s %-30s\n", "CASE", "RESTORE", "FIRST REQUEST", "SECOND REQUEST")
	for _, c := range cases {
		s := res[c]
		fmt.Fprintf(&r, "%-10s %-16s %-30s %-30s\n", c, p50p95(s.restore), s.req[0].String(), s.req[1].String())
	}
	fmt.Fprintf(&r, "\ntemplate resident in the host's page cache, p50 MiB: ")
	for _, c := range cases {
		fmt.Fprintf(&r, "%s %s after the restore, %s after the first request; ", c,
			fmtMiB(s50(res[c].residentRestored)), fmtMiB(s50(res[c].residentServed)))
	}
	fmt.Fprintf(&r, "\nreclaimed: the first request ran before the reclaim, the second after it; "+
		"the VM's cgroup held %s MiB before and %s MiB after (p50)\n",
		fmtMiB(s50(res["reclaimed"].before)), fmtMiB(s50(res["reclaimed"].after)))
	t.Log(r.String())
}

type restoreBench struct {
	out, dir string
	memMiB   int
	n        int
}

type restoreSamples struct {
	restore       []float64
	req           [2]request
	before, after []float64
	// How much of the template is in the host's page cache once the VM is running, and once
	// it has served one request: where the reads a cold restore makes actually happen.
	residentRestored, residentServed []float64
}

// request is one column: the host's round trip, the guest's own timing of the same work, and
// the major faults QEMU took in between.
type request struct{ host, guest, majflt []float64 }

func (q request) String() string {
	return fmt.Sprintf("%s (%s) [%.0f]", p50p95(q.host), p50p95(q.guest), s50(q.majflt))
}

func (q *request) add(host, guest, majflt float64) {
	q.host = append(q.host, host)
	q.guest = append(q.guest, guest)
	q.majflt = append(q.majflt, majflt)
}

func (b *restoreBench) template() string { return filepath.Join(b.dir, "template.mem") }
func (b *restoreBench) state() string    { return filepath.Join(b.dir, "template.state") }

// freeze boots the one machine every case restores from, has it write its working set, and
// takes the template.
func (b *restoreBench) freeze(t *testing.T, wsMiB int) {
	t.Helper()
	f, err := os.Create(b.template())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(int64(b.memMiB) << 20); err != nil {
		t.Fatal(err)
	}
	v := b.start(t, false, "--memory-file", b.template(), "--memory-share")
	defer v.stop()
	con := v.console(t)
	defer func() { _ = con.Close() }()

	// The prompt comes when the shell does; until then, what is typed is lost or echoed by
	// a tty nobody reads. Asked until it answers.
	ready := regexp.MustCompile(`UP`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := con.say("echo U''P", ready, 500*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guest's shell never answered on the console, which ends:\n%s", tail([]byte(con.seen.String()), 2000))
		}
	}
	// Root is the base image read-only; /tmp is made writable in guest memory. urandom and
	// not zeros, so that nothing on either side can share or skip the pages.
	setup := fmt.Sprintf("mount -t tmpfs -o size=%dm none /tmp && dd if=/dev/urandom of=/tmp/ws bs=1M count=%d 2>/dev/null && echo WS''-OK", wsMiB+8, wsMiB)
	if _, err := con.say(setup, regexp.MustCompile(`WS-OK`), time.Minute); err != nil {
		t.Fatalf("writing the working set: %v", err)
	}
	b.spin(t, "save", "--template", "--qmp", v.qmp, "--to", b.state())
	// The source mapped the template shared and wrote it; those pages are dirty until they
	// reach the disk, and a dirty page is one no eviction can drop.
	if err := f.Sync(); err != nil {
		t.Fatalf("syncing %s: %v", b.template(), err)
	}
}

// run restores once in the named case and records what it saw.
func (b *restoreBench) run(t *testing.T, c string, s *restoreSamples) {
	t.Helper()
	switch c {
	case "warm":
		readAll(t, b.template())
	case "cold", "reclaimed":
		evict(t, b.template())
		// Checked, not assumed: fadvise is advice, and a page it declined to drop turns
		// a cold row into a warm one without a word.
		if mib := residentMiB(t, b.template()); mib > 1 {
			t.Fatalf("%s: %.0f MiB of the template still resident after eviction", c, mib)
		}
	}

	v := b.start(t, true, "--memory-file", b.template(), "--incoming", "defer")
	defer v.stop()
	t0 := time.Now()
	b.spin(t, "restore", "--template", "--qmp", v.qmp, "--from", b.state())
	s.restore = append(s.restore, ms(time.Since(t0)))
	s.residentRestored = append(s.residentRestored, residentMiB(t, b.template()))

	con := v.console(t)
	defer func() { _ = con.Close() }()
	s.req[0].add(v.request(t, con))
	s.residentServed = append(s.residentServed, residentMiB(t, b.template()))
	if c == "reclaimed" {
		s.before = append(s.before, v.memoryMiB(t))
		v.reclaim(t)
		s.after = append(s.after, v.memoryMiB(t))
	}
	s.req[1].add(v.request(t, con))
}

// restoreVM is one QEMU, in a systemd scope of its own so that its memory can be reclaimed
// without touching anybody else's.
type restoreVM struct {
	cmd              *exec.Cmd
	qmp, consoleSock string
}

func (b *restoreBench) start(t *testing.T, restored bool, extra ...string) *restoreVM {
	t.Helper()
	b.n++
	v := &restoreVM{
		qmp:         filepath.Join(b.dir, fmt.Sprintf("q%d.sock", b.n)),
		consoleSock: filepath.Join(b.dir, fmt.Sprintf("c%d.sock", b.n)),
	}
	args := []string{"--user", "--scope", "--quiet", "--collect", "--",
		filepath.Join(b.out, "bin", "spin-machine"), "boot", "--release", b.out,
		"--memory", strconv.Itoa(b.memMiB), "--cpus", "1", "--disk-readonly",
		"--qmp", v.qmp, "--console", "unix:" + v.consoleSock + ",server=on,wait=off",
		"--init", "/bin/sh"}
	v.cmd = exec.Command("systemd-run", append(args, extra...)...)
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	v.cmd.Stderr = &stderr
	if err := v.cmd.Start(); err != nil {
		t.Fatalf("starting a VM: %v", err)
	}
	t.Cleanup(v.stop)
	deadline := time.Now().Add(10 * time.Second)
	for _, sock := range []string{v.qmp, v.consoleSock} {
		for {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no socket at %s after 10s; QEMU said:\n%s", sock, stderr.String())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	return v
}

func (v *restoreVM) stop() {
	if v.cmd.Process != nil {
		_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
		_ = v.cmd.Wait()
	}
}

// pid is QEMU's: systemd-run --scope execs its command in place, and spin-machine boot
// execs QEMU, so the process started is the process that runs the machine.
func (v *restoreVM) pid() int { return v.cmd.Process.Pid }

var doneLine = regexp.MustCompile(`DONE (\d+)`)

// request is the work the guest does once restored: read its working set and report how
// long that took by its own clock. The marker is split in what is typed so that the tty's
// echo of the command cannot be mistaken for its answer.
func (v *restoreVM) request(t *testing.T, con *consoleConn) (host, guest, majflt float64) {
	t.Helper()
	const cmd = `t0=$(date +%s%N); cat /tmp/ws >/dev/null; echo DO''NE $(( ($(date +%s%N) - t0) / 1000 ))`
	f0 := majorFaults(t, v.pid())
	t0 := time.Now()
	m, err := con.say(cmd, doneLine, time.Minute)
	if err != nil {
		t.Fatalf("the request: %v", err)
	}
	host = ms(time.Since(t0))
	us, _ := strconv.Atoi(m[1])
	return host, float64(us) / 1000, float64(majorFaults(t, v.pid()) - f0)
}

// cgroup is the directory of the VM's scope in the unified hierarchy.
func (v *restoreVM) cgroup(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", v.pid()))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	path, ok := strings.CutPrefix(line, "0::")
	if !ok {
		t.Fatalf("not a cgroup v2 path: %q", line)
	}
	return filepath.Join("/sys/fs/cgroup", path)
}

func (v *restoreVM) memoryMiB(t *testing.T) float64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v.cgroup(t), "memory.current"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		t.Fatal(err)
	}
	return n / (1 << 20)
}

// reclaim asks the kernel to take back the file pages the VM's cgroup holds: the template's,
// which is the memory this measures. Anonymous memory, the guest's writes since the restore,
// only swap can take, and it is not the question.
//
// Asked for exactly what memory.stat reports as file, with swappiness=0, and not for
// everything the cgroup holds. Asked for more than it can reclaim, the write did not fail:
// it sat in the kernel for minutes, reclaiming a page at a time from an idle guest, which
// hung this test twice (2026-09-28). EAGAIN, when it falls short, is expected; what it did
// reclaim is read from memory.current, not from the return.
func (v *restoreVM) reclaim(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v.cgroup(t), "memory.stat"))
	if err != nil {
		t.Fatal(err)
	}
	var file int64
	for l := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(l, "file "); ok {
			file, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	want := fmt.Sprintf("%d swappiness=0", file)
	err = os.WriteFile(filepath.Join(v.cgroup(t), "memory.reclaim"), []byte(want), 0)
	if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EIO) {
		t.Fatalf("reclaiming the VM's memory: %v", err)
	}
}

func (v *restoreVM) console(t *testing.T) *consoleConn {
	t.Helper()
	c, err := net.Dial("unix", v.consoleSock)
	if err != nil {
		t.Fatalf("connecting to the console: %v", err)
	}
	return &consoleConn{c: c, r: bufio.NewReader(c)}
}

type consoleConn struct {
	c net.Conn
	r *bufio.Reader
	// seen is everything the console printed, for the failure that has to say why.
	seen strings.Builder
}

func (c *consoleConn) Close() error { return c.c.Close() }

// say types one line and reads the console until a line matches want.
func (c *consoleConn) say(line string, want *regexp.Regexp, timeout time.Duration) ([]string, error) {
	if err := c.c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	defer func() { _ = c.c.SetDeadline(time.Time{}) }()
	if _, err := c.c.Write([]byte(line + "\n")); err != nil {
		return nil, err
	}
	for {
		l, err := c.r.ReadString('\n')
		c.seen.WriteString(l)
		if m := want.FindStringSubmatch(l); m != nil {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (b *restoreBench) spin(t *testing.T, args ...string) {
	t.Helper()
	mustRun(t, filepath.Join(b.out, "bin", "spin-machine"), args...)
}

// evict drops a file from the host's page cache. It is the file's owner asking about its own
// file, so it needs no privilege, and it only works on pages nobody maps: callers evict
// between machines, never under one.
func evict(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a file this test made
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const posixFadvDontneed = 4
	if _, _, e := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, posixFadvDontneed, 0, 0); e != 0 {
		t.Fatalf("fadvise %s: %v", path, e)
	}
}

func readAll(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a file this test made
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1<<20)
	for {
		if _, err := f.Read(buf); err != nil {
			return
		}
	}
}

// majorFaults is field 12 of /proc/PID/stat: faults that had to wait for the disk.
func majorFaults(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	// The command name is in parentheses and may contain spaces; fields count from after it.
	s := string(b)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	n, err := strconv.Atoi(fields[9])
	if err != nil {
		t.Fatalf("reading majflt from /proc/%d/stat: %v", pid, err)
	}
	return n
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func s50(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return pct(s, 50)
}

func p50p95(v []float64) string {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return fmt.Sprintf("%.1f / %.1f", pct(s, 50), pct(s, 95))
}

func fmtMiB(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
