// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A row's boots in a report: every phase any boot reached, in milliseconds, and a failure
// counted, with its tail, only for a boot that did not reach a login prompt.
func TestSummarise(t *testing.T) {
	ms := time.Millisecond
	runs := []Run{
		{At: map[Phase]time.Duration{Firmware: 50 * ms, Usable: 300 * ms}},
		{At: map[Phase]time.Duration{Firmware: 70 * ms, Usable: 500 * ms}},
		{At: map[Phase]time.Duration{Firmware: 60 * ms}},
	}
	b := Summarise(runs, "no login")
	if b.Boots != 3 || b.Failed != 1 || b.Error != "no login" {
		t.Errorf("boots %d, failed %d, error %q; want 3, 1, the failure", b.Boots, b.Failed, b.Error)
	}
	if got := b.Phases["firmware"]; got != (Summary{P50: 60, P95: 70, N: 3}) {
		t.Errorf("firmware %+v, want p50 60, p95 70 over 3", got)
	}
	if got := b.Phases["usable"]; got != (Summary{P50: 300, P95: 500, N: 2}) {
		t.Errorf("usable %+v, want p50 300, p95 500 over 2", got)
	}
	if _, ok := b.Phases["kernel"]; ok {
		t.Error("a phase no boot reached has a summary")
	}
	if ok := Summarise(runs[:2], "unused"); ok.Failed != 0 || ok.Error != "" {
		t.Errorf("two boots that reached a login: %+v, want no failure and no error", ok)
	}
	if got := Summarise([]Run{{At: map[Phase]time.Duration{Usable: 1234567 * time.Microsecond}}}, "").Phases["usable"].P50; got != 1234.57 {
		t.Errorf("1234.567 ms is reported as %v, want two decimals", got)
	}
}

// one is a row of a single boot, which either reached a login prompt or did not.
func one(id string, failed int) Row {
	return Row{ID: id, Boot: Boot{Boots: 1, Failed: failed}}
}

func row(id, fp string, usable float64, failed int) Row {
	return Row{ID: id, Fingerprint: fp, Args: []string{"-m", "2048"},
		Boot: Boot{Boots: 3, Failed: failed, Phases: map[string]Summary{"usable": {P50: usable, P95: usable + 10, N: 3}}}}
}

// What compare says: each row that moved and how, the artefacts that changed, and the two
// ways a comparison can be of something other than two releases.
func TestDiff(t *testing.T) {
	host := Host{CPU: "a", CPUs: 8}
	old := Report{Release: "v1", Host: host, Login: "echo", Artifacts: map[string]string{"kernel_sha256": "aaaaaaaaaaaaa", "qemu_sha256": "q"},
		Specs: []Row{row("same", "f", 100, 0), row("slower", "f", 100, 0), row("refused", "f", 100, 0),
			row("fingerprint", "f", 100, 0), row("gone", "f", 100, 0), row("just under", "f", 100, 0),
			row("sub-millisecond", "f", 0.5, 0), one("one boot", 0)},
		Variants: []Row{row("baseline", "", 200, 0)}}
	nw := Report{Release: "v2", Host: host, Login: "echo", Artifacts: map[string]string{"kernel_sha256": "bbbbbbbbbbbbbbbb", "qemu_sha256": "q"},
		Specs: []Row{row("same", "f", 101, 0), row("slower", "f", 120, 0), row("refused", "f", 0, 3),
			row("fingerprint", "g", 100, 0), row("added", "f", 90, 0), row("just under", "f", 105, 0),
			row("sub-millisecond", "f", 1, 0), one("one boot", 1)},
		Variants: []Row{row("baseline", "", 200, 0)}}
	nw.Specs[2].Boot.Phases = nil
	nw.Specs[0].Args = []string{"-m", "4096"}
	old.Specs[4].Args = []string{"-gone"}
	nw.Specs[4].Args = []string{"-added"}
	// An axis dropped from the matrix: the row is renamed and boots what it booted before.
	renamedOld, renamedNew := row("mem=a,disk=default", "f", 100, 0), row("mem=a", "f", 80, 0)
	renamedOld.Args, renamedNew.Args = []string{"-renamed"}, []string{"-renamed"}
	// Two old rows with one new row's command line: which one it was is not known.
	twinA, twinB, twinNew := row("twin,x=1", "f", 100, 0), row("twin,x=2", "f", 100, 0), row("twin", "f", 100, 0)
	twinA.Args, twinB.Args, twinNew.Args = []string{"-twin"}, []string{"-twin"}, []string{"-twin"}
	old.Specs = append(old.Specs, renamedOld, twinA, twinB)
	nw.Specs = append(nw.Specs, renamedNew, twinNew)

	var b bytes.Buffer
	Diff(&b, old, nw, 0.05)
	got := b.String()
	for _, want := range []string{
		"## v2 against v1",
		"- `kernel_sha256` changed: `aaaaaaaaaaaa` → `bbbbbbbbbbbb`",
		"| same | command line | 100.0 / 110.0 → 101.0 / 111.0 |",
		"| slower | usable p50 +20.0% | 100.0 / 110.0 → 120.0 / 130.0 |",
		"| refused | boots: some → none | 100.0 / 110.0 → - |",
		"| fingerprint | fingerprint | 100.0 / 110.0 → 100.0 / 110.0 |",
		"| added | new row | 90.0 / 100.0 |",
		"| gone | row gone | 100.0 / 110.0 |",
		"| sub-millisecond | usable p50 +100.0% |",
		"| one boot | boots: some → none |",
		"| mem=a | was mem=a,disk=default, usable p50 -20.0% | 100.0 / 110.0 → 80.0 / 90.0 |",
		"| twin | new row |",
		"| twin,x=1 | row gone |",
		"| twin,x=2 | row gone |",
		"### Image variants\n\nNo row changed beyond 5%.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the diff has no %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"qemu_sha256", "just under", "different hosts", "reached differently",
		"| mem=a,disk=default | row gone", "| mem=a | new row"} {
		if strings.Contains(got, absent) {
			t.Errorf("the diff names %q, which did not change enough:\n%s", absent, got)
		}
	}

	// A partial run, an experiment, another host and another login.
	nw.Only, nw.Flags, nw.Release = "slower", []string{"--kernel=/k"}, ""
	nw.Host, nw.Login = Host{CPU: "b", CPUs: 4}, "agetty"
	b.Reset()
	Diff(&b, old, nw, 0.9)
	got = b.String()
	for _, want := range []string{"## an unreleased tree with --kernel=/k against v1", "No row changed beyond 90%", "different hosts (a, 8 CPUs; b, 4 CPUs)", "reached differently (echo, agetty)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diff has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "row gone") {
		t.Errorf("a row a partial run did not ask about is reported gone:\n%s", got)
	}
}
