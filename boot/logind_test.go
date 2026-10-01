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
	// The shipped image does not start logind at boot: the first login activates it over
	// the Varlink socket, which is worth 18 ms (see the 10-seats.conf drop-in). So the
	// state this asserts before any login is `inactive`, and a machine that answers
	// `active` has put logind back into the boot transaction.
	//
	// SPIN_LOGIND_NO_SEATS=1 removes the drop-in and nothing else. It is the experiment
	// that says what the drop-in does, and it is expected to fail: without
	// /run/systemd/seats, pam_systemd never asks logind for a session and never activates
	// it, so every login — SSH and console alike — comes up with XDG_RUNTIME_DIR unset and
	// no error anywhere (2026-09-12).
	expectedState := "inactive"
	if os.Getenv("SPIN_LOGIND_NO_SEATS") == "1" {
		root.remove("/etc/systemd/system/systemd-logind-varlink.socket.d/10-seats.conf")
	}
	content, err := os.ReadFile("testdata/logind-session.sh")
	if err != nil {
		t.Fatal(err)
	}
	root.write("/logind-session.sh", string(content))
	oneshotAtBoot(t, root, "logind-check", "logind-check.sh", "Environment=LOGIND_EXPECT="+expectedState)
	t.Log(checkOnConsole(t, out, rel, root, "LOGIND_CHECK_OK", "LOGIND_CHECK_FAILED"))
}
