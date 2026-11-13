// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"bufio"
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What waited on what, in early userspace.
//
// `systemd-analyze critical-chain` lists, for each unit, the After= dependency that finished
// last. That is a list of what finished last, not of what was waiting: it does not separate a
// unit's own run time from the time it spent queued behind another one, and it does not say
// when nothing was running at all. bootvine (codeberg.org/wvhulle/bootvine) draws the live
// version of the answer on a console - a unit nested under the After= dependency that still
// has a job pending - and this is the same rule applied after the fact, from the timestamps
// systemd keeps for every unit, so that it can be read without a screen and repeated.
//
// After the fact rather than live because a live listener is a process on the guest's vCPUs
// during exactly the phase being measured; the price is that a job's queue time is not kept,
// so a unit's wait is measured from the moment its last ordering dependency was ready, not
// from the moment its job was queued. Every job of the initial transaction is queued at once,
// so the two only differ for a job queued later (socket or path activation), which this does
// not follow.

// Manager is PID 1's own timeline, as guest CLOCK_MONOTONIC: what systemd did before it ran
// its first job.
type Manager struct {
	Userspace        time.Duration // PID 1 exec'd: the kernel handed over
	SecurityStart    time.Duration
	SecurityFinish   time.Duration
	GeneratorsStart  time.Duration
	GeneratorsFinish time.Duration
	UnitsLoadStart   time.Duration
	UnitsLoadFinish  time.Duration
	Finish           time.Duration // the initial transaction's last job completed
}

// Unit is one unit's activation in a boot, as guest CLOCK_MONOTONIC.
type Unit struct {
	ID string
	// Names is every name it answers to; After= may use any of them.
	Names []string
	State string // ActiveState when it was read
	// Idle is a service of Type=idle, whose process systemd forks and then holds until the
	// job queue is empty: its Ready is when it was forked, and what it waited for after that
	// is in no unit's timestamps.
	Idle  bool
	After []string
	// Start is when it left inactive; Ready is when a unit ordered after it could start: when
	// it became active, or, for a oneshot that does not remain, when it was done. Zero Ready
	// is a unit that never got there.
	Start, Ready time.Duration
}

// Name is the unit's shortest name, which is the one to report and to compare across boots: a
// device's Id changes from boot to boot and dev-vda.device does not.
func (u Unit) Name() string {
	n := u.ID
	for _, m := range u.Names {
		if len(m) < len(n) || (len(m) == len(n) && m < n) {
			n = m
		}
	}
	return n
}

// Own is what a unit took itself, from starting to ready.
func (u Unit) Own() time.Duration { return u.Ready - u.Start }

// ParseWaits reads what the probe's unit prints: `systemctl show` of the manager, a line
// "SPIN-WAITS-UNITS", then `systemctl show '*'` - one block of Key=Value lines per unit,
// separated by blank lines. Units that were never started in this boot are left out.
func ParseWaits(s string) (Manager, []Unit, error) {
	head, units, ok := strings.Cut(s, "SPIN-WAITS-UNITS")
	if !ok {
		return Manager{}, nil, fmt.Errorf("no SPIN-WAITS-UNITS line between the manager and the units")
	}
	mb := blocks(head)
	if len(mb) != 1 {
		return Manager{}, nil, fmt.Errorf("want one block of manager properties, found %d", len(mb))
	}
	var m Manager
	var err error
	for _, f := range []struct {
		key string
		dst *time.Duration
	}{
		{"UserspaceTimestampMonotonic", &m.Userspace},
		{"SecurityStartTimestampMonotonic", &m.SecurityStart},
		{"SecurityFinishTimestampMonotonic", &m.SecurityFinish},
		{"GeneratorsStartTimestampMonotonic", &m.GeneratorsStart},
		{"GeneratorsFinishTimestampMonotonic", &m.GeneratorsFinish},
		{"UnitsLoadStartTimestampMonotonic", &m.UnitsLoadStart},
		{"UnitsLoadFinishTimestampMonotonic", &m.UnitsLoadFinish},
		{"FinishTimestampMonotonic", &m.Finish},
	} {
		if *f.dst, err = usec(mb[0], f.key); err != nil {
			return Manager{}, nil, fmt.Errorf("reading the manager: %w", err)
		}
	}
	// A boot that has not finished has no answer to how it finished, and the probe waits for
	// this; zero here is the probe reading too early, not a fast boot.
	if m.Userspace == 0 || m.Finish == 0 {
		return Manager{}, nil, fmt.Errorf("the manager's userspace or finish timestamp is zero: the boot had not finished")
	}

	var us []Unit
	for _, b := range blocks(units) {
		u := Unit{ID: b["Id"], Names: names(b["Names"]), State: b["ActiveState"]}
		if u.ID == "" {
			return Manager{}, nil, fmt.Errorf("a unit block with no Id: %v", b)
		}
		// Type is the filesystem on a mount and the service type only on a service.
		u.Idle = strings.HasSuffix(u.ID, ".service") && b["Type"] == "idle"
		u.After = names(b["After"])
		var exit, active, inactive time.Duration
		for _, f := range []struct {
			key string
			dst *time.Duration
		}{
			{"InactiveExitTimestampMonotonic", &exit},
			{"ActiveEnterTimestampMonotonic", &active},
			{"InactiveEnterTimestampMonotonic", &inactive},
		} {
			if *f.dst, err = usec(b, f.key); err != nil {
				return Manager{}, nil, fmt.Errorf("reading %s: %w", u.ID, err)
			}
		}
		switch {
		case active > 0:
			u.Ready = active
		case inactive > exit && exit > 0:
			// A oneshot that does not remain active goes activating -> inactive, and a unit
			// ordered after it starts when it is done; a failed unit lands here too.
			u.Ready = inactive
		}
		u.Start = exit
		if u.Start == 0 || (u.Ready > 0 && u.Start > u.Ready) {
			// Active without ever leaving inactive in this boot - a device, the root mount, a
			// slice - or left inactive again since; either way it was ready when it said so.
			u.Start = u.Ready
		}
		if u.Start == 0 && u.Ready == 0 {
			continue
		}
		us = append(us, u)
	}
	if len(us) == 0 {
		return Manager{}, nil, fmt.Errorf("no unit was started in this boot")
	}
	return m, us, nil
}

