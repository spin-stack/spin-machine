// SPDX-License-Identifier: Apache-2.0

// Package pressure is the line the guest's pressure sender (cmd/spin-machine-pressure) writes to
// the host, and the host's parse of it (machine.PressureListener). One package for both ends, so
// the port and the format are written once.
//
// The format is an external contract between a guest and the host that runs it, and a guest booted
// from one release can be resumed on a host running the next: newline-terminated ASCII, one record
// a line, at most MaxLine bytes, durations in microseconds, counters cumulative since the guest's
// boot so that a line lost or sent twice changes no total.
//
//	spin-machine-pressure 1 oom=3     the hello, first on every connection: version, oom_kill so far
//	stall memory 150000 1000000 8123  a PSI trigger fired: resource, its threshold and window, the
//	                                  resource's "some" total stall so far
//	oom 4                             the guest's OOM killer has killed 4 processes since its boot
package pressure

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Port is the host's vsock port the sender dials, on the host's CID (2). spin's guest and runner
// use 1025-1031 (spin's internal/vm/vsock/ports.go, read 2026-11-21); 1032 is the next.
const Port = 1032

// Version is the format's version, said in the hello. A host refuses a version it does not know.
const Version = 1

// helloWord starts the hello.
const helloWord = "spin-machine-pressure"

// MaxLine bounds a line, newline included: a guest is not trusted to end its lines.
const MaxLine = 128

// Resource is what a guest or a host stalls on, as PSI names it.
type Resource int

const (
	CPU Resource = iota + 1
	Memory
	IO
)

// resources is every Resource by its name in PSI's files (/proc/pressure/NAME, NAME.pressure).
var resources = map[string]Resource{"cpu": CPU, "memory": Memory, "io": IO}

func (r Resource) String() string {
	for name, v := range resources {
		if v == r {
			return name
		}
	}
	return "resource(" + strconv.Itoa(int(r)) + ")"
}

// Resources is every Resource, in their order.
func Resources() []Resource { return []Resource{CPU, Memory, IO} }

// Line is one record: Hello, Stall or OOM.
type Line interface {
	Append(b []byte) []byte
}

// Hello is the first line on a connection.
type Hello struct {
	// OOMKills is the guest's oom_kill since its boot, the base the next OOM line counts from.
	OOMKills uint64
}

// Stall is a PSI trigger that fired: at least Threshold of stall on Resource within Window.
type Stall struct {
	Resource          Resource
	Threshold, Window time.Duration
	// Total is the resource's "some" stall since the guest's boot, read when the trigger fired.
	Total time.Duration
}

// OOM is the guest's oom_kill since its boot.
type OOM struct {
	Kills uint64
}

func (h Hello) Append(b []byte) []byte {
	return fmt.Appendf(b, "%s %d oom=%d\n", helloWord, Version, h.OOMKills)
}

func (s Stall) Append(b []byte) []byte {
	return fmt.Appendf(b, "stall %s %d %d %d\n", s.Resource, s.Threshold.Microseconds(), s.Window.Microseconds(), s.Total.Microseconds())
}

func (o OOM) Append(b []byte) []byte { return fmt.Appendf(b, "oom %d\n", o.Kills) }

// ErrVersion is a hello of a version this host does not read.
var ErrVersion = errors.New("a version of the pressure line this host does not read")

