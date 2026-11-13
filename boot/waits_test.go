// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A real boot's dump, as the console carried it (\r\n and all): task boot:waits RAW=<dir> on
// 2026-11-12, the release as it stood then.
func realWaits(t *testing.T) Waits {
	t.Helper()
	b, err := os.ReadFile("testdata/waits-2026-11-12.txt")
	if err != nil {
		t.Fatal(err)
	}
	m, us, err := ParseWaits(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return NewWaits(m, us)
}

func TestParseWaitsReadsARealBoot(t *testing.T) {
	w := realWaits(t)
	if w.Manager.Userspace == 0 || w.Manager.UnitsLoadFinish <= w.Manager.Userspace || w.Manager.Finish <= w.Manager.UnitsLoadFinish {
		t.Fatalf("manager timeline out of order: %+v", w.Manager)
	}
	// The getty is the login; it must have been read with its Type and its ordering intact.
	g, ok := w.Unit("serial-getty@ttyS0.service")
	if !ok || !g.Idle || g.Start == 0 {
		t.Fatalf("serial-getty@ttyS0.service = %+v, %v", g, ok)
	}
	// A quoted, escaped name in After= is read back to the name the slice has.
	if !slices.Contains(g.After, `system-serial\x2dgetty.slice`) {
		t.Errorf("After= of the getty did not unquote the slice: %q", g.After)
	}
	if _, ok := w.Unit(`system-serial\x2dgetty.slice`); !ok {
		t.Errorf("the getty's slice is not found by the name After= uses")
	}
	for _, u := range w.Units {
		if u.Ready > 0 && u.Start > u.Ready {
			t.Errorf("%s starts at %v after it is ready at %v", u.ID, u.Start, u.Ready)
		}
	}
}

// The property the report rests on: from the end of the unit load to the login, a critical
// path accounts for every microsecond, and each hop waits on a unit it is really ordered after.
func TestCriticalPathAccountsForTheWholeWait(t *testing.T) {
	w := realWaits(t)
	const target = "serial-getty@ttyS0.service"
	path, err := w.CriticalPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if path[len(path)-1].Unit.ID != target {
		t.Fatalf("path ends at %s", path[len(path)-1].Unit.ID)
	}
	sum := w.Manager.UnitsLoadFinish
	for i, h := range path {
		sum += h.Lag + h.Unit.Own()
		if i == 0 {
			if h.Blocker != "" {
				t.Errorf("the root %s has a blocker %s", h.Unit.ID, h.Blocker)
			}
			continue
		}
		prev := path[i-1].Unit
		if h.Blocker != prev.Name() {
			t.Errorf("hop %s names %s as its blocker, the path has %s before it", h.Unit.ID, h.Blocker, prev.Name())
		}
		if !slices.ContainsFunc(h.Unit.After, func(n string) bool { return n == prev.ID || slices.Contains(prev.Names, n) }) {
			t.Errorf("%s waits on %s, which is not in its After=", h.Unit.ID, prev.ID)
		}
		if h.Lag < 0 {
			t.Errorf("%s started %v before %s was ready", h.Unit.ID, -h.Lag, prev.ID)
		}
	}
	if got := w.Units[target].Ready; sum != got {
		t.Errorf("unit load + lags + own times = %v, the target is ready at %v", sum, got)
	}
}

// A gap is a time no unit's process was working; one that overlaps a unit's activation is a
// gap that is not there.
func TestGapsOverlapNoActivation(t *testing.T) {
	w := realWaits(t)
	until := w.Units["serial-getty@ttyS0.service"].Start
	gaps := w.Gaps(until, 0)
	if len(gaps) == 0 {
		t.Fatal("no gaps in a boot with devices to wait for")
	}
	for _, g := range gaps {
		if g.From < w.Manager.UnitsLoadFinish || g.To > until || g.To <= g.From {
			t.Errorf("gap %v-%v outside %v-%v", g.From, g.To, w.Manager.UnitsLoadFinish, until)
		}
		for _, u := range w.Units {
			if strings.HasSuffix(u.ID, ".device") || u.Ready <= u.Start {
				continue
			}
			if u.Start < g.To && u.Ready > g.From {
				t.Errorf("gap %v-%v overlaps %s activating %v-%v", g.From, g.To, u.ID, u.Start, u.Ready)
			}
		}
	}
}

func unitBlock(id, after string, exit, active, inactive int) string {
	return "Id=" + id + "\nNames=" + id + "\nAfter=" + after +
		"\nActiveState=active" +
		"\nInactiveExitTimestampMonotonic=" + strconv.Itoa(exit) +
		"\nActiveEnterTimestampMonotonic=" + strconv.Itoa(active) +
		"\nInactiveEnterTimestampMonotonic=" + strconv.Itoa(inactive) + "\n\n"
}

const manager = "UserspaceTimestampMonotonic=100\nSecurityStartTimestampMonotonic=110\nSecurityFinishTimestampMonotonic=120\n" +
	"GeneratorsStartTimestampMonotonic=130\nGeneratorsFinishTimestampMonotonic=140\n" +
	"UnitsLoadStartTimestampMonotonic=150\nUnitsLoadFinishTimestampMonotonic=1000\nFinishTimestampMonotonic=9000\n\n"

// The blocker is the dependency ready last before the unit started; a oneshot that finished is
// ready when it finished; a dependency ready only after the unit started was not ordered against
// it and is not its blocker.
func TestBlocker(t *testing.T) {
	dump := manager + "SPIN-WAITS-UNITS\n" +
		unitBlock("early.service", "", 1000, 1500, 0) +
		unitBlock("oneshot.service", "", 1000, 0, 2500) + // ran 1000-2500, did not remain
		unitBlock("later.service", "", 1000, 5000, 0) + // ready after the unit below started
		unitBlock("never.service", "", 0, 0, 0) + // loaded, never started: dropped
		unitBlock("unit.service", "early.service oneshot.service later.service never.service", 3000, 3200, 0)
	m, us, err := ParseWaits(dump)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWaits(m, us)
	if _, ok := w.Units["never.service"]; ok {
		t.Error("a unit that never started was kept")
	}
	if got := w.Units["oneshot.service"].Ready; got != 2500*time.Microsecond {
		t.Errorf("a finished oneshot is ready at %v, want 2.5ms", got)
	}
	b, ok := w.Blocker(w.Units["unit.service"])
	if !ok || b.ID != "oneshot.service" {
		t.Errorf("blocker = %s, %v; want oneshot.service", b.ID, ok)
	}
	path, err := w.CriticalPath("unit.service")
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 || path[0].Lag != 0 || path[1].Lag != 500*time.Microsecond {
		t.Errorf("path = %+v; want oneshot from the unit load, then unit 0.5ms after it", path)
	}
	late := w.After(3100 * time.Microsecond)
	if len(late) != 2 || late[0].ID != "unit.service" || late[1].ID != "later.service" {
		t.Errorf("after 3.1ms: %v; want unit then later", late)
	}
}

// A dump that is not one is an error that says so, never an empty boot: the probe reading too
// early, or a systemd that renamed a property, would otherwise report a boot in which nothing
// waited.
func TestParseWaitsRefuses(t *testing.T) {
	for name, dump := range map[string]string{
		"no separator":      manager,
		"unfinished":        strings.Replace(manager, "FinishTimestampMonotonic=9000", "FinishTimestampMonotonic=0", 1) + "SPIN-WAITS-UNITS\n" + unitBlock("a.service", "", 1, 2, 0),
		"renamed property":  manager + "SPIN-WAITS-UNITS\nId=a.service\nInactiveExitTimestampMonotonic=1\nActiveEnterTimestampMonotonic=2\n",
		"no userspace":      strings.Replace(manager, "UserspaceTimestampMonotonic=100", "UserspaceTimestampMonotonic=0", 1) + "SPIN-WAITS-UNITS\n" + unitBlock("a.service", "", 1, 2, 0),
		"nothing started":   manager + "SPIN-WAITS-UNITS\n" + unitBlock("a.service", "", 0, 0, 0),
		"systemctl refused": manager + "SPIN-WAITS-UNITS\nsystemctl: invalid option -- '.'\n",
	} {
		if _, _, err := ParseWaits(dump); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The name a unit is reported and compared by is its shortest, and of two as short the first in
// order: a device's Id is whichever alias systemd met first.
func TestUnitNameIsTheShortest(t *testing.T) {
	for _, c := range []struct {
		u    Unit
		want string
	}{
		{Unit{ID: "sys-devices-virtio2-block-vda.device", Names: []string{"sys-devices-virtio2-block-vda.device", "dev-vda.device", "dev-disk-by-path-x.device"}}, "dev-vda.device"},
		{Unit{ID: "b.device", Names: []string{"b.device", "a.device"}}, "a.device"},
		{Unit{ID: "a.device", Names: []string{"b.device", "a.device"}}, "a.device"},
		{Unit{ID: "a.service", Names: []string{"longer.service"}}, "a.service"},
		{Unit{ID: "a.service"}, "a.service"},
	} {
		if got := c.u.Name(); got != c.want {
			t.Errorf("%v: Name() = %q, want %q", c.u.Names, got, c.want)
		}
	}
}

func us(n int) time.Duration { return time.Duration(n) * time.Microsecond }

// Each way a unit can be in the dump, read to the start and readiness the rest of this file
// rests on - at the smallest values, where an off-by-one in a comparison with zero shows.
func TestParseWaitsReadsEachKindOfUnit(t *testing.T) {
	typed := func(id, typ string, exit, active, inactive int) string {
		b := unitBlock(id, "", exit, active, inactive)
		return strings.TrimSuffix(b, "\n") + "Type=" + typ + "\n\n"
	}
	dump := manager + "SPIN-WAITS-UNITS\n" +
		unitBlock("dev.device", "", 0, 5, 0) + // plugged: no exit, ready when active
		unitBlock("restarted.service", "", 9, 4, 0) + // left inactive again after it was ready
		unitBlock("oneshot.service", "", 2, 0, 7) + // ran and did not remain
		unitBlock("tiny.service", "", 1, 1, 0) + // active at the first microsecond
		unitBlock("short.service", "", 1, 0, 3) + // a oneshot that started at the first
		unitBlock("early.service", "", 1, 5, 0) +
		unitBlock("activating.service", "", 1, 0, 0) + // started, not ready when read
		unitBlock("stopped.service", "", 0, 0, 3) + // inactive, never started: not this boot's
		typed("idle.service", "idle", 2, 3, 0) +
		typed("simple.service", "simple", 2, 3, 0) +
		typed("tmp.mount", "idle", 2, 3, 0) // a mount's Type is its filesystem
	m, us0, err := ParseWaits(dump)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWaits(m, us0)
	for _, c := range []struct {
		id           string
		start, ready int
		idle         bool
	}{
		{"dev.device", 5, 5, false},
		{"restarted.service", 4, 4, false},
		{"oneshot.service", 2, 7, false},
		{"tiny.service", 1, 1, false},
		{"short.service", 1, 3, false},
		{"early.service", 1, 5, false},
		{"activating.service", 1, 0, false},
		{"idle.service", 2, 3, true},
		{"simple.service", 2, 3, false},
		{"tmp.mount", 2, 3, false},
	} {
		u, ok := w.Units[c.id]
		if !ok {
			t.Errorf("%s: dropped", c.id)
			continue
		}
		if u.Start != us(c.start) || u.Ready != us(c.ready) || u.Idle != c.idle {
			t.Errorf("%s: start %v ready %v idle %v; want %v %v %v", c.id, u.Start, u.Ready, u.Idle, us(c.start), us(c.ready), c.idle)
		}
	}
	if _, ok := w.Units["stopped.service"]; ok {
		t.Error("a unit that never left inactive in this boot was kept")
	}
}

// The dump arrives through a console: a line that is not Key=Value is noise that neither ends a
// block nor starts one, and only a blank line ends one.
func TestBlocksReadKeyValueLinesOnly(t *testing.T) {
	got := blocks("Id=a\r\n kernel noise\nnot a key=1\n=empty\nAfter=x y\n\n\nId=b\nAfter=\n")
	if len(got) != 2 || got[0]["Id"] != "a" || got[0]["After"] != "x y" || got[1]["Id"] != "b" {
		t.Fatalf("blocks = %v", got)
	}
	if _, ok := got[0]["not a key"]; ok {
		t.Error("a line with a space before its = was read as a key")
	}
	if _, ok := got[0][""]; ok {
		t.Error("a line starting with = was read as a key")
	}
	if v, ok := got[1]["After"]; !ok || v != "" {
		t.Errorf("an empty value was not kept: %q, %v", v, ok)
	}
}

func unitAt(id string, start, ready int, after ...string) Unit {
	return Unit{ID: id, Start: us(start), Ready: us(ready), After: after}
}

// What a unit waited on: the After= dependency ready last by the time it started - itself, a
// unit never ready, and one ready only later excluded - with ties to the one that took longer,
// then by name.
func TestBlockerRules(t *testing.T) {
	w := NewWaits(Manager{}, []Unit{
		unitAt("aa.target", 100, 100, "aa.target", "late.service", "activating.service", "edge.service", "early.service"),
		unitAt("late.service", 120, 150),
		unitAt("activating.service", 50, 0),
		unitAt("edge.service", 100, 100),
		unitAt("early.service", 40, 50),
		unitAt("waits-on-nothing.service", 100, 110, "activating.service"),
		unitAt("first.service", 100, 110, "one.service"),
		unitAt("one.service", 1, 1),
		unitAt("tied.service", 200, 210, "c.service", "a.service", "b.service"),
		// A name earlier in order does not beat a dependency that took longer.
		unitAt("tied-by-own.service", 200, 210, "c.service", "a.service"),
		unitAt("a.service", 145, 150),
		unitAt("b.service", 130, 150),
		unitAt("c.service", 130, 150),
	})
	for _, c := range []struct{ unit, want string }{
		{"aa.target", "edge.service"},
		{"waits-on-nothing.service", ""},
		{"first.service", "one.service"},
		{"tied.service", "b.service"},
		{"tied-by-own.service", "c.service"},
	} {
		b, ok := w.Blocker(w.Units[c.unit])
		if got := map[bool]string{true: b.ID}[ok]; got != c.want {
			t.Errorf("%s waited on %q, want %q", c.unit, got, c.want)
		}
	}
}

// Gaps are the intervals no unit was activating: a device's activation is not work, a unit still
// activating is busy to the end, a gap shorter than the minimum is left out and one exactly as
// long is kept, and each says which unit ended it and what that unit had waited on.
func TestGapsBoundaries(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	at := func(id string, start, ready int, after ...string) Unit {
		return Unit{ID: id, Start: ms(start), Ready: ms(ready), After: after}
	}
	units := []Unit{
		{ID: "before.service", Start: 0, Ready: us(1)}, // ended before the unit load did
		at("s1.service", 10, 20),
		at("vda.device", 12, 60),
		at("dep.target", 29, 29),
		at("s2.service", 30, 40, "dep.target"),
		at("blip.service", 42, 43),
		at("act.service", 48, 0),
	}
	w := NewWaits(Manager{UnitsLoadFinish: ms(10)}, units)
	got := w.Gaps(ms(60), ms(5))
	want := []Gap{
		{From: ms(20), To: ms(30), Next: "s2.service", NextBlocker: "dep.target"},
		{From: ms(43), To: ms(48), Next: "act.service"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("gaps = %+v\nwant %+v", got, want)
	}
	// Without the unit still activating, the boot ends idle, and that is a gap to until.
	w = NewWaits(Manager{UnitsLoadFinish: ms(10)}, units[:len(units)-1])
	got = w.Gaps(ms(60), ms(5))
	if last := got[len(got)-1]; last.From != ms(43) || last.To != ms(60) || last.Next != "" {
		t.Errorf("the last gap is %+v, want 43ms to 60ms ended by nothing", last)
	}
}

// The unit that ended a gap: the first to start at or after it, one that took time before one
// that did not, then by name; never a device.
func TestStartingAt(t *testing.T) {
	w := NewWaits(Manager{}, []Unit{
		unitAt("before.service", 5, 50),
		unitAt("z.device", 10, 30),
		unitAt("instant.target", 10, 10),
		unitAt("0-instant.target", 10, 10),
		unitAt("b.service", 10, 20),
		unitAt("a.service", 10, 20),
		unitAt("later.service", 11, 12),
	})
	// The units are a map, and which of the tied ones is met first changes from call to call: the
	// answer may not.
	for range 64 {
		if u, ok := w.startingAt(us(10)); !ok || u.ID != "a.service" {
			t.Fatalf("startingAt(10us) = %s, %v; want a.service", u.ID, ok)
		}
	}
	if u, ok := w.startingAt(us(11)); !ok || u.ID != "later.service" {
		t.Errorf("startingAt(11us) = %s, %v; want later.service", u.ID, ok)
	}
	if _, ok := w.startingAt(us(12)); ok {
		t.Error("startingAt past every start found one")
	}
}

// Behind the prompt: every unit not ready by t, by when it was, the ones never ready last and
// by name; devices left out, and a unit ready exactly at t is not behind.
func TestAfterOrder(t *testing.T) {
	w := NewWaits(Manager{}, []Unit{
		unitAt("ready.service", 1, 5),
		unitAt("at.service", 1, 10),
		unitAt("b-never.service", 1, 0),
		unitAt("a-never.service", 1, 0),
		unitAt("one.service", 1, 11),
		unitAt("late.service", 1, 30),
		unitAt("late.device", 1, 40),
	})
	want := []string{"one.service", "late.service", "a-never.service", "b-never.service"}
	// From a map: the order the sort meets them in changes from call to call, and the answer may not.
	for range 64 {
		var got []string
		for _, u := range w.After(us(10)) {
			got = append(got, u.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("After(10us) = %v, want %v", got, want)
		}
	}
}
