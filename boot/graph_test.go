// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Inside an initcall, or outside all of them: the initcall table says acpi_init costs 20 ms
// and not which 20, and the kernel spends another 37 ms between initcalls where it has
// nothing to say at all. function_graph does: it times every call under the functions named,
// from the kernel's own entry and exit hooks, and the trace is read after the boot like the
// ring buffer is.
//
// What it cannot say is what the call costs untraced. Every traced call goes through a
// trampoline, so a function that makes many small calls reads as slower than it is and one
// that waits reads as it is: acpi_init read 29.5 ms traced against 18 untraced on
// 2026-09-29. So these boots are separate from the timed ones and their numbers are a map of
// where the time goes, not a measurement of how much there is. Compare a tree with a tree.
//
// It also cannot see what runs before ftrace_init, which is most of start_kernel's memory
// setup: a root has to be a function entered after tracing is on.

// graphReport boots reps machines with fns traced and prints the tree of p50 durations.
func graphReport(t *testing.T, out string, base variant, fns string, reps, depth int) {
	t.Helper()
	// Only opens, closes and calls marked as over 10 us leave the guest. Every byte on the
	// serial port is a VM exit, and the whole trace of acpi_init at depth 6 did not finish
	// printing in three minutes (2026-09-29); the unmarked calls under 10 us are what made it
	// long and are not where a millisecond goes. $$ is a literal $ in a unit file.
	const dump = `[Unit]
Description=Print the function_graph trace and the kernel ring buffer
After=multi-user.target
[Service]
Type=oneshot
StandardOutput=null
ExecStart=/bin/sh -c "mount -t tracefs none /sys/kernel/tracing 2>/dev/null; echo 0 > /sys/kernel/tracing/tracing_on; { echo SPIN-TRACE-BEGIN; grep -E '[{]$$|[}] /[*]|[+!#*@$$] +[0-9]' /sys/kernel/tracing/trace; echo SPIN-TRACE-END; echo SPIN-DMESG-END; } > /dev/ttyS0"
[Install]
WantedBy=multi-user.target
`
	v := base
	v.label = "graph"
	v.profile = true
	v.files = map[string]string{"/etc/systemd/system/spin-dmesg.service": dump}
	v.extra = strings.Join([]string{
		"ftrace=function_graph",
		"ftrace_graph_filter=" + fns,
		"ftrace_graph_max_depth=" + strconv.Itoa(depth),
		"trace_options=funcgraph-tail",
		"trace_buf_size=16M",
	}, " ")

	var boots []map[string]float64
	for range reps {
		console := bootUntil(t, out, v, "", "SPIN-DMESG-END", 180*time.Second)
		begin := strings.Index(console, "SPIN-TRACE-BEGIN")
		end := strings.Index(console, "SPIN-TRACE-END")
		if begin < 0 || end < begin {
			t.Fatalf("no trace in the console:\n%s", tail([]byte(console), 2000))
		}
		calls := parseGraph(console[begin:end])
		if len(calls) == 0 {
			t.Fatalf("the trace of %s is empty: is each a function entered after ftrace_init?\n%s",
				fns, tail([]byte(console[begin:end]), 2000))
		}
		boots = append(boots, calls)
	}
	t.Log(graphTree(boots, envFloat(t, "GRAPH_MIN_MS", 0.3)))
}

var (
	// " 0)   0.274 us    |    __kmalloc_cache_noprof();"
	reGraphLeaf = regexp.MustCompile(`^\s*(\d+)\)\s+[+!#*@$ ]*([\d.]+) us\s+\|(\s*)([\w.]+)\(\);`)
	// " 0)               |    acpi_bus_scan() {"
	reGraphOpen = regexp.MustCompile(`^\s*(\d+)\)\s+\|(\s*)([\w.]+)\(\) \{`)
	// " 0) # 4005.067 us |    } /* acpi_bus_scan */"
	reGraphClose = regexp.MustCompile(`^\s*(\d+)\)\s+[+!#*@$ ]*([\d.]+) us\s+\|\s*\} /\* ([\w.]+) \*/`)
)

