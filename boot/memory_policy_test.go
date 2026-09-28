// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMemoryPolicy runs a process that takes memory until something stops it, in a login
// session of a 1 GiB guest, and reports whether SSH still got in while it grew, how long until
// it was gone and what killed it - with the image as built, and without the one setting that
// decides it, MGLRU's min_ttl_ms (image/.../etc/tmpfiles.d/lru-gen.conf).
//
// Two cases, from testdata/ssh-load.sh: memory taken at full speed, which the kernel stops
// within a second or two either way; and memory taken at ~80 MB/s while the session re-reads
// its libraries, which is the one that hangs a machine on the old LRU.
//
// Chosen 2026-09-28 from this, 3 boots of each, kernel 7.2.8 with CONFIG_LRU_GEN and
// CONFIG_ZRAM (removed since); killed after, median, and the worst SSH login in the slow case:
//
//	                   fast      slow      worst login, slow
//	nothing            753 ms    13.8 s    611 / 693 / 1785 ms
//	min_ttl_ms=1000    615 ms    11.2 s    106 / 105 / 106 ms
//	zram, 512M lz4    1664 ms    20.2 s    1341 / 530 / 3817 ms
//	zram + min_ttl     662 ms    18.2 s    204 / 107 / 106 ms
//
// zram gave the runaway process room and it was killed later, with worse logins: compressed
// swap is for cold memory, not for this. systemd-oomd (one boot each, an image with the package,
// a 50%/5 s policy on user.slice) killed nothing: the kernel always got there first. Ubuntu's
// package also ships its policy for user@.service and -.slice only, and a login's processes are
// in session-N.scope beside user@.service, so out of its sight. Neither is in the machine.
//
//	SPIN_MEMORY_POLICY=1   run at all
//	REPS=<n>               boots per variant (default 3)
//
// Needs /dev/kvm and a built release; no sudo.
func TestMemoryPolicy(t *testing.T) {
	if os.Getenv("SPIN_MEMORY_POLICY") == "" {
		t.Skip("set SPIN_MEMORY_POLICY=1: this exhausts the memory of guests on purpose")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 3)

	variants := []struct {
		label string
		guest sshLoadGuest
	}{
		{"as built", sshLoadGuest{}},
		{"no min_ttl", sshLoadGuest{remove: []string{"/etc/tmpfiles.d/lru-gen.conf"}}},
	}

	var r strings.Builder
	r.WriteString("\nlogins while it grows (ms, FAIL after 20 s, 90 s at most), then what the guest did about it\n")
	for range reps {
		for _, v := range variants {
			g := v.guest
			g.cases = "idle exhaust exhaust_slow"
			res := sshLoadRun(t, out, g, 8*time.Minute)
			for _, c := range []string{"exhaust", "exhaust-slow"} {
				fmt.Fprintf(&r, "\n%-11s %-13s logins: %s\n", v.label, c, strings.Join(res.logins[c], " "))
				for _, n := range res.notes {
					if strings.HasPrefix(n, c+" gone_after_ms") {
						fmt.Fprintf(&r, "%-25s %s\n", "", strings.TrimPrefix(n, c+" "))
					}
				}
			}
			if !res.finished {
				fmt.Fprintf(&r, "%-11s the guest did not finish in 8 minutes\n", "")
			}
		}
	}
	t.Log(r.String())
}
