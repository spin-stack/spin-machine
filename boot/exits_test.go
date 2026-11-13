// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What the guest made KVM do in each phase of a boot: VM exits by kind, EPT faults, and how
// its memory ended up mapped, from the host.
//
// It exists because the bench fleet and a laptop disagree by 3-4x on exactly the initcalls
// that touch emulated hardware - acpi_init 14 ms against 4, virtio_pci 6.5 against 2.3 - and
// by nothing on one that does not (inet_init, 7.6 and 7.4), 2026-11-12. Time spent outside the
// guest is invisible from inside it: a trace sees a function take long, not that it took long
// because every config space access left the guest. So this counts the leaving.
//
// KVM keeps the counts in /sys/kernel/debug/kvm, summed over every VM on the host, so the
// host must run one VM at a time - the bench fleet does. A root shell reads them in a loop
// from before QEMU is exec'd to the login; boot:waits's dump, in the same boot, says where on
// the host's clock each phase began, and each phase gets the counts that rose inside it.
//
//	SPIN_BOOT_EXITS=1  run at all
//	REPS=<n>           boots of each row (default 5)
//	CPUS=<n>           vCPUs (default 2)
//
// The rows: the machine as built, and the same machine without its virtio-rng device - a
// machine with another fingerprint, booted through machineCommand.
func TestExitTimeline(t *testing.T) {
	if os.Getenv("SPIN_BOOT_EXITS") == "" {
		t.Skip("set SPIN_BOOT_EXITS=1: boots VMs and reads KVM's counters through sudo")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("KVM's counters are in debugfs, which needs root")
	}
	if err := exec.Command("sudo", "test", "-r", "/sys/kernel/debug/kvm/exits").Run(); err != nil {
		t.Skip("no /sys/kernel/debug/kvm/exits: debugfs is not mounted, or KVM keeps no counters here")
	}
	reps := envInt(t, "REPS", 5)
	cpus := cmp.Or(os.Getenv("CPUS"), "2")
	const target = "serial-getty@ttyS0.service"

	asBuilt := waitsVariant(cpus)
	noRNG := waitsVariant(cpus)
	noRNG.label, noRNG.without = "no virtio-rng", []string{"virtio-rng-pci"}
	rows := []variant{asBuilt, noRNG}
	rows[0].label = "as built"

	got := map[string][][]phaseCounts{}
	ram := map[string][]hostRAM{}
	for range reps {
		for _, v := range rows {
			stop := sampleKVM(t)
			b := bootWaits(t, out, v, target, "SPIN-WAITS-END")
			got[v.label] = append(got[v.label], phasesOf(b, stop()))
			ram[v.label] = append(ram[v.label], b.ram)
		}
	}

	var r strings.Builder
	fmt.Fprintf(&r, "\nwhat each phase made KVM do, p50 over %d boots of each, %s vCPUs, interleaved\n", reps, cpus)
	for _, v := range rows {
		fmt.Fprintf(&r, "\n  %s\n\n  %-28s %7s %8s %7s %7s %8s %7s %7s\n", v.label,
			"PHASE", "MS", "EXITS", "IO", "MMIO", "EPT-PF", "HALT", "EMUL")
		for i, name := range phaseNames {
			col := func(f func(phaseCounts) float64) float64 {
				var xs []float64
				for _, boot := range got[v.label] {
					xs = append(xs, f(boot[i]))
				}
				return pct(xs, 50)
			}
			fmt.Fprintf(&r, "  %-28s %7.1f %8.0f %7.0f %7.0f %8.0f %7.0f %7.0f\n", name,
				col(func(p phaseCounts) float64 { return p.ms }),
				col(func(p phaseCounts) float64 { return float64(p.d[cExits]) }),
				col(func(p phaseCounts) float64 { return float64(p.d[cIO]) }),
				col(func(p phaseCounts) float64 { return float64(p.d[cMMIO]) }),
				col(func(p phaseCounts) float64 { return float64(p.d[cPF]) }),
				col(func(p phaseCounts) float64 { return float64(p.d[cHalt]) }),
				col(func(p phaseCounts) float64 { return float64(p.d[cEmul]) }))
		}
		last := got[v.label]
		var k4, m2, g1 []float64
		for _, boot := range last {
			end := boot[len(boot)-1].end
			k4, m2, g1 = append(k4, float64(end[c4k])), append(m2, float64(end[c2m])), append(g1, float64(end[c1g]))
		}
		var rss, huge []float64
		for _, b := range ram[v.label] {
			rss, huge = append(rss, b.residentMB), append(huge, b.hugeMB)
		}
		fmt.Fprintf(&r, "\n  at the login, p50: KVM maps the guest in %.0f 4K pages, %.0f 2M, %.0f 1G;\n"+
			"  the host holds %.0f MiB of its RAM, %.0f MiB of it in huge pages\n",
			pct(k4, 50), pct(m2, 50), pct(g1, 50), pct(rss, 50), pct(huge, 50))
	}
	t.Log(r.String())
}

