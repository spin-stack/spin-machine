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
		"nothing started":   manager + "SPIN-WAITS-UNITS\n" + unitBlock("a.service", "", 0, 0, 0),
		"systemctl refused": manager + "SPIN-WAITS-UNITS\nsystemctl: invalid option -- '.'\n",
	} {
		if _, _, err := ParseWaits(dump); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
