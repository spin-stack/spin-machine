// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A machine handed /dev/kvm and /dev/vhost-vsock as descriptors uses those and opens
// neither node: given /dev/null in the place of one it fails, and given the devices it
// answers on its monitor. A QEMU whose root has no /dev depends on exactly that.
//
// Under the ordinary build and KVM, the only accelerator that opens a device; skipped on
// a host that will not give both up.
func TestAMachineTakesItsDevicesAsDescriptors(t *testing.T) {
	out := outputDir(t)
	qemu := filepath.Join(out, qemuName)
	if _, err := os.Stat(qemu); err != nil {
		t.Skipf("no QEMU at %s: %v", qemu, err)
	}
	for _, dev := range []string{"/dev/kvm", "/dev/vhost-vsock"} {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			t.Skipf("this host will not give up %s: %v", dev, err)
		}
		_ = f.Close()
	}
	kernel := pvhStub(t)

	for _, tc := range []struct {
		name       string
		kvm, vsock string
		refused    bool
	}{
		{"both devices", "/dev/kvm", "/dev/vhost-vsock", false},
		{"/dev/null for /dev/kvm", os.DevNull, "/dev/vhost-vsock", true},
		{"/dev/null for /dev/vhost-vsock", "/dev/kvm", os.DevNull, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var files []*os.File
			for _, path := range []string{tc.kvm, tc.vsock} {
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				files = append(files, f)
			}
			socket := qmpSocket(t)
			spec := Spec{
				QEMU: qemu, Kernel: kernel, Firmware: filepath.Join(out, firmwareDir),
				BootCPUs: 1, Memory: Memory{SizeMB: 256}, Cmdline: DefaultCmdline(),
				Monitors: []Monitor{{Socket: socket}},
				// Descriptors 3 and 4, in the order above.
				FDSets:   []FDSet{{ID: 1, FDs: []FD{{Num: 3, Opaque: tc.kvm}}}},
				KVMFDSet: 1,
				VsockCID: 12346, VsockFD: 4,
			}
			args, err := spec.Args()
			if err != nil {
				t.Fatal(err)
			}

			vm := startQEMU(t, qemu, append(args, "-S"), files...)

			if tc.refused {
				// A QEMU that opened the nodes itself runs, stopped at -S, and is still
				// running when this gives up on it.
				select {
				case <-vm.done:
				case <-time.After(20 * time.Second):
					t.Fatalf("QEMU is running on %s and %s: it opened the devices itself", tc.kvm, tc.vsock)
				}
				if vm.exit == nil {
					t.Fatalf("QEMU started on %s and %s: it opened the devices itself", tc.kvm, tc.vsock)
				}
				t.Logf("refused: %s", strings.TrimSpace(vm.out.String()))
				return
			}
			dialQMP(t, socket, vm).do("query-status", nil)
		})
	}
}
