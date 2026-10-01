// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"os"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// The guest's root is not the kernel's: the kernel runs lockdown at confidentiality and the BPF
// LSM, and has no /dev/mem and no /proc/kcore. A consumer that enforces policy in the guest
// with BPF LSM programs relies on root being unable to read or rewrite the kernel they live in;
// kernel/Dockerfile fails a build whose config lost these, and this asks the kernel that boots.
func TestTheGuestKernelIsLockedDown(t *testing.T) {
	if os.Getenv("SPIN_LOCKDOWN_TEST") != "1" {
		t.Skip("set SPIN_LOCKDOWN_TEST=1 to check the guest kernel's lockdown in a built image")
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
	oneshotAtBoot(t, root, "lockdown-check", "lockdown-check.sh", "")
	t.Log(checkOnConsole(t, out, rel, root, "LOCKDOWN_CHECK_OK", "LOCKDOWN_CHECK_FAILED"))
}