// Parse reads one line, without its newline.
func Parse(line []byte) (Line, error) {
	f := bytes.Fields(line)
	if len(f) == 0 {
		return nil, errors.New("an empty pressure line")
	}
	switch string(f[0]) {
	case helloWord:
		if len(f) != 3 {
			break
		}
		if v, err := strconv.Atoi(string(f[1])); err != nil || v != Version {
			return nil, fmt.Errorf("%w: %q", ErrVersion, line)
		}
		n, ok := bytes.CutPrefix(f[2], []byte("oom="))
		if !ok {
			break
		}
		kills, err := strconv.ParseUint(string(n), 10, 64)
		if err != nil {
			break
		}
		return Hello{OOMKills: kills}, nil
	case "stall":
		if len(f) != 5 {
			break
		}
		r, ok := resources[string(f[1])]
		if !ok {
			break
		}
		var us [3]time.Duration
		for i := range us {
			n, err := strconv.ParseInt(string(f[2+i]), 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("a pressure line that is not one: %q", line)
			}
			us[i] = time.Duration(n) * time.Microsecond
		}
		return Stall{Resource: r, Threshold: us[0], Window: us[1], Total: us[2]}, nil
	case "oom":
		if len(f) != 2 {
			break
		}
		kills, err := strconv.ParseUint(string(f[1]), 10, 64)
		if err != nil {
			break
		}
		return OOM{Kills: kills}, nil
	}
	return nil, fmt.Errorf("a pressure line that is not one: %q", line)
}

// Threshold and Window are the trigger both ends arm, guest and host: 150 ms of stall in 1 s, as
// spin's guest memory notice arms it. A window under 2 s is a privileged trigger, root's alone, and
// the kernel runs it on a psimon thread of its own per group of triggers (kernel/sched/psi.c). The
// prototype's guest triggers woke 164-255 ms after a stall began (laptop, 2026-11-21).
const (
	Threshold = 150 * time.Millisecond
	Window    = time.Second
)

// Trigger is a PSI trigger armed on one pressure file: its descriptor polls POLLPRI each time the
// stall it was armed with is reached, at most once a window.
type Trigger struct {
	Resource Resource
	// File is the pressure file, open for the trigger and for reading it again.
	File *os.File
}

// Arm arms "some Threshold Window" on the pressure file at path.
func Arm(path string, r Resource) (Trigger, error) {
	// #nosec G304 -- a pressure file: /proc/pressure/* or a cgroup's *.pressure
	f, err := os.OpenFile(path, os.O_RDWR, 0) // mutate-exempt: the mode is read only with O_CREATE, which this has not
	if err != nil {
		return Trigger{}, fmt.Errorf("opening %s for a trigger: %w", path, err)
	}
	trigger := fmt.Sprintf("some %d %d", Threshold.Microseconds(), Window.Microseconds())
	if _, err := f.WriteString(trigger + "\x00"); err != nil {
		_ = f.Close()
		return Trigger{}, fmt.Errorf("arming %q on %s: %w", trigger, path, err)
	}
	return Trigger{Resource: r, File: f}, nil
}

// Total is the "some" stall the trigger's file says so far: a pressure file is read whole by a
// read at offset 0, from the trigger's own descriptor.
func (t Trigger) Total() (time.Duration, error) {
	b := make([]byte, 256) // mutate-exempt: a pressure file is two lines of ~70 bytes; any buffer over them reads it whole
	n, err := t.File.ReadAt(b, 0)
	if n == 0 { // mutate-exempt: a file of one byte has no total either, and SomeTotal refuses it
		return 0, fmt.Errorf("reading %s: %w", t.File.Name(), err)
	}
	return SomeTotal(b[:n])
}

// SomeTotal is the total of a pressure file's "some" line, "some avg10=0.00 avg60=0.00
// avg300=0.00 total=1234", whose total is in microseconds.
func SomeTotal(text []byte) (time.Duration, error) {
	for line := range bytes.Lines(text) {
		f := bytes.Fields(line)
		if len(f) == 0 || string(f[0]) != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := bytes.CutPrefix(kv, []byte("total=")); ok {
				us, err := strconv.ParseInt(string(v), 10, 64)
				if err != nil {
					return 0, fmt.Errorf("a pressure total that is not a number: %q", kv)
				}
				return time.Duration(us) * time.Microsecond, nil
			}
		}
	}
	return 0, fmt.Errorf("no some total in %q", text)
}
