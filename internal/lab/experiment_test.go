// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// recorder is a probe that boots nothing: it records which measurement it was asked for, in which
// phase, and what the test tells it to.
type recorder struct {
	phases  int
	rows    []string
	calls   []string
	cleaned []string
	miss    string // tree/row/rep/phase whose measurement is lost
	fail    string // tree/row/rep/phase whose measurement stops the experiment
	refuse  string // tree/row/rep/phase whose measurement refuses its tree
	spins   []bool // each measurement's key, whether it logs in by spin's path
}

func (r *recorder) Name() string  { return "recorder" }
func (r *recorder) About() string { return "what it was asked" }
func (r *recorder) Options() []Option {
	return append(shape, Option{"n", "3", "a number"}, Option{"on", "false", "a switch"})
}
func (r *recorder) Metrics() []Metric { return []Metric{{"a", "ms", ""}, {"b", "count", ""}} }
func (r *recorder) Rows(o Options) []string {
	if o.Bool("on") {
		return append(slices.Clone(r.rows), "extra")
	}
	return r.rows
}
func (r *recorder) Phases() int          { return r.phases }
func (r *recorder) Needs(Options) []Need { return nil }

func (r *recorder) Measure(_ context.Context, m *Measurement) error {
	at := fmt.Sprintf("%s/%s/%d/%d", m.Tree.Name(), m.Row, m.Rep, m.Phase)
	r.calls = append(r.calls, at)
	r.spins = append(r.spins, m.Key != nil && m.Key.SpinsPath)
	switch at {
	case r.miss:
		return Miss("never answered: %d", 7)
	case r.fail:
		return errors.New("broke")
	case r.refuse:
		return Refuse("QEMU will not make it: %s", "Host doesn't support requested features")
	}
	if fi, err := os.Stat(m.Dir); err != nil || !fi.IsDir() || len(m.Socks) > 40 || m.Key == nil {
		return fmt.Errorf("the measurement's own directory %s, sockets %s, key %v", m.Dir, m.Socks, m.Key)
	}
	if m.Phase == 0 {
		m.State = at
		m.Cleanup(func() { r.cleaned = append(r.cleaned, at+" first") })
		m.Cleanup(func() { r.cleaned = append(r.cleaned, at+" second") })
	} else if m.State != strings.TrimSuffix(at, "1")+"0" {
		return fmt.Errorf("phase 1 got state %v", m.State)
	}
	m.RecordMs("a", time.Duration(m.Rep+1)*time.Millisecond)
	m.Record("b", float64(m.Options.Int("n")))
	m.Detail("k", 1)
	m.Logf("measured %s", at)
	return nil
}

func trees(names ...string) []*Tree {
	var ts []*Tree
	for _, n := range names {
		ts = append(ts, &Tree{Dir: "/" + n, Manifest: Manifest{Name: n, Parts: map[string]Part{"kernel": {Source: "release v1"}}}})
	}
	return ts
}

func TestAnExperimentInterleavesAndFinishesEachPhaseBeforeTheNext(t *testing.T) {
	p := &recorder{phases: 2, rows: []string{"x", "y"}}
	var log bytes.Buffer
	scratch := t.TempDir()
	r, err := Experiment{Probe: p, Trees: trees("A", "B"), Reps: 2, Options: Options{"n": "5"}, Scratch: scratch, Log: &log}.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for phase := range 2 {
		for rep := range 2 {
			for _, tree := range []string{"A", "B"} {
				for _, row := range []string{"x", "y"} {
					want = append(want, fmt.Sprintf("%s/%s/%d/%d", tree, row, rep, phase))
				}
			}
		}
	}
	if !slices.Equal(p.calls, want) {
		t.Errorf("measured in the order\n%v\nwant\n%v", p.calls, want)
	}
	if len(r.Samples) != 8 || r.Samples[0].Rep != 1 || r.Samples[7].Rep != 2 || r.Samples[7].Tree != "B" {
		t.Errorf("samples: %+v", r.Samples)
	}
	if r.Options["n"] != "5" || r.Options["cpus"] != "1" || r.Options["on"] != "false" || r.Reps != 2 || r.Probe != "recorder" ||
		!slices.Equal(r.Rows, []string{"x", "y"}) || len(r.Trees) != 2 || r.Finished.Before(r.Started) {
		t.Errorf("result: %+v", r)
	}
	if got := r.Values("B", "y", "b"); !slices.Equal(got, []float64{5, 5}) {
		t.Errorf("B y b = %v", got)
	}
	if !strings.Contains(log.String(), "A, x, repetition 1: measured A/x/0/0") {
		t.Errorf("log: %s", log.String())
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("the measurements' directories outlived the experiment: %v", left)
	}
	// Last registered, first run: a machine is stopped before the directory under it goes.
	if len(p.cleaned) != 16 || p.cleaned[0] != "A/x/0/0 second" || p.cleaned[1] != "A/x/0/0 first" {
		t.Errorf("cleaned up in the order %v", p.cleaned)
	}
}

