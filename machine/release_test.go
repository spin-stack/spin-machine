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

var whole = []string{qemuName, qemuImgName, kernelName, filepath.Join(firmwareDir, "pvh.bin"), filepath.Join(firmwareDir, DefaultBIOS)}

func TestOpenReleaseNamesWhatIsMissing(t *testing.T) {
	// One file at a time, because a release with a hole in it is the failure
	// this exists to turn into a sentence: the message has to say which file.
	for _, absent := range whole {
		t.Run(absent, func(t *testing.T) {
			var kept []string
			for _, f := range whole {
				if f != absent {
					kept = append(kept, f)
				}
			}
			if _, err := OpenRelease(tree(t, kept...)); err == nil {
				t.Errorf("OpenRelease succeeded on a tree with no %s", absent)
			} else if !strings.Contains(err.Error(), filepath.Base(absent)) {
				t.Errorf("error does not name %s: %v", absent, err)
			}
		})
	}
}

func TestOpenReleaseAcceptsAMachineWithoutTheOptionalParts(t *testing.T) {
	// The TCG binary and the base image are not required, and asking for one
	// that is absent must fail where it is asked for rather than at OpenRelease.
	r, err := OpenRelease(tree(t, whole...))
	if err != nil {
		t.Fatalf("OpenRelease on a whole machine: %v", err)
	}
	if _, err := r.Rootfs(); err == nil {
		t.Error("Rootfs returned a path for an image that is not there")
	}
	if _, err := r.QEMUTCG(); err == nil {
		t.Error("QEMUTCG returned a path for a binary that is not there")
	}
}

// machine.env, as hack/release writes it and as it can arrive damaged.
func TestOpenReleaseReadsTheManifest(t *testing.T) {
	for _, tc := range []struct {
		name string
		// manifest is machine.env's content; nil is a tree with no manifest, which a
		// developer's build directory is.
		manifest *string
		version  string
		kernel   string
		// wantErr is a part of the error, which must say where the damage is.
		wantErr string
	}{
		{name: "no manifest"},
		{name: "comments, blank lines and spaces",
			manifest: new("# a comment\n\nversion=v20260908.01\nkernel_version = 6.19.2\n"),
			version:  "v20260908.01", kernel: "6.19.2"},
		// A truncated or hand-edited manifest is refused rather than read past: the
		// version it would report is of a file nobody wrote.
		{name: "a line that is not key=value",
			manifest: new("version=v20260908.01\nkernel_version\n"),
			wantErr:  "machine.env:2"},
		{name: "a value with no key",
			manifest: new("=v20260908.01\n"),
			wantErr:  "machine.env:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tree(t, whole...)
			if tc.manifest != nil {
				if err := os.WriteFile(filepath.Join(dir, manifest), []byte(*tc.manifest), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := OpenRelease(dir)
			switch {
			case tc.wantErr != "" && err == nil:
				t.Fatalf("OpenRelease accepted a damaged manifest, version %q", r.Version())
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error does not say where the damage is (%s): %v", tc.wantErr, err)
			case tc.wantErr != "":
				return
			case err != nil:
				t.Fatal(err)
			}
			if got := r.Version(); got != tc.version {
				t.Errorf("Version = %q, want %q", got, tc.version)
			}
			if got := r.env["kernel_version"]; got != tc.kernel {
				t.Errorf("kernel_version = %q, want %q", got, tc.kernel)
			}
		})
	}
}

func TestSpecTakesItsPathsFromTheRelease(t *testing.T) {
	dir := tree(t, append(whole, rootfsName)...)
	r, err := OpenRelease(dir)
	if err != nil {
		t.Fatal(err)
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
