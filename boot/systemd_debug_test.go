// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Inside early userspace, in systemd's own words.
//
// `systemd-analyze` says userspace is 139 ms and `blame` names dev-vda.device at 109, but
// neither says what was happening during it, and the critical chain is a list of what
// finished last rather than what was waiting. This turns systemd's debug log on and reads the
// gaps in it, which is the only account of the phase that comes from the thing running it.
//
// Not to the console, which is the same discipline the initcall probe needs for the same
// reason: debug logging is thousands of lines and on a serial port every one is a VM exit
// inside the phase being measured. And not to kmsg either, which was the first attempt and
// lost data: systemd rate-limits its own kmsg output and says so in the log it is writing —
// "Too many messages being logged to kmsg, ignoring" — so lines disappear in bursts, which
// is exactly where a gap would be.
//
// So the default target, which is journal-or-kmsg: systemd uses kmsg until journald exists
// and the journal after, journald keeps both, and `journalctl -o short-monotonic` prints the
// lot against the monotonic clock. Reading the journal also settles a bug the kmsg version
// had: in dmesg, kernel lines like "IOAPIC[0]: apic_id 0" are indistinguishable from a
// userspace "systemd[1]: ..." by shape, so 61.6 ms of kernel time was attributed to a writer
// called IOAPIC. journalctl puts a hostname before the tag and gives the kernel no pid.
//
// This still inflates early userspace, so the absolute numbers are not comparable to
// `task boot:bench` — but the shape is, and the shape is what nothing else shows.
//
//	SPIN_SYSTEMD_DEBUG=1  run at all
//	TOP=<n>               how many gaps to print (default 25)
//	GAP=<ms>              ignore gaps under this (default 2)
func TestSystemdDebug(t *testing.T) {
	if os.Getenv("SPIN_SYSTEMD_DEBUG") == "" {
		t.Skip("set SPIN_SYSTEMD_DEBUG=1: boots a VM and needs sudo to write into its overlay")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	top := envInt(t, "TOP", 25)
	minGap := float64(envInt(t, "GAP", 2))

	const dump = `[Unit]
Description=Print the kernel ring buffer for the systemd debug probe
After=multi-user.target
[Service]
Type=simple
StandardOutput=null
ExecStart=/bin/sh -c "{ echo SPIN-DMESG-BEGIN; journalctl -b -o short-monotonic --no-pager; echo SPIN-DMESG-END; } > /dev/ttyS0"
[Install]
WantedBy=multi-user.target
`
	v := variant{
		label: "systemd debug", cpus: "2", memory: "2048",
		// log_buf_len because the part before journald exists still goes to kmsg, and that is
		// the part nothing else can see.
		extra: "systemd.log_level=debug printk.devkmsg=on log_buf_len=16M",
		files: map[string]string{
			"/etc/systemd/system/spin-dmesg.service": dump,
			// The third way this measurement loses data, after the serial console's cost and
			// kmsg's rate limit. The runtime journal is a tmpfs with a default cap, and at
			// debug level it fills during the boot: journald logs "Journal header limits
			// reached or header out-of-date, rotating" and then "Vacuuming", and what it
			// vacuums is the beginning — the part before journald existed, which is the only
			// part nothing else can see. It left 48 of some nine hundred lines behind, and a
			// ranked list of gaps over 48 lines looks like an answer.
			"/etc/systemd/journald.conf.d/zz-probe.conf": "[Journal]\n" +
				"RuntimeMaxUse=256M\nRuntimeMaxFileSize=256M\nRateLimitIntervalSec=0\nRateLimitBurst=0\n",
		},
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/spin-dmesg.service": "/etc/systemd/system/spin-dmesg.service",
		},
	}

	console := bootUntil(t, out, v, "", "SPIN-DMESG-END", 120*time.Second)
	if f := os.Getenv("SPIN_CONSOLE_OUT"); f != "" {
		if err := os.WriteFile(f, []byte(console), 0o644); err != nil {
			t.Fatalf("writing the console to %s: %v", f, err)
		}
		t.Logf("raw console written to %s", f)
	}
	reportSystemd(t, console, top, minGap)
}

// journalctl -o short-monotonic: "[    2.123456] hostname systemd[1]: message".
//
// The pid is required, and that is the whole point of the pattern: the kernel's own lines
// come through as "hostname kernel: message" with no pid, so requiring one is what keeps a
// kernel message from being counted as a process that spent time.
// "Vacuuming done, freed 0B of archived journals from …" — anything but 0B is loss.
var reVacuumed = regexp.MustCompile(`Vacuuming done, freed ((?:[1-9][0-9]*(?:\.[0-9]+)?)[KMGT]?i?B)`)

var reJournal = regexp.MustCompile(`^\[\s*(\d+)\.(\d+)\]\s+\S+\s+([a-zA-Z0-9@._:\\-]+)\[\d+\]:\s*(.*)$`)

