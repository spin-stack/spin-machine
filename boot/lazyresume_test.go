// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// What a resume costs when the guest runs before its memory is loaded (spin-stack/spin#1004).
//
// A resume today reads the whole state file into the machine and then runs it, so its cost is
// the guest's memory: 0.3 s plus about 0.2 s a gigabyte from a local disk (spin's warm-prebuild
// lab, 2026-10-16), and on another host the whole file is fetched first. QEMU's fast snapshot
// load (docs/devel/migration/fast-snapshot-load.rst, master after 11.1) loads the devices, runs
// the guest and reads each page of a mapped-ram file when the guest faults on it, with a thread
// loading the rest behind. This asks what that buys with the file on a local disk, which is the
// question to answer before anything is designed to do it from a bucket, and whether it works on
// this machine at all: postcopy under the machine's seccomp filter, with its balloon, and with
// virtio-mem when SPIN_LAZY_MAX_MEMORY_MB gives it a region.
//
// A guest fills a tmpfs with HEAP MiB of /dev/urandom and counts on its console. It is saved
// once as a stream, as a resume is today, and once as mapped-ram, and each load is one of:
//
//	stream   the stream, loaded whole before the guest runs: a resume as it is
//	mapped   the mapped-ram file, loaded whole before the guest runs
//	lazy     the mapped-ram file with postcopy-ram: the guest runs, then its pages arrive
//
// alternated, REPS of each, with the file in the host's page cache (warm) and dropped from it
// (cold: a file that has just been fetched is on the disk and not in memory). For each load:
// until the guest counts again, until QEMU says the load is complete - for lazy, how long the
// guest depends on the file - and how long the guest then takes to read its whole heap, which is
// where lazy puts the cost it took out of the first number.
//
// Needs a QEMU with fast snapshot load: the lab's qemu_run, booted instead of the release's.
//
//	SPIN_LAZY_RESUME=1            run it (task boot:lazy-resume)
//	SPIN_LAZY_HEAP_MB=0,1024      the heaps, in MiB (default 0,1024,2048,4096)
//	SPIN_LAZY_MAX_MEMORY_MB=n     give the machine a virtio-mem region up to n MiB
//	REPS=n                        loads per row (default 10)
func TestWhatALazyResumeSaves(t *testing.T) {
	if os.Getenv("SPIN_LAZY_RESUME") == "" {
		t.Skip("set SPIN_LAZY_RESUME=1: task boot:lazy-resume")
	}
	out := releaseDir(t)
	cli := filepath.Join(out, "bin", "spin-machine")
	reps := envInt(t, "REPS", 10)
	heaps := []int{0, 1024, 2048, 4096}
	if v := os.Getenv("SPIN_LAZY_HEAP_MB"); v != "" {
		heaps = nil
		for _, s := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 0 {
				t.Fatalf("SPIN_LAZY_HEAP_MB=%q: %q is not a size in MiB", v, s)
			}
			heaps = append(heaps, n)
		}
	}
	if b, err := exec.Command(filepath.Join(out, "bin", "qemu-system-x86_64"), "--version").Output(); err == nil { //nolint:gosec // the release's QEMU
		t.Logf("    %s", strings.SplitN(string(b), "\n", 2)[0])
	}

	variants := []lazyVariant{
		{name: "stream", format: "stream"},
		{name: "mapped", format: "mapped", caps: []string{"mapped-ram"}},
		{name: "lazy", format: "mapped", caps: []string{"mapped-ram", "postcopy-ram"}},
	}
	for _, heap := range heaps {
		// Not under the test's directory: a socket's path has 108 bytes, and the lab runner's
		// TMPDIR alone takes 80 of them.
		socks, err := os.MkdirTemp("/tmp", "lz")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(socks) })
		l := &lazyLab{t: t, cli: cli, out: out, heap: heap, dir: t.TempDir(), socks: socks}
		saved := map[string]lazySaved{}
		for _, format := range []string{"stream", "mapped"} {
			saved[format] = l.save(format)
			s := saved[format]
			t.Logf("    heap %4d MiB  saved as %-6s in %6.0f ms: %5d MiB long, %5d MiB on disk",
				heap, format, s.ms, s.apparent>>20, s.onDisk>>20)
		}
		// One lazy load before the rows: a QEMU without the feature, or a host that refuses
		// userfaultfd, says so once and leaves the other two rows to be measured.
		use := slices.Clone(variants)
		if _, err := l.load(variants[2], saved["mapped"], false); err != nil {
			t.Errorf("heap %d MiB: a lazy load does not work here, so its rows are missing: %v", heap, err)
			use = use[:2]
		}
		rows := map[string]*lazyRow{}
		for range reps {
			for _, cold := range []bool{false, true} {
				for _, v := range use {
					r, err := l.load(v, saved[v.format], cold)
					if err != nil {
						t.Fatalf("heap %d MiB, %s: %v", heap, v.name, err)
					}
					key := fmt.Sprintf("%s/%t", v.name, cold)
					if rows[key] == nil {
						rows[key] = &lazyRow{}
					}
					rows[key].add(r)
				}
			}
		}
		for _, cold := range []bool{false, true} {
			for _, v := range use {
				r := rows[fmt.Sprintf("%s/%t", v.name, cold)]
				cache := "warm"
				if cold {
					cache = "cold"
				}
				t.Logf("    heap %4d MiB  %s  %-6s  counting again p50 %6.0f p90 %6.0f ms   complete p50 %6.0f ms   heap read p50 %6.0f p90 %6.0f ms   n=%d",
					heap, cache, v.name, pct(r.tick, 50), pct(r.tick, 90), pct(r.done, 50), pct(r.read, 50), pct(r.read, 90), len(r.tick))
			}
		}
		for _, s := range saved {
			_ = os.Remove(s.state) // gigabytes, and the next heap's are larger
		}
	}
}

