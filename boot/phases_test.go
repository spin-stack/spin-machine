// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"io"
	"strings"
	"testing"
	"time"
)

// chunks is a reader that hands out exactly the pieces it was given, so a test can decide
// where the read boundaries fall. Every interesting bug in Watch is about a boundary.
type chunks struct {
	parts []string
	i     int
}

func (c *chunks) Read(p []byte) (int, error) {
	if c.i >= len(c.parts) {
		return 0, io.EOF
	}
	n := copy(p, c.parts[c.i])
	if n < len(c.parts[c.i]) {
		c.parts[c.i] = c.parts[c.i][n:]
		return n, nil
	}
	c.i++
	return n, nil
}

// ticker returns a clock that advances by step on every call, so the nth read is stamped at
// n*step and a test can assert on exact durations.
func ticker(t0 time.Time, step time.Duration) func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return t0.Add(time.Duration(n) * step)
	}
}

func TestWatchStampsEachPhaseAtTheReadThatRevealedIt(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &chunks{parts: []string{
		"SeaBIOS (version rel-1.17.0)\n",
		"[    0.058798] Run /sbin/init as init process\n",
		"nothing interesting here\n",
		"Startup finished in 62ms (kernel) + 127ms (userspace) = 190ms\n",
		"spin login: ",
	}}
	run, err := Watch(r, t0, ticker(t0, 10*time.Millisecond), Usable)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	for phase, want := range map[Phase]time.Duration{
		Firmware: 10 * time.Millisecond,
		PID1:     20 * time.Millisecond,
		Started:  40 * time.Millisecond,
		Usable:   50 * time.Millisecond,
	} {
		if got := run.At[phase]; got != want {
			t.Errorf("%s at %v, want %v", phase, got, want)
		}
	}
}

// The reason Watch keeps a window instead of matching each read on its own. A serial console
// hands over whatever has arrived, and a marker lands across two reads often enough that a
// harness without this reports "never reached a login" for a machine sitting at a prompt.
func TestWatchFindsAMarkerSplitAcrossReads(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &chunks{parts: []string{"Startup fin", "ished in 190ms\n"}}
	run, err := Watch(r, t0, ticker(t0, time.Millisecond), Usable)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !run.Reached(Started) {
		t.Fatal("a marker split across two reads was missed")
	}
	if got, want := run.At[Started], 2*time.Millisecond; got != want {
		t.Errorf("stamped at %v, want %v — the read that completed it", got, want)
	}
}

// A phase that never happened must be absent, not zero. A boot that never offered a login is
// the failure this harness exists to catch, and a zero would report it as the fastest one.
func TestWatchLeavesAPhaseThatNeverHappenedUnset(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &chunks{parts: []string{"SeaBIOS\n", "Kernel panic - not syncing\n"}}
	run, err := Watch(r, t0, ticker(t0, time.Millisecond), Usable)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if run.Reached(Usable) {
		t.Error("a panicking machine was recorded as having offered a login")
	}
	if !run.Reached(Firmware) {
		t.Error("the firmware did run")
	}
}

// Watch must return the moment `until` is reached and not read on. A machine sitting at a
// login prompt sends no EOF, so a Watch that keeps reading only ends when something kills the
// process — which is what happened: every boot ran to a 70 s timeout and left QEMU behind,
// found as a stray `qemu-system-x86_64` still holding an overlay in /tmp.
func TestWatchStopsAtTheRequestedPhase(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &chunks{parts: []string{"SeaBIOS\n", "Startup finished\n", "spin login: ", "MORE"}}
	run, err := Watch(r, t0, ticker(t0, time.Millisecond), Started)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if run.Reached(Usable) {
		t.Error("Watch read past the phase it was asked to stop at")
	}
	if got, want := string(run.Output), "SeaBIOS\nStartup finished\n"; got != want {
		t.Errorf("console kept as %q, want %q — it should have stopped reading", got, want)
	}
}

// The first appearance wins. A login prompt is reprinted after every failed login and the
// kernel banner appears twice when a quiet boot later raises its log level, so a harness
// taking the last match measures how long the console was watched.
func TestWatchTakesTheFirstAppearance(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &chunks{parts: []string{"spin login: ", "root\n", "spin login: "}}
	run, err := Watch(r, t0, ticker(t0, time.Millisecond), Usable)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if got, want := run.At[Usable], time.Millisecond; got != want {
		t.Errorf("stamped at %v, want %v", got, want)
	}
}

func TestPercentileIsNearestRank(t *testing.T) {
	ds := []time.Duration{50, 10, 40, 20, 30}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{{0, 10}, {0.5, 30}, {0.95, 50}, {1, 50}} {
		got, ok := Percentile(ds, c.p)
		if !ok {
			t.Fatalf("p%v: no answer", c.p)
		}
		if got != c.want {
			t.Errorf("p%v = %v, want %v", c.p, got, c.want)
		}
	}
	if _, ok := Percentile(nil, 0.5); ok {
		t.Error("an empty set has no percentile and must say so")
	}
}

// Samples arrive interleaved across variants and stay in arrival order, which is what makes
// a run auditable after the fact. Percentile must not disturb them.
func TestPercentileDoesNotReorderItsInput(t *testing.T) {
	ds := []time.Duration{50, 10, 40}
	Percentile(ds, 0.5)
	if ds[0] != 50 || ds[1] != 10 || ds[2] != 40 {
		t.Errorf("the caller's samples were reordered: %v", ds)
	}
}

func TestWatchReportsTheConsoleItRead(t *testing.T) {
	t0 := time.Unix(0, 0)
	const want = "SeaBIOS\nStartup finished\n"
	run, err := Watch(strings.NewReader(want), t0, ticker(t0, time.Millisecond), Usable)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if string(run.Output) != want {
		t.Errorf("console kept as %q, want %q", run.Output, want)
	}
}