// The counters, in the order the sampler prints them.
var kvmCounters = []string{"exits", "io_exits", "mmio_exits", "pf_taken", "halt_exits",
	"insn_emulation", "pages_4k", "pages_2m", "pages_1g"}

const (
	cExits = iota
	cIO
	cMMIO
	cPF
	cHalt
	cEmul
	c4k
	c2m
	c1g
)

type kvmSample struct {
	at time.Time
	v  []int64
}

// sampleKVM starts a root shell that prints the counters with a nanosecond timestamp as fast as
// it can, waits for its first sample, and returns a function that stops it and returns what it
// read. Without a sleep: a sample is a fork of date and nine reads, a millisecond or so, on a
// core of its own.
func sampleKVM(t *testing.T) (stop func() []kvmSample) {
	t.Helper()
	dir := t.TempDir()
	halt := filepath.Join(dir, "stop")
	script := `cd /sys/kernel/debug/kvm || exit 1
while [ ! -e "$1" ]; do
	line=$(date +%s%N)
	for f in ` + strings.Join(kvmCounters, " ") + `; do read v < "$f"; line="$line $v"; done
	echo "$line"
done`
	cmd := exec.Command("sudo", "sh", "-c", script, "sampler", halt)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("sampler pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the sampler: %v", err)
	}
	done := make(chan []kvmSample)
	first := make(chan struct{})
	go func() {
		var got []kvmSample
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 1+len(kvmCounters) {
				continue
			}
			ns, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				continue
			}
			s := kvmSample{at: time.Unix(0, ns)}
			for _, x := range f[1:] {
				n, _ := strconv.ParseInt(x, 10, 64)
				s.v = append(s.v, n)
			}
			got = append(got, s)
			if len(got) == 1 {
				close(first)
			}
		}
		done <- got
	}()
	// The first sample is taken before the machine is launched, so a phase that starts at the
	// launch has a count to start from.
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler printed nothing in ten seconds")
	}
	return func() []kvmSample {
		if err := os.WriteFile(halt, nil, 0o644); err != nil {
			t.Fatalf("stopping the sampler: %v", err)
		}
		_ = cmd.Wait()
		s := <-done
		if len(s) < 10 {
			t.Fatalf("the sampler read %d samples", len(s))
		}
		return s
	}
}

var phaseNames = []string{
	"QEMU and firmware",
	"kernel",
	"PID 1 to units loaded",
	"units loaded to getty forked",
	"getty forked to login prompt",
}

type phaseCounts struct {
	ms  float64
	d   []int64 // counts that rose in the phase
	end []int64 // counts at its end
}

// phasesOf cuts one boot's samples at its phase boundaries: the launch, guest monotonic zero,
// PID 1, the end of the unit load, the getty's fork and the login prompt, each on the host's
// clock by boot:waits's anchor.
func phasesOf(b waitsBoot, s []kvmSample) []phaseCounts {
	at := func(d time.Duration) time.Time { return b.start.Add(d) }
	edges := []time.Time{
		b.start,
		at(b.kernel),
		at(b.kernel + b.w.Manager.Userspace),
		at(b.kernel + b.w.Manager.UnitsLoadFinish),
		at(b.kernel + b.target().Start),
		at(b.kernel + b.usable),
	}
	// The last sample taken at or before t.
	value := func(t time.Time) []int64 {
		i, _ := slices.BinarySearchFunc(s, t, func(x kvmSample, t time.Time) int { return x.at.Compare(t) })
		if i < len(s) && s[i].at.Equal(t) {
			return s[i].v
		}
		return s[max(0, i-1)].v
	}
	var out []phaseCounts
	for i := 0; i+1 < len(edges); i++ {
		a, z := value(edges[i]), value(edges[i+1])
		p := phaseCounts{ms: float64(edges[i+1].Sub(edges[i])) / float64(time.Millisecond), end: z}
		for j := range a {
			p.d = append(p.d, z[j]-a[j])
		}
		out = append(out, p)
	}
	return out
}
