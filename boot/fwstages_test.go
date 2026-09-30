// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Where the time before the kernel goes: QEMU's own start, and the firmware's steps, on the
// machine this repository defines rather than a firmware probe's bare one - the PCI walk
// depends on the devices there are.
//
// It needs no instrumented firmware. qboot names what it asks QEMU for through fw_cfg, and
// each of its steps starts with a select of its own: the file directory, the ACPI tables and
// their loader, e820, SMBIOS, the command line, the initrd, the kernel's entry. So QEMU's
// trace of those selects - with the configuration accesses of the PCI walk and every exit to
// QEMU beside them, all stamped by QEMU's own clock - is the firmware's timeline, read from
// outside it. The exec is stamped by this process, on the same wall clock.
//
// Tracing costs a little in every interval it measures - a formatted line per event - so the
// numbers are where the time goes, not how much a boot without tracing takes.
//
//	SPIN_FIRMWARE_STAGES=1   run at all
//	REPS=<n>                 boots (default 10)
//	FLAGS=<flags>            spin-machine flags for the machine (default: a workspace's shape)
//	SPIN_PROBE_INITRD=<cpio> as for boot:firmware
func TestFirmwareStages(t *testing.T) {
	if os.Getenv("SPIN_FIRMWARE_STAGES") == "" {
		t.Skip("set SPIN_FIRMWARE_STAGES=1: this boots VMs under QEMU's tracing")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 10)
	initrd := probeInitrd(t)

	flags := strings.Fields(orElse("--cpus 1 --memory 512 --max-cpus 16 --max-memory 8192 --hotplug-disks 1", os.Getenv("FLAGS")))
	args := machineArgs(t, out, append([]string{"--disk", "-", "--initrd", initrd, "--console", "file:/dev/stdout"}, flags...))

	var runs []map[string]stage
	var spent []map[string]float64
	for range reps {
		st, w := traceOneBoot(t, args)
		runs, spent = append(runs, st), append(spent, w)
	}
	t.Log(stageTable(runs))
	t.Log(waitTable(spent, 15))
}

// stage is one step's wall time and the exits to QEMU inside it.
type stage struct {
	ms    float64
	exits int
}

// machineArgs is the command line `spin-machine args` prints, one argument per line: QEMU's
// binary first. Split on lines and not on spaces, because -append's value has them.
func machineArgs(t *testing.T, out string, flags []string) []string {
	t.Helper()
	b, err := exec.Command(filepath.Join(out, "bin", "spin-machine"), append([]string{"args", "--release", out}, flags...)...).Output()
	if err != nil {
		t.Fatalf("spin-machine args: %v", err)
	}
	var args []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		args = append(args, strings.TrimSuffix(strings.TrimPrefix(l, "  "), " \\"))
	}
	return args
}

// traceOneBoot boots once with QEMU tracing the firmware's selects, its PCI walk and every exit
// to QEMU, waits for the diagnostic init, and returns each step's time and the firmware's waits.
func traceOneBoot(t *testing.T, args []string) (map[string]stage, map[string]float64) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "trace")
	full := append(slices.Clone(args[1:]),
		"-msg", "timestamp=on", "-D", log,
		"-trace", "enable=fw_cfg_add_file", "-trace", "enable=fw_cfg_select", "-trace", "enable=pci_cfg_read", "-trace", "enable=pci_cfg_write",
		"-trace", "enable=kvm_run_exit")
	cmd := exec.Command(args[0], full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	exec0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching QEMU: %v", err)
	}
	_ = w.Close()
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = r.Close()
	}()
	var console strings.Builder
	buf := make([]byte, 65536)
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(console.String(), "SPIN-READY") {
		if err := r.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		n, err := r.Read(buf)
		console.Write(buf[:n])
		if err != nil {
			t.Fatalf("no SPIN-READY: %v\n%s", err, tail([]byte(console.String()), 2000))
		}
	}
	ready := time.Now()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	evs := parseTrace(string(raw))
	st := stages(evs, exec0, ready)
	if len(st) < 3 {
		// A trace that names no step is a trace this parser does not read - a format QEMU
		// changed, or events it was not built with - and a table of nothing reads as a finding.
		lines := strings.SplitN(string(raw), "\n", 21)
		t.Fatalf("the trace names no firmware step; its first lines:\n%s", strings.Join(lines[:min(len(lines), 20)], "\n"))
	}
	return st, waits(evs)
}