// parseGraph sums, per call path ("acpi_init/acpi_scan_init/acpi_bus_scan"), the milliseconds
// a function_graph trace spent there. Paths rather than names, because the same function
// called under two parents is two answers to "where does this parent's time go". A stack per
// CPU, because two CPUs' lines interleave.
func parseGraph(trace string) map[string]float64 {
	total := map[string]float64{}
	stacks := map[string][]string{}
	for line := range strings.SplitSeq(trace, "\n") {
		if m := reGraphOpen.FindStringSubmatch(line); m != nil {
			stacks[m[1]] = append(stacks[m[1]], m[3])
			continue
		}
		if m := reGraphClose.FindStringSubmatch(line); m != nil {
			st := stacks[m[1]]
			if len(st) == 0 || st[len(st)-1] != m[3] {
				// A close whose open was before the buffer began, or on another CPU.
				continue
			}
			us, _ := strconv.ParseFloat(m[2], 64)
			total[strings.Join(st, "/")] += us / 1000
			stacks[m[1]] = st[:len(st)-1]
			continue
		}
		if m := reGraphLeaf.FindStringSubmatch(line); m != nil {
			us, _ := strconv.ParseFloat(m[2], 64)
			total[strings.Join(append(slices.Clone(stacks[m[1]]), m[4]), "/")] += us / 1000
		}
	}
	return total
}

// graphTree prints every path whose p50 over the boots is at least minMS, as a tree: a path
// under its parent, siblings by cost.
func graphTree(boots []map[string]float64, minMS float64) string {
	p50 := map[string]float64{}
	for _, path := range slices.Sorted(maps.Keys(unionKeys(boots))) {
		var v []float64
		for _, b := range boots {
			v = append(v, b[path])
		}
		if m := pct(v, 50); m >= minMS {
			p50[path] = m
		}
	}
	children := map[string][]string{}
	for path := range p50 {
		parent := ""
		if i := strings.LastIndex(path, "/"); i >= 0 {
			parent = path[:i]
		}
		children[parent] = append(children[parent], path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nfunction_graph, p50 over %d boots, traced (inflated: compare trees, not totals)\n\n", len(boots))
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		kids := children[parent]
		slices.SortFunc(kids, func(a, c string) int {
			if p50[a] != p50[c] {
				if p50[a] > p50[c] {
					return -1
				}
				return 1
			}
			return strings.Compare(a, c)
		})
		for _, k := range kids {
			fmt.Fprintf(&b, "%9.2f ms  %s%s\n", p50[k], strings.Repeat("  ", depth), k[strings.LastIndex(k, "/")+1:])
			walk(k, depth+1)
		}
	}
	walk("", 0)
	return b.String()
}

func unionKeys(ms []map[string]float64) map[string]bool {
	u := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			u[k] = true
		}
	}
	return u
}

func envFloat(t *testing.T, name string, def float64) float64 {
	t.Helper()
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%s=%q is not a number: %v", name, s, err)
	}
	return f
}

func TestAGraphTraceIsSummedByCallPath(t *testing.T) {
	trace := ` 0)               |  acpi_init() {
 0)               |    acpi_bus_scan() {
 0)   0.500 us    |      acpi_ns_walk();
 1)   9.000 us    |  other_cpu_leaf();
 0)   1.500 us    |      acpi_ns_walk();
 0) # 4005.067 us |    } /* acpi_bus_scan */
 0) + 12.000 us   |    acpi_ev_init();
 0) # 5000.000 us |  } /* acpi_init */
 1)   3.000 us    |  } /* never_opened */
`
	got := parseGraph(trace)
	want := map[string]float64{
		"acpi_init":                            5,
		"acpi_init/acpi_bus_scan":              4.005067,
		"acpi_init/acpi_bus_scan/acpi_ns_walk": 0.002,
		"acpi_init/acpi_ev_init":               0.012,
		"other_cpu_leaf":                       0.009,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, w := range want {
		if d := got[k] - w; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s: %v ms, want %v", k, got[k], w)
		}
	}

	tree := graphTree([]map[string]float64{got}, 0.005)
	if !strings.Contains(tree, "5.00 ms  acpi_init\n") || !strings.Contains(tree, "4.01 ms    acpi_bus_scan\n") {
		t.Errorf("the tree does not nest acpi_bus_scan under acpi_init:\n%s", tree)
	}
	if strings.Contains(tree, "acpi_ns_walk") {
		t.Errorf("a path under the floor was printed:\n%s", tree)
	}
}
