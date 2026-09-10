// SPDX-License-Identifier: Apache-2.0

// Package boot measures what a boot of this machine costs, from the host.
//
// It exists because the number this repository had been quoting was systemd's own
// `Startup finished`, which begins counting when the kernel hands over. Exec'ing QEMU, the
// firmware and loading the kernel are all before it; a login prompt is after it. Neither end
// had ever been measured, and both are part of what an operator waits for.
//
// The package is split so that the part that can be wrong is testable without a VM: Watch
// turns a stream of console bytes into phase timings, and Percentile turns a set of runs
// into a number. Booting is the part that needs KVM, and it is in the test that needs KVM.
//
// # Going below a phase, into the kernel
//
// These phases stop at the kernel's edge. To go inside it, boot with `--profile` (silent
// console, initcall_debug, full ring buffer), have a systemd timer mount tracefs, write
// `dmesg` and `/sys/kernel/tracing/trace` to a file, and power the machine off; then read
// the overlay back. That is how acpi_init's 10 ms was taken apart on 2026-09-10:
//
//	--append "… ftrace=function_graph ftrace_graph_filter=acpi_bus_scan ftrace_graph_max_depth=1"
//
// Two things about it are worth carrying, because both cost a wrong answer here first.
//
// The checked-in kernel config is not the kernel's config. CONFIG_FUNCTION_GRAPH_TRACER and
// CONFIG_DYNAMIC_FTRACE are absent from kernel/config-*, and the running kernel has both —
// olddefconfig selects them. Ask the guest (`cat /sys/kernel/tracing/available_tracers`),
// which is this repository's rule about asserting what came out rather than what went in,
// applied to a question about what is possible.
//
// And **keep the filter narrow and the depth at 1**. A function_graph duration is wall
// clock, so a function that sleeps is credited with everything that ran while it slept, and
// a wide filter also pays tracing overhead on every call in the subtree. Same boot, same
// function, two filters: acpi_purge_cached_objects read 5.83 ms under a filter on all of
// acpi_init at depth 6, and 223 us on its own — the first number is 26x wrong and led to a
// confident, false conclusion about where ACPI's time goes. The check that catches it is
// free: initcall_debug prints acpi_init's own cost in the same boot, so if that number has
// moved from its untraced baseline, the trace is measuring itself.
package boot

