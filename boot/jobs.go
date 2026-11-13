// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What PID 1 said about its jobs while the guest booted, as boot/jobwatch recorded it: the
// live complement to Waits. Waits knows when each unit started and was ready; a JobLog knows
// what systemd was holding in its queue at each moment, which job it was running, how each
// one ended, and the jobs queued after the initial transaction.
//
// Every time here is when jobwatch received the signal, on guest CLOCK_MONOTONIC: later than
// the event by the signal's trip, which JobLog.Lag measures against systemd's own timestamps.

// Job is one job's life. Queued is zero for a job of the initial transaction, which was
// queued before jobwatch connected; Running is zero for a job that went from waiting straight
// to removed without being reported running, which a job that finishes in the same dispatch
// does.
type Job struct {
	ID      int
	Unit    string
	Initial bool // queued before jobwatch connected: listed, not announced
	Queued  time.Duration
	Running time.Duration
	Removed time.Duration
	Result  string // done, canceled, timeout, failed, dependency, skipped; empty while pending
}

// UnitState is a unit's ActiveState changing.
type UnitState struct {
	At    time.Duration
	Unit  string
	State string
}

// JobLog is one boot's record.
type JobLog struct {
	Connected time.Duration // Subscribe returned: nothing before this was seen live
	Finished  time.Duration // StartupFinished arrived; zero when it came before Connected
	Jobs      []Job
	States    []UnitState
}

// ParseJobs reads jobwatch's record: one event per line, "<usec> <kind> <fields>".
func ParseJobs(s string) (JobLog, error) {
	var l JobLog
	byID := map[int]int{}
	job := func(id string) (*Job, error) {
		n, err := strconv.Atoi(id)
		if err != nil {
			return nil, fmt.Errorf("job id %q: %w", id, err)
		}
		i, ok := byID[n]
		if !ok {
			i = len(l.Jobs)
			byID[n] = i
			l.Jobs = append(l.Jobs, Job{ID: n})
		}
		return &l.Jobs[i], nil
	}
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 2 {
			continue
		}
		us, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue // the console around the record
		}
		at := time.Duration(us) * time.Microsecond
		want := map[string]int{"C": 2, "F": 2, "L": 6, "N": 4, "S": 4, "R": 5, "A": 4, "E": 2}[f[1]]
		if want == 0 || len(f) < want {
			return JobLog{}, fmt.Errorf("event %q: unknown, or fewer than %d fields", line, want)
		}
		switch f[1] {
		case "E":
			return JobLog{}, fmt.Errorf("jobwatch stopped: %s", strings.Join(f[2:], " "))
		case "C":
			l.Connected = at
		case "F":
			if len(f) == 2 {
				l.Finished = at
			}
		case "L":
			j, err := job(f[2])
			if err != nil {
				return JobLog{}, err
			}
			j.Unit, j.Initial = f[3], true
			if f[5] == "running" {
				j.Running = at
			}
		case "N":
			j, err := job(f[2])
			if err != nil {
				return JobLog{}, err
			}
			j.Unit, j.Queued = f[3], at
		case "S":
			j, err := job(f[2])
			if err != nil {
				return JobLog{}, err
			}
			if f[3] == "running" && j.Running == 0 {
				j.Running = at
			}
		case "R":
			j, err := job(f[2])
			if err != nil {
				return JobLog{}, err
			}
			j.Unit, j.Removed, j.Result = f[3], at, f[4]
		case "A":
			l.States = append(l.States, UnitState{At: at, Unit: f[2], State: f[3]})
		}
	}
	if l.Connected == 0 {
		return JobLog{}, fmt.Errorf("no C line: jobwatch never subscribed")
	}
	// A job whose only trace is a state change or a removal was queued before jobwatch
	// connected and finished before ListJobs answered: it was there, and nothing more is known.
	for i := range l.Jobs {
		j := &l.Jobs[i]
		if j.Queued == 0 {
			j.Initial = true
		}
		// systemd announces few of its jobs' state changes - 5 of 28 in a boot on 2026-11-12 -
		// and a job that ran unannounced would read as waiting until it was removed. Its unit
		// leaving inactive is the job running, and that is announced.
		if j.Running == 0 {
			from := max(j.Queued, l.Connected)
			for _, s := range l.States {
				if s.Unit == j.Unit && s.At >= from && (j.Removed == 0 || s.At <= j.Removed) &&
					(s.State == "activating" || s.State == "active") {
					j.Running = s.At
					break
				}
			}
		}
	}
	return l, nil
}

// Pending is the jobs in PID 1's queue at t, split by whether systemd was running them or
// they were waiting on another job. A job of the initial transaction counts from Connected,
// since nothing earlier was seen.
func (l JobLog) Pending(t time.Duration) (waiting, running []Job) {
	for _, j := range l.Jobs {
		from := j.Queued
		if j.Initial {
			from = l.Connected
		}
		if t < from || (j.Removed > 0 && t >= j.Removed) {
			continue
		}
		if j.Running > 0 && t >= j.Running {
			running = append(running, j)
		} else {
			waiting = append(waiting, j)
		}
	}
	return waiting, running
}

// Unfinished is every job that did not end done: a failure, a timeout, a job cancelled or
// skipped. A dependency result is a job that failed only because another did.
func (l JobLog) Unfinished() []Job {
	var out []Job
	for _, j := range l.Jobs {
		if j.Result != "done" {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b Job) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Late is every job queued after jobwatch connected, by when.
func (l JobLog) Late() []Job {
	var out []Job
	for _, j := range l.Jobs {
		if !j.Initial {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b Job) int { return cmp.Or(cmp.Compare(a.Queued, b.Queued), cmp.Compare(a.ID, b.ID)) })
	return out
}

// Lag is how far behind systemd's own clock jobwatch heard a unit become active: the stamp
// of each ActiveState=active it received against the unit's ActiveEnterTimestampMonotonic,
// for every unit Waits has. It is what "live" means for this record.
//
// Only the first active a unit is heard to enter, and only for a unit that entered it after
// jobwatch subscribed: systemd sends ActiveState with every change to a unit's properties, so
// a unit already active is announced again and again, and counting those read a p95 of 42 ms
// that was a unit's age and not a delay (2026-11-12).
func (l JobLog) Lag(w Waits) []time.Duration {
	var out []time.Duration
	seen := map[string]bool{}
	for _, s := range l.States {
		if s.State != "active" || seen[s.Unit] {
			continue
		}
		seen[s.Unit] = true
		u, ok := w.Unit(s.Unit)
		if !ok || u.Ready < l.Connected || s.At < u.Ready {
			continue
		}
		out = append(out, s.At-u.Ready)
	}
	return out
}
