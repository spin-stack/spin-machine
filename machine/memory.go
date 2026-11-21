// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/spin-stack/machine/internal/qmpdef"
	"github.com/spin-stack/machine/qmp"
)

var (
	queryMemorySizeSummary = qmpdef.Define[qmp.None, memorySize]("query-memory-size-summary")
	// qom-get answers any, and qom-set takes any: what a property holds is its reader's to know.
	qomGet = qmpdef.Define[qomGetArgs, json.RawMessage]("qom-get")
	qomSet = qmpdef.Define[qomSetArgs, qmp.None]("qom-set")
)

type memorySize struct {
	Base    int64 `json:"base-memory"`
	Plugged int64 `json:"plugged-memory"`
}

type qomGetArgs struct {
	Path     string `json:"path"`
	Property string `json:"property"`
}

type qomSetArgs struct {
	Path     string `json:"path"`
	Property string `json:"property"`
	Value    any    `json:"value"`
}

// readProperty is the value of the property of the object at path, as a T.
func readProperty[T any](ctx context.Context, m *QEMU, path, property string) (T, error) {
	var v T
	raw, err := qmp.Do(ctx, m.c, qomGet, qomGetArgs{Path: path, Property: property})
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("reading %s's %s: %w", path, property, err)
	}
	return v, nil
}

const memPath = peripheral + virtioMemID

// mib is a MiB in bytes, which QEMU counts memory in.
const mib = 1 << 20

// MemorySize is a machine's memory, in MiB: what the guest has now, what it booted with, how far
// it may grow, and the step it grows and shrinks by. A machine that may not grow has its Ceiling
// at its Base and no Block.
type MemorySize struct {
	Now, Base, Ceiling, Block int
}

// Memory is the memory the guest booted with and what virtio-mem has plugged since, and the
// virtio-mem device's ceiling and block, asked of QEMU each time: two round trips.
func (m *QEMU) Memory(ctx context.Context) (MemorySize, error) {
	summary, err := qmp.Do(ctx, m.c, queryMemorySizeSummary, qmp.None{})
	if err != nil {
		return MemorySize{}, fmt.Errorf("reading the machine's memory: %w", err)
	}
	size := MemorySize{Now: int((summary.Base + summary.Plugged) / mib), Base: int(summary.Base / mib)}
	size.Ceiling = size.Base
	// The backend's size is the ceiling: the device's own size is what it has plugged.
	objs, err := m.qomObjects(ctx, memPath, "/objects/"+memGrowthID)
	if refused := (*qmp.Error)(nil); errors.As(err, &refused) && refused.Class == "DeviceNotFound" {
		// A machine started with no ceiling above its memory has no virtio-mem (Spec.appendDevices).
		return size, nil
	}
	if err != nil {
		return MemorySize{}, fmt.Errorf("reading how far the machine's memory may grow: %w", err)
	}
	var block, growth int64
	if err := json.Unmarshal(objs[0].value("block-size"), &block); err != nil {
		return MemorySize{}, fmt.Errorf("reading virtio-mem's block size: %w", err)
	}
	if err := json.Unmarshal(objs[1].value("size"), &growth); err != nil {
		return MemorySize{}, fmt.Errorf("reading the size of the memory virtio-mem plugs from: %w", err)
	}
	size.Ceiling += int(growth / mib)
	size.Block = int(block / mib)
	return size, nil
}

// SetMemory asks virtio-mem for what is above the memory the machine booted with.
func (m *QEMU) SetMemory(ctx context.Context, mb int) error {
	size, err := m.Memory(ctx)
	if err != nil {
		return err
	}
	if mb < size.Base || mb > size.Ceiling {
		return fmt.Errorf("%d MiB, and the machine has %d MiB to %d MiB", mb, size.Base, size.Ceiling)
	}
	if _, err := qmp.Do(ctx, m.c, qomSet, qomSetArgs{Path: memPath, Property: "requested-size", Value: int64(mb-size.Base) * mib}); err != nil {
		return fmt.Errorf("asking for %d MiB: %w", mb, err)
	}
	return nil
}

// balloonPath is the balloon's QOM path, by the id Spec.appendDevices gives it.
const balloonPath = peripheral + balloonID

// statsInterval is how often, in seconds, the guest is asked to report its memory once
// GuestMemory has been called: spin's runner waits up to three reports for one after the
// request it reads it for.
const statsInterval = 1

// ErrNoGuestStats is a guest that has not reported its memory since GuestMemory first asked it
// to.
var ErrNoGuestStats = errors.New("the guest has not reported its memory yet")

// GuestMemory is what the guest last said of its memory through the balloon, in MiB, and when
// it said it.
type GuestMemory struct {
	// Total is the memory the guest's kernel manages.
	Total int
	// Available is the kernel's MemAvailable: what it could give a process without swapping,
	// which counts no tmpfs or /dev/shm.
	Available int
	// At is when the guest reported it, to the second.
	At time.Time
}

// unreported is what QEMU answers for a statistic the guest has not reported: -1 as a uint64.
const unreported = math.MaxUint64

type balloonStats struct {
	Updated int64 `json:"last-update"`
	Stats   struct {
		Total       uint64 `json:"stat-total-memory"`
		Available   uint64 `json:"stat-available-memory"`
		OOMKills    uint64 `json:"stat-oom-kills"`
		AllocStalls uint64 `json:"stat-alloc-stalls"`
	} `json:"stats"`
}

// GuestMemory is the guest's last report of its memory. The first call turns the reports on, at
// one a second: QEMU asks the guest for none until a monitor sets an interval, and set on the
// command line the interval would be a timer in every boot from its first second, on the goal's
// clock. So that call, and any before the guest's first report, is ErrNoGuestStats.
func (m *QEMU) GuestMemory(ctx context.Context) (GuestMemory, error) {
	st, err := m.guestStats(ctx)
	if err != nil {
		return GuestMemory{}, err
	}
	if st.Stats.Total == unreported || st.Stats.Available == unreported {
		return GuestMemory{}, errors.New("the guest's report of its memory has no total or no available memory")
	}
	return GuestMemory{Total: int(st.Stats.Total / mib), Available: int(st.Stats.Available / mib), At: time.Unix(st.Updated, 0)}, nil
}

// guestStats is the balloon's last report, turning the reports on at the first call.
func (m *QEMU) guestStats(ctx context.Context) (balloonStats, error) {
	if !m.polling.Load() {
		if _, err := qmp.Do(ctx, m.c, qomSet, qomSetArgs{Path: balloonPath, Property: "guest-stats-polling-interval", Value: statsInterval}); err != nil {
			return balloonStats{}, fmt.Errorf("asking the guest to report its memory: %w", err)
		}
		m.polling.Store(true)
	}
	st, err := readProperty[balloonStats](ctx, m, balloonPath, "guest-stats")
	if err != nil {
		return balloonStats{}, fmt.Errorf("reading the guest's report of its memory: %w", err)
	}
	if st.Updated == 0 {
		return balloonStats{}, ErrNoGuestStats
	}
	return st, nil
}
