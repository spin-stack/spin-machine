// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"io"
	"slices"
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

// Watch, one console per row. Every interesting bug in it is about where a read
// boundary falls or when to stop, so each row fixes the reads and the clock: the nth
// read is stamped at n*step.
func TestWatch(t *testing.T) {
	const ms = time.Millisecond
	for _, tc := range []struct {
		name  string
		reads []string
		step  time.Duration
		until Phase
		// at is when each phase must have been reached; unreached must not have been.
		at        map[Phase]time.Duration
		unreached []Phase
		// output is the console Watch kept; empty skips the check.
		output string
	}{{
		name: "each phase is stamped at the read that revealed it",
		reads: []string{
			"SeaBIOS (version rel-1.17.0)\n",
			"[    0.058798] Run /sbin/init as init process\n",
			"nothing interesting here\n",
			"Startup finished in 62ms (kernel) + 127ms (userspace) = 190ms\n",
			"spin login: ",
		},
		step: 10 * ms, until: Usable,
		at: map[Phase]time.Duration{Firmware: 10 * ms, PID1: 20 * ms, Started: 40 * ms, Usable: 50 * ms},
	}, {
		// The reason Watch keeps a window instead of matching each read on its own. A
		// serial console hands over whatever has arrived, and a marker lands across two
		// reads often enough that a harness without this reports "never reached a
		// login" for a machine sitting at a prompt. Stamped at the read completing it.
		name:  "a marker split across reads",
		reads: []string{"Startup fin", "ished in 190ms\n"},
		step:  ms, until: Usable,
		at: map[Phase]time.Duration{Started: 2 * ms},
	}, {
		// Absent, not zero. A boot that never offered a login is the failure this
		// harness exists to catch, and a zero would report it as the fastest one.
		name:  "a phase that never happened",
		reads: []string{"SeaBIOS\n", "Kernel panic - not syncing\n"},
		step:  ms, until: Usable,
		at:        map[Phase]time.Duration{Firmware: ms},
		unreached: []Phase{Usable},
	}, {
		// Watch returns the moment until is reached and reads no further. A machine at
		// a login prompt sends no EOF, so a Watch that keeps reading ends only when
		// something kills the process — which is what happened: every boot ran to a
		// 70 s timeout and left QEMU behind, still holding an overlay in /tmp.
		name:  "it stops at the requested phase",
		reads: []string{"SeaBIOS\n", "Startup finished\n", "spin login: ", "MORE"},
		step:  ms, until: Started,
		unreached: []Phase{Usable},
		output:    "SeaBIOS\nStartup finished\n",
	}, {
		// A login prompt is reprinted after every failed login and the kernel banner
		// appears twice when a quiet boot later raises its log level, so a harness
		// taking the last match measures how long the console was watched.
		name:  "the first appearance wins",
		reads: []string{"spin login: ", "root\n", "spin login: "},
		step:  ms, until: Usable,
		at: map[Phase]time.Duration{Usable: ms},
	}, {
		name:  "the console is kept",
		reads: []string{"SeaBIOS\nStartup finished\n"},
		step:  ms, until: Usable,
		output: "SeaBIOS\nStartup finished\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t0 := time.Unix(0, 0)
			run, err := Watch(&chunks{parts: tc.reads}, t0, ticker(t0, tc.step), tc.until)
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			for phase, want := range tc.at {
				if got, ok := run.At[phase]; !ok || got != want {
					t.Errorf("%s at %v (reached %v), want %v", phase, got, ok, want)
				}
			}
			for _, phase := range tc.unreached {
				if run.Reached(phase) {
					t.Errorf("%s recorded as reached", phase)
				}
			}
			if tc.output != "" && string(run.Output) != tc.output {
				t.Errorf("console kept as %q, want %q", run.Output, tc.output)
			}
		})
	}
}

// Nearest-rank, and without reordering the caller's samples: they arrive
// interleaved across variants and stay in arrival order, which is what makes a run
// auditable after the fact.
func TestPercentile(t *testing.T) {
	five := []time.Duration{50, 10, 40, 20, 30}
	for _, tc := range []struct {
		name string
		ds   []time.Duration
		p    float64
		want time.Duration
		ok   bool
	}{
		{"p0 is the fastest", five, 0, 10, true},
		{"p50", five, 0.5, 30, true},
		// ceil(p*N)-1. Indexing p*(N-1) answered the 4th of five here, so the slowest
		// boot — the one the number exists to expose — could never be reported.
		{"p95 of five is the slowest", five, 0.95, 50, true},
		{"p100", five, 1, 50, true},
		{"an empty set has none, and says so", nil, 0.5, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.ds)
			got, ok := Percentile(tc.ds, tc.p)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Percentile(%v, %v) = %v, %v; want %v, %v", tc.ds, tc.p, got, ok, tc.want, tc.ok)
			}
			if !slices.Equal(tc.ds, before) {
				t.Errorf("the caller's samples were reordered: %v, were %v", tc.ds, before)
			}
		})
	}
}
