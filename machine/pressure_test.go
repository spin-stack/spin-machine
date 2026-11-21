// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spin-stack/machine/qmp"
)

// told is a watch on a listener nobody dials, and what it was told.
type told struct {
	w  *PressureWatch
	mu sync.Mutex
	ps []Pressure
}

func newTold(t *testing.T) *told {
	t.Helper()
	x := &told{}
	x.w = (&PressureListener{byCID: map[uint32]*PressureWatch{}}).Watch(func(p Pressure) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.ps = append(x.ps, p)
	})
	x.w.observe(qmp.Observer{}, false)
	return x
}

func (x *told) pressures() []Pressure {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]Pressure(nil), x.ps...)
}

// zeroAt is ps with every time taken out, to compare.
func zeroAt(ps []Pressure) []Pressure {
	out := make([]Pressure, 0, len(ps))
	for _, p := range ps {
		if p.When().IsZero() {
			panic("a pressure with no time")
		}
		switch p := p.(type) {
		case Stall:
			p.At = time.Time{}
			out = append(out, p)
		case OOMKill:
			p.At = time.Time{}
			out = append(out, p)
		case SlowCommand:
			p.At = time.Time{}
			out = append(out, p)
		case GuestLost:
			p.At = time.Time{}
			out = append(out, p)
		}
	}
	return out
}

// A guest's lines are what the watch is told. A resumed guest's OOM kills count from its hello: a
// kill it had before this machine heard it was the machine before's to report.
func TestAGuestsLinesAreWhatItsMachineIsTold(t *testing.T) {
	x := newTold(t)
	x.w.observe(qmp.Observer{}, true)
	err := x.w.read(strings.NewReader("spin-machine-pressure 1 oom=3\nstall memory 150000 1000000 8123\noom 3\noom 5\noom 6\nstall io 150000 1000000 1\n"))
	if err == nil || !strings.Contains(err.Error(), "the guest closed its connection") {
		t.Errorf("a connection that ended: %v", err)
	}
	want := []Pressure{
		Stall{Seen: Guest, Resource: MemoryPressure, Threshold: 150 * time.Millisecond, Window: time.Second, Total: 8123 * time.Microsecond},
		OOMKill{Kills: 2},
		OOMKill{Kills: 1},
		Stall{Seen: Guest, Resource: IOPressure, Threshold: 150 * time.Millisecond, Window: time.Second, Total: time.Microsecond},
	}
	if got := zeroAt(x.pressures()); !reflect.DeepEqual(got, want) {
		t.Errorf("told %+v, want %+v", got, want)
	}
	// The guest dials again, as after a restore, and its hello says the kills it could not send.
	_ = x.w.read(strings.NewReader("spin-machine-pressure 1 oom=7\n"))
	if got := zeroAt(x.pressures()); len(got) != 5 || !reflect.DeepEqual(got[4], OOMKill{Kills: 1}) {
		t.Errorf("after the guest dialled again: %+v", got)
	}
}

// A guest booted cold has killed nothing before it boots: a kill before its sender first dialled
// is in its hello, and is news.
func TestAColdGuestsFirstKillsAreNews(t *testing.T) {
	x := newTold(t)
	_ = x.w.read(strings.NewReader("spin-machine-pressure 1 oom=2\noom 2\n"))
	if got := zeroAt(x.pressures()); !reflect.DeepEqual(got, []Pressure{OOMKill{Kills: 2}}) {
		t.Errorf("told %+v", got)
	}
}