// A tree QEMU refuses is refused once: its measurements still to come are lost with the reason and
// not measured, and the other trees go on.
func TestARefusedTreeLosesItsMeasurementsStillToComeAtOnce(t *testing.T) {
	p := &recorder{phases: 1, rows: []string{"x"}, refuse: "A/x/0/0"}
	r, err := Experiment{Probe: p, Trees: trees("A", "B"), Reps: 3, Scratch: t.TempDir(), Log: io.Discard}.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A/x/0/0", "B/x/0/0", "B/x/1/0", "B/x/2/0"}; !slices.Equal(p.calls, want) {
		t.Errorf("measured %v, want %v: a refused tree is not measured again", p.calls, want)
	}
	for _, s := range r.Samples {
		refused := strings.HasPrefix(s.Missed, "refused: QEMU will not make it")
		if (s.Tree == "A") != refused {
			t.Errorf("sample %s rep %d missed %q", s.Tree, s.Rep, s.Missed)
		}
	}
}

func TestALostMeasurementIsCountedAndAFailureStops(t *testing.T) {
	p := &recorder{phases: 2, rows: []string{"x"}, miss: "A/x/0/0"}
	r, err := Experiment{Probe: p, Trees: trees("A"), Reps: 2, Scratch: t.TempDir(), Log: io.Discard}.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.calls, "A/x/0/1") {
		t.Error("a measurement lost in its first phase was measured in its second")
	}
	if r.Missed("A", "x") != 1 || r.Samples[0].Missed != "never answered: 7" || len(r.Values("A", "x", "a")) != 1 {
		t.Errorf("samples: %+v", r.Samples)
	}

	p = &recorder{phases: 1, rows: []string{"x"}, fail: "B/x/0/0"}
	_, err = Experiment{Probe: p, Trees: trees("A", "B", "C"), Reps: 1, Scratch: t.TempDir(), Log: io.Discard}.Run(t.Context())
	if err == nil || err.Error() != "B, x, repetition 1: broke" || slices.Contains(p.calls, "C/x/0/0") {
		t.Errorf("err %v after %v", err, p.calls)
	}
	for _, e := range []Experiment{
		{Probe: p, Reps: 1},
		{Probe: p, Trees: trees("A"), Reps: 0},
		{Probe: p, Trees: trees("A"), Reps: 1, Options: Options{"nope": "1"}},
		{Probe: p, Trees: trees("A"), Reps: 1, Options: Options{"login": "vsock"}, Scratch: t.TempDir()},
		{Probe: p, Trees: trees("A"), Reps: 1, Scratch: "/nonexistent/scratch"},
	} {
		if _, err := e.Run(t.Context()); err == nil {
			t.Errorf("%+v ran", e)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (Experiment{Probe: p, Trees: trees("A"), Reps: 1, Scratch: t.TempDir(), Log: io.Discard}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled experiment: %v", err)
	}
}

func TestAnOptionIsOneTheProbeTakesAndOfItsKind(t *testing.T) {
	p := &recorder{}
	for _, tc := range []struct {
		given Options
		want  string
	}{
		{Options{"nope": "1"}, `takes no option "nope"; it takes: cpus, flags, hotplug-disks, login, max-cpus, max-memory, memory, n, on, plugged`},
		{Options{"n": "many"}, "is a number"},
		{Options{"n": "-1"}, "is a number"},
		{Options{"on": "maybe"}, "true or false"},
		// A number whose default is 1 is a number, not a boolean.
		{Options{"cpus": "true"}, "is a number"},
	} {
		if _, err := settle(p, tc.given); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("settle(%v) = %v, want %q", tc.given, err, tc.want)
		}
	}
	if o, err := settle(p, Options{"n": "0"}); err != nil || o.Int("n") != 0 {
		t.Errorf("settle(n=0) = %v, %v", o, err)
	}
	o, err := settle(p, Options{"n": "4", "on": "true", "flags": "--a b"})
	if err != nil || o.Int("n") != 4 || !o.Bool("on") || !slices.Equal(o.Fields("flags"), []string{"--a", "b"}) || o["memory"] != "128" {
		t.Errorf("settle = %v, %v", o, err)
	}
	if !slices.Equal(p.Rows(o), []string{"extra"}) {
		t.Errorf("rows %v", p.Rows(o))
	}
}

