// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Report is one run of the feature matrix against one release tree: what each combination
// of machine features is (its command line, shape and fingerprint), whether QEMU runs it,
// and what a boot of it costs, beside the boot variants of the image. It is written as JSON
// so that two of them - two releases, or a release and an experiment built from it - can be
// compared by Diff rather than by eye.
//
// Two reports compare on the numbers only when they came from the same kind of host: boot
// times are wall clock, and Host says what the clock was measured on.
type Report struct {
	// Release is the version machine.env names, or empty for a tree that was never packed.
	Release string `json:"release"`
	// Artifacts are machine.env's hashes: the files the fingerprint is made of, and the image.
	Artifacts map[string]string `json:"artifacts"`
	Host      Host              `json:"host"`
	Reps      int               `json:"reps"`
	// Flags is what the caller added to every boot, for an experiment: another kernel, a
	// kernel parameter. A report with flags describes that experiment, not the release.
	Flags []string `json:"flags,omitempty"`
	// Only is the pattern that picked the rows, when not all of them were run: a row such a
	// report does not have was not asked about, and is not gone.
	Only string `json:"only,omitempty"`
	// Login is how a login prompt was reached: "echo" when the getty was replaced by one, so
	// agetty's unconditional second is not in the usable column; "agetty" when it was not.
	Login string `json:"login"`
	// FirstLogin is the goal's number: the release as built, logged into over SSH through its
	// vsock from the moment before QEMU is exec'd, in phases "accepts" (sshd takes a
	// connection) and "login" (`true` has run as spin). The rows below time the serial
	// console's prompt, which nothing a workspace does waits for: a change can move the two
	// in opposite directions, as v20261113.03 did. Absent from a report made without sudo or
	// /dev/vhost-vsock, and from releases before it was added.
	FirstLogin *Boot     `json:"first_login,omitempty"`
	Specs      []Row     `json:"specs"`
	Variants   []Row     `json:"variants"`
	Skipped    []Skipped `json:"skipped,omitempty"`
}

// Host is what the boot times were measured on.
type Host struct {
	CPU    string `json:"cpu"`
	Kernel string `json:"kernel"`
	CPUs   int    `json:"cpus"`
}