// names splits a list of unit names as systemctl prints it: a name with a backslash in it is
// quoted and the backslash doubled - "system-serial\\x2dgetty.slice" - and has to be read back
// to match the unit's own Names.
func names(s string) []string {
	f := strings.Fields(s)
	for i, n := range f {
		if u, err := strconv.Unquote(n); err == nil {
			f[i] = u
		}
	}
	return f
}

func blocks(s string) []map[string]string {
	var out []map[string]string
	var cur map[string]string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			// A blank line ends a block; anything else that is not Key=Value is the console
			// around the dump, and ends nothing.
			if strings.TrimSpace(line) == "" && cur != nil {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		if cur == nil {
			cur = map[string]string{}
		}
		cur[k] = v
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

// usec reads a *TimestampMonotonic property. A missing one is an error rather than zero: a
// systemd that renamed it would otherwise report every unit as never started.
func usec(b map[string]string, key string) (time.Duration, error) {
	v, ok := b[key]
	if !ok {
		return 0, fmt.Errorf("no %s", key)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", key, v, err)
	}
	return time.Duration(n) * time.Microsecond, nil
}

// Waits is one boot's units, indexed, with the questions this file asks of them.
type Waits struct {
	Manager Manager
	// Units is every unit by its Id. byName finds one by any of its names, which is how
	// After= refers to it.
	Units  map[string]Unit
	byName map[string]Unit
}

// NewWaits indexes a boot's units by Id and by every other name they have.
func NewWaits(m Manager, us []Unit) Waits {
	w := Waits{Manager: m, Units: make(map[string]Unit, len(us)), byName: map[string]Unit{}}
	for _, u := range us {
		w.Units[u.ID] = u
		w.byName[u.ID] = u
		for _, n := range u.Names {
			w.byName[n] = u
		}
	}
	return w
}

// Unit finds a unit by any of its names.
func (w Waits) Unit(name string) (Unit, bool) { u, ok := w.byName[name]; return u, ok }

// Blocker is the unit a unit was waiting on: of its After= dependencies that were ready by
// the time it started, the one that was ready last. A unit none of whose After= dependencies
// ran in this boot has none, and its start is the manager's doing.
//
// A dependency that was ready only after the unit started was not ordered against it in this
// boot's transaction - After= orders two jobs only when both have one - and is ignored.
func (w Waits) Blocker(u Unit) (Unit, bool) {
	var best Unit
	var found bool
	for _, id := range u.After {
		d, ok := w.byName[id]
		if !ok || d.ID == u.ID || d.Ready == 0 || d.Ready > u.Start {
			continue
		}
		// Ties go to the unit that took longer itself, then by name: a target is ready the
		// microsecond its last dependency is, and naming the dependency says more.
		if !found || d.Ready > best.Ready ||
			(d.Ready == best.Ready && (d.Own() > best.Own() || (d.Own() == best.Own() && d.ID < best.ID))) {
			best, found = d, true
		}
	}
	return best, found
}

// Hop is one unit on a critical path. Lag is the time between its blocker being ready and it
// starting: systemd dispatching it, or waiting on something that is not a unit. The first hop
// has no blocker, and its Lag is from the end of the manager's unit load.
type Hop struct {
	Unit    Unit
	Blocker string
	Lag     time.Duration
}

// CriticalPath walks back from target through each unit's Blocker and returns the chain from
// its root to target. Along it, the root's Start plus every hop's Own and every later hop's Lag
// is target's Ready, so nothing between the end of the unit load and target is unaccounted
// for.
func (w Waits) CriticalPath(target string) ([]Hop, error) {
	u, ok := w.byName[target]
	if !ok {
		return nil, fmt.Errorf("%s was not started in this boot", target)
	}
	var rev []Hop
	seen := map[string]bool{}
	for {
		if seen[u.ID] {
			return nil, fmt.Errorf("%s is its own blocker: ordering cycle in the timestamps", u.ID)
		}
		seen[u.ID] = true
		b, ok := w.Blocker(u)
		if !ok {
			rev = append(rev, Hop{Unit: u, Lag: u.Start - w.Manager.UnitsLoadFinish})
			break
		}
		rev = append(rev, Hop{Unit: u, Blocker: b.Name(), Lag: u.Start - b.Ready})
		u = b
	}
	slices.Reverse(rev)
	return rev, nil
}

// Gap is an interval in which no unit was activating: systemd was waiting for something that
// is not a unit's process - the kernel announcing a device, its own dispatch - or for nothing.
// Next is the unit whose start ended it, and NextBlocker what that unit had been waiting on.
type Gap struct {
	From, To    time.Duration
	Next        string
	NextBlocker string
}

// Gaps is every interval of at least min between the end of the unit load and until in which
// no unit was between starting and ready.
//
// A device is not busy while it is activating: its job is waiting for the kernel and udev to
// announce it, which is the thing a gap is for. Counted as busy, dev-vda.device's 109 ms
// covered every gap in the boot and the first version of this reported none (2026-11-12).
func (w Waits) Gaps(until, min time.Duration) []Gap {
	type span struct{ from, to time.Duration }
	var busy []span
	for _, u := range w.Units {
		if strings.HasSuffix(u.ID, ".device") {
			continue
		}
		end := u.Ready
		if end == 0 {
			end = until // still activating when it was read
		}
		if end > u.Start {
			busy = append(busy, span{u.Start, end})
		}
	}
	slices.SortFunc(busy, func(a, b span) int { return cmp.Compare(a.from, b.from) })

	var gaps []Gap
	cursor := w.Manager.UnitsLoadFinish
	add := func(to time.Duration) {
		if to-cursor < min {
			return
		}
		g := Gap{From: cursor, To: to}
		if n, ok := w.startingAt(to); ok {
			g.Next = n.Name()
			if b, ok := w.Blocker(n); ok {
				g.NextBlocker = b.Name()
			}
		}
		gaps = append(gaps, g)
	}
	for _, s := range busy {
		if s.from >= until {
			break
		}
		if s.from > cursor {
			add(s.from)
		}
		cursor = max(cursor, s.to)
	}
	if until > cursor {
		add(until)
	}
	return gaps
}

// startingAt is the unit that starts first at or after t: the one that ended a gap there. One
// that takes time beats one that does not at the same instant, and a device is never one, for
// the reason Gaps gives.
func (w Waits) startingAt(t time.Duration) (Unit, bool) {
	var best Unit
	var found bool
	for _, u := range w.Units {
		if u.Start < t || strings.HasSuffix(u.ID, ".device") {
			continue
		}
		if !found || u.Start < best.Start ||
			(u.Start == best.Start && (u.Own() > best.Own() || (u.Own() == best.Own() && u.ID < best.ID))) {
			best, found = u, true
		}
	}
	return best, found
}

// After is every unit that was not ready by t - the boot still going on behind a login prompt,
// competing for the vCPUs with whatever the first login does - ordered by when it was ready,
// with the units still not ready last. Devices are left out: they cost the guest nothing by
// being late.
func (w Waits) After(t time.Duration) []Unit {
	var out []Unit
	for _, u := range w.Units {
		if strings.HasSuffix(u.ID, ".device") {
			continue
		}
		if u.Ready == 0 || u.Ready > t {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b Unit) int {
		ra, rb := a.Ready, b.Ready
		if ra == 0 {
			ra = 1 << 62
		}
		if rb == 0 {
			rb = 1 << 62
		}
		return cmp.Or(cmp.Compare(ra, rb), cmp.Compare(a.ID, b.ID))
	})
	return out
}
