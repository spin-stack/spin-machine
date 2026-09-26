// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Where early userspace goes.
//
// This exists because the initcall probe found the largest single gap in the boot and it is
// not in the kernel: between the kernel's last message and journald's first line there are
// 115-144 ms, which is more than the kernel's own boot. Nothing measured inside it, because
// nothing in the ring buffer is printed there — systemd has not started journald yet, so
// the one instrument the other probes use is blind exactly where the time is.
//
// systemd knows, and will say so after the fact. `systemd-analyze` splits the boot at the
// point the kernel handed over, and `blame` and `critical-chain` say which units are on the
// far side of it. Read after the boot for the same reason the ring buffer is: asking during
// the boot changes it.
//
//	SPIN_USERSPACE_PROBE=1  run at all
//	REPS=<n>                boots to take (default 5)
//	SPIN_SYSTEMD_VARIANT=console-swap
//	                      measure the "console no dev" configuration. Its 331 ms lands
//	                      entirely after `Startup finished` — every phase before that is
//	                      unchanged — so the question is whether the login process starts
//	                      late or starts on time and its bytes are held. systemd's own
//	                      accounting is the only side that can say.
func TestUserspaceCost(t *testing.T) {
	if os.Getenv("SPIN_USERSPACE_PROBE") == "" {
		t.Skip("set SPIN_USERSPACE_PROBE=1: boots a VM and needs sudo to write into its overlay")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	reps := envInt(t, "REPS", 5)

	// Type=simple, and it is the difference between this working and deadlocking.
	//
	// `systemd-analyze` refuses while the boot is still going — "Bootup is not yet finished
	// (FinishTimestampMonotonic=0)" — and startup is finished when every job of the initial
	// transaction has completed. A unit in multi-user.target.wants is one of those jobs, so
	// as Type=oneshot this waits for a timestamp that cannot be set until it exits: it
	// retried for four seconds and gave up, while `blame`, which needs no finish timestamp,
	// answered immediately and made it look like a flake.
	//
	// A Type=simple job completes when the process is exec'd rather than when it exits. So
	// startup finishes, and the loop below is what waits for it.
	const dump = `[Unit]
Description=Print systemd's own boot accounting
After=multi-user.target
[Service]
Type=simple
StandardOutput=null
ExecStart=/bin/sh -c "{ echo SPIN-ANALYZE-BEGIN; i=0; while [ $i -lt 40 ]; do systemd-analyze && break; i=$((i+1)); sleep 0.1; done; echo '--- BLAME ---'; systemd-analyze blame --no-pager | head -25; echo '--- CHAIN ---'; systemd-analyze critical-chain --no-pager; echo SPIN-ANALYZE-END; } > /dev/ttyS0 2>&1"
[Install]
WantedBy=multi-user.target
`
	v := variant{
		label: "userspace", cpus: "2", memory: "2048",
		files: map[string]string{"/etc/systemd/system/spin-analyze.service": dump},
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/spin-analyze.service": "/etc/systemd/system/spin-analyze.service",
		},
	}

	if os.Getenv("SPIN_SYSTEMD_VARIANT") == "console-swap" {
		sw := consoleSwap()
		v.mask = sw.mask
		for k, val := range sw.files {
			v.files[k] = val
		}
		for k, val := range sw.links {
			v.links[k] = val
		}
		v.label += " + console swap"
	}

	var kernel, userspace, total []float64
	var last string
	for range reps {
		console := bootUntil(t, out, v, "", "SPIN-ANALYZE-END", 90*time.Second)
		begin := strings.Index(console, "SPIN-ANALYZE-BEGIN")
		if begin < 0 {
			t.Fatalf("no analyze output; console tail:\n%s", tail([]byte(console), 1500))
		}
		last = console[begin:]
		k, u, tt := splitStartup(t, last)
		kernel = append(kernel, k)
		userspace = append(userspace, u)
		total = append(total, tt)
	}

	sort.Float64s(kernel)
	sort.Float64s(userspace)
	sort.Float64s(total)
	t.Logf("\nsystemd's own split, p50/p95 over %d boots\n\n"+
		"  %-12s %8.1f %8.1f ms\n  %-12s %8.1f %8.1f ms\n  %-12s %8.1f %8.1f ms\n",
		reps,
		"kernel", pct(kernel, 50), pct(kernel, 95),
		"userspace", pct(userspace, 50), pct(userspace, 95),
		"total", pct(total, 50), pct(total, 95))

	// The last boot's own words, because `blame` and `critical-chain` are the part worth
	// reading and averaging them across boots would lose which unit waited on which.
	t.Logf("\nthe last boot, verbatim:\n\n%s", strings.TrimSpace(last))
}

// "Startup finished in 43ms (kernel) + 158ms (userspace) = 201ms" — and the units vary: a
// number under a second is printed in ms, over one as "1.234s", and systemd will write
// "1min 2.345s" given the chance.
var reStartup = regexp.MustCompile(
	`Startup finished in ([^(]+)\(kernel\)(?: \+ ([^(]+)\(initrd\))? \+ ([^(]+)\(userspace\) = (.+)`)

func splitStartup(t *testing.T, s string) (kernel, userspace, total float64) {
	t.Helper()
	m := reStartup.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no \"Startup finished\" line in systemd-analyze's output:\n%s", tail([]byte(s), 1200))
	}
	return mustDur(t, m[1]), mustDur(t, m[3]), mustDur(t, m[4])
}

// mustDur reads systemd's spelling of a duration into milliseconds. Written out rather than
// handed to time.ParseDuration because systemd separates its parts with a space —
// "1min 2.345s" — which ParseDuration rejects, and because a wrong answer here is a boot
// time that is wrong by a factor of sixty and looks plausible.
func mustDur(t *testing.T, s string) float64 {
	t.Helper()
	var ms float64
	for _, part := range strings.Fields(strings.TrimSpace(s)) {
		var n float64
		var unit string
		switch {
		case strings.HasSuffix(part, "min"):
			unit, part = "min", strings.TrimSuffix(part, "min")
		case strings.HasSuffix(part, "ms"):
			unit, part = "ms", strings.TrimSuffix(part, "ms")
		case strings.HasSuffix(part, "s"):
			unit, part = "s", strings.TrimSuffix(part, "s")
		default:
			t.Fatalf("%q in %q has no unit this understands", part, s)
		}
		if _, err := fmt.Sscanf(part, "%g", &n); err != nil {
			t.Fatalf("%q in %q is not a number: %v", part, s, err)
		}
		switch unit {
		case "min":
			ms += n * 60000
		case "s":
			ms += n * 1000
		case "ms":
			ms += n
		}
	}
	if ms == 0 {
		t.Fatalf("%q read as zero, which no boot takes", s)
	}
	return ms
}