// login=spin, the default, logs into every machine the experiment boots by spin's path, and lane
// by the image's own.
func TestTheLoginOptionIsHowEveryMachineIsLoggedInto(t *testing.T) {
	for login, spins := range map[string]bool{"": true, "spin": true, "lane": false} {
		p := &recorder{phases: 1, rows: []string{"x"}}
		o := Options{}
		if login != "" {
			o["login"] = login
		}
		if _, err := (Experiment{Probe: p, Trees: trees("A"), Reps: 2, Options: o, Scratch: t.TempDir(), Log: io.Discard}).Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if want := []bool{spins, spins}; !slices.Equal(p.spins, want) {
			t.Errorf("login=%q: by spin's path %v, want %v", login, p.spins, want)
		}
	}
}

// A probe boots the machine spin boots a workspace in unless the run asks for another: one vCPU
// of 16, 128 MiB of 8 GiB, four hotplug disks (spin-stack/spin internal/runner/vmm/vmm.go).
func TestBootArgsAreTheTreesInTheExperimentsShape(t *testing.T) {
	spins, err := settle(Probes["ssh-first"], nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &Measurement{Tree: trees("A")[0], Options: spins}
	want := []string{"boot", "--release", "/A", "--disk", "/o.qcow2", "--memory", "128", "--cpus", "1", "--max-memory", "8192",
		"--max-cpus", "16", "--hotplug-disks", "16", "--vsock-cid", "9"}
	if got := m.BootArgs("/o.qcow2", "--vsock-cid", "9"); !slices.Equal(got, want) {
		t.Errorf("BootArgs = %v", got)
	}
	if got := shapeOf(spins); got != "1 of 16 vCPUs, 128 of 8192 MiB, 16 hotplug disks, sized to 2048 MiB, spin's login" {
		t.Errorf("spin's shape is said %q", got)
	}
	m.Options, err = settle(Probes["ssh-first"], Options{"cpus": "2", "memory": "2048", "max-cpus": "0", "max-memory": "0",
		"hotplug-disks": "0", "plugged": "0", "login": "lane", "flags": "--cpu host"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"boot", "--release", "/A", "--disk", "/o.qcow2", "--memory", "2048", "--cpus", "2", "--max-memory", "0",
		"--max-cpus", "0", "--hotplug-disks", "0", "--cpu", "host"}
	if got := m.BootArgs("/o.qcow2"); !slices.Equal(got, want) {
		t.Errorf("BootArgs in the shape asked for = %v", got)
	}
	if got := shapeOf(m.Options); got != shapeBefore+", --cpu host" {
		t.Errorf("the shape asked for is said %q", got)
	}
	// A ceiling of one is a ceiling, and one hotplug disk is one.
	if got := shapeOf(Options{"cpus": "1", "max-cpus": "1", "memory": "128", "max-memory": "1", "hotplug-disks": "1"}); got != "1 of 1 vCPUs, 128 of 1 MiB, 1 hotplug disks" {
		t.Errorf("ceilings of one are said %q", got)
	}
	// A tree's own boot flags come last, so a tree that names a CPU model is that model whatever
	// the experiment's flags say.
	named := &Measurement{Tree: &Tree{Dir: "/B", Manifest: Manifest{Boot: []string{"--cpu=Skylake-Server-v4"}}}, Options: m.Options}
	if got := named.BootArgs("/o.qcow2"); got[len(got)-1] != "--cpu=Skylake-Server-v4" {
		t.Errorf("BootArgs of a tree with boot flags = %v, want them last", got)
	}
	c := &checkpoint{cid: 42}
	m.Tree.Manifest.Append = "quiet"
	if got := c.bootFlags(m, "/s/q", "--held"); !slices.Equal(got, []string{"--vsock-cid", "42", "--qmp", "/s/q", "--console", "file:/s/q.console", "--append", "init=/sbin/init quiet", "--held"}) {
		t.Errorf("bootFlags = %v", got)
	}
}

// The table says the shape it booted, each tree's p50/p95 and lost, and each tree's Δ p50 against
// the first with its interval, within noise when that includes 0.
func TestTheTableIsEachTreesP50AndP95AndItsDistanceFromTheFirst(t *testing.T) {
	r := &Result{Probe: "p", About: "about", Reps: 3, Rows: []string{"x"}, Options: Options{"cpus": "2", "memory": "2048", "flags": "", "top": "5"},
		Metrics: []Metric{{"a", "ms", ""}, {"curve25519_in", "count", ""}},
		Trees:   []Manifest{{Name: "release", Parts: map[string]Part{"kernel": {Source: "release v1"}}, Edits: []NamedEdit{{Name: "lane"}}, Append: "q"}, {Name: "kaslr", Boot: []string{"--cpu=Cascadelake-Server-v5"}}},
		Host:    Host{Name: "h", Instance: "c8id.4xlarge", CPU: "Xeon", CPUs: 16, Nested: true, ScratchDevice: "/dev/nvme1n1"}}
	for i, v := range []float64{100, 110, 300} {
		r.Samples = append(r.Samples, Sample{Tree: "release", Row: "x", Rep: i + 1, Values: map[string]float64{"a": v, "curve25519_in": 1}})
		r.Samples = append(r.Samples, Sample{Tree: "kaslr", Row: "x", Rep: i + 1, Values: map[string]float64{"a": v - 10.5}})
	}
	r.Samples[1].Values["curve25519_in"] = 0 // one boot of kaslr's: a Δ with no interval
	r.Samples = append(r.Samples, Sample{Tree: "kaslr", Row: "x", Rep: 4, Missed: "gone", Values: map[string]float64{"a": 1}})
	got := r.Table()
	for _, want := range []string{
		"p: about\n",
		"p50/p95 over 3 of each, interleaved, in 2 vCPUs, 2048 MiB, no hotplug disks; top=5; h on c8id.4xlarge (Xeon, 16 CPUs, nested true), scratch on /dev/nvme1n1",
		"  release          110/300                 1/1      0\n",
		"  kaslr           99.5/290                 0/0      1\n",
		"  Δ p50 against release [95% interval]: a change only when the interval excludes 0\n",
		"  kaslr      a ms                -10.5 [",
		"] within noise\n",
		"  kaslr      curve25519_in count -1.0, no interval from one boot\n",
		"    release: kernel release v1; qemu ; image ; spin-machine ; edit lane; append \"q\";\n",
		// A tree's own boot flag is said, even one alone: two trees of one release differ by it.
		"; boot \"--cpu=Cascadelake-Server-v5\";\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table has no %q:\n%s", want, got)
		}
	}
	// A second row the first tree booted and the second did not: no Δ.
	two := *r
	two.Rows = []string{"x", "y"}
	two.Samples = append(slices.Clone(r.Samples), Sample{Tree: "release", Row: "y", Rep: 1, Values: map[string]float64{"a": 5}})
	if got := two.Table(); !strings.Contains(got, "release, x") || !strings.Contains(got, "  kaslr, y   a ms                -\n") {
		t.Errorf("with two rows a line is not named by its row, or a row one tree has no boots of has a Δ:\n%s", got)
	}
	path := filepath.Join(t.TempDir(), "result.json")
	if err := r.Write(path); err != nil {
		t.Fatal(err)
	}
	back, err := ReadResult(path)
	if err != nil || len(back.Samples) != 7 || back.Host.Instance != "c8id.4xlarge" || back.Samples[6].Missed != "gone" {
		t.Errorf("read back: %+v, %v", back, err)
	}
	if _, err := ReadResult(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("read a result that is not there")
	}
	write(t, path, []byte("{"))
	if _, err := ReadResult(path); err == nil {
		t.Error("read a result that is not JSON")
	}
	if err := r.Write(filepath.Join(t.TempDir(), "no/such/dir/result.json")); err == nil {
		t.Error("wrote into a directory that is not there")
	}
	for v, want := range map[float64]string{7: "7", 7.25: "7.2", 123.4: "123", 100.4: "100", 99.4: "99.4", 0.06: "0.1"} {
		if got := num(v); got != want {
			t.Errorf("num(%v) = %q, want %q", v, got, want)
		}
	}
}

