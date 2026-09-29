// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Who else had the CPU while an initcall ran. On one vCPU an initcall's duration is its own
// work plus everything the scheduler ran in between - kworkers, async probes, ksoftirqd - and
// the initcall table charges all of it to the initcall. acpi_init read 20 ms on two vCPUs and 34
// on one (2026-09-29), with the kernel's total unchanged: the difference was other work, run
// inside the window. This says whose, from sched_switch, per kernel.
//
// The boots are separate from the timed ones: sched_switch events cost a little each.

// cpuShareReport boots reps machines per kernel with the scheduler traced and prints, for the
// window of the initcall named, the p50 milliseconds each task spent on the CPU.
func cpuShareReport(t *testing.T, out string, base variant, initcall string, reps int) {
	t.Helper()
	// Only the window leaves the guest: sched_switch fires through the whole boot, and every byte
	// on the serial port is a VM exit - the unfiltered trace did not finish in three minutes.
	const dump = `[Unit]
Description=Print the scheduler and initcall trace events
After=multi-user.target
[Service]
Type=oneshot
StandardOutput=null
ExecStart=/bin/sh -c "mount -t tracefs none /sys/kernel/tracing 2>/dev/null; echo 0 > /sys/kernel/tracing/tracing_on; { echo SPIN-TRACE-BEGIN; sed -n '/initcall_start: func=FN+/,/initcall_finish: func=FN+/p' /sys/kernel/tracing/trace; echo SPIN-TRACE-END; echo SPIN-DMESG-END; } > /dev/ttyS0"
[Install]
WantedBy=multi-user.target
`
	v := base
	v.label = "cpu share"
	v.files = map[string]string{"/etc/systemd/system/spin-dmesg.service": strings.ReplaceAll(dump, "FN", initcall)}
	// Not --profile: it passes trace_event=initcall:*, and a second trace_event= replaces the
	// first rather than adding to it.
	v.profile = false
	v.extra = "trace_event=initcall:*,sched:sched_switch,workqueue:workqueue_execute_start,workqueue:workqueue_execute_end trace_buf_size=16M"

	kernels := []struct{ label, path string }{{"release", ""}}
	if k := kernelB(t); k != "" {
		kernels = append(kernels, struct{ label, path string }{"kernel B", k})
	}
	shares := map[string][]map[string]float64{}
	for range reps {
		for _, k := range kernels {
			console := bootUntil(t, out, v, k.path, "SPIN-DMESG-END", 180*time.Second)
			begin, end := strings.Index(console, "SPIN-TRACE-BEGIN"), strings.Index(console, "SPIN-TRACE-END")
			if begin < 0 || end < begin {
				t.Fatalf("no trace in the console:\n%s", tail([]byte(console), 2000))
			}
			s := cpuShare(console[begin:end], initcall)
			if len(s) == 0 {
				t.Fatalf("no window for %s in the trace of %s", initcall, k.label)
			}
			shares[k.label] = append(shares[k.label], s)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nwho had the CPU while %s ran, p50 ms over %d boots\n\n", initcall, reps)
	tasks := map[string]bool{}
	for _, ss := range shares {
		maps.Copy(tasks, unionKeys(ss))
	}
	fmt.Fprintf(&b, "%-28s", "TASK")
	for _, k := range kernels {
		fmt.Fprintf(&b, " %12s", k.label)
	}
	b.WriteString("\n")
	p50 := func(label, task string) float64 {
		var v []float64
		for _, s := range shares[label] {
			v = append(v, s[task])
		}
		return pct(v, 50)
	}
	names := slices.Collect(maps.Keys(tasks))
	slices.SortFunc(names, func(a, c string) int {
		if d := p50(kernels[0].label, c) - p50(kernels[0].label, a); d != 0 {
			if d > 0 {
				return 1
			}
			return -1
		}
		return strings.Compare(a, c)
	})
	for _, n := range names {
		fmt.Fprintf(&b, "%-28s", n)
		for _, k := range kernels {
			fmt.Fprintf(&b, " %12.2f", p50(k.label, n))
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
}

// "  kworker/0:1-12  [000] d..2.  0.034567: sched_switch: prev_comm=... ==> next_comm=kworker/0:2 next_pid=13 ..."
var reTraceEvent = regexp.MustCompile(`^\s*(.+?)-(\d+)\s+\[(\d+)\].*?\s(\d+\.\d+): (sched_switch|initcall_start|initcall_finish|workqueue_execute_start|workqueue_execute_end): (.*)$`)

var (
	reNext     = regexp.MustCompile(`next_comm=(.+?) next_pid=(\d+)`)
	reWorkFunc = regexp.MustCompile(`function (\S+)`)
)

// cpuShare is the milliseconds each task spent on the CPU between the initcall's start and
// finish events, by comm, with the window's total under "(window)". Tasks are named without the
// number after a slash, so kworker/0:1 and kworker/0:2 are one kworker; a kworker running a work
// item is named by the item's function, "kworker:trace_eval_sync", because which kworker ran it
// says nothing and what it ran is the answer.
func cpuShare(trace, initcall string) map[string]float64 {
	type cpu struct {
		task, pid string
		at        float64
	}
	work := map[string]string{} // pid -> the work function it is executing
	cpus := map[string]*cpu{}
	share := map[string]float64{}
	open, start := false, 0.0
	for line := range strings.SplitSeq(trace, "\n") {
		m := reTraceEvent.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ts, _ := strconv.ParseFloat(m[4], 64)
		c := cpus[m[3]]
		if c == nil {
			c = &cpu{task: m[1], pid: m[2], at: ts}
			cpus[m[3]] = c
		}
		account := func(until float64) {
			if open && until > c.at {
				name := taskName(c.task)
				if w := work[c.pid]; w != "" {
					name += ":" + w
				}
				share[name] += (until - max(c.at, start)) * 1000
			}
		}
		switch m[5] {
		case "initcall_start":
			if strings.HasPrefix(m[6], "func="+initcall+"+") {
				open, start = true, ts
				c.task, c.pid, c.at = m[1], m[2], ts
			}
		case "initcall_finish":
			if open && strings.HasPrefix(m[6], "func="+initcall+"+") {
				account(ts)
				share["(window)"] = (ts - start) * 1000
				return share
			}
		case "sched_switch":
			account(ts)
			if n := reNext.FindStringSubmatch(m[6]); n != nil {
				c.task, c.pid = n[1], n[2]
			}
			c.at = ts
		case "workqueue_execute_start", "workqueue_execute_end":
			account(ts)
			c.at = ts
			if f := reWorkFunc.FindStringSubmatch(m[6]); f != nil && m[5] == "workqueue_execute_start" {
				work[m[2]] = f[1]
			} else {
				delete(work, m[2])
			}
		}
	}
	return map[string]float64{}
}

func taskName(comm string) string {
	if i := strings.Index(comm, "/"); i >= 0 {
		return comm[:i]
	}
	return comm
}

func TestTheCPUIsSharedOutByTaskWithinTheInitcall(t *testing.T) {
	trace := `       swapper/0-1       [000] .....     0.010000: initcall_start: func=acpi_init+0x0/0x420
       swapper/0-1       [000] d..2.     0.012000: sched_switch: prev_comm=swapper/0 prev_pid=1 prev_prio=120 prev_state=R+ ==> next_comm=kworker/u4:0 next_pid=11 next_prio=120
    kworker/u4:0-11      [000] .....     0.013000: workqueue_execute_start: work struct 00000000deadbeef: function trace_eval_sync
    kworker/u4:0-11      [000] .....     0.015000: workqueue_execute_end: work struct 00000000deadbeef: function trace_eval_sync
    kworker/u4:0-11      [000] d..2.     0.016000: sched_switch: prev_comm=kworker/u4:0 prev_pid=11 prev_prio=120 prev_state=I ==> next_comm=swapper/0 next_pid=1 next_prio=120
       swapper/0-1       [000] .....     0.020000: initcall_finish: func=acpi_init+0x0/0x420 ret=0
       swapper/0-1       [000] d..2.     0.021000: sched_switch: prev_comm=swapper/0 prev_pid=1 prev_prio=120 prev_state=R+ ==> next_comm=kworker/u4:1 next_pid=12 next_prio=120
`
	got := cpuShare(trace, "acpi_init")
	want := map[string]float64{"swapper": 6, "kworker": 2, "kworker:trace_eval_sync": 2, "(window)": 10}
	for k, w := range want {
		if d := got[k] - w; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s: %v ms, want %v (all: %v)", k, got[k], w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("shares %v, want %v", got, want)
	}
	if s := cpuShare(trace, "inet_init"); len(s) != 0 {
		t.Errorf("a window for an initcall that is not in the trace: %v", s)
	}
}
