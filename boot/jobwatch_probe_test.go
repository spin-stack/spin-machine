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
	"strings"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/boot"
)

// PID 1's job queue as it changed during the boot, recorded live by boot/jobwatch over
// systemd's private socket, beside task boot:waits's account of the same boot.
//
// Live means a process on the guest's vCPUs during the phase being measured, so this
// interleaves boots with jobwatch and without it, and the first table is what recording cost.
// CPUS=3 gives the recorder a vCPU to itself and is compared against three vCPUs without it in
// the same run: a vCPU more is a different machine, and its numbers are not the shipped
// machine's.
//
// After the boot it makes the first SSH login (testdata/first-login.sh) and records the jobs that
// sets off - sshd through ssh.socket, logind, the user's manager - which is the part of being
// usable that a workspace waits for and the boot's own timestamps never contain.
//
//	SPIN_BOOT_JOBS=1  run at all
//	REPS=<n>          boots of each variant (default 5)
//	CPUS=<n>          vCPUs of both (default 2)
//	LINGER=<d>        how long jobwatch keeps recording after startup finishes (default 2s)
//	BUCKET=<ms>       the queue's resolution over time (default 10)
//	RAW=<dir>         also write each recorded boot's two dumps there, as testdata is made
//	TARGET=, TOP=, GAP= as task boot:waits
func TestBootJobs(t *testing.T) {
	if os.Getenv("SPIN_BOOT_JOBS") == "" {
		t.Skip("set SPIN_BOOT_JOBS=1: boots VMs and needs sudo to write into their overlays")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit and a binary into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	reps := envInt(t, "REPS", 5)
	top := envInt(t, "TOP", 15)
	minGap := time.Duration(envInt(t, "GAP", 1)) * time.Millisecond
	bucket := time.Duration(envInt(t, "BUCKET", 10)) * time.Millisecond
	target := cmp.Or(os.Getenv("TARGET"), "serial-getty@ttyS0.service")
	raw := os.Getenv("RAW")
	linger := cmp.Or(os.Getenv("LINGER"), "2s")
	if _, err := time.ParseDuration(linger); err != nil {
		t.Fatalf("LINGER=%q: %v", linger, err)
	}

	// Static, because it runs in a guest whose libraries are not this host's.
	bin := filepath.Join(t.TempDir(), "spin-jobwatch")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "./jobwatch")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building jobwatch: %v\n%s", err, b)
	}

	plain := waitsVariant(cmp.Or(os.Getenv("CPUS"), "2"))
	watched := waitsVariant(plain.cpus)
	watched.label = "jobwatch"
	// DefaultDependencies=no and nothing it is ordered after, so it starts with the first
	// dispatch of the initial transaction. It writes its record once spin-login.service is
	// done, since everything here shares the console.
	watched.files["/etc/systemd/system/spin-jobwatch.service"] = `[Unit]
Description=Record PID 1's jobs as they happen
DefaultDependencies=no
[Service]
Type=simple
ExecStart=/usr/local/sbin/spin-jobwatch /dev/ttyS0 ` + linger + ` spin-login.service
[Install]
WantedBy=sysinit.target
`
	watched.links["/etc/systemd/system/sysinit.target.wants/spin-jobwatch.service"] = "/etc/systemd/system/spin-jobwatch.service"
	watched.setup = `install -D -m 0755 ` + bin + ` "$MNT/usr/local/sbin/spin-jobwatch"`

	// The first SSH login, started when spin-waits.service exits rather than wanted by a
	// target: a oneshot in the initial transaction would hold startup open until it was done,
	// and move the finish timestamp boot:waits reads.
	script, err := os.ReadFile(filepath.Join("testdata", "first-login.sh"))
	if err != nil {
		t.Fatal(err)
	}
	watched.files["/usr/local/sbin/spin-first-login.sh"] = string(script)
	watched.files["/etc/systemd/system/spin-login.service"] = `[Unit]
Description=The first SSH login, for jobwatch to record
[Service]
Type=oneshot
StandardOutput=null
ExecStart=/bin/sh /usr/local/sbin/spin-first-login.sh
`
	watched.files["/etc/systemd/system/spin-waits.service.d/login.conf"] = "[Service]\nExecStopPost=/bin/systemctl start --no-block spin-login.service\n"

	type jobsBoot struct {
		waitsBoot
		log   boot.JobLog
		login string // the first SSH login in ms as the guest timed it, or FAIL
		again string // a second, right after it
		cold  string // a third, after the page cache was dropped
	}
	var base []waitsBoot
	var rec []jobsBoot
	for rep := range reps {
		base = append(base, bootWaits(t, out, plain, target, "SPIN-WAITS-END"))
		b := bootWaits(t, out, watched, target, "SPIN-JOBS-END")
		s := b.console
		begin, stop := strings.Index(s, "SPIN-JOBS-BEGIN"), strings.Index(s, "SPIN-JOBS-END")
		if begin < 0 || stop < begin {
			t.Fatalf("no record between SPIN-JOBS-BEGIN and -END; tail:\n%s", tail([]byte(s), 2000))
		}
		record := s[begin+len("SPIN-JOBS-BEGIN") : stop]
		if raw != "" {
			for name, content := range map[string]string{"waits": b.dump, "jobs": record} {
				f := filepath.Join(raw, fmt.Sprintf("%s-%02d.txt", name, rep+1))
				if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
					t.Fatalf("writing %s: %v", f, err)
				}
			}
		}
		l, err := boot.ParseJobs(record)
		if err != nil {
			t.Fatalf("reading jobwatch's record: %v", err)
		}
		m := reFirstLogin.FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("no SPIN-LOGIN line: the first login did not run; tail:\n%s", tail([]byte(s), 2000))
		}
		rec = append(rec, jobsBoot{b, l, m[1], m[2], m[3]})
	}

	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }
	p := func(ds []time.Duration, q float64) string { d, _ := boot.Percentile(ds, q); return ms(d) }
	var b strings.Builder

	fmt.Fprintf(&b, "\nWHAT RECORDING COSTS: ms on the host's clock, p50/p95 over %d boots of each, %s vCPUs, interleaved\n\n", reps, plain.cpus)
	fmt.Fprintf(&b, "  %-12s %15s %15s %15s\n", "", "UNITS LOADED", "GETTY FORKED", "LOGIN")
	row := func(name string, bs []waitsBoot) {
		var loaded, forked, login []time.Duration
		for _, x := range bs {
			loaded = append(loaded, x.kernel+x.w.Manager.UnitsLoadFinish)
			forked = append(forked, x.kernel+x.target().Start)
			login = append(login, x.kernel+x.usable)
		}
		fmt.Fprintf(&b, "  %-12s %7s/%-7s %7s/%-7s %7s/%-7s\n", name,
			p(loaded, .5), p(loaded, .95), p(forked, .5), p(forked, .95), p(login, .5), p(login, .95))
	}
	row("without", base)
	var recW []waitsBoot
	for _, x := range rec {
		recW = append(recW, x.waitsBoot)
	}
	row("jobwatch", recW)

	var logins, again, cold []string
	for _, x := range rec {
		logins, again, cold = append(logins, x.login), append(again, x.again), append(cold, x.cold)
	}
	fmt.Fprintf(&b, "\n  SSH logins after the boot, ms as the guest timed them, p50 / p95 / max\n"+
		"    the first                      %s\n    a second, right after          %s\n    a third, page cache dropped    %s\n",
		loginSummary(logins), loginSummary(again), loginSummary(cold))

	var lag []time.Duration
	var connected []time.Duration
	for _, x := range rec {
		lag = append(lag, x.log.Lag(x.w)...)
		connected = append(connected, x.log.Connected)
	}
	fmt.Fprintf(&b, "\n  jobwatch subscribed at %s ms of guest time (p50), units loaded at %s; it heard a unit\n"+
		"  become active %s ms after systemd stamped it (p50, p95 %s, %d events)\n",
		p(connected, .5), ms(rec[0].w.Manager.UnitsLoadFinish), p(lag, .5), p(lag, .95), len(lag))

	med := slices.Clone(rec)
	slices.SortFunc(med, func(a, c jobsBoot) int { return cmp.Compare(a.kernel+a.usable, c.kernel+c.usable) })
	x := med[len(med)/2]
	login := max(x.usable, x.target().Ready)
	fmt.Fprintf(&b, "\nTHE MEDIAN RECORDED BOOT, login at %s ms; guest ms from the kernel's start\n", ms(x.kernel+x.usable))

	// The queue over time: what bootvine's tree is, one line per bucket. Running is systemd
	// working on a job - a service starting, a device job waiting for udev - and waiting is a
	// job held behind another's ordering.
	fmt.Fprintf(&b, "\n  PID 1's queue every %s ms, from jobwatch subscribing to the login\n\n", ms(bucket))
	fmt.Fprintf(&b, "  %8s %7s %7s  %s\n", "AT", "WAITING", "RUNNING", "RUNNING JOBS")
	for at := x.log.Connected.Truncate(bucket) + bucket; at <= login+bucket; at += bucket {
		waiting, running := x.log.Pending(at)
		var names []string
		for _, j := range running {
			names = append(names, j.Unit)
		}
		slices.Sort(names)
		fmt.Fprintf(&b, "  %8s %7d %7d  %s\n", ms(at), len(waiting), len(running), clipList(names, 4))
	}

	// What systemd was doing while no unit was activating: the running jobs are what it was
	// waiting on, and a gap with none was systemd's own time.
	fmt.Fprintf(&b, "\n  the boot:waits gaps, with the jobs that were running through them\n\n")
	for _, g := range x.w.Gaps(x.target().Start, minGap) {
		mid := g.From + (g.To-g.From)/2
		_, running := x.log.Pending(mid)
		var names []string
		for _, j := range running {
			names = append(names, j.Unit)
		}
		slices.Sort(names)
		what := clipList(names, 4)
		if g.From < x.log.Connected {
			what += " (jobwatch not yet subscribed for part of it)"
		}
		fmt.Fprintf(&b, "  %8s %8s %7s  %s\n", ms(g.From), ms(g.To), ms(g.To-g.From), cmp.Or(what, "(no job running)"))
	}

	unfinished := x.log.Unfinished()
	fmt.Fprintf(&b, "\n  jobs that did not end done: %d\n\n", len(unfinished))
	for _, j := range unfinished[:min(top, len(unfinished))] {
		fmt.Fprintf(&b, "  %8s  %-10s %s\n", ms(j.Removed), cmp.Or(j.Result, "pending"), j.Unit)
	}

	late := x.log.Late()
	fmt.Fprintf(&b, "\n  jobs queued after jobwatch subscribed: %d (startup finished at %s; the first login took %s ms)\n\n",
		len(late), ms(x.w.Manager.Finish), x.login)
	fmt.Fprintf(&b, "  %8s %8s %8s  %-10s %s\n", "QUEUED", "RUNNING", "REMOVED", "RESULT", "UNIT")
	for _, j := range late[:min(top, len(late))] {
		fmt.Fprintf(&b, "  %8s %8s %8s  %-10s %s\n", ms(j.Queued), msOrDash(j.Running), msOrDash(j.Removed), cmp.Or(j.Result, "pending"), j.Unit)
	}
	t.Log(b.String())
}

// SPIN-LOGIN 123 again=45 cold=80, any of them FAIL: testdata/first-login.sh's one line.
var reFirstLogin = regexp.MustCompile(`SPIN-LOGIN (\d+|FAIL) again=(\d+|FAIL) cold=(\d+|FAIL)`)

func msOrDash(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond))
}

func clipList(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, " ")
	}
	return strings.Join(names[:n], " ") + fmt.Sprintf(" +%d", len(names)-n)
}