const ringBufferSample = `[    0.000000] Linux version 7.3.5
[    0.010000] calling  acpi_init+0x0/0x420 @ 1
[    0.014000] initcall acpi_init+0x0/0x420 returned 0 after 4000 usecs
[    0.020000] initcall inet_init+0x0/0x2e0 returned 0 after 1500 usecs
[    0.080000] Freeing unused kernel image (initmem) memory: 2628K
[    0.090000] systemd-journald[62]: Collecting audit messages is disabled.
[    0.050000] a reprinted line going backwards
`

func TestTheRingBufferIsItsInitcallsItsGapsAndWhenTheKernelWasDone(t *testing.T) {
	p, err := parseRingBuffer(ringBufferSample)
	if err != nil {
		t.Fatal(err)
	}
	if p.total != 5500 || p.calls["acpi_init+0x0/0x420"] != 4000 || p.calls["inet_init+0x0/0x2e0"] != 1500 || p.freeing != 0.08 {
		t.Errorf("parsed %+v", p)
	}
	gaps := map[string]gap{}
	for _, g := range p.gaps {
		gaps[g.before] = g
	}
	journald := gaps["systemd-journald[]: Collecting audit messages is disabled."]
	if len(p.gaps) != 5 || journald.after != "Freeing unused kernel image (initmem) memory: 2628K" || journald.ms < 9.995 || journald.ms > 10.005 {
		t.Errorf("gaps %+v", p.gaps)
	}
	if _, err := parseRingBuffer("[    0.1] [    0.2] twice"); err == nil {
		t.Log("not a double stamp at six digits")
	}
	if _, err := parseRingBuffer("[    0.100000] sh[1]: [    0.000100] re-stamped"); err == nil || !strings.Contains(err.Error(), "two kernel stamps") {
		t.Errorf("a re-stamped dump: %v", err)
	}
	if _, err := parseRingBuffer("[    0.100000] no initcalls here"); err == nil || !strings.Contains(err.Error(), "no initcall lines") {
		t.Errorf("no initcalls: %v", err)
	}
	if clip(strings.Repeat("x", 100)) != strings.Repeat("x", 88) || clip(strings.Repeat("x", 89)) != strings.Repeat("x", 88) ||
		clip("short") != "short" || tail("abcdef", 2) != "ef" || tail("ab", 5) != "ab" {
		t.Error("clip or tail")
	}
}