type lazyVariant struct {
	name   string
	format string
	caps   []string
}

// lazySaved is one saved guest: its state, the overlay as it was at the save, and the tick it
// had reached.
type lazySaved struct {
	state, overlay   string
	last             int
	ms               float64
	apparent, onDisk int64
}

type lazyLoad struct{ tick, done, read float64 }

type lazyRow struct{ tick, done, read []float64 }

func (r *lazyRow) add(l lazyLoad) {
	r.tick, r.done, r.read = append(r.tick, l.tick), append(r.done, l.done), append(r.read, l.read)
}

type lazyLab struct {
	t        *testing.T
	cli, out string
	heap     int
	dir      string
	socks    string
	n        int
}

// flags is the machine every guest of this heap is: the heap and a gigabyte for the rest.
func (l *lazyLab) flags(overlay, qmp string, more ...string) []string {
	f := []string{"--qmp", qmp, "--disk", overlay, "--disk-format", "qcow2", "--init", "/bin/sh",
		"--memory", strconv.Itoa(l.heap + 1024), "--cpus", "2"}
	if v := os.Getenv("SPIN_LAZY_MAX_MEMORY_MB"); v != "" {
		f = append(f, "--max-memory", v)
	}
	return append(f, more...)
}

var lazyTick = regexp.MustCompile(`TICK-(\d+)`)