// "2026-09-30T01:52:04.431886Z fw_cfg_select 0x7c8f2b993390 key 0x0019 'file_dir', ret: 1": QEMU's
// log backend under -msg timestamp=on, in UTC.
var reTrace = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6})Z (\w+) (.*)$`)

var (
	reSelect  = regexp.MustCompile(`key 0x([0-9a-f]+) '([^']*)'`)
	reAddFile = regexp.MustCompile(`#\d+: (\S+) \(`)
	reCfg     = regexp.MustCompile(`^(\S+) \S+ @(0x[0-9a-f]+)`)
)

// event is one line of the trace.
type event struct {
	at   time.Time
	kind string // the trace event's name
	what string // what a reader calls it: the file a select names, the register a write hits
}

// parseTrace reads QEMU's trace into events. The trace names a file's select 'unknown': a file's
// key is its place in the directory, which QEMU keeps sorted by name, so the names come from the
// registrations (fw_cfg_add_file), all made before the firmware runs.
func parseTrace(trace string) []event {
	var evs []event
	var files []string
	for line := range strings.SplitSeq(trace, "\n") {
		m := reTrace.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02T15:04:05.000000", m[1], time.UTC)
		if err != nil {
			continue
		}
		ev := event{at: at, kind: m[2]}
		switch m[2] {
		case "fw_cfg_add_file":
			if f := reAddFile.FindStringSubmatch(m[3]); f != nil {
				files = append(files, f[1])
			}
			continue
		case "fw_cfg_select":
			s := reSelect.FindStringSubmatch(m[3])
			if s == nil {
				continue
			}
			key, _ := strconv.ParseUint(s[1], 16, 16)
			ev.what = fmt.Sprintf("0x%02x %s", key, s[2])
		case "pci_cfg_read", "pci_cfg_write":
			if c := reCfg.FindStringSubmatch(m[3]); c != nil {
				ev.what = c[1] + " @" + c[2]
			}
		}
		evs = append(evs, ev)
	}
	slices.Sort(files)
	for i, ev := range evs {
		if ev.kind != "fw_cfg_select" {
			continue
		}
		var key int
		if _, err := fmt.Sscanf(ev.what, "0x%x", &key); err != nil {
			continue
		}
		if k := key - 0x20; k >= 0 && k < len(files) { // FW_CFG_FILE_FIRST
			evs[i].what = fmt.Sprintf("0x%02x %s", key, files[k])
		}
	}
	return evs
}

// stageOf names the step a firmware's fw_cfg select starts, or "" when it continues the step
// before it. The keys are QEMU's (hw/nvram/fw_cfg.c); files by their name.
func stageOf(what string) string {
	key, name, _ := strings.Cut(what, " ")
	switch {
	case key == "0x19": // the file directory
		return "fw_cfg directory"
	case strings.HasPrefix(name, "etc/table-loader"), strings.HasPrefix(name, "etc/acpi"):
		return "ACPI tables"
	case name == "etc/e820":
		return "e820"
	case strings.HasPrefix(name, "etc/smbios"):
		return "SMBIOS"
	case key == "0x14" || key == "0x15": // command line size and data
		return "command line"
	case key == "0x0b" || key == "0x0a" || key == "0x12": // initrd size, address, data
		return "initrd"
	case key == "0x10" || key == "0x08": // kernel entry and size, read last before the jump
		return "kernel handoff"
	}
	return ""
}

