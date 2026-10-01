// SPDX-License-Identifier: Apache-2.0

package main

import (
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

const versionsAt = `# pins
- name: qemu
  kind: download
  version: %s
- name: alpine
  kind: image
  version: "3.22"
`

// The notes say, for one release against the one before it, every way the tree that builds the
// machine changed: the commits by the part they touched, a pin that moved, a kernel option turned
// on, off or changed, and a patch added, changed or removed - by its Subject, or by its commit
// where it has none. What did not reach the machine is listed apart.
func TestTheNotesSayWhatChangedInTheMachine(t *testing.T) {
	r := newRepo(t)
	r.commit("the first release", map[string]*string{
		"versions.yaml":                     s(strings.Replace(versionsAt, "%s", "11.1.0", 1)),
		"kernel/config-7.3-x86_64":          s("CONFIG_DEVMEM=y\n# CONFIG_BPF_LSM is not set\nCONFIG_HZ=100\n"),
		"qemu/patches/0001-old.patch":       s("Subject: [PATCH] vl: the old change\n\ndiff\n"),
		"kernel/patches/0001-keep.patch":    s("Subject: [PATCH 1/2] keep: this one\n stays\n\ndiff\n"),
		"qemu/qboot/pam.patch":              s("diff --git a/x b/x\n"),
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
	r.commit("qemu: bump to 11.1.1", map[string]*string{"versions.yaml": s(strings.Replace(versionsAt, "%s", "11.1.1", 1))})
	r.commit("boot: a probe (#77)", map[string]*string{"boot/report_test.go": s("package boot // more\n")})
	r.commit("spin-machine: a flag and a lab step", map[string]*string{
		"cmd/spin-machine/main.go":      s("package main // flag\n"),
		".github/workflows/release.yml": s("on: workflow_dispatch\n"),
	})

	var b strings.Builder
	if err := write(&b, r.dir, "v1", "HEAD"); err != nil {
		t.Fatal(err)
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
	for _, absent := range []string{"alpine", "keep: this one", "qemu: bump to 11.1.1", "first release", "pam.patch"} {
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
