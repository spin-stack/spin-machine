// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"os"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// A correctness test, including under TCG: none of its durations are boot
// performance measurements. It edits a private raw copy, without mounts or NBD.
func TestLogindSessions(t *testing.T) {
	if os.Getenv("SPIN_LOGIND_TEST") != "1" {
		t.Skip("set SPIN_LOGIND_TEST=1 to check first SSH login in a built image")
	}
	out := releaseTree(t)
	rel, err := machine.OpenRelease(out)
	if err != nil {
		t.Fatal(err)
	}
	base, err := rel.Rootfs()
	if err != nil {
		t.Fatal(err)
	}
	root := newRawRoot(t, out, base)
	// The shipped image starts logind at boot, because spin lingers and logind is what starts
	// a lingering user's manager (optimize-systemd.sh). So the state this asserts before any
	// login is `active`, and a machine that answers `inactive` has taken logind out of the boot
	// again and moved spin's manager back into the first login.
	//
	// SPIN_LOGIND_NO_SEATS=1 removes the 10-seats.conf drop-in and nothing else, and logind is
	// then held back from the boot as it once was: that is the case the drop-in exists for - a
	// login that arrives before logind has run - and it is expected to fail. Without
	// /run/systemd/seats, pam_systemd never asks logind for a session and never activates it,
	// so every login - SSH and console alike - comes up with XDG_RUNTIME_DIR unset and no
	// error anywhere (2026-09-12).
	expectedState := "active"
	if os.Getenv("SPIN_LOGIND_NO_SEATS") == "1" {
		root.remove("/etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf")
		root.link("/etc/systemd/system/multi-user.target.wants/systemd-logind.service", "/dev/null")
		expectedState = "inactive"
	}
	content, err := os.ReadFile("testdata/logind-session.sh")
	if err != nil {
		t.Fatal(err)
	}
	root.write("/logind-session.sh", string(content))
	oneshotAtBoot(t, root, "logind-check", "logind-check.sh", "Environment=LOGIND_EXPECT="+expectedState)
	t.Log(checkOnConsole(t, rel, root, "LOGIND_CHECK_OK", "LOGIND_CHECK_FAILED"))
}
