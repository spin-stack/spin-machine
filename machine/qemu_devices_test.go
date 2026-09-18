// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"cmp"
	"encoding/json"
	"os"
	"os/exec"
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
	out, err := filepath.Abs(cmp.Or(os.Getenv("SPIN_MACHINE_OUTPUT"), "../_output"))
	if err != nil {
		t.Fatal(err)
	}
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
				BootCPUs: 1, Memory: Memory{SizeMB: 256}, Cmdline: DefaultCmdline().String(),
				QMPSocket: socket,
				// Descriptors 3 and 4, in the order above.
				FDSets:   []FDSet{{ID: 1, FDs: []FD{{Num: 3, Opaque: tc.kvm}}}},
				KVMFDSet: 1,
				VsockCID: 12346, VsockFD: 4,
			}
			args, err := spec.Args()
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(qemu, append(args, "-S")...) // #nosec G204 -- the binary and arguments this test built
			var stderr strings.Builder
			cmd.Stderr, cmd.Stdout = &stderr, &stderr
			cmd.ExtraFiles = files
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting %s: %v", qemu, err)
			}
			var exit error
			done := make(chan struct{})
			go func() { exit = cmd.Wait(); close(done) }()
			defer func() {
				_ = cmd.Process.Kill()
				<-done
			}()

			if tc.refused {
				// A QEMU that opened the nodes itself runs, stopped at -S, and is still
				// running when this gives up on it.
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatalf("QEMU is running on %s and %s: it opened the devices itself", tc.kvm, tc.vsock)
				}
				if exit == nil {
					t.Fatalf("QEMU started on %s and %s: it opened the devices itself", tc.kvm, tc.vsock)
				}
				t.Logf("refused: %s", strings.TrimSpace(stderr.String()))
				return
			}
			conn := dialQMP(t, socket, &stderr)
			defer func() { _ = conn.Close() }()
			enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
			for _, command := range []string{"", "qmp_capabilities", "query-status"} {
				if command != "" {
					if err := enc.Encode(map[string]string{"execute": command}); err != nil {
						t.Fatal(err)
					}
				}
				var reply map[string]json.RawMessage
				if err := dec.Decode(&reply); err != nil {
					t.Fatalf("no answer to %q on the monitor: %v\n\nQEMU said:\n%s", command, err, stderr.String())
				}
				if e, ok := reply["error"]; ok {
					t.Fatalf("%s was refused: %s", command, e)
				}
			}
		})
	}
}
