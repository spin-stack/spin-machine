// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"os"
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