// stages splits one boot's trace into steps: QEMU's start (exec to the first exit), the
// firmware's steps (a PCI walk, then each run of fw_cfg selects), and the kernel and init (the
// last select to SPIN-READY). Each step's exits are counted from kvm_run_exit.
func stages(evs []event, exec0, ready time.Time) map[string]stage {
	type mark struct {
		name string
		at   time.Time
	}
	var marks []mark
	var exits []time.Time
	add := func(name string, at time.Time) {
		if len(marks) == 0 || marks[len(marks)-1].name != name {
			marks = append(marks, mark{name, at})
		}
	}
	for _, ev := range evs {
		switch ev.kind {
		case "kvm_run_exit":
			if len(exits) == 0 {
				add("firmware start", ev.at)
			}
			exits = append(exits, ev.at)
		case "pci_cfg_read", "pci_cfg_write":
			if len(marks) > 0 && marks[len(marks)-1].name == "firmware start" {
				add("PCI walk", ev.at)
			}
		case "fw_cfg_select":
			if name := stageOf(ev.what); name != "" && len(exits) > 0 {
				add(name, ev.at)
			}
		}
	}
	// What a step was is decided by where the next one starts; the last firmware step ends at
	// the jump, which the kernel's first exit does not mark, so it ends at its own last event.
	out := map[string]stage{}
	if len(marks) == 0 {
		return out
	}
	out["QEMU start"] = stage{ms: float64(marks[0].at.Sub(exec0).Microseconds()) / 1000}
	for i, mk := range marks {
		end := ready
		if i+1 < len(marks) {
			end = marks[i+1].at
		}
		name := mk.name
		if i == len(marks)-1 {
			name = "kernel and init"
		}
		n := 0
		for _, e := range exits {
			if !e.Before(mk.at) && e.Before(end) {
				n++
			}
		}
		prev := out[name]
		out[name] = stage{ms: prev.ms + float64(end.Sub(mk.at).Microseconds())/1000, exits: prev.exits + n}
	}
	return out
}

// waits is where the firmware's time goes, event by event: each interval between two events from
// the firmware's first exit to its last select, summed by what the interval was spent on.
//
// QEMU traces a configuration write before it applies it, so the time after one is that write's
// cost - a register that remaps memory makes QEMU rebuild its memory map. It traces a select
// after the entry's callback has run, so the time before one is the callback's - the ACPI tables
// are built again on the first read of the loader. Any other interval is the firmware's own code,
// or an exit the trace does not name.
func waits(evs []event) map[string]float64 {
	first, last := -1, -1
	for i, ev := range evs {
		if ev.kind == "kvm_run_exit" && first < 0 {
			first = i
		}
		if ev.kind == "fw_cfg_select" && first >= 0 {
			last = i
		}
	}
	out := map[string]float64{}
	if first < 0 || last < 0 {
		return out
	}
	for i := first; i < last; i++ {
		prev, next := evs[i], evs[i+1]
		var on string
		switch {
		case prev.kind == "pci_cfg_write":
			on = "config write " + prev.what
		case next.kind == "fw_cfg_select":
			on = "select " + next.what
		default:
			on = "firmware code, then " + next.kind
		}
		out[on] += float64(next.at.Sub(prev.at).Microseconds()) / 1000
	}
	return out
}

// stageTable is the p50 of each step over the boots, in the order a boot takes them.
func stageTable(runs []map[string]stage) string {
	order := []string{"QEMU start", "firmware start", "PCI walk", "fw_cfg directory", "ACPI tables", "e820",
		"SMBIOS", "command line", "initrd", "kernel handoff", "kernel and init"}
	var b strings.Builder
	fmt.Fprintf(&b, "\nbefore the kernel, p50 over %d traced boots (tracing inflates every step a little)\n\n", len(runs))
	fmt.Fprintf(&b, "%-18s %10s %8s\n", "STEP", "MS", "EXITS")
	for _, name := range order {
		var ms, ex []float64
		for _, r := range runs {
			if s, ok := r[name]; ok {
				ms = append(ms, s.ms)
				ex = append(ex, float64(s.exits))
			}
		}
		if len(ms) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-18s %10.2f %8.0f\n", name, pct(ms, 50), pct(ex, 50))
	}
	return b.String()
}