func TestAGuestThatSpeaksOutOfTurnIsCutOff(t *testing.T) {
	for in, want := range map[string]string{
		"stall cpu 1 2 3\n": "first line is not its hello",
		"spin-machine-pressure 1 oom=0\nspin-machine-pressure 1 oom=0\n": "a hello came again",
		"spin-machine-pressure 2 oom=0\n":                                "a version of the pressure line this host does not read",
		"spin-machine-pressure 1 oom=0\nnonsense\n":                      "a pressure line that is not one",
		"spin-machine-pressure 1 oom=0\n" + strings.Repeat("x", 200):     "longer than 128 bytes",
		"spin-machine-pressure 1 oom=0\nstall cpu 1 2 3":                 "the guest closed its connection",
	} {
		x := newTold(t)
		if err := x.w.read(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40q: %v, not %q", in, err, want)
		}
		if got := x.pressures(); len(got) != 0 {
			t.Errorf("%.40q told %+v", in, got)
		}
	}
	x := newTold(t)
	if err := x.w.read(failingReader{}); err == nil || !strings.Contains(err.Error(), "reading the guest's pressure: broken") {
		t.Errorf("a read that failed: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken") }

// A guest holds one connection: a second replaces the first, which ends as no GuestLost. The one it
// holds ends as GuestLost while its machine runs, and as nothing once QEMU has exited.
func TestAGuestHoldsOneConnectionAndItsEndIsSaid(t *testing.T) {
	x := newTold(t)
	l := x.w.l
	if err := x.w.register(7); err != nil {
		t.Fatal(err)
	}
	pipe := func() (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		return r, w
	}
	if l.take(8, nil) != nil {
		t.Error("a connection from a CID nobody watches was taken")
	}
	first, firstIn := pipe()
	if l.take(7, first) != x.w {
		t.Fatal("the watched guest's connection was not its machine's")
	}
	done := make(chan struct{})
	go func() { l.serve(first, x.w); close(done) }()
	second, secondIn := pipe()
	if l.take(7, second) != x.w {
		t.Fatal("the guest's second connection was refused")
	}
	<-done // the first was closed under its reader
	if got := x.pressures(); len(got) != 0 {
		t.Errorf("a replaced connection told %+v", got)
	}
	_, _ = secondIn.WriteString("spin-machine-pressure 1 oom=0\nnonsense\n")
	l.serve(second, x.w)
	got := zeroAt(x.pressures())
	if len(got) != 1 || !strings.Contains(got[0].(GuestLost).Err.Error(), "not one") {
		t.Errorf("a connection that ended told %+v", got)
	}
	third, thirdIn := pipe()
	l.take(7, third)
	_ = thirdIn.Close()
	x.w.unregister()
	l.serve(third, x.w)
	if got := x.pressures(); len(got) != 1 {
		t.Errorf("a machine that exited was told %+v", got[1:])
	}
	_ = firstIn
	// Closed, the listener ends the connections it holds and takes no more.
	y := newTold(t)
	f, _ := pipe()
	y.w.l.f = f
	if err := y.w.register(9); err != nil {
		t.Fatal(err)
	}
	held, _ := pipe()
	y.w.l.take(9, held)
	if err := y.w.l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Stat(); err == nil {
		t.Error("Close left the guest's connection open")
	}
	late, _ := pipe()
	if y.w.l.take(9, late) != nil {
		t.Error("a closed listener took a connection")
	}
	y.w.l.serve(held, y.w)
	if got := y.pressures(); len(got) != 0 {
		t.Errorf("a closed listener's connection told %+v", got)
	}
}

func TestOneCIDOneWatchOneMachine(t *testing.T) {
	l := &PressureListener{byCID: map[uint32]*PressureWatch{}}
	if err := l.Watch(nil).register(4); err == nil || !strings.Contains(err.Error(), "nothing to tell") {
		t.Errorf("a watch with no notify: %v", err)
	}
	a, b := l.Watch(func(Pressure) {}), l.Watch(func(Pressure) {})
	if err := a.register(5); err != nil {
		t.Fatal(err)
	}
	if err := a.register(6); err == nil || !strings.Contains(err.Error(), "one Start takes one watch") {
		t.Errorf("a watch registered twice: %v", err)
	}
	if err := b.register(5); err == nil || !strings.Contains(err.Error(), "vsock CID 5 is watched") {
		t.Errorf("a CID watched twice: %v", err)
	}
	b.unregister() // b holds no CID, and takes none from a
	if l.byCID[5] != a {
		t.Error("unregistering a watch that holds nothing took another's CID")
	}
	a.unregister()
	if err := l.Watch(func(Pressure) {}).register(5); err != nil {
		t.Errorf("the CID of a machine gone: %v", err)
	}
}

// A command is slow against what it took before, by slowFactor and by slowCommand at least, and a
// command never answered before is never slow.
func TestACommandIsSlowAgainstWhatItTookBefore(t *testing.T) {
	x := newTold(t)
	at := time.Unix(1, 0)
	x.w.timed("query-status", time.Second, false, at) // the first: its base, never slow
	x.w.timed("query-status", 10*time.Millisecond, false, at)
	// The base moves an eighth of the way: (1 s - 10 ms) / 8 below 1 s.
	if base := x.w.baseline["query-status"]; base != time.Second-(990*time.Millisecond)/8 {
		t.Errorf("the base is %v", base)
	}
	x.w.baseline["query-status"] = 20 * time.Millisecond
	x.w.timed("query-status", 99*time.Millisecond, false, at) // 4x, under slowCommand
	x.w.baseline["query-status"] = 30 * time.Millisecond
	x.w.timed("query-status", 119*time.Millisecond, false, at) // over slowCommand, under 4x
	x.w.baseline["query-status"] = 30 * time.Millisecond
	x.w.timed("query-status", 500*time.Millisecond, true, at) // a machine loading a state
	if got := x.pressures(); len(got) != 0 {
		t.Fatalf("told %+v", got)
	}
	if base := x.w.baseline["query-status"]; base != 30*time.Millisecond {
		t.Errorf("a busy machine's answer moved the base to %v", base)
	}
	x.w.timed("query-status", 120*time.Millisecond, false, at)
	want := []Pressure{SlowCommand{At: at, Command: "query-status", Took: 120 * time.Millisecond, Baseline: 30 * time.Millisecond}}
	if got := x.pressures(); !reflect.DeepEqual(got, want) {
		t.Errorf("told %+v, want %+v", got, want)
	}
	if base := x.w.baseline["query-status"]; base != 30*time.Millisecond {
		t.Errorf("a slow answer moved the base to %v", base)
	}
	x.w.timed("qom-get", time.Hour, false, at)
	if len(x.pressures()) != 1 {
		t.Error("a command never answered before was slow")
	}
}

// The watch's Observer runs the caller's hooks as they were, and the migration's events say when
// a machine is loading or saving a state, from its start when it was started to load one.
func TestTheWatchsObserverKeepsTheCallersAndFollowsTheMigration(t *testing.T) {
	type key struct{}
	var named []string
	var events []string
	caller := qmp.Observer{
		Command: func(ctx context.Context, name string) (context.Context, func(error)) {
			named = append(named, name)
			return context.WithValue(ctx, key{}, name), func(err error) { named = append(named, "end "+name) }
		},
		Event: func(e qmp.Event) { events = append(events, e.Name) },
	}
	x := newTold(t)
	o := x.w.observe(caller, true)
	if !x.w.isBusy() {
		t.Error("a machine started to load a state is not busy")
	}
	ctx, end := o.Command(context.Background(), "query-status")
	if ctx.Value(key{}) != "query-status" {
		t.Error("the caller's context was not the call's")
	}
	// The load ends under the command: a command asked of a busy machine is not timed.
	for _, status := range []string{"active", "completed"} {
		o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{"status":"` + status + `"}`)})
	}
	end(nil)
	if _, ok := x.w.baseline["query-status"]; ok {
		t.Error("a busy machine's answer was timed")
	}
	if x.w.isBusy() {
		t.Error("a migration completed left the machine busy")
	}
	o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{"status":"setup"}`)})
	o.Event(qmp.Event{Name: "STOP"})
	o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{nonsense`)})
	if !x.w.isBusy() {
		t.Error("a save under way is not busy")
	}
	o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{"status":"failed"}`)})
	// A save starts under the command: a command answered by a busy machine is not timed either.
	_, end = o.Command(context.Background(), "query-migrate")
	o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{"status":"setup"}`)})
	end(nil)
	if _, ok := x.w.baseline["query-migrate"]; ok {
		t.Error("an answer from a busy machine was timed")
	}
	o.Event(qmp.Event{Name: "MIGRATION", Data: []byte(`{"status":"cancelled"}`)})
	_, end = o.Command(context.Background(), "cont")
	end(errors.New("refused"))
	if _, ok := x.w.baseline["cont"]; !ok {
		t.Error("an answer once the machine was not busy was not timed")
	}
	if strings.Join(named, ",") != "query-status,end query-status,query-migrate,end query-migrate,cont,end cont" {
		t.Errorf("the caller's command hook saw %v", named)
	}
	if strings.Join(events, ",") != "MIGRATION,MIGRATION,MIGRATION,STOP,MIGRATION,MIGRATION,MIGRATION,MIGRATION" {
		t.Errorf("the caller's event hook saw %v", events)
	}
	// A caller with no hooks of its own is timed all the same.
	y := newTold(t)
	p := y.w.observe(qmp.Observer{}, false)
	_, end = p.Command(context.Background(), "query-status")
	end(nil)
	p.Event(qmp.Event{Name: "SHUTDOWN"})
	if _, ok := y.w.baseline["query-status"]; !ok {
		t.Error("a caller with no hooks was not timed")
	}
}

func TestCgroupOfIsTheUnifiedHierarchysPath(t *testing.T) {
	dir, err := cgroupOf("self")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, "/sys/fs/cgroup/") && dir != "/sys/fs/cgroup" {
		t.Errorf("this process's cgroup is %s", dir)
	}
	if _, err := cgroupOf("0"); err == nil || !strings.Contains(err.Error(), "reading the cgroup of process 0") {
		t.Errorf("a process that is not there: %v", err)
	}
}

// A machine whose QEMU shares its caller's cgroup has no pressure of its own on the host: Start
// refuses its watch, and kills the QEMU it started. A Spec with no vsock is refused before.
func TestStartRefusesAWatchItCannotKeep(t *testing.T) {
	x := newTold(t)
	if _, err := startFake(t, ctx(t), "obeys", Spec{}, StartOptions{Pressure: x.w}); err == nil || !strings.Contains(err.Error(), "its Spec has no vsock") {
		t.Errorf("a machine with no vsock: %v", err)
	}
	vsock, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vsock.Close() }()
	s := Spec{VsockCID: 9, VsockFD: 3}
	_, err = startFake(t, ctx(t), "obeys", s, StartOptions{Pressure: x.w, Files: []*os.File{vsock}})
	if err == nil || !strings.Contains(err.Error(), "QEMU is in its caller's cgroup") || !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("a QEMU in its caller's cgroup: %v", err)
	}
	if err := x.w.l.Watch(func(Pressure) {}).register(9); err != nil {
		t.Errorf("a refused machine still holds its CID: %v", err)
	}
	other := newTold(t)
	if err := other.w.register(9); err != nil {
		t.Fatal(err)
	}
	other.w.l = x.w.l
	other.w.l.byCID[9] = other.w
	if _, err := startFake(t, ctx(t), "obeys", s, StartOptions{Pressure: x.w.l.Watch(func(Pressure) {}), Files: []*os.File{vsock}}); err == nil || !strings.Contains(err.Error(), "vsock CID 9 is watched") {
		t.Errorf("a CID another machine is watched by: %v", err)
	}
}

// Usage reads QEMU's cgroup only when it is QEMU's own.
func TestUsageRefusesTheCallersCgroup(t *testing.T) {
	p, err := startFake(t, ctx(t), "obeys", Spec{}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Usage(ctx(t)); err == nil || !strings.Contains(err.Error(), "QEMU is in its caller's cgroup") {
		t.Errorf("Usage of a QEMU in its caller's cgroup: %v", err)
	}
}

func TestCgroupUsageReadsEachCounter(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"cpu.stat":        "usage_usec 2000000\nuser_usec 1\nthrottled_usec 3000\nnr_periods\n",
		"memory.current":  "1048576\n",
		"memory.stat":     "anon 10\nfile 20\nfile_dirty 30\nfile_writeback 40\nweird x\n",
		"io.stat":         "259:0 rbytes=1 wbytes=2 rios=3 wios=4 dbytes=0 dios=0\n8:0 rbytes=10 wbytes=20 rios=30 wios=40\n\n7:0 rbytes=100\n7:1\n",
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=5\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"memory.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=6\n",
		"io.pressure":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=7\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u, err := cgroupUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		CPU:    CPUUsage{Used: 2 * time.Second, Throttled: 3 * time.Millisecond, Stalled: 5 * time.Microsecond},
		Memory: MemoryUsage{Current: 1 << 20, Anon: 10, File: 20, Dirty: 30, Writeback: 40, Stalled: 6 * time.Microsecond},
		IO:     IOUsage{Read: 111, Written: 22, Reads: 33, Writes: 44, Stalled: 7 * time.Microsecond},
	}
	if !reflect.DeepEqual(u, want) {
		t.Errorf("read %+v, want %+v", u, want)
	}
	for name, bad := range map[string]string{"memory.current": "lots\n", "io.pressure": "full total=1\n", "memory.pressure": "", "cpu.pressure": ""} {
		saved := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := cgroupUsage(dir); err == nil {
			t.Errorf("a %s of %q read", name, bad)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(saved), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name := range files {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+".gone")); err != nil {
			t.Fatal(err)
		}
		if _, err := cgroupUsage(dir); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a cgroup with no %s: %v", name, err)
		}
		if err := os.Rename(filepath.Join(dir, name+".gone"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestADisksDeviceIsItsID(t *testing.T) {
	for qdev, want := range map[string]string{
		"/machine/peripheral/slot0/virtio-backend": "slot0",
		"/machine/peripheral/hd3":                  "hd3",
		"/machine/unattached/device[4]":            "/machine/unattached/device[4]",
	} {
		if got := deviceOf(qdev); got != want {
			t.Errorf("%s is %s, not %s", qdev, got, want)
		}
	}
}
