// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// A core attached before systemd starts is online once it has: a consumer boots one shape and
// gives a workspace its cores while its own init is PID 1, with no udev there to see them
// arrive, so what onlines them is udev's coldplug replaying their add to 40-spin-hotadd.rules.
// A coldplug filtered to the devices the image uses, and not to cpu, left every such core
// offline: spin's workspaces came up with one core whatever they were sized to (spin's kvm run
// 43527839765, 2026-11-13).
func TestACoreAttachedBeforeSystemdStartsComesOnline(t *testing.T) {
	if os.Getenv("SPIN_HOTADD_TEST") != "1" {
		t.Skip("set SPIN_HOTADD_TEST=1 to check a core attached before systemd in a built image")
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
	hold, err := os.ReadFile(filepath.Join("testdata", "hotadd-init.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root.write("/hotadd-init.sh", string(hold))
	// debugfs writes a file nobody may execute, and the kernel execs its init.
	root.edit("sif /hotadd-init.sh mode 0100755")
	oneshotAtBoot(t, root, "hotadd-check", "hotadd-check.sh", "")

	// Its own short directory: a socket's path has 108 bytes.
	dir, err := os.MkdirTemp("", "q")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	monitor := filepath.Join(dir, "q")

	attached := false
	t.Log(checkGuest(t, rel, root, "HOTADD_CHECK_OK", "HOTADD_CHECK_FAILED",
		func(s *machine.Spec) {
			s.BootCPUs, s.MaxCPUs = 1, 2
			s.Cmdline.Init = "/hotadd-init.sh"
			s.Monitors = []machine.Monitor{{Socket: monitor}}
		},
		func(console string) {
			if attached || !strings.Contains(console, "HOTADD_WAITING") {
				return
			}
			attached = true
			q := dial(t, monitor, func() string { return console })
			defer q.close()
			var slots []struct {
				Type  string         `json:"type"`
				Props map[string]any `json:"props"`
				Path  string         `json:"qom-path"`
			}
			if err := json.Unmarshal(q.do(t, "query-hotpluggable-cpus", nil), &slots); err != nil {
				t.Fatal(err)
			}
			for _, slot := range slots {
				if slot.Path != "" {
					continue
				}
				add := map[string]any{"driver": slot.Type, "id": "core1"}
				maps.Copy(add, slot.Props)
				q.do(t, "device_add", add)
				return
			}
			t.Fatal("the machine has no free core to attach")
		}))
}
