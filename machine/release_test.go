// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes a release-shaped directory containing exactly the given files.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var whole = []string{qemuName, qemuImgName, kernelName, firmwareDir + "/pvh.bin"}

func TestOpenNamesWhatIsMissing(t *testing.T) {
	// One file at a time, because a release with a hole in it is the failure
	// this exists to turn into a sentence: the message has to say which file.
	for _, absent := range whole {
		var kept []string
		for _, f := range whole {
			if f != absent {
				kept = append(kept, f)
			}
		}
		if _, err := Open(tree(t, kept...)); err == nil {
			t.Errorf("Open succeeded on a tree with no %s", absent)
		} else if !strings.Contains(err.Error(), filepath.Base(absent)) {
			t.Errorf("missing %s: error does not name it: %v", absent, err)
		}
	}
}

func TestOpenAcceptsAMachineWithoutTheOptionalParts(t *testing.T) {
	// The TCG binary and the base image are not required, and asking for one
	// that is absent must fail where it is asked for rather than at Open.
	r, err := Open(tree(t, whole...))
	if err != nil {
		t.Fatalf("Open on a whole machine: %v", err)
	}
	if _, err := r.Rootfs(); err == nil {
		t.Error("Rootfs returned a path for an image that is not there")
	}
	if _, err := r.QEMUTCG(); err == nil {
		t.Error("QEMUTCG returned a path for a binary that is not there")
	}
	if v := r.Version(); v != "" {
		t.Errorf("Version of a tree with no manifest = %q, want empty", v)
	}
}

func TestSpecTakesItsPathsFromTheRelease(t *testing.T) {
	dir := tree(t, append(whole, rootfsName, "machine.env")...)
	if err := os.WriteFile(filepath.Join(dir, "machine.env"),
		[]byte("# a comment\nversion=v20260908.01\nkernel_version = 6.19.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Version(); got != "v20260908.01" {
		t.Errorf("Version = %q", got)
	}
	if got := r.env["kernel_version"]; got != "6.19.2" {
		t.Errorf("kernel_version = %q, want the value with its spaces trimmed", got)
	}

	s := r.Spec()
	for _, c := range []struct{ name, got, want string }{
		{"QEMU", s.QEMU, filepath.Join(dir, qemuName)},
		{"Kernel", s.Kernel, filepath.Join(dir, kernelName)},
		{"Firmware", s.Firmware, filepath.Join(dir, firmwareDir)},
	} {
		if c.got != c.want {
			t.Errorf("Spec().%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	// The rest of a machine is about one VM and is the caller's to fill in.
	if s.Initrd != "" || s.Memory.SizeMB != 0 || len(s.Disks) != 0 {
		t.Errorf("Spec() decided something a release does not know: %+v", s)
	}
}
