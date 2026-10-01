// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a git repository in a temporary directory, with commit and tag at hand.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t, t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "test")
	return r
}

func (r *repo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit writes files (a nil body removes one) and commits them under subject.
func (r *repo) commit(subject string, files map[string]*string) {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, name)
		if body == nil {
			r.git("rm", "-q", name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(*body), 0o644); err != nil {
			r.t.Fatal(err)
		}
		r.git("add", name)
	}
	r.git("commit", "-q", "-m", subject)
}

func s(v string) *string { return &v }

// versionsAt is a versions.yaml with qemu at a version and debian's digest at a pin.
const versionsAt = `# pins
- name: qemu
  kind: download
  source: https://download.qemu.org/qemu-{version}.tar.xz
  version: %s
  pin: 079ffbff8a7111bbc89022107cbabf3bbfd614d5fc9d7cc675991196aca12482
  track: tags https://gitlab.com/qemu-project/qemu.git
- name: debian
  kind: image
  source: debian
  version: trixie
  pin: sha256:%s
  track: digest
`

const (
	digestA = "9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c"
	digestB = "294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
)

// The notes say, for one release against the one before it, every way the tree that builds the
// machine changed: the commits by the part they touched, a pin that moved, a kernel option turned
// on, off or changed, and a patch added, changed or removed - by its Subject, or by its commit
// where it has none. What did not reach the machine is listed apart.
func TestTheNotesSayWhatChangedInTheMachine(t *testing.T) {
	r := newRepo(t)
	r.commit("the first release", map[string]*string{
		"versions.yaml":                     s(fmt.Sprintf(versionsAt, "11.1.0", digestA)),
		"kernel/config-7.3-x86_64":          s("CONFIG_DEVMEM=y\n# CONFIG_BPF_LSM is not set\nCONFIG_HZ=100\n"),
		"qemu/patches/0001-old.patch":       s("Subject: [PATCH] vl: the old change\n\ndiff\n"),
		"kernel/patches/0001-keep.patch":    s("Subject: [PATCH 1/2] keep: this one\n stays\n\ndiff\n"),
		"qemu/qboot/pam.patch":              s("diff --git a/x b/x\n"),
		"qemu/qboot/README.md":              s("what the patches are\n"),
		"boot/report_test.go":               s("package boot\n"),
		"image/mkosi.extra/etc/fstrim.conf": s("weekly\n"),
		"kernel/patches/0002-changes.patch": s("Subject: [PATCH 2/2] changes: before\n\ndiff\n"),
		".github/workflows/release.yml":     s("on: push\n"),
		"cmd/spin-machine/main.go":          s("package main\n"),
	})
	r.git("tag", "v1")

	r.commit("qboot: enable the MTRRs (#75)", map[string]*string{"qemu/qboot/mtrr.patch": s("diff --git a/main.c b/main.c\n")})
	r.commit("kernel: lockdown and the BPF LSM", map[string]*string{
		"kernel/config-7.3-x86_64": s("# CONFIG_DEVMEM is not set\nCONFIG_BPF_LSM=y\nCONFIG_HZ=1000\nCONFIG_LSM=\"lockdown,bpf\"\n"),
	})
	r.commit("kernel: a patch changed, another dropped", map[string]*string{
		"kernel/patches/0002-changes.patch": s("Subject: [PATCH 2/2] changes: after\n\ndiff\n"),
		"qemu/patches/0001-old.patch":       nil,
	})
	r.commit("image: fstrim hourly (#76)", map[string]*string{"image/mkosi.extra/etc/fstrim.conf": s("hourly\n")})
	r.commit("qemu: bump to 11.1.1", map[string]*string{"versions.yaml": s(fmt.Sprintf(versionsAt, "11.1.1", digestA))})
	r.commit("debian: the digest under trixie moved", map[string]*string{"versions.yaml": s(fmt.Sprintf(versionsAt, "11.1.1", digestB))})
	r.commit("qboot: say what the patches are", map[string]*string{"qemu/qboot/README.md": s("each patch, and why\n")})
	r.commit("boot: a probe (#77)", map[string]*string{"boot/report_test.go": s("package boot // more\n")})
	r.commit("spin-machine: a flag and a lab step", map[string]*string{
		"cmd/spin-machine/main.go":      s("package main // flag\n"),
		".github/workflows/release.yml": s("on: workflow_dispatch\n"),
	})

	var b, stderr strings.Builder
	if code := run([]string{"-from", "v1"}, r.dir, &b, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	notes := b.String()
	t.Log(notes)

	// Each wanted line, and the heading it must be under.
	for _, want := range []struct{ under, line string }{
		{"### QEMU and firmware", "- qboot: enable the MTRRs (#75)"},
		{"### Kernel\n", "- kernel: lockdown and the BPF LSM"},
		{"### Kernel\n", "- kernel: a patch changed, another dropped"},
		{"### Base image", "- image: fstrim hourly (#76)"},
		{"### The machine's definition and the spin-machine CLI", "- spin-machine: a flag and a lab step"},
		{"### Pins (versions.yaml)", "- `qemu` `11.1.0` → `11.1.1`"},
		{"### Pins (versions.yaml)", "- `debian` `trixie` pin `9cc080028c43` → `294b683cb724`"},
		{"### Kernel configuration", "- `CONFIG_BPF_LSM` n → y"},
		{"### Kernel configuration", "- `CONFIG_DEVMEM` y → n"},
		{"### Kernel configuration", "- `CONFIG_HZ` 100 → 1000"},
		{"### Kernel configuration", "- `CONFIG_LSM` n → \"lockdown,bpf\""},
		{"### Patches to upstream source", "- added `qemu/qboot/mtrr.patch`: qboot: enable the MTRRs (#75)"},
		{"### Patches to upstream source", "- removed `qemu/patches/0001-old.patch`: vl: the old change"},
		{"### Patches to upstream source", "- changed `kernel/patches/0002-changes.patch`: changes: after"},
		{"<details>", "- boot: a probe (#77)"},
	} {
		head := strings.Index(notes, want.under)
		if head < 0 || !strings.Contains(underHeading(notes[head:]), want.line) {
			t.Errorf("%q is not under %q", want.line, want.under)
		}
	}
	for _, absent := range []string{"alpine", "keep: this one", "qemu: bump to 11.1.1", "first release", "pam.patch", "README.md"} {
		if strings.Contains(notes, absent) {
			t.Errorf("the notes name %q, which did not change in the machine", absent)
		}
	}
	if !strings.Contains(notes, "1 more, none of them in the machine") {
		t.Errorf("what did not reach the machine is not counted apart")
	}
}

// underHeading is the text from a heading to the next one.
func underHeading(from string) string {
	if next := strings.Index(from[1:], "\n### "); next >= 0 {
		return from[:next+1]
	}
	return from
}

// What the command answers, and with what exit, for a command line it cannot use, a ref git does
// not know, and a release built from the same tree as the one before it.
func TestTheCommandSaysWhyItWroteNothing(t *testing.T) {
	r := newRepo(t)
	r.commit("the first release", map[string]*string{"versions.yaml": s(fmt.Sprintf(versionsAt, "11.1.1", digestA))})
	r.git("tag", "v1")
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"no previous release", nil, 2, "", "-from is required"},
		{"a flag it does not take", []string{"-since", "v1"}, 2, "", "flag provided but not defined"},
		{"a ref git does not know", []string{"-from", "v0"}, 1, "", "git log"},
		{"the same tree", []string{"-from", "v1"}, 0, "Nothing: this is the tree v1 was built from.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tc.args, r.dir, &stdout, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d; stderr: %s", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.stdout) || (tc.stdout == "" && stdout.Len() > 0) {
				t.Errorf("stdout %q, want %q", stdout.String(), tc.stdout)
			}
			if !strings.Contains(stderr.String(), tc.stderr) || (tc.stderr == "" && stderr.Len() > 0) {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}