func TestInitcallsDetailRanksEachTreesInitcallsAndGaps(t *testing.T) {
	r := &Result{Options: Options{"top": "2"}, Trees: []Manifest{{Name: "A"}, {Name: "B"}}}
	for i, tr := range []string{"A", "B"} {
		r.Samples = append(r.Samples, Sample{Tree: tr, Detail: map[string]float64{
			"initcall\x00acpi_init+0x0/0x420": 4 + float64(i), "initcall\x00slow_init+0x0/0x10": 9, "initcall\x00fast_init+0x0/0x10": 1,
			"gap\x00before it\x00after it": 30, "gap\x00x\x00y": 2,
		}})
	}
	// B's own ranking: its acpi_init, 5, not A's 4 with it.
	r.Samples = append(r.Samples, Sample{Tree: "B", Detail: map[string]float64{"initcall\x00b_only+0x0/0x10": 50}})
	got := initcalls{}.Detail(r)
	if !strings.Contains(got, "  B\n   GAP P50      P95  BETWEEN (ms)\n     30.00    30.00  after:  before it") ||
		!strings.Contains(got, "     50.00    50.00  b_only+0x0/0x10\n      9.00     9.00  slow_init") {
		t.Errorf("B's own ranking is not its own:\n%s", got)
	}
	if strings.Count(got, "b_only") != 1 {
		t.Errorf("B's initcall was listed under A:\n%s", got)
	}
	// With no top, thirty initcalls a tree: of 31, the fastest is left out.
	many := map[string]float64{}
	for i := range 31 {
		many[fmt.Sprintf("initcall\x00f%02d_init+0x0/0x10", i)] = float64(100 - i)
	}
	all := (initcalls{}).Detail(&Result{Options: Options{}, Trees: []Manifest{{Name: "A"}}, Samples: []Sample{{Tree: "A", Detail: many}}})
	if !strings.Contains(all, "f29_init") || strings.Contains(all, "f30_init") {
		t.Errorf("with no top, not thirty initcalls:\n%s", all)
	}
	r.Options = Options{"top": "2"}
	for _, want := range []string{
		"  acpi_init                            4.00           5.00\n",
		"  inet_init                               -              -\n",
		"  A\n   GAP P50      P95  BETWEEN (ms)\n     30.00    30.00  after:  before it\n                     before: after it\n       P50      P95  INITCALL\n      9.00     9.00  slow_init+0x0/0x10\n      4.00     4.00  acpi_init+0x0/0x420\n",
		"     30.00    30.00  after:  before it\n                     before: after it\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the detail has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fast_init") || strings.Contains(got, "after:  x\n") {
		t.Errorf("top=2 listed more than two initcalls or one gap:\n%s", got)
	}
}

