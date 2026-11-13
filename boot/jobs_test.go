// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A real boot's record and the waits dump of the same boot: task boot:jobs RAW=<dir> on
// 2026-11-12, the release as it stood then.
func realJobs(t *testing.T) (JobLog, Waits) {
	t.Helper()
	b, err := os.ReadFile("testdata/jobs-2026-11-12.txt")
	if err != nil {
		t.Fatal(err)
	}
	l, err := ParseJobs(string(b))
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("testdata/jobs-waits-2026-11-12.txt")
	if err != nil {
		t.Fatal(err)
	}
	m, us, err := ParseWaits(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return l, NewWaits(m, us)
}

func TestParseJobsReadsARealBoot(t *testing.T) {
	l, w := realJobs(t)
	if l.Connected == 0 || l.Finished <= l.Connected {
		t.Fatalf("connected at %v, finished at %v", l.Connected, l.Finished)
	}
	if len(l.Jobs) == 0 {
		t.Fatal("no jobs")
	}
	for _, j := range l.Jobs {
		if j.Unit == "" {
			t.Errorf("job %d has no unit", j.ID)
		}
		if j.Removed > 0 && j.Result == "" {
			t.Errorf("job %d (%s) removed with no result", j.ID, j.Unit)
		}
		if j.Running > 0 && j.Removed > 0 && j.Running > j.Removed {
			t.Errorf("job %d (%s) running at %v after it was removed at %v", j.ID, j.Unit, j.Running, j.Removed)
		}
	}
	// The record and the timestamps are two accounts of one boot: jobwatch cannot have heard
	// a unit become active before systemd stamped it, and should not be far behind.
	lag := l.Lag(w)
	if len(lag) == 0 {
		t.Fatal("no unit heard becoming active")
	}
	for _, d := range lag {
		if d < 0 || d > 100*time.Millisecond {
			t.Errorf("a lag of %v", d)
		}
	}
}

func TestPending(t *testing.T) {
	l, err := ParseJobs(`
1000 C
1001 L 1 a.service start waiting
1002 L 2 b.service start running
1500 N 3 c.service
2000 A a.service activating
3000 R 2 b.service done
4000 R 1 a.service failed
5000 R 3 c.service done
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at               int
		waiting, running int
	}{
		{999, 0, 0},  // before jobwatch connected, nothing was seen
		{1500, 2, 1}, // a waiting, c just queued, b running since listed
		{2500, 1, 2}, // a's unit activating is a running
		{3500, 1, 1}, // b removed
		{5000, 0, 0}, // c removed at that instant
	} {
		w, r := l.Pending(time.Duration(c.at) * time.Microsecond)
		if len(w) != c.waiting || len(r) != c.running {
			t.Errorf("at %dus: %d waiting, %d running; want %d, %d", c.at, len(w), len(r), c.waiting, c.running)
		}
	}
	if u := l.Unfinished(); len(u) != 1 || u[0].Unit != "a.service" {
		t.Errorf("unfinished = %+v", u)
	}
	if late := l.Late(); len(late) != 1 || late[0].Unit != "c.service" {
		t.Errorf("late = %+v", late)
	}
}

// A record that ends in an error, or never subscribed, is that and not a boot with no jobs.
func TestParseJobsRefuses(t *testing.T) {
	for name, rec := range map[string]string{
		"error":         "1000 C\n2000 E subscribing to PID 1: timeout\n",
		"no subscribe":  "1001 L 1 a.service start waiting\n",
		"short line":    "1000 C\n1001 R 1 a.service\n",
		"unknown event": "1000 C\n1001 Q 1\n",
	} {
		if _, err := ParseJobs(rec); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// Every kind of event has its number of fields, and a line with fewer is an error, not an event
// read short; an E line is jobwatch's own error, said in its words.
func TestParseJobsHoldsEachEventToItsFields(t *testing.T) {
	for _, short := range []string{
		"1001 L 1 a.service start", "1001 N 1", "1001 S 1", "1001 R 1 a.service", "1001 A a.service",
	} {
		if _, err := ParseJobs("1000 C\n" + short + "\n"); err == nil {
			t.Errorf("%q: parsed", short)
		}
	}
	_, err := ParseJobs("1000 C\n2000 E subscribing to PID 1: timeout\n")
	if err == nil || !strings.Contains(err.Error(), "jobwatch stopped: subscribing to PID 1: timeout") {
		t.Errorf("an E line read as %v", err)
	}
	if _, err := ParseJobs("1000 C\n2000 E\n"); err == nil || !strings.Contains(err.Error(), "jobwatch stopped") {
		t.Errorf("a bare E line read as %v", err)
	}
	l, err := ParseJobs("1000 C\n1500 F\n9000 F already\n")
	if err != nil || l.Finished != 1500*time.Microsecond {
		t.Errorf("finished at %v (%v); want the plain F line's 1.5ms", l.Finished, err)
	}
}

// What jobwatch saw of each job: queued before it connected or after, when it ran - the first
// state change to running, or else its unit leaving inactive while the job was there - and the
// smallest timestamps, where a comparison with zero is off by one or not.
func TestParseJobsReadsEachJob(t *testing.T) {
	l, err := ParseJobs(`
1 C
1 L 7 x.service start waiting
1 L 8 y.service start running
1 N 9 z.service
1 R 7 x.service done
2 S 10 running
3 S 10 running
3 R 10 seen-removed-only.service done
5 R 11 removed-only.service done
1500 N 20 c.service
1400 A c.service activating
1600 A other.service activating
1700 A c.service deactivating
1800 A c.service active
5000 R 20 c.service done
6000 A c.service activating
2000 N 21 pending.service
2100 A pending.service activating
`)
	if err != nil {
		t.Fatal(err)
	}
	job := map[string]Job{}
	for _, j := range l.Jobs {
		job[j.Unit] = j
	}
	if j := job["seen-removed-only.service"]; j.Running != us(2) {
		t.Errorf("the first state change to running is at %v, want 2us", j.Running)
	}
	if j := job["c.service"]; j.Running != us(1800) {
		t.Errorf("c.service ran from %v, want 1.8ms: its unit becoming active after it was queued", j.Running)
	}
	if j := job["pending.service"]; j.Running != us(2100) || j.Removed != 0 {
		t.Errorf("a job still pending ran from %v, removed %v; want 2.1ms and not removed", j.Running, j.Removed)
	}
	var late []string
	for _, j := range l.Late() {
		late = append(late, j.Unit)
	}
	if want := []string{"z.service", "c.service", "pending.service"}; !slices.Equal(late, want) {
		t.Errorf("late = %v, want %v: a job listed or seen only removed was queued before", late, want)
	}
	// At the first microsecond: x was removed, y was running since it was listed, z had just
	// been queued, and the two seen only later were there from when jobwatch connected.
	waiting, running := l.Pending(us(1))
	if len(running) != 1 || running[0].Unit != "y.service" {
		t.Errorf("running at 1us: %v", running)
	}
	var w []string
	for _, j := range waiting {
		w = append(w, j.Unit)
	}
	slices.Sort(w)
	if want := []string{"removed-only.service", "seen-removed-only.service", "z.service"}; !slices.Equal(w, want) {
		t.Errorf("waiting at 1us: %v, want %v", w, want)
	}
}

// How far behind systemd jobwatch heard a unit become active: once per unit, only for a unit
// that became active after jobwatch subscribed, never a stamp before systemd's.
func TestLagCountsEachUnitOnce(t *testing.T) {
	l, err := ParseJobs(`
1000 C
1600 A u.service active
1700 A u.service active
1400 A odd.service active
950 A early.service active
1200 A v.service activating
1300 A gone.service active
`)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWaits(Manager{}, []Unit{
		unitAt("u.service", 1400, 1500),
		unitAt("odd.service", 1400, 1500),
		unitAt("early.service", 800, 900),
		unitAt("v.service", 1100, 1150),
	})
	if got := l.Lag(w); !slices.Equal(got, []time.Duration{us(100)}) {
		t.Errorf("lag = %v, want [100us]", got)
	}
}