func reportSystemd(t *testing.T, console string, top int, minGap float64) {
	t.Helper()
	type ev struct {
		at  float64
		who string
		msg string
	}
	var evs []ev
	var first, last float64
	for _, line := range strings.Split(console, "\n") {
		m := reJournal.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		sec, _ := strconv.Atoi(m[1])
		frac := m[2]
		for len(frac) < 6 {
			frac += "0"
		}
		us, _ := strconv.Atoi(frac[:6])
		at := float64(sec) + float64(us)/1e6
		evs = append(evs, ev{at: at, who: m[3], msg: clip(m[4])})
	}
	// Sorted, because journalctl does not print in timestamp order. Two writers' streams
	// arrive interleaved and come out that way:
	//
	//	[    1.046419] localhost systemd[1]: tmp.mount: Child 62 belongs to tmp.mount.
	//	[    1.049546] localhost systemd-journald[63]: Data hash table … suggesting rotation.
	//	[    1.046434] localhost systemd[1]: tmp.mount: Mount process exited …
	//
	// The first version of this treated a stamp going backwards as the end of the window and
	// stopped, which left it ranking gaps across fifteen lines of a nine-hundred-line log and
	// reporting the largest gap in the boot as 8.9 ms.
	sort.Slice(evs, func(i, j int) bool { return evs[i].at < evs[j].at })
	if len(evs) > 0 {
		first, last = evs[0].at, evs[len(evs)-1].at
	}
	if len(evs) < 2 {
		t.Fatalf("fewer than two tagged userspace lines in the journal; did "+
			"systemd.log_level=debug take effect, and did journalctl run?\nconsole tail:\n%s",
			tail([]byte(console), 1500))
	}
	// The rate limit that made the kmsg version lose data. Asserted rather than hoped for:
	// a log with holes in it ranks gaps that are missing lines, not gaps that are time.
	if strings.Contains(console, "Too many messages being logged to kmsg") {
		t.Errorf("systemd rate-limited its own kmsg output, so the log has holes and the " +
			"gaps below may be missing lines rather than containing time")
	}
	// journald vacuums as routine housekeeping and says so even when it discards nothing, so
	// the word is not the signal — "freed 0B" is the common case and the first version of this
	// check failed every green run on it. What means data is gone is a non-zero amount.
	if m := reVacuumed.FindStringSubmatch(console); m != nil {
		t.Errorf("journald freed %s of journal during the boot, so the earliest entries are "+
			"gone — raise RuntimeMaxUse further; %d lines survived", m[1], len(evs))
	}
	// systemd is thousands of lines at debug level. A few hundred means something dropped
	// them, and every number below would be computed over whatever was left.
	if len(evs) < 400 {
		t.Errorf("only %d tagged lines: systemd at debug writes far more than that, so this "+
			"log is truncated and the gaps are between surviving lines rather than events",
			len(evs))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%d tagged userspace lines, %.1f ms from the first to the last\n",
		len(evs), (evs[len(evs)-1].at-evs[0].at)*1000)
	fmt.Fprintf(&b, "first at %.1f ms, last at %.1f ms\n\n", first*1000, last*1000)

	// Who spent the time, before which gap. Aggregated per writer as well as listed, because
	// one 40 ms gap and forty 1 ms gaps in the same unit are different problems and the
	// ranked list alone hides the second.
	type g struct {
		ms          float64
		who, before string
		after       string
	}
	var gaps []g
	spent := map[string]float64{}
	for i := 1; i < len(evs); i++ {
		d := (evs[i].at - evs[i-1].at) * 1000
		spent[evs[i-1].who] += d
		if d >= minGap {
			gaps = append(gaps, g{ms: d, who: evs[i-1].who, after: evs[i-1].msg, before: evs[i].msg})
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].ms > gaps[j].ms })

	type ws struct {
		who string
		ms  float64
	}
	var writers []ws
	for w, ms := range spent {
		writers = append(writers, ws{w, ms})
	}
	sort.Slice(writers, func(i, j int) bool { return writers[i].ms > writers[j].ms })
	fmt.Fprintf(&b, "  %10s  %s\n", "MS AFTER", "WRITER (time between its line and the next, summed)")
	for i, w := range writers {
		if i >= 12 {
			break
		}
		fmt.Fprintf(&b, "  %10.1f  %s\n", w.ms, w.who)
	}

	fmt.Fprintf(&b, "\n  %8s  %s\n", "GAP MS", fmt.Sprintf("GAPS OVER %.0f ms", minGap))
	for i, x := range gaps {
		if i >= top {
			break
		}
		fmt.Fprintf(&b, "  %8.1f  %s\n%10s  after:  %s\n%10s  before: %s\n",
			x.ms, x.who, "", x.after, "", x.before)
	}
	t.Log(b.String() + "\n")
}