func TestReadUntilReturnsOnceTheMarkerIsThere(t *testing.T) {
	if got, err := readUntil(strings.NewReader("a b END c"), "END", time.Second); err != nil || got != "a b END c" {
		t.Errorf("readUntil = %q, %v", got, err)
	}
	if got, err := readUntil(strings.NewReader("no marker"), "END", time.Second); err == nil || !strings.Contains(err.Error(), "ended before") || got != "no marker" {
		t.Errorf("at EOF: %q, %v", got, err)
	}
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	if _, err := readUntil(r, "END", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "never saw") {
		t.Errorf("a console that says nothing: %v", err)
	}
	// A console that stops short of the marker: what it said is the tail a lost boot is read by.
	r, w = io.Pipe()
	defer func() { _ = w.Close() }()
	go func() { _, _ = w.Write([]byte("Freeing unused kernel image")) }()
	if got, err := readUntil(r, "END", 200*time.Millisecond); err == nil || got != "Freeing unused kernel image" {
		t.Errorf("a console that stopped short: %q, %v", got, err)
	}
}

func TestALoginKeyGoesIntoTheOverlayOwnedBySpin(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	e := k.Edit()
	if !strings.Contains(e.Setup, "printf '%s' '"+string(k.public)+"'") || !strings.Contains(e.Setup, "chmod 600") ||
		!strings.Contains(e.Files["/etc/systemd/system/serial-getty@ttyS0.service.d/zz-bench.conf"], "SPIN-READY-login") ||
		!strings.Contains(e.Files["/etc/systemd/system/sshd@.service.d/keygen.conf"], "After=spin-machine-sshd-keygen.service") {
		t.Errorf("the key's edit: %+v", e)
	}
	for range 50 {
		if c := NewCID(); c < 100000 || c >= 100000+1<<20 {
			t.Fatalf("cid %d out of its range", c)
		}
	}
	d, err := shortDir()
	if err != nil || !strings.HasPrefix(d, "/tmp/lab") || len(d) > 30 {
		t.Errorf("shortDir = %q, %v", d, err)
	}
	_ = os.RemoveAll(d)
	var c vsockConn
	if c.LocalAddr().Network() != "vsock" || c.RemoteAddr().String() != "vsock" {
		t.Error("vsock addresses")
	}
	if (&MissError{Why: "w"}).Error() != "w" {
		t.Error("MissError")
	}
}