// Row is one configuration: a combination of machine features, or a variant of the image.
type Row struct {
	ID string `json:"id"`
	// Features names the choice made on each axis, for a spec row.
	Features map[string]string `json:"features,omitempty"`
	// Args is the command line with the release directory written as $RELEASE and the
	// per-boot files at fixed names, so that it compares across hosts and runs.
	Args        []string `json:"args,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Shape       any      `json:"shape,omitempty"`
	Boot        Boot     `json:"boot"`
}

// Boot is what the boots of one row did.
type Boot struct {
	Boots int `json:"boots"`
	// Failed counts boots that did not reach a login prompt. A row that fails every boot is
	// one QEMU or the guest refuses; Error holds the first failure's tail.
	Failed int                `json:"failed"`
	Error  string             `json:"error,omitempty"`
	Phases map[string]Summary `json:"phases,omitempty"`
}

// Summary is one phase over a row's boots, in milliseconds from QEMU's exec.
type Summary struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	N   int     `json:"n"`
}

// Skipped is a row the run could not measure on this host, and why.
type Skipped struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

// Summarise turns one row's runs into a Boot.
func Summarise(runs []Run, failure string) Boot {
	b := Boot{Boots: len(runs), Phases: map[string]Summary{}}
	for _, r := range runs {
		if !r.Reached(Usable) {
			b.Failed++
		}
	}
	if b.Failed > 0 {
		b.Error = failure
	}
	for p := range numPhases {
		var ds []time.Duration
		for _, r := range runs {
			if d, ok := r.At[p]; ok {
				ds = append(ds, d)
			}
		}
		if s, ok := SummaryOf(ds); ok {
			b.Phases[p.String()] = s
		}
	}
	return b
}

// SummaryOf is the p50 and p95 of ds, and false when ds is empty.
func SummaryOf(ds []time.Duration) (Summary, bool) {
	p50, ok := Percentile(ds, 0.5)
	if !ok {
		return Summary{}, false
	}
	p95, _ := Percentile(ds, 0.95)
	return Summary{P50: ms(p50), P95: ms(p95), N: len(ds)}, true
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// Diff writes, as Markdown, what changed from old to new: every row whose command line,
// fingerprint or outcome moved, and every row whose time to a usable machine moved by more
// than threshold (a fraction: 0.05 is five percent). Rows only one side has are named too,
// because a matrix that lost a row is a change of its own.
//
// It is deliberately not a verdict. A slower row is a question for whoever reads it; the
// same host kind on both sides is what makes it one worth asking, and the header says
// whether the two were measured on the same kind.
func Diff(w io.Writer, old, new Report, threshold float64) {
	fmt.Fprintf(w, "## %s against %s\n\n", name(new), name(old))
	if old.Host != new.Host {
		fmt.Fprintf(w, "Measured on different hosts (%s, %d CPUs; %s, %d CPUs): the times below compare two machines, not two releases.\n\n",
			old.Host.CPU, old.Host.CPUs, new.Host.CPU, new.Host.CPUs)
	}
	if old.Login != new.Login {
		fmt.Fprintf(w, "The login prompt was reached differently (%s, %s): the usable column moves by agetty's second for that reason alone.\n\n",
			old.Login, new.Login)
	}
	for _, k := range sortedKeys(old.Artifacts, new.Artifacts) {
		if a, b := old.Artifacts[k], new.Artifacts[k]; a != b {
			fmt.Fprintf(w, "- `%s` changed: `%s` → `%s`\n", k, short(a), short(b))
		}
	}
	firstLogin(w, old.FirstLogin, new.FirstLogin)
	partial := new.Only != ""
	section(w, "Feature matrix", old.Specs, new.Specs, threshold, partial)
	section(w, "Image variants", old.Variants, new.Variants, threshold, partial)
}

// firstLogin writes the goal's number first and always, moved or not: it is the one a change is
// decided on, and the sections after it time the console's prompt.
func firstLogin(w io.Writer, old, new *Boot) {
	if old == nil && new == nil {
		return
	}
	fmt.Fprintf(w, "\n### First SSH login\n\n")
	fmt.Fprintln(w, "| phase | p50 / p95 (ms) | p50 |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, p := range []string{"accepts", "login"} {
		a, aok := phase(old, p)
		b, bok := phase(new, p)
		move := "-"
		if aok && bok && a.P50 > 0 {
			move = fmt.Sprintf("%+.1f%%", 100*(b.P50-a.P50)/a.P50)
		}
		fmt.Fprintf(w, "| %s | %s → %s | %s |\n", p, summary(a, aok), summary(b, bok), move)
	}
	if new != nil && new.Failed > 0 {
		fmt.Fprintf(w, "\n%d of %d boots had no login: %s\n", new.Failed, new.Boots, new.Error)
	}
	fmt.Fprintln(w, "\nThe sections below time the serial console's prompt, which no workspace waits for.")
}

func phase(b *Boot, p string) (Summary, bool) {
	if b == nil {
		return Summary{}, false
	}
	s, ok := b.Phases[p]
	return s, ok
}

func summary(s Summary, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.1f / %.1f", s.P50, s.P95)
}

func section(w io.Writer, title string, old, new []Row, threshold float64, partial bool) {
	before := map[string]Row{}
	for _, r := range old {
		before[r.ID] = r
	}
	renamed := renames(old, new)
	var lines []string
	seen := map[string]bool{}
	for _, r := range new {
		seen[r.ID] = true
		o, ok := before[r.ID]
		var what []string
		if !ok {
			if o, ok = renamed[r.ID]; !ok {
				lines = append(lines, fmt.Sprintf("| %s | new row | %s |", r.ID, usable(r)))
				continue
			}
			seen[o.ID] = true
			what = append(what, "was "+o.ID)
		}
		if o.Fingerprint != r.Fingerprint {
			what = append(what, "fingerprint")
		}
		if !slices.Equal(o.Args, r.Args) {
			what = append(what, "command line")
		}
		if (o.Boot.Failed == o.Boot.Boots) != (r.Boot.Failed == r.Boot.Boots) {
			what = append(what, "boots: "+outcome(o)+" → "+outcome(r))
		}
		a, aok := o.Boot.Phases["usable"]
		b, bok := r.Boot.Phases["usable"]
		if aok && bok && a.P50 > 0 && math.Abs(b.P50-a.P50)/a.P50 > threshold {
			what = append(what, fmt.Sprintf("usable p50 %+.1f%%", 100*(b.P50-a.P50)/a.P50))
		}
		if len(what) > 0 {
			lines = append(lines, fmt.Sprintf("| %s | %s | %s → %s |", r.ID, strings.Join(what, ", "), usable(o), usable(r)))
		}
	}
	for _, r := range old {
		if !seen[r.ID] && !partial {
			lines = append(lines, fmt.Sprintf("| %s | row gone | %s |", r.ID, usable(r)))
		}
	}
	fmt.Fprintf(w, "\n### %s\n\n", title)
	if len(lines) == 0 {
		fmt.Fprintf(w, "No row changed beyond %.0f%%.\n", 100*threshold)
		return
	}
	fmt.Fprintln(w, "| row | what changed | usable p50 / p95 (ms) |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// renames pairs a row whose id only one report has with the row of the other that stands for
// it: a row's id is made of the matrix's axes, so an axis added or removed renames every row,
// and a comparison by id alone would call them all new and gone.
//
// Two rows can stand for each other when they agree on every axis both reports have, and the
// pair is the two that boot the nearest command lines, each the other's nearest and with no
// tie. Nearest and not equal, because two releases apart the command line has moved too (a
// firmware added, say) on every row alike: that is the change being compared, and the dropped
// axis's value that added nothing to the command line is still the closest. Rows with no axes
// (the image's variants) pair only on an identical command line. Anything else is left to read
// as new and gone.
func renames(old, new []Row) map[string]Row {
	ids := map[string]bool{}
	for _, r := range old {
		ids[r.ID] = true
	}
	newIDs := map[string]bool{}
	for _, r := range new {
		newIDs[r.ID] = true
	}
	var olds, news []Row
	for _, r := range old {
		if !newIDs[r.ID] {
			olds = append(olds, r)
		}
	}
	for _, r := range new {
		if !ids[r.ID] {
			news = append(news, r)
		}
	}
	nearest := func(r Row, among []Row) (Row, bool) {
		var (
			best       Row
			n          int
			found, tie bool
		)
		for _, c := range among {
			if !sameAxes(r, c) {
				continue
			}
			d := distance(r.Args, c.Args)
			if len(r.Features) == 0 && d != 0 {
				continue
			}
			switch {
			case !found || d < n:
				best, n, found, tie = c, d, true, false
			case d == n:
				tie = true
			}
		}
		return best, found && !tie
	}
	out := map[string]Row{}
	for _, r := range news {
		o, ok := nearest(r, olds)
		if !ok {
			continue
		}
		if back, ok := nearest(o, news); ok && back.ID == r.ID {
			out[r.ID] = o
		}
	}
	return out
}

// sameAxes says whether two rows made the same choice on every axis both have. A row with no
// axes has none in common with one that has them.
func sameAxes(a, b Row) bool {
	if (len(a.Features) == 0) != (len(b.Features) == 0) {
		return false
	}
	for k, v := range a.Features {
		if w, ok := b.Features[k]; ok && w != v {
			return false
		}
	}
	return true
}

// distance is how many arguments one command line has that the other does not, counting
// repeats.
func distance(a, b []string) int {
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
	}
	d := 0
	for _, n := range count {
		d += max(n, -n)
	}
	return d
}

func outcome(r Row) string {
	if r.Boot.Boots > 0 && r.Boot.Failed == r.Boot.Boots {
		return "none"
	}
	return "some"
}

func usable(r Row) string {
	s, ok := r.Boot.Phases["usable"]
	return summary(s, ok)
}

func name(r Report) string {
	n := r.Release
	if n == "" {
		n = "an unreleased tree"
	}
	if len(r.Flags) > 0 {
		n += " with " + strings.Join(r.Flags, " ")
	}
	return n
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func sortedKeys(a, b map[string]string) []string {
	var ks []string
	for k := range a {
		ks = append(ks, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			ks = append(ks, k)
		}
	}
	slices.Sort(ks)
	return ks
}