import (
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

// Phase is a moment in a boot, named by what has just become true.
type Phase int

const (
	// Firmware is the first byte the guest writes to the console: SeaBIOS's banner. Reaching
	// it bounds QEMU's own start-up, which is time this repository had never counted.
	//
	// There is firmware at all because on q35 the CPU resets into it, whatever QEMU was told
	// to boot. The kernel here is an ELF carrying the PVH note (XEN_ELFNOTE_PHYS32_ENTRY,
	// CONFIG_PVH=y), so QEMU direct-boots it — but that decides which option ROM SeaBIOS
	// jumps through, not whether SeaBIOS runs. Only a machine type with no firmware skips it.
	//
	// The name is a warning as much as a label: almost none of this phase is firmware.
	// SeaBIOS prints that banner as the second statement of handle_post(), before any
	// hardware init, so reaching it says the guest started, not what starting cost. Split
	// with KVM tracepoints and measured 2026-09-10, the ~35 ms breaks down as
	//
	//     1.4 ms   the launcher, before it exec's QEMU
	//    ~27 ms    QEMU, exec to the guest's first instruction
	//     ~7 ms    the guest, first instruction to first console byte
	//
	// and inside QEMU's 27, from its own trace events (`-msg timestamp=on -trace
	// enable=kvm_ioctl -trace enable=loader_write_rom …`, which the shipped binary supports
	// without a rebuild):
	//
	//     ~2.5 ms   process start
	//     ~5 ms     option parsing, backends, everything before KVM
	//    ~12 ms     accel, memslots, vCPUs, device realize, reset, the ACPI build
	//     ~6 ms     rom_reset copying the kernel's 36 MB, at ~6 GB/s
	//     ~2 ms     cont to the first vCPU entering the guest
	//
	// There is no gap after machine init, and an earlier version of this comment said there
	// was — it read "~8 ms to a QMP round trip" and put ~14 ms after it. The 8 ms was the QMP
	// *greeting*, which the monitor emits from qemu_create_late_backends() at vl.c:3835,
	// before qmp_x_exit_preconfig() builds the board at vl.c:3862. The reply to
	// qmp_capabilities is 18-20 ms, and `-S` defers only the qmp_cont() at vl.c:2849 — reset,
	// the ACPI tables and rom_reset all happen inside that window. A checkpoint that answers
	// before the thing it is supposed to bracket has started is not a checkpoint.
	//
	// Trace it with `-e sched:sched_process_exec -e kvm:kvm_entry` and nothing else. Adding
	// kvm:kvm_pio inflates the same interval from 27 to 49 ms, because that tracepoint fires
	// thousands of times: the harness that measures the VMM must not be one of the things
	// the VMM is doing.
	Firmware Phase = iota
	// Kernel is the kernel's own first line, so the gap from Firmware is what the firmware
	// costs. It needs loglevel 7 to appear.
	Kernel
	// PID1 is the kernel handing over. The kernel must be printing at loglevel 7 for this
	// to appear at all, so it is absent from a quiet boot rather than zero.
	PID1
	// Started is systemd's `Startup finished`, the number this repository used to quote in
	// full. Kept so the old number stays comparable to the new ones.
	Started
	// Usable is a login prompt. It is the only one of these that is not the machine making
	// a claim about itself, and it is the one an operator is actually waiting for.
	Usable
	numPhases
)

func (p Phase) String() string {
	switch p {
	case Firmware:
		return "firmware"
	case Kernel:
		return "kernel"
	case PID1:
		return "pid1"
	case Started:
		return "started"
	case Usable:
		return "usable"
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

// markers are matched against the raw console byte stream, in no particular order: a boot
// that never reaches one simply leaves that phase unset.
//
// Substrings and not regexps, matched against a window of the stream rather than line by
// line. Line-oriented matching is what the shell version of this did and it does not survive
// contact with a serial console: systemd writes escape sequences and partial lines, the
// kernel interleaves with it, and `\r` arrives without `\n`.
var markers = [numPhases][]string{
	Firmware: {"SeaBIOS"},
	Kernel:   {"Linux version"},
	PID1:     {"Run /sbin/init as init process"},
	Started:  {"Startup finished"},
	Usable:   {"login:"},
}

// Run is what one boot cost, as durations from the moment before QEMU was exec'd.
//
// A phase that was never reached is absent from the map rather than zero, because the
// difference matters: a machine that never offered a login has not offered one quickly.
type Run struct {
	At     map[Phase]time.Duration
	Output []byte
}

// Reached says whether this boot got as far as p.
func (r Run) Reached(p Phase) bool { _, ok := r.At[p]; return ok }

// Watch reads a boot's console and records when each phase first appeared. It returns as
// soon as until is reached, or when the console ends.
//
// t0 is the caller's start of the boot — taken before the process is started, so that the
// launch itself is inside the measurement. now is injected so the tests do not need a clock.
//
// Returning early is not an optimisation, it is the difference between a harness that cleans
// up and one that does not. A machine that has offered a login sits there offering it: there
// is no EOF coming, and a first version of this waited for one, so every boot ran until a
// timeout killed it and left QEMU behind. What answers the question is the moment the phase
// appears; nothing after it is being measured.
//
// It stamps on each read rather than on each line. The shell version of this stamped every
// line, in a `while read` loop, and that was slow enough to throttle QEMU's writes to the
// serial port: it reported a boot at 1424 ms that systemd inside the same boot reported at
// 190 ms. A harness on the critical path of what it measures reports the harness.
func Watch(r io.Reader, t0 time.Time, now func() time.Time, until Phase) (Run, error) {
	run := Run{At: make(map[Phase]time.Duration, numPhases)}

	buf := make([]byte, 32*1024)
	// Carry the tail of the previous read, so a marker split across two reads is still
	// found. One byte less than the longest marker is all that can be needed.
	overlap := 0
	for _, ms := range markers {
		for _, m := range ms {
			overlap = max(overlap, len(m)-1)
		}
	}

	var window []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			at := now().Sub(t0)
			run.Output = append(run.Output, buf[:n]...)

			window = append(window, buf[:n]...)
			for p := Phase(0); p < numPhases; p++ {
				if run.Reached(p) {
					continue
				}
				for _, m := range markers[p] {
					if containsBytes(window, m) {
						run.At[p] = at
						break
					}
				}
			}
			if run.Reached(until) {
				return run, nil
			}
			if len(window) > overlap {
				window = append(window[:0], window[len(window)-overlap:]...)
			}
		}
		if err != nil {
			if err == io.EOF {
				return run, nil
			}
			return run, err
		}
	}
}

func containsBytes(b []byte, s string) bool {
	if len(s) == 0 || len(b) < len(s) {
		return false
	}
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}

// Percentile is the p-th percentile of ds, nearest-rank, with p in [0,1].
//
// Nearest-rank and not interpolated: these are wall-clock samples of a thing that either
// happened or did not, and an interpolated p95 is a number no boot ever took. It sorts a
// copy — a caller collecting samples across interleaved variants keeps them in arrival
// order, which is what makes a run auditable afterwards.
func Percentile(ds []time.Duration, p float64) (time.Duration, bool) {
	if len(ds) == 0 {
		return 0, false
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	// ceil(p*N)-1, which is the rank definition. Indexing p*(N-1) instead is the other
	// convention and it is wrong here by a whole sample at the tail: over five runs it
	// answered the 4th for p95, so the slowest boot in the set — the one the number exists
	// to expose — could never be reported.
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i], true
}