// spin's login is root's, through a forwarder from vsock to ssh.service with Nagle off on its
// loopback side, and systemd's own vsock sshd masked; the lane's is spin's, with no forwarder.
func TestSpinsLoginIsRootThroughAForwarderToSSHService(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if k.User() != "spin" || len(k.Edits()) != 1 {
		t.Errorf("the lane's login: %s, %d edits", k.User(), len(k.Edits()))
	}
	k.SpinsPath = true
	e := k.Edits()
	if k.User() != "root" || len(e) != 2 {
		t.Fatalf("spin's login: %s, %d edits", k.User(), len(e))
	}
	fwd := e[1].Files["/etc/systemd/system/spin-lab-forward.service"]
	if !slices.Equal(e[1].Mask, []string{"sshd-vsock.socket"}) || !strings.Contains(fwd, "TCP:127.0.0.1:22,nodelay") ||
		!strings.Contains(fwd, "After=ssh.service") || !strings.Contains(e[1].Setup, "/root/.ssh/authorized_keys") ||
		e[1].Links["/etc/systemd/system/multi-user.target.wants/spin-lab-forward.service"] == "" {
		t.Errorf("spin's path: %+v", e[1])
	}
	o, err := settle(Probes["ssh-first"], Options{"plugged": "2048", "login": "spin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := shapeOf(o); got != "1 of 16 vCPUs, 128 of 8192 MiB, 16 hotplug disks, sized to 2048 MiB, spin's login" {
		t.Errorf("spin's shape sized, logged into as spin does, is said %q", got)
	}
	if n := plugNeeds(o); !slices.Contains(n, Need{"memory", "to"}) || plugNeeds(Options{"plugged": "0"}) != nil {
		t.Errorf("sizing needs %v", n)
	}
	if err := (&Measurement{Options: Options{"plugged": "0"}}).Plug("/nonexistent")(); err != nil {
		t.Errorf("a guest left at its memory: %v", err)
	}
}

// Every probe that boots spin's shape sizes the guest as the shape says: initcalls, which did not,
// lost 7 of 80 boots of a guest left at 128 MiB in the release reports of 2026-11-20, while its
// record said "sized to 2048 MiB". A probe's Needs naming memory --to is what says it plugs.
func TestEveryProbeInSpinsShapeSizesItsGuest(t *testing.T) {
	for _, name := range ProbeNames() {
		p := Probes[name]
		if !slices.ContainsFunc(p.Options(), func(o Option) bool { return o.Name == "plugged" }) {
			continue
		}
		if !slices.Contains(p.Needs(Options{"plugged": "2048"}), Need{"memory", "to"}) {
			t.Errorf("%s boots spin's shape and never sizes its guest", name)
		}
		if slices.Contains(p.Needs(Options{"plugged": "0"}), Need{"memory", "to"}) {
			t.Errorf("%s sizes a guest the options leave at its memory", name)
		}
	}
	if got := (&Measurement{Options: Options{"plugged": "2048"}}).qmpToPlug("/s/m.qmp"); !slices.Equal(got, []string{"--qmp", "/s/m.qmp"}) {
		t.Errorf("a sized boot's monitor: %v", got)
	}
	if got := (&Measurement{Options: Options{"plugged": "0"}}).qmpToPlug("/s/m.qmp"); got != nil {
		t.Errorf("a boot left at its memory opens a monitor: %v", got)
	}
}

// A state's data is read and its holes are not: a mapped-ram state is as long as the machine's
// RAM ceiling and mostly holes. Data that ends the file, and a file of no data, are read too.
func TestAStatesDataIsReadAndASocketWaitedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{0, 64 << 20, 1 << 30} { // three extents of 1.5 MiB, holes between and after
		if _, err := f.WriteAt(bytes.Repeat([]byte("s"), 1<<20+1<<19), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(2 << 30); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if d, n, err := readData(path); err != nil || d <= 0 || n != 3*(1<<20+1<<19) {
		t.Errorf("readData = %v, %d bytes, %v", d, n, err)
	}
	tail := filepath.Join(t.TempDir(), "tail")
	write(t, tail, bytes.Repeat([]byte("s"), 3<<20))
	if _, n, err := readData(tail); err != nil || n != 3<<20 {
		t.Errorf("readData of data to the end = %d bytes, %v", n, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil || os.Truncate(empty, 1<<30) != nil {
		t.Fatal(err)
	}
	if _, n, err := readData(empty); err != nil || n != 0 {
		t.Errorf("readData of holes alone = %d bytes, %v", n, err)
	}
	if _, _, err := readData(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("read a state that is not there")
	}
	dir, err := shortDir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "q")
	if err := waitSocket(sock, time.Now().Add(10*time.Millisecond)); err == nil {
		t.Error("waited for a socket nothing listens on")
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := waitSocket(sock, time.Now().Add(time.Second)); err != nil {
		t.Errorf("waitSocket: %v", err)
	}
}

func TestTheProbesAreTheGoalsAndTheKernels(t *testing.T) {
	if got := ProbeNames(); !slices.Equal(got, []string{"density", "initcalls", "prestart", "restore-first", "restore-together", "run-delay", "ssh-first", "transport"}) {
		t.Errorf("probes %v", got)
	}
	for _, name := range ProbeNames() {
		p := Probes[name]
		o, err := settle(p, nil)
		if err != nil || p.About() == "" || len(p.Metrics()) == 0 || len(p.Rows(o)) == 0 || p.Phases() < 1 {
			t.Errorf("%s: %v", name, err)
		}
		// A result names its probe by Name, and lab show finds its detail by that name.
		if p.Name() != name {
			t.Errorf("Probes[%q] is named %q", name, p.Name())
		}
	}
	rf := Probes["restore-first"]
	if !slices.Equal(rf.Rows(Options{"fast": "true"}), []string{"evicted", "cached", "fast"}) || rf.Phases() != 2 {
		t.Error("restore-first's rows")
	}
	if Probes["prestart"].Phases() != 2 || Probes["ssh-first"].Phases() != 1 || Probes["initcalls"].Phases() != 1 || Probes["transport"].Phases() != 2 {
		t.Error("phases")
	}
}

// mounts are this host's mount points of each filesystem type, from /proc/mounts.
func mounts(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for line := range strings.Lines(string(b)) {
		if f := strings.Fields(line); len(f) > 2 {
			out[f[2]] = append(out[f[2]], f[1])
		}
	}
	return out
}

func TestATmpfsIsKnownByItsFilesystem(t *testing.T) {
	m := mounts(t)
	if len(m["tmpfs"]) == 0 {
		t.Fatal("this host mounts no tmpfs to ask about")
	}
	if !isTmpfs(m["tmpfs"][0]) {
		t.Errorf("%s is a tmpfs and was not taken for one", m["tmpfs"][0])
	}
	if isTmpfs("/proc") || isTmpfs("/nonexistent") {
		t.Error("procfs, or nothing, taken for a tmpfs")
	}
}

func TestTheScratchIsTMPDIROnADiskOrTheLabsOwn(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	m := mounts(t)
	lab := filepath.Join(t.TempDir(), "lab") // before TMPDIR moves: TempDir is made under it
	// A TMPDIR on a tmpfs is not taken: overlays and states there are held in RAM.
	t.Setenv("TMPDIR", m["tmpfs"][0])
	if got, err := Scratch(lab); err != nil || got != filepath.Join(lab, "scratch") || !fileExists(got) {
		t.Errorf("Scratch with TMPDIR on a tmpfs = %s, %v", got, err)
	}
	t.Setenv("TMPDIR", "")
	if got, err := Scratch(lab); err != nil || got != filepath.Join(lab, "scratch") {
		t.Errorf("Scratch with no TMPDIR = %s, %v", got, err)
	}
	if _, err := Scratch("/proc/no/such/lab"); err == nil {
		t.Error("a scratch directory was made where none can be")
	}
	// A TMPDIR on a disk is taken: the root filesystem is one on every host this runs on.
	if isTmpfs("/") {
		t.Skip("/ is a tmpfs here")
	}
	t.Setenv("TMPDIR", "/")
	if got, err := Scratch(lab); err != nil || got != "/" {
		t.Errorf("Scratch with TMPDIR on a disk = %s, %v", got, err)
	}
}

func TestDispatchWaitsForABusyLabAndFindsItsOwnRun(t *testing.T) {
	now := time.Now()
	runs := []Run{{ID: 1, Status: "completed", CreatedAt: now.Add(-time.Hour)}, {ID: 2, Status: "queued", URL: "u2", CreatedAt: now}}
	if busy(runs) != "u2" || busy(runs[:1]) != "" {
		t.Error("busy")
	}
	if r, ok := started(runs, now.Add(-time.Minute)); !ok || r.ID != 2 {
		t.Errorf("started = %+v, %v", r, ok)
	}
	if _, ok := started(runs, now.Add(time.Minute)); ok {
		t.Error("a run from before the dispatch was taken for its own")
	}
	got := dispatchArgs("o/r", "main", map[string]string{"trees": "a=b", "probe": "ssh-first"})
	if !slices.Equal(got, []string{"workflow", "run", "lab.yml", "-R", "o/r", "--ref", "main", "-f", "probe=ssh-first", "-f", "trees=a=b"}) {
		t.Errorf("dispatchArgs = %v", got)
	}
}
