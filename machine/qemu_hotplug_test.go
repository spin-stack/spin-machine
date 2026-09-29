// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestARootPortTakesADeviceWhoseBackendIsAnInheritedDescriptor is the question the empty
// root ports exist to answer, asked of a machine that is running rather than of one that
// started.
//
// A machine restored from a template has no disks and no NIC — that is what lets one
// template serve every machine on a host — so both arrive afterwards, through device_add
// into a root port. For a disk the backend is a file and whoever adds it can name a path.
// For a NIC it cannot: the backend is a TAP that lives inside a network namespace the QEMU
// process is not in, so there is no name that reaches it, only a descriptor.
//
// Three ways to give a running QEMU that descriptor, measured against this binary
// (2026-09-09):
//
//   - netdev_add with a numeric fd. Accepted: with fd 3 bound to something that is not a
//     TAP, QEMU answers "Unable to query TUNGETIFF on FD 3", which is the ioctl it makes
//     on a descriptor it has already taken. It uses the number as a descriptor the process
//     holds, which is what a descriptor inherited across exec is.
//   - netdev_add with ifname. Rejected unless QEMU can open /dev/net/tun itself and find
//     the interface, which means the QEMU process must be inside the namespace.
//   - getfd. Present, and it answers "No file descriptor supplied via SCM_RIGHTS": it
//     needs the descriptor sent as a control message on the monitor socket, which is a
//     capability whoever drives the monitor has to have.
//
// The first is what this machine's callers use, so it is what is checked here. The check
// uses a socket netdev rather than a tap: what is under test is that a root port takes a
// virtio-net at run time and that a netdev binds to an inherited descriptor, and a TAP
// needs CAP_NET_ADMIN, which this lane does not have and must not require. The tap-ness
// is the one part the manual measurement above covers and this does not.
func TestARootPortTakesADeviceWhoseBackendIsAnInheritedDescriptor(t *testing.T) {
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
		HotplugPorts: 1,
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

	// The descriptor the child inherits. One end of a socketpair, so it is a real
	// connected socket a netdev can be built on; ExtraFiles puts it at 3, which is the
	// number the caller passes and the number machine.NIC documents.
	ours, theirs, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ours.Close() }()

	vm := startQEMU(t, qemu, append(args, "-S"), theirs)
	_ = theirs.Close()
	q := dialQMP(t, socket, vm)

	// The backend, on the descriptor the process inherited. "3" is a number and not a
	// name, which is the whole point: a name would have to have been registered with
	// getfd, and that needs SCM_RIGHTS on the monitor.
	q.do("netdev_add", map[string]any{
		"type": "socket",
		"id":   "net0",
		"fd":   "3",
	})

	// And the device, into the root port. bus is the port's id — HotplugPortID(0) — which
	// is what makes this a hotplug rather than an attempt at the root complex, and QEMU
	// refuses the latter with "Bus 'pcie.0' does not support hotplugging".
	q.do("device_add", map[string]any{
		"driver":  "virtio-net-pci",
		"id":      "nic0",
		"netdev":  "net0",
		"bus":     HotplugPortID(0),
		"mac":     "52:54:00:00:00:01",
		"romfile": "",
	})

	// The device exists and is bound to the netdev built on the inherited descriptor.
	//
	// qom-get and not query-pci: this machine is stopped at -S, so no guest has handled
	// the hot-plug event and the device is not on the bridge's secondary bus yet —
	// query-pci reports the root port with no devices behind it, which is true and is not
	// the question. What device_add did is create and realize the device, and its binding
	// to the backend is the part that could have silently not happened.
	reply, err := q.try("qom-get", map[string]any{"path": "/machine/peripheral/nic0", "property": "netdev"})
	if err != nil {
		t.Fatalf("the NIC was accepted and there is no device at /machine/peripheral/nic0: %v", err)
	}
	var bound string
	if err := json.Unmarshal(reply, &bound); err != nil {
		t.Fatal(err)
	}
	if bound != "net0" {
		t.Errorf("the NIC's backend is %q, want %q — the device was created and bound to nothing",
			bound, "net0")
	}

	// And the root port is what made it possible. The same device_add onto the root
	// complex has to be refused, or the ports are a cost this machine pays for nothing.
	if _, err := q.try("device_add", map[string]any{
		"driver": "virtio-net-pci",
		"id":     "nic1",
		"netdev": "net0",
		"bus":    "pcie.0",
	}); err == nil {
		t.Error("the root complex accepted a device_add; if that were true the empty root ports would be a bus the guest scans for nothing")
	} else {
		t.Logf("the root complex refused it, as it must: %v", err)
	}
}

// socketPair is socketpair(2) as two files; the second is what the child inherits.
//
// A socket and not a pipe: a socket netdev needs one, and the point of the pair is that the
// descriptor is real and connected, so what is under test is the binding and not QEMU's
// reaction to a descriptor it cannot use.
func socketPair() (ours, theirs *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	return os.NewFile(uintptr(fds[0]), "netdev-a"), os.NewFile(uintptr(fds[1]), "netdev-b"), nil
}