// save boots a guest, fills its heap, and saves it stopped, as format.
func (l *lazyLab) save(format string) lazySaved {
	t := l.t
	t.Helper()
	overlay := filepath.Join(l.dir, format+".qcow2")
	if b, err := exec.Command(filepath.Join(l.out, "bin", "qemu-img"), "create", "-q", "-f", "qcow2", //nolint:gosec // the release's qemu-img
		"-b", filepath.Join(l.out, "image", "rootfs.qcow2"), "-F", "qcow2", overlay).CombinedOutput(); err != nil {
		t.Fatalf("an overlay of the base image: %v: %s", err, b)
	}
	sock := filepath.Join(l.socks, format+"-save.sock")
	g := startGuest(t, machineArgs(t, l.cli, l.out, l.flags(overlay, sock)))
	defer g.kill()
	script := "mount -t proc proc /proc; "
	if l.heap > 0 {
		script += fmt.Sprintf("mount -t tmpfs -o size=%dm tmpfs /mnt && dd if=/dev/urandom of=/mnt/heap bs=1M count=%d 2>/dev/null; ", l.heap+16, l.heap)
	}
	script += "echo HEAP-FILLED; ( i=0; while :; do i=$((i+1)); echo TICK-$i; sleep 0.02; done ) &"
	g.type_(t, script)
	// Five seconds of counting before the save: a guest saved in its first second went on
	// counting 250 ms after every kind of load, and one saved ten seconds in after 10 ms (lab
	// run 42726681234), which is the guest's own and hides what the load cost.
	g.await(t, regexp.MustCompile(`HEAP-FILLED\s[\s\S]*TICK-250\b`), 5*time.Minute)

	q := dial(t, sock, &vm{})
	defer q.close()
	// dial gives the connection half a minute, and a save of gigabytes is longer.
	if err := q.conn.SetDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	q.do(t, "stop", nil)
	s := lazySaved{state: filepath.Join(l.dir, format+".state"), last: g.lastTick()}
	if format == "mapped" {
		q.do(t, "migrate-set-capabilities", map[string]any{"capabilities": []map[string]any{{"capability": "mapped-ram", "state": true}}})
	}
	// QEMU's default is 128 MiB/s, which is what a save then measures: a gigabyte took 9.6 s.
	q.do(t, "migrate-set-parameters", map[string]any{"max-bandwidth": 0})
	start := time.Now()
	q.do(t, "migrate", map[string]any{"uri": "file:" + s.state})
	q.until(t, "query-migrate", "completed", 5*time.Minute)
	s.ms = float64(time.Since(start).Microseconds()) / 1000
	g.kill()

	// On the disk before anything loads it: a cold load drops the file from the page cache,
	// and pages not yet written back are not dropped.
	f, err := os.Open(s.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		t.Fatalf("syncing %s: %v", s.state, err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	s.apparent, s.onDisk = st.Size, st.Blocks*512
	s.overlay = overlay + ".saved"
	lazyCopy(t, overlay, s.overlay)
	return s
}

// load resumes s as v in a new QEMU and says what it cost. An error is QEMU refusing the load;
// anything else that goes wrong fails the test.
func (l *lazyLab) load(v lazyVariant, s lazySaved, cold bool) (lazyLoad, error) {
	t := l.t
	t.Helper()
	l.n++
	// The disk as the saved guest left it: the guest before this one wrote to its own copy.
	overlay := filepath.Join(l.dir, fmt.Sprintf("load-%d.qcow2", l.n))
	lazyCopy(t, s.overlay, overlay)
	defer func() { _ = os.Remove(overlay) }()
	if cold {
		f, err := os.Open(s.state)
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		_ = f.Close()
		if err != nil {
			t.Fatalf("dropping %s from the page cache: %v", s.state, err)
		}
	}
	sock := filepath.Join(l.socks, fmt.Sprintf("load-%d.sock", l.n))
	g := startGuest(t, machineArgs(t, l.cli, l.out, l.flags(overlay, sock, "--incoming", "defer")))
	defer g.kill()
	q := dial(t, sock, &vm{})
	defer q.close()
	refused := func(what string, err error) (lazyLoad, error) {
		return lazyLoad{}, fmt.Errorf("%s: %w\n\nQEMU said:\n%s", what, err, tail([]byte(g.said()), 1500))
	}
	if len(v.caps) > 0 {
		caps := make([]map[string]any, 0, len(v.caps))
		for _, c := range v.caps {
			caps = append(caps, map[string]any{"capability": c, "state": true})
		}
		if _, err := q.ask("migrate-set-capabilities", map[string]any{"capabilities": caps}); err != nil {
			return refused("setting "+strings.Join(v.caps, " and "), err)
		}
	}

	var r lazyLoad
	start := time.Now()
	since := func() float64 { return float64(time.Since(start).Microseconds()) / 1000 }
	if _, err := q.ask("migrate-incoming", map[string]any{"uri": "file:" + s.state}); err != nil {
		return refused("migrate-incoming", err)
	}
	// The guest was saved stopped, so it is continued by hand, as soon as QEMU has a machine to
	// continue: the load complete, or for a lazy one the devices loaded and the pages pending.
	running := false
	for r.tick == 0 || r.done == 0 {
		if since() > 300_000 {
			t.Fatalf("%s: the guest was not counting again, or the load not complete, in 5 minutes:\n%s", v.name, tail([]byte(g.said()), 1500))
		}
		if r.tick == 0 && lazyTick.MatchString(g.said()) {
			r.tick = since()
		}
		var m struct{ Status string }
		raw, err := q.ask("query-migrate", nil)
		if err != nil {
			return refused("query-migrate", err)
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("reading query-migrate: %v", err)
		}
		switch m.Status {
		case "failed":
			return refused("the load", errors.New("query-migrate says failed"))
		case "completed":
			if r.done == 0 {
				r.done = since()
			}
		}
		if !running && (m.Status == "completed" || m.Status == "postcopy-active") {
			var rs struct{ Status string }
			raw, err := q.ask("query-status", nil)
			if err != nil {
				return refused("query-status", err)
			}
			if err := json.Unmarshal(raw, &rs); err != nil {
				t.Fatalf("reading query-status: %v", err)
			}
			switch rs.Status {
			case "running":
				running = true
			case "inmigrate":
			default:
				if _, err := q.ask("cont", nil); err != nil {
					return refused("cont", err)
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	if got := g.lastTick(); got <= s.last {
		t.Fatalf("%s: the guest said TICK-%d, and it was saved at TICK-%d: this is not the saved guest going on", v.name, got, s.last)
	}
	if l.heap > 0 {
		at := time.Now()
		// Quoted, so the console's echo of this line is not what is awaited.
		g.type_(t, `cat /mnt/heap > /dev/null; echo HEAP-"READ"`)
		g.await(t, regexp.MustCompile(`HEAP-READ\s`), 5*time.Minute)
		r.read = float64(time.Since(at).Microseconds()) / 1000
	}
	return r, nil
}

// ask is do that returns QEMU's refusal: a load QEMU refuses is a result here, not a broken test.
func (q *qmp) ask(cmd string, args map[string]any) (json.RawMessage, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	if err := q.conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, err
	}
	if err := q.enc.Encode(req); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmd, err)
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
			return nil, fmt.Errorf("reading the reply to %s: %w", cmd, err)
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("%s was refused: %s", cmd, reply.Error.Desc)
		}
		if reply.Event != "" {
			continue
		}
		return reply.Return, nil
	}
}

func lazyCopy(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copying %s: %v", from, err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