// waitTable is the p50 of the largest waits, longest first.
func waitTable(runs []map[string]float64, top int) string {
	all := map[string][]float64{}
	for _, r := range runs {
		for k, v := range r {
			all[k] = append(all[k], v)
		}
	}
	type row struct {
		on string
		ms float64
	}
	var rows []row
	for k, v := range all {
		for len(v) < len(runs) {
			v = append(v, 0)
		}
		rows = append(rows, row{k, pct(v, 50)})
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(b.ms, a.ms) })
	var b strings.Builder
	fmt.Fprintf(&b, "\nthe firmware's largest waits, p50 over %d traced boots\n\n", len(runs))
	fmt.Fprintf(&b, "%-60s %8s\n", "SPENT ON", "MS")
	for _, r := range rows[:min(top, len(rows))] {
		fmt.Fprintf(&b, "%-60s %8.3f\n", r.on, r.ms)
	}
	return b.String()
}

func TestTheFirmwareTraceIsSplitIntoItsSteps(t *testing.T) {
	exec0 := time.Unix(1000, 0)
	trace := `1970-01-01T00:16:39.900000Z fw_cfg_add_file 0x55 #0: etc/table-loader (4096 bytes)
1970-01-01T00:16:39.900001Z fw_cfg_add_file 0x55 #0: etc/acpi/tables (131072 bytes)
1970-01-01T00:16:39.900002Z fw_cfg_add_file 0x55 #1: etc/e820 (40 bytes)
1970-01-01T00:16:39.990000Z fw_cfg_select 0x55 key 0x0000 'signature', ret: 1
1970-01-01T00:16:40.030000Z kvm_run_exit cpu_index 0, reason 2
1970-01-01T00:16:40.030500Z pci_cfg_write mch 00:00.0 @0x91 <- 0x33
1970-01-01T00:16:40.031000Z kvm_run_exit cpu_index 0, reason 2
1970-01-01T00:16:40.032000Z fw_cfg_select 0x55 key 0x0019 'file_dir', ret: 1
1970-01-01T00:16:40.033000Z fw_cfg_select 0x55 key 0x0022 'unknown', ret: 1
1970-01-01T00:16:40.033500Z kvm_run_exit cpu_index 0, reason 2
1970-01-01T00:16:40.034000Z fw_cfg_select 0x55 key 0x0020 'unknown', ret: 1
1970-01-01T00:16:40.036000Z fw_cfg_select 0x55 key 0x0021 'unknown', ret: 1
1970-01-01T00:16:40.037000Z fw_cfg_select 0x55 key 0x0014 'cmdline_size', ret: 1
1970-01-01T00:16:40.038000Z fw_cfg_select 0x55 key 0x000b 'initdr_size', ret: 1
1970-01-01T00:16:40.041000Z fw_cfg_select 0x55 key 0x0008 'kernel_size', ret: 1
1970-01-01T00:16:40.041200Z fw_cfg_select 0x55 key 0x0010 'kernel_entry', ret: 1
`
	evs := parseTrace(trace)
	got := stages(evs, exec0, time.Unix(1000, 100_000_000))
	want := map[string]stage{
		"QEMU start":       {ms: 30},
		"firmware start":   {ms: 0.5, exits: 1},
		"PCI walk":         {ms: 1.5, exits: 1},
		"fw_cfg directory": {ms: 1},
		"ACPI tables":      {ms: 3, exits: 1},
		"e820":             {ms: 1},
		"command line":     {ms: 1},
		"initrd":           {ms: 3},
		"kernel and init":  {ms: 59},
	}
	for k, w := range want {
		g := got[k]
		if d := g.ms - w.ms; d > 1e-6 || d < -1e-6 || g.exits != w.exits {
			t.Errorf("%s: %+v, want %+v", k, g, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("steps %v, want %v", got, want)
	}

	w := waits(evs)
	for on, ms := range map[string]float64{
		"config write mch @0x91":            0.5,
		"select 0x20 etc/acpi/tables":       0.5,
		"select 0x21 etc/e820":              2,
		"select 0x22 etc/table-loader":      1,
		"firmware code, then pci_cfg_write": 0.5,
	} {
		if d := w[on] - ms; d > 1e-6 || d < -1e-6 {
			t.Errorf("waits[%q] = %v, want %v (all: %v)", on, w[on], ms, w)
		}
	}
}
