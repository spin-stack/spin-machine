// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"encoding/json"
	"testing"
)

// TestADiskArrivesAtItsTargetOnTheHotplugController is the question the hotplug controller
// exists to answer, asked of a machine that is running rather than of one that started: a
// disk HotplugDisk describes is taken at run time, at the target it names, over a block node
// added the way a caller adds one, and a second disk at the same target is refused rather than
// put somewhere the caller did not say.
//
// The machine is stopped at -S, so no guest is involved: what is under test is QEMU's side of
// the contract. How quickly a guest sees the disk and lets it go is Spec.HotplugDisks'
// measurement.
func TestADiskArrivesAtItsTargetOnTheHotplugController(t *testing.T) {
	qemu, firmware := qemuTCG(t)
	kernel := pvhStub(t)

	socket := qmpSocket(t)
	spec := Spec{
		QEMU:         qemu,
		Kernel:       kernel,
		Firmware:     firmware,
		BootCPUs:     2,
		Memory:       Memory{SizeMB: 512},
		Cmdline:      DefaultCmdline(),
		Monitors:     []Monitor{{Socket: socket}},
		HotplugDisks: 2,
	}
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	args, rewrites, err := tcgOnly(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rewrites {
		t.Logf("rewritten for the TCG binary: %s", r)
	}

	vm := startQEMU(t, qemu, append(args, "-S"))
	q := dialQMP(t, socket, vm)

	image := qcow2In(t, qemu, t.TempDir(), "extension.qcow2", "")
	q.do("blockdev-add", map[string]any{
		"node-name": "ext0",
		"driver":    "qcow2",
		"read-only": true,
		"file":      map[string]any{"driver": "file", "filename": image, "read-only": true},
	})
	q.do("device_add", HotplugDisk(1, "hd1", "ext0", "vsc-abc"))

	// The disk exists, on the block node it was given and at the target it was given.
	for property, want := range map[string]any{"drive": "ext0", "scsi-id": float64(1), "serial": "vsc-abc"} {
		reply, err := q.try("qom-get", map[string]any{"path": "/machine/peripheral/hd1", "property": property})
		if err != nil {
			t.Fatalf("the disk was accepted and has no %s: %v", property, err)
		}
		var got any
		if err := json.Unmarshal(reply, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("the disk's %s is %v, want %v", property, got, want)
		}
	}

	// A target is one disk's.
	q.do("blockdev-add", map[string]any{
		"node-name": "ext1",
		"driver":    "qcow2",
		"read-only": true,
		"file":      map[string]any{"driver": "file", "filename": image, "read-only": true},
	})
	if _, err := q.try("device_add", HotplugDisk(1, "hd2", "ext1", "")); err == nil {
		t.Error("a second disk was taken at a target that holds one")
	} else {
		t.Logf("the second disk at target 1 was refused, as it must be: %v", err)
	}
}
