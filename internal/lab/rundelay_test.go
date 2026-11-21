// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"testing"
	"time"
)

func TestParseSchedstat(t *testing.T) {
	ran, waited, ok := parseSchedstat("1710591 2500000 2\n")
	if !ok || ran != 1710591 || waited != 2500*time.Microsecond {
		t.Fatalf("got %v %v %v", ran, waited, ok)
	}
	for _, bad := range []string{"", "1 2", "1 x 3", "1 2 3 4"} {
		if _, _, ok := parseSchedstat(bad); ok {
			t.Errorf("%q read as a schedstat", bad)
		}
	}
}

func TestStatSteal(t *testing.T) {
	d, ok := statSteal("cpu  100 0 50 9000 3 0 1 7 0 0\ncpu0 100 0 50 9000 3 0 1 7 0 0\n")
	if !ok || d != 70*time.Millisecond {
		t.Fatalf("got %v %v", d, ok)
	}
	if _, ok := statSteal("cpu  100 0 50 9000 3 0 1\n"); ok {
		t.Error("a line short of steal read as one")
	}
}

func TestSummaryWaitedBySplitsAtTheLastSampleBefore(t *testing.T) {
	t0 := time.Now()
	s := schedSummary{points: []schedPoint{
		{t0, 1 * time.Millisecond},
		{t0.Add(5 * time.Millisecond), 3 * time.Millisecond},
		{t0.Add(10 * time.Millisecond), 8 * time.Millisecond},
	}}
	for _, c := range []struct {
		at   time.Time
		want time.Duration
	}{
		{t0.Add(-time.Millisecond), 0},
		{t0.Add(7 * time.Millisecond), 3 * time.Millisecond},
		{t0.Add(time.Second), 8 * time.Millisecond},
	} {
		if got := s.waitedBy(c.at); got != c.want {
			t.Errorf("waitedBy(t0%+v) = %v, want %v", c.at.Sub(t0), got, c.want)
		}
	}
}

func TestSummarizeCountsExitedThreadsAndTheVCPUs(t *testing.T) {
	threads := map[int]threadSched{
		1: {name: "qemu-system-x86", ran: 10 * time.Millisecond, waited: 2 * time.Millisecond},
		2: {name: "CPU 0/KVM", ran: 300 * time.Millisecond, waited: 4 * time.Millisecond},
		3: {name: "worker", waited: time.Millisecond}, // exited, at its last sample
		4: {name: "worker", waited: time.Millisecond},
	}
	s := summarize(9, threads, map[int]threadSched{7: {waited: 5 * time.Millisecond}}, nil, 3)
	if s.threads != 4 || s.waited != 8*time.Millisecond || s.vcpuWaited != 4*time.Millisecond ||
		s.ran != 310*time.Millisecond || s.spinWaited != 5*time.Millisecond || s.byName["worker"] != 2*time.Millisecond {
		t.Fatalf("got %+v", s)
	}
}
