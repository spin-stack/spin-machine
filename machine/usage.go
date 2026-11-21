// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spin-stack/machine/internal/pressure"
	"github.com/spin-stack/machine/internal/qmpdef"
	"github.com/spin-stack/machine/qmp"
)

// Usage is what a machine has used so far, every number a counter that only grows but for
// Memory's, which is what it holds now: two Usages of one machine say what it used between them,
// and the same of every machine on a host say which of them used it. Where a Pressure says that a
// machine suffers, Usage says who causes it.
type Usage struct {
	At time.Time
	// CPU, Memory and IO are QEMU's cgroup's: the machine as the host sees it, QEMU's own threads
	// and the host's cache of its disks included.
	CPU    CPUUsage
	Memory MemoryUsage
	IO     IOUsage
	// Disks are what each disk device did, as QEMU counts it.
	Disks []DiskUsage
	// Guest is what the guest's balloon last reported, or nil before its first report.
	Guest *GuestUsage
}

// CPUUsage is cpu.stat, and cpu.pressure's some total.
type CPUUsage struct {
	Used, Throttled, Stalled time.Duration
}

// MemoryUsage is memory.current and memory.stat, in bytes, and memory.pressure's some total.
type MemoryUsage struct {
	Current, Anon, File, Dirty, Writeback int64
	Stalled                               time.Duration
}

// IOUsage is io.stat over every device, and io.pressure's some total.
type IOUsage struct {
	Read, Written, Reads, Writes int64
	Stalled                      time.Duration
}

// DiskUsage is one disk device's query-blockstats.
type DiskUsage struct {
	// Device is the device's id: slot<i> for Spec.Places[i], hd<i> for a disk attached at
	// hotplug target i.
	Device                                string
	Read, Written, Reads, Writes, Flushes int64
}

// GuestUsage is the guest's own word, through the balloon, on what it went without.
type GuestUsage struct {
	OOMKills, AllocStalls uint64
	At                    time.Time
}

var queryBlockstats = qmpdef.Define[qmp.None, []blockStats]("query-blockstats")

type blockStats struct {
	QDev  string `json:"qdev"`
	Stats struct {
		Read    int64 `json:"rd_bytes"`
		Written int64 `json:"wr_bytes"`
		Reads   int64 `json:"rd_operations"`
		Writes  int64 `json:"wr_operations"`
		Flushes int64 `json:"flush_operations"`
	} `json:"stats"`
}

// Usage reads what the machine has used: QEMU's cgroup, then two questions to QEMU,
// query-blockstats (6 ms p50 with 16 hotplug places, laptop, 2026-11-21) and the balloon's report
// (0.45 ms). It refuses a QEMU in its caller's cgroup, whose counters are every machine's.
// mutate-lane: TestThePressureProbeReportsEachPressure
func (p *Process) Usage(ctx context.Context) (Usage, error) {
	if p.cgroup == "" {
		return Usage{}, errors.New("reading a machine's usage: QEMU is in its caller's cgroup, whose counters are every machine's")
	}
	u, err := cgroupUsage(p.cgroup)
	if err != nil {
		return Usage{}, err
	}
	stats, err := qmp.Do(ctx, p.c, queryBlockstats, qmp.None{})
	if err != nil {
		return Usage{}, fmt.Errorf("reading the machine's disk counters: %w", err)
	}
	for _, s := range stats {
		u.Disks = append(u.Disks, DiskUsage{Device: deviceOf(s.QDev), Read: s.Stats.Read, Written: s.Stats.Written,
			Reads: s.Stats.Reads, Writes: s.Stats.Writes, Flushes: s.Stats.Flushes})
	}
	st, err := p.guestStats(ctx)
	switch {
	case errors.Is(err, ErrNoGuestStats):
	case err != nil:
		return Usage{}, err
	default:
		u.Guest = &GuestUsage{OOMKills: st.Stats.OOMKills, AllocStalls: st.Stats.AllocStalls, At: time.Unix(st.Updated, 0)}
		if st.Stats.OOMKills == unreported {
			u.Guest.OOMKills = 0
		}
		if st.Stats.AllocStalls == unreported {
			u.Guest.AllocStalls = 0
		}
	}
	u.At = time.Now()
	return u, nil
}

// deviceOf is the id of the device a block backend's qdev path names:
// /machine/peripheral/slot0/virtio-backend is slot0's.
func deviceOf(qdev string) string {
	rest, ok := strings.CutPrefix(qdev, peripheral)
	if !ok {
		return qdev
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// cgroupUsage reads a cgroup's counters.
func cgroupUsage(dir string) (Usage, error) {
	var u Usage
	read := func(name string) ([]byte, error) {
		// #nosec G304 -- a file of QEMU's cgroup
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading the machine's usage: %w", err)
		}
		return b, nil
	}
	stalled := func(name string) (time.Duration, error) {
		b, err := read(name)
		if err != nil {
			return 0, err
		}
		d, err := pressure.SomeTotal(b)
		if err != nil {
			return 0, fmt.Errorf("reading the machine's usage: %s: %w", name, err)
		}
		return d, nil
	}
	b, err := read("cpu.stat")
	if err != nil {
		return u, err
	}
	cpu := keyed(b)
	u.CPU.Used, u.CPU.Throttled = time.Duration(cpu["usage_usec"])*time.Microsecond, time.Duration(cpu["throttled_usec"])*time.Microsecond
	if b, err = read("memory.current"); err != nil {
		return u, err
	}
	if u.Memory.Current, err = strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64); err != nil {
		return u, fmt.Errorf("reading the machine's usage: memory.current %q is not a number", b)
	}
	if b, err = read("memory.stat"); err != nil {
		return u, err
	}
	mem := keyed(b)
	u.Memory.Anon, u.Memory.File, u.Memory.Dirty, u.Memory.Writeback = mem["anon"], mem["file"], mem["file_dirty"], mem["file_writeback"]
	if b, err = read("io.stat"); err != nil {
		return u, err
	}
	// One line a device: "259:0 rbytes=1 wbytes=2 rios=3 wios=4 dbytes=0 dios=0".
	for line := range bytes.Lines(b) {
		f := bytes.Fields(line)
		if len(f) < 2 {
			continue
		}
		io := map[string]int64{}
		for _, kv := range f[1:] {
			k, v, _ := bytes.Cut(kv, []byte("="))
			n, _ := strconv.ParseInt(string(v), 10, 64)
			io[string(k)] = n
		}
		u.IO.Read += io["rbytes"]
		u.IO.Written += io["wbytes"]
		u.IO.Reads += io["rios"]
		u.IO.Writes += io["wios"]
	}
	if u.CPU.Stalled, err = stalled("cpu.pressure"); err != nil {
		return u, err
	}
	if u.Memory.Stalled, err = stalled("memory.pressure"); err != nil {
		return u, err
	}
	if u.IO.Stalled, err = stalled("io.pressure"); err != nil {
		return u, err
	}
	return u, nil
}

// keyed is a flat keyed file's "name value" lines: cpu.stat, memory.stat.
func keyed(b []byte) map[string]int64 {
	m := map[string]int64{}
	for line := range bytes.Lines(b) {
		k, v, ok := bytes.Cut(bytes.TrimSpace(line), []byte(" "))
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			m[string(k)] = n
		}
	}
	return m
}
