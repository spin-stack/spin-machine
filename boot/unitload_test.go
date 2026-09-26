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

// What systemd spends loading units and building the initial transaction, and what the
// generators cost inside it.
//
// The number comes from systemd itself. src/core/main.c brackets manager_startup() and
// do_queue_default_job() and logs "Loaded units and determined initial transaction in %s" —
// at LOG_DEBUG normally, and at LOG_INFO under `--test`. So `systemd --test --system` reports
// it without the debug logging that inflates every other way of seeing it, and it does only
// that work: MANAGER_IS_TEST_RUN skips the preset pass and nothing is started.
//
// Which makes this a paired measurement inside one boot rather than a boot per sample. That
// matters more than it sounds: two boots on this host have read 59.5 and 167.7 ms for the
// same kernel, and nothing about generators is a 100 ms effect.
//
// What manager_startup() does, from the source rather than from a guess:
// lookup_paths_init, the environment generators, the generators, manager_preset_all,
// manager_enumerate (device units out of udev, mount units out of mountinfo), deserialize,
// fd distribution. preset_all does not run here — it needs first_boot, and systemd decides
// that from /etc/machine-id, which this image ships present and empty rather than absent or
// "uninitialized", so systemd reads it as an initialised system.
//
//	SPIN_UNITLOAD_PROBE=1  run at all
//	REPS=<n>               `systemd --test` runs per configuration (default 7)
func TestUnitLoadCost(t *testing.T) {
	if os.Getenv("SPIN_UNITLOAD_PROBE") == "" {
		t.Skip("set SPIN_UNITLOAD_PROBE=1: boots a VM and needs sudo to write into its overlay")
	}
	out := releaseDir(t)
	if !canSudo() {
		t.Skip("this writes a unit into the boot's own overlay through qemu-nbd, which needs sudo")
	}
	reps := envInt(t, "REPS", 7)

	// Masked, not removed, and both are generators that cannot do anything on this machine:
	// the tpm2 one because there is no TPM — the boot log says libtss2-esys.so.0 cannot be
	// opened — and the factory-reset one because this machine is not booted in EFI mode, which
	// the log also says. Seven of the thirteen the image carries are masked already.
	const masked = "systemd-tpm2-generator systemd-factory-reset-generator"

	// Not as root: systemd refuses test mode for the root user outright ("Don't run test mode
	// as root."), so this drops to nobody with setpriv. The generators and the unit files are
	// world readable, and both configurations run as the same user, so the comparison holds
	// even where a generator behaves differently without privileges.
	//
	// It also means the absolute number is a floor for what PID 1 pays: this run sets up no
	// cgroups, inherits no file descriptors and deserialises nothing. What it is for is the
	// order of magnitude and the difference between two configurations, and it fails rather
	// than measuring the wrong user if setpriv cannot drop privileges.
	script := fmt.Sprintf(`set -u
AS="setpriv --reuid=65534 --regid=65534 --clear-groups"
$AS true || { echo "setpriv cannot drop to nobody, nothing to measure" >&2; exit 1; }
run() {
  i=0
  while [ $i -lt %d ]; do
    $AS /usr/lib/systemd/systemd --test --system 2>&1 | grep -F 'initial transaction'
    i=$((i+1))
  done
}
{
  echo SPIN-UNITLOAD-BEGIN
  echo TAG:generators-on
  run
  mkdir -p /etc/systemd/system-generators
  for g in %s; do ln -sf /dev/null "/etc/systemd/system-generators/$g"; done
  echo TAG:generators-masked
  run
  echo SPIN-UNITLOAD-END
} > /dev/ttyS0 2>&1
`, reps, masked)

	v := variant{
		label: "unit load", cpus: "2", memory: "2048",
		files: map[string]string{
			"/usr/local/bin/spin-unitload": script,
			"/etc/systemd/system/spin-unitload.service": `[Unit]
Description=Measure systemd's unit load with and without two generators
After=multi-user.target
[Service]
Type=simple
StandardOutput=null
ExecStart=/bin/sh /usr/local/bin/spin-unitload
[Install]
WantedBy=multi-user.target
`,
		},
		links: map[string]string{
			"/etc/systemd/system/multi-user.target.wants/spin-unitload.service": "/etc/systemd/system/spin-unitload.service",
		},
	}

	console := bootUntil(t, out, v, "", "SPIN-UNITLOAD-END", 180*time.Second)
	reportUnitLoad(t, console, reps)
}

// "Loaded units and determined initial transaction in 212ms." — and systemd's spelling of a
// duration varies with its size, so this takes the same route mustDur does.
var reLoaded = regexp.MustCompile(`initial transaction in ([^.]+)\.`)

func reportUnitLoad(t *testing.T, console string, reps int) {
	t.Helper()
	samples := map[string][]float64{}
	var order []string
	var tag string
	for _, line := range strings.Split(console, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if i := strings.Index(line, "TAG:"); i >= 0 {
			tag = line[i+4:]
			order = append(order, tag)
			continue
		}
		m := reLoaded.FindStringSubmatch(line)
		if m == nil || tag == "" {
			continue
		}
		samples[tag] = append(samples[tag], mustDur(t, m[1]))
	}
	if len(samples) < 2 {
		t.Fatalf("expected two tagged sets of `systemd --test` output, got %d; console tail:\n%s",
			len(samples), tail([]byte(console), 1500))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nsystemd's own \"loaded units and determined initial transaction\", "+
		"p50/p95 over %d runs in one boot\n\n", reps)
	fmt.Fprintf(&b, "%-20s %8s %8s %6s\n", "CONFIGURATION", "P50", "P95", "N")
	var first, second float64
	for i, tag := range dedup(order) {
		s := append([]float64(nil), samples[tag]...)
		sort.Float64s(s)
		fmt.Fprintf(&b, "%-20s %8.1f %8.1f %6d ms\n", tag, pct(s, 50), pct(s, 95), len(s))
		if i == 0 {
			first = pct(s, 50)
		} else if i == 1 {
			second = pct(s, 50)
		}
	}
	fmt.Fprintf(&b, "\ntwo generators masked: %.1f ms\n", first-second)
	t.Log(b.String())
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
