// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// bzImage is a file with a bzImage's setup header magic where a kernel's is, and body after it.
func bzImage(body string) []byte {
	b := make([]byte, 0x210)
	copy(b[0x202:], "HdrS")
	return append(b, body...)
}

// fakeRelease is a directory laid out as a release, every file a few bytes: what a tree is made
// of, without a machine to boot.
func fakeRelease(t *testing.T, kernel string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"bin/qemu-system-x86_64": []byte("qemu"),
		"bin/qemu-img":           []byte("qemu-img"),
		"bin/spin-machine":       []byte("spin-machine"),
		"qemu/qboot.bin":         []byte("qboot"),
		"qemu/linuxboot_dma.bin": []byte("rom"),
		"image/rootfs.qcow2":     []byte("base"),
		"kernel/vmlinux":         []byte("elf"),
	}
	if kernel != "" {
		files["kernel/bzImage"] = bzImage(kernel)
	}
	for p, b := range files {
		write(t, filepath.Join(dir, p), b)
	}
	return dir
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func src(t *testing.T) Sources {
	return Sources{Checkout: checkoutRoot(t), Cache: t.TempDir(), Log: io.Discard}
}

func TestATreeIsItsBaseLinkedAndSaysWhereEachPartCameFrom(t *testing.T) {
	base := fakeRelease(t, "k1")
	tr, err := MakeTree(filepath.Join(t.TempDir(), "t"), TreeSpec{Name: "t", From: base, Append: "quiet x=1", Boot: []string{"--cpu=Skylake-Server-v4"}}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Name() != "t" || tr.Cmdline() != "init=/sbin/init quiet x=1" {
		t.Errorf("name %q, cmdline %q", tr.Name(), tr.Cmdline())
	}
	// Read back from the manifest on disk, as a measurement opens it.
	if again, err := OpenTree(tr.Dir); err != nil || !slices.Equal(again.Manifest.Boot, []string{"--cpu=Skylake-Server-v4"}) {
		t.Errorf("the tree's boot flags, reopened: %v, %v", again, err)
	}
	for name := range partFiles {
		if got := tr.Manifest.Parts[name].Source; got != base {
			t.Errorf("%s came from %q, want %q", name, got, base)
		}
	}
	sum, _ := fileSHA256(filepath.Join(base, "kernel/bzImage"))
	if tr.Manifest.Parts["kernel"].SHA256 != sum || tr.Manifest.Parts["qemu"].SHA256 == "" || tr.Manifest.Parts["spin-machine"].SHA256 == "" {
		t.Errorf("the one-file parts' sums: %+v", tr.Manifest.Parts)
	}
	if tr.Manifest.Parts["image"].SHA256 != "" {
		t.Error("the image is a gigabyte, and is not summed")
	}
	for _, p := range []string{"bin/qemu-system-x86_64", "image/rootfs.qcow2", "kernel/bzImage"} {
		if target, err := os.Readlink(filepath.Join(tr.Dir, p)); err != nil || target != filepath.Join(base, p) {
			t.Errorf("%s links to %q (%v), want the base's", p, target, err)
		}
	}
	if tr.SpinMachine() != filepath.Join(tr.Dir, "bin/spin-machine") || tr.QEMUImg() != filepath.Join(tr.Dir, "bin/qemu-img") ||
		tr.Rootfs() != filepath.Join(tr.Dir, "image/rootfs.qcow2") {
		t.Error("a part's path is not the tree's")
	}
	var m Manifest
	if err := json.Unmarshal([]byte(read(t, filepath.Join(tr.Dir, "manifest.json"))), &m); err != nil || m.Name != "t" || m.Append != "quiet x=1" {
		t.Errorf("manifest.json: %+v, %v", m, err)
	}
	if _, err := MakeTree(filepath.Join(t.TempDir(), "x"), TreeSpec{From: base}, src(t)); err == nil {
		t.Error("a tree with no name was made")
	}
}

func TestAPartPutIntoATreeIsACopyAndTheBaseIsUntouched(t *testing.T) {
	base := fakeRelease(t, "release kernel")
	newKernel := filepath.Join(t.TempDir(), "k", "bzImage")
	write(t, newKernel, bzImage("branch kernel"))
	write(t, filepath.Join(filepath.Dir(newKernel), "vmlinux"), []byte("branch elf"))
	newQEMU := t.TempDir()
	write(t, filepath.Join(newQEMU, "bin/qemu-system-x86_64"), []byte("qemu 11.2"))
	write(t, filepath.Join(newQEMU, "qemu/qboot.bin"), []byte("qboot 2"))
	write(t, filepath.Join(newQEMU, "qemu/linuxboot_dma.bin"), []byte("rom 2"))

	tr, err := MakeTree(filepath.Join(t.TempDir(), "b"), TreeSpec{Name: "b", From: base, Kernel: newKernel, QEMU: newQEMU}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(read(t, filepath.Join(tr.Dir, "kernel/bzImage")), "branch kernel") ||
		read(t, filepath.Join(tr.Dir, "kernel/vmlinux")) != "branch elf" ||
		read(t, filepath.Join(tr.Dir, "bin/qemu-system-x86_64")) != "qemu 11.2" ||
		read(t, filepath.Join(tr.Dir, "qemu/qboot.bin")) != "qboot 2" {
		t.Error("the tree does not hold the parts put into it")
	}
	if !strings.HasSuffix(read(t, filepath.Join(base, "kernel/bzImage")), "release kernel") || read(t, filepath.Join(base, "kernel/vmlinux")) != "elf" ||
		read(t, filepath.Join(base, "bin/qemu-system-x86_64")) != "qemu" || read(t, filepath.Join(base, "qemu/qboot.bin")) != "qboot" {
		t.Error("putting a part into the tree wrote the base")
	}
	if fi, err := os.Lstat(filepath.Join(tr.Dir, "kernel/bzImage")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a part put into the tree is still a link")
	}
	if tr.Manifest.Parts["kernel"].Source != newKernel || tr.Manifest.Parts["qemu"].Source != newQEMU || tr.Manifest.Parts["firmware"].Source != newQEMU {
		t.Errorf("the manifest's sources: %+v", tr.Manifest.Parts)
	}
	sum, _ := fileSHA256(newKernel)
	if tr.Manifest.Parts["kernel"].SHA256 != sum {
		t.Error("the kernel's sum is not the one put in")
	}

	// A kernel with no vmlinux beside it leaves none: the release's would describe another kernel.
	lone := filepath.Join(t.TempDir(), "bzImage")
	write(t, lone, bzImage("lone"))
	tr, err = MakeTree(filepath.Join(t.TempDir(), "c"), TreeSpec{Name: "c", From: base, Kernel: lone}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(tr.Dir, "kernel/vmlinux")) {
		t.Error("the release's vmlinux was left beside another kernel")
	}
}

func TestATreeRefusesWhatIsNotWhole(t *testing.T) {
	base := fakeRelease(t, "k")
	elf := filepath.Join(t.TempDir(), "vmlinux")
	write(t, elf, []byte("\x7fELF not a bzImage"))
	longELF := filepath.Join(t.TempDir(), "vmlinux")
	write(t, longELF, append([]byte("\x7fELF"), make([]byte, 4096)...))
	for _, tc := range []struct {
		name string
		spec TreeSpec
		want string
	}{
		{"a vmlinux for a kernel", TreeSpec{Kernel: elf}, "not a bzImage"},
		{"a long file with no setup header", TreeSpec{Kernel: longELF}, "not a bzImage"},
		{"a run id that is not one", TreeSpec{Kernel: "run:12x"}, "is a number"},
		{"a missing kernel", TreeSpec{Kernel: "/nonexistent"}, "opening kernel"},
		{"a QEMU directory without one", TreeSpec{QEMU: t.TempDir()}, "qemu-system-x86_64"},
		{"a spin-machine from nowhere", TreeSpec{SpinMachine: "elsewhere"}, "is here, release, or left out"},
		{"an edit with no name", TreeSpec{Edits: []string{"nope"}}, "no edit named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Name, tc.spec.From = "x", base
			_, err := MakeTree(filepath.Join(t.TempDir(), "x"), tc.spec, src(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("MakeTree = %v, want an error saying %q", err, tc.want)
			}
		})
	}

	good, err := MakeTree(filepath.Join(t.TempDir(), "good"), TreeSpec{Name: "good", From: base}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		break_ func(dir string)
		want   string
	}{
		{"no manifest", func(d string) { _ = os.Remove(filepath.Join(d, manifestName)) }, "has no manifest"},
		{"a manifest that is not one", func(d string) { write(t, filepath.Join(d, manifestName), []byte("{")) }, "reading"},
		{"a part replaced after", func(d string) {
			_ = os.Remove(filepath.Join(d, "bin/spin-machine"))
			write(t, filepath.Join(d, "bin/spin-machine"), []byte("another"))
		}, "its manifest says"},
		{"a part with no source", func(d string) {
			var m Manifest
			_ = json.Unmarshal([]byte(read(t, filepath.Join(d, manifestName))), &m)
			delete(m.Parts, "image")
			b, _ := json.Marshal(m)
			write(t, filepath.Join(d, manifestName), b)
		}, "where its image came from"},
		{"a part with an empty source", func(d string) {
			var m Manifest
			_ = json.Unmarshal([]byte(read(t, filepath.Join(d, manifestName))), &m)
			m.Parts["image"] = Part{}
			b, _ := json.Marshal(m)
			write(t, filepath.Join(d, manifestName), b)
		}, "where its image came from"},
		{"a part gone", func(d string) { _ = os.Remove(filepath.Join(d, "image/rootfs.qcow2")) }, "has no image"},
		{"not a machine", func(d string) { _ = os.Remove(filepath.Join(d, "qemu/qboot.bin")) }, "not a whole machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "copy")
			if err := linkTree(good.Dir, d); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(filepath.Join(d, manifestName))
			write(t, filepath.Join(d, manifestName), []byte(read(t, filepath.Join(good.Dir, manifestName))))
			if _, err := OpenTree(d); err != nil {
				t.Fatalf("the copy before breaking it: %v", err)
			}
			tc.break_(d)
			if _, err := OpenTree(d); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("OpenTree = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// A release from before 2026-11-19 has a vmlinux and no bzImage, and nothing boots it now: it
// is no tree, and the refusal names the file it lacks.
func TestAReleaseFromBeforeTheBzImageIsNoTree(t *testing.T) {
	base := fakeRelease(t, "")
	if _, err := MakeTree(filepath.Join(t.TempDir(), "old"), TreeSpec{Name: "old", From: base}, src(t)); err == nil ||
		!strings.Contains(err.Error(), "kernel/bzImage") {
		t.Errorf("MakeTree of a release without a bzImage = %v", err)
	}
}

func TestTheLaneEditIsThisCheckoutsLane(t *testing.T) {
	base := fakeRelease(t, "k")
	tr, err := MakeTree(filepath.Join(t.TempDir(), "l"), TreeSpec{Name: "l", From: base, Edits: []string{"lane"}}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Manifest.Edits) != 1 || tr.Manifest.Edits[0].Name != "lane" {
		t.Fatalf("edits: %+v", tr.Manifest.Edits)
	}
	e := tr.Manifest.Edits[0].Edit
	want := read(t, filepath.Join(checkoutRoot(t), "image/mkosi.extra/usr/local/lib/spin-base/rootfs/etc/systemd/system/sshd@.service.d/10-lane.conf"))
	if e.Files["/etc/systemd/system/sshd@.service.d/10-lane.conf"] != want || len(e.Files) != 15 || len(e.Links) != 1 ||
		!strings.Contains(e.Setup, "class=user-early-light") {
		t.Errorf("the lane edit: %d files, %d links, setup %q", len(e.Files), len(e.Links), e.Setup)
	}
	if _, err := laneEdit(t.TempDir()); err == nil || !strings.Contains(err.Error(), "reading the lane's") {
		t.Errorf("a checkout with no image: %v", err)
	}
	if s := systemdPackages("/x/debs").Setup; !strings.Contains(s, "debs=/x/debs") || !strings.Contains(s, "test \"$replaced\" -gt 0") {
		t.Errorf("systemd's edit: %q", s)
	}
}

// The edits an unknown name is told about are the ones a tree can ask for, each one an edit.
func TestEveryEditTheErrorNamesIsOne(t *testing.T) {
	_, err := namedEdit("nope", checkoutRoot(t), t.TempDir())
	_, list, ok := strings.Cut(fmt.Sprint(err), "the edits are: ")
	if !ok {
		t.Fatalf("an unknown edit: %v", err)
	}
	for name := range strings.SplitSeq(list, ", ") {
		e, err := namedEdit(name, checkoutRoot(t), t.TempDir())
		if err != nil || len(e.Files)+len(e.Links) == 0 && e.Setup == "" {
			t.Errorf("edit %s: %+v, %v", name, e, err)
		}
	}
}

// --spin-machine here builds this checkout's: the tree's is a copy, named for its commit, and a
// checkout that does not build, or a systemd that does not, is no tree.
func TestATreesSpinMachineCanBeThisCheckouts(t *testing.T) {
	base := fakeRelease(t, "k")
	tr, err := MakeTree(filepath.Join(t.TempDir(), "h"), TreeSpec{Name: "h", From: base, SpinMachine: "here"}, src(t))
	if err != nil {
		t.Fatal(err)
	}
	// Named by its commit where git can say it, and by its directory in a copy where it cannot.
	root := checkoutRoot(t)
	want := "built from " + root
	if rev, err := exec.Command("git", "-C", root, "describe", "--always", "--dirty").Output(); err == nil {
		want = "built from " + strings.TrimSpace(string(rev))
	}
	if s := tr.Manifest.Parts["spin-machine"].Source; s != want {
		t.Errorf("source %q, want %q", s, want)
	}
	if fi, err := os.Lstat(tr.SpinMachine()); err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Size() < 1<<20 {
		t.Errorf("the built spin-machine: %v, %v", fi, err)
	}
	if read(t, filepath.Join(base, "bin/spin-machine")) != "spin-machine" {
		t.Error("building wrote the base's")
	}
	// Left out, a base that has one keeps it.
	if tr, err := MakeTree(filepath.Join(t.TempDir(), "k"), TreeSpec{Name: "k", From: base}, src(t)); err != nil || tr.Manifest.Parts["spin-machine"].Source != base {
		t.Errorf("a base's own spin-machine: %+v, %v", tr, err)
	}
	// A release ships no spin-machine: left out, it is this checkout's.
	_ = os.Remove(filepath.Join(base, "bin/spin-machine"))
	tr, err = MakeTree(filepath.Join(t.TempDir(), "r"), TreeSpec{Name: "r", From: base}, src(t))
	if err != nil || !strings.HasPrefix(tr.Manifest.Parts["spin-machine"].Source, "built from ") {
		t.Errorf("a base with no spin-machine: %v", err)
	}
	nowhere := Sources{Checkout: t.TempDir(), Cache: t.TempDir(), Log: io.Discard}
	if _, err := MakeTree(filepath.Join(t.TempDir(), "x"), TreeSpec{Name: "x", From: base, SpinMachine: "here"}, nowhere); err == nil ||
		!strings.Contains(err.Error(), "building this checkout's spin-machine") {
		t.Errorf("a checkout that does not build: %v", err)
	}
	if _, err := MakeTree(filepath.Join(t.TempDir(), "y"), TreeSpec{Name: "y", From: fakeRelease(t, "k"), Systemd: "pid1-marks"}, nowhere); err == nil ||
		!strings.Contains(err.Error(), "building systemd with the experiment pid1-marks") {
		t.Errorf("a systemd that does not build: %v", err)
	}
}

func TestOneIsTheOneFileWithThatName(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a/bzImage"), nil)
	write(t, filepath.Join(d, "a/notbzImage"), nil)
	if got, err := one(d, "bzImage"); err != nil || got != filepath.Join(d, "a/bzImage") {
		t.Errorf("one = %q, %v", got, err)
	}
	write(t, filepath.Join(d, "b/bzImage"), nil)
	if _, err := one(d, "bzImage"); err == nil || !strings.Contains(err.Error(), "found 2") {
		t.Errorf("two: %v", err)
	}
	if _, err := one(d, "vmlinux"); err == nil || !strings.Contains(err.Error(), "found 0") {
		t.Errorf("none: %v", err)
	}
	if _, err := one(filepath.Join(d, "missing"), "x"); err == nil {
		t.Error("a missing directory has one of something")
	}
}

// A run's kernel or QEMU is its artifact, downloaded once into the cache: a run with no bzImage is
// one from before 2026-11-19, and is refused by name.
func TestAKernelOrQEMUFromARunIsTheRunsBuild(t *testing.T) {
	base := fakeRelease(t, "release kernel")
	cache := t.TempDir()
	s := Sources{Checkout: checkoutRoot(t), Cache: cache, Log: io.Discard}
	write(t, filepath.Join(cache, "runs/11/vmlinux/vmlinux-abc/bzImage"), bzImage("run kernel"))
	qemu := filepath.Join(cache, "runs/22/qemu-*-linux-amd64/qemu-11.2-linux-amd64")
	write(t, filepath.Join(qemu, "bin/qemu-system-x86_64"), []byte("run qemu"))
	write(t, filepath.Join(qemu, "qemu/qboot.bin"), []byte("run qboot"))
	write(t, filepath.Join(qemu, "qemu/linuxboot_dma.bin"), []byte("run rom"))
	tr, err := MakeTree(filepath.Join(t.TempDir(), "r"), TreeSpec{Name: "r", From: base, Kernel: "run:11", QEMU: "run:22"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Manifest.Parts["kernel"].Source != "run 11" || tr.Manifest.Parts["qemu"].Source != "run 22" ||
		!strings.HasSuffix(read(t, filepath.Join(tr.Dir, "kernel/bzImage")), "run kernel") ||
		read(t, filepath.Join(tr.Dir, "qemu/qboot.bin")) != "run qboot" {
		t.Errorf("the run's parts: %+v", tr.Manifest.Parts)
	}
	write(t, filepath.Join(cache, "runs/33/vmlinux/vmlinux-old/vmlinux"), []byte("elf"))
	if err := os.MkdirAll(filepath.Join(cache, "runs/44/qemu-*-linux-amd64/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	noFirmware := t.TempDir()
	write(t, filepath.Join(noFirmware, "bin/qemu-system-x86_64"), []byte("q"))
	for _, tc := range []struct {
		spec TreeSpec
		says string
	}{
		{TreeSpec{Kernel: "run:33"}, "kernel run 33: want one bzImage"},
		{TreeSpec{QEMU: "run:44"}, "QEMU run 44: want one"},
		{TreeSpec{QEMU: noFirmware}, "copying " + noFirmware + "'s firmware"},
		{TreeSpec{QEMU: t.TempDir()}, "opening"},
	} {
		spec, says := tc.spec, tc.says
		spec.Name, spec.From = "x", base
		if _, err := MakeTree(filepath.Join(t.TempDir(), "x"), spec, s); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%+v: %v, want an error saying %q", spec, err, says)
		}
	}
}

// A release asked for by its tag is the one unpacked under the cache, when it is there.
func TestAReleaseByItsTagIsTheOneInTheCache(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "releases", "v20260101.01", "spin-machine-v20260101.01-linux-x86_64", "usr", "share", "spin-stack")
	write(t, filepath.Join(dir, partFiles["qemu"]), []byte("qemu"))
	got, source, err := Sources{Cache: cache}.from("release:v20260101.01")
	if err != nil || got != dir || source != "release v20260101.01" {
		t.Errorf("from = %s, %q, %v", got, source, err)
	}
	// A directory named like one is a directory: only "release" and "release:TAG" are releases.
	wd, _ := os.Getwd()
	if got, _, err := (Sources{Cache: cache}).from("releases-of-mine"); err != nil || got != filepath.Join(wd, "releases-of-mine") {
		t.Errorf("from(releases-of-mine) = %s, %v", got, err)
	}
}

// A release's own spin-machine is built from the commit its machine.env names, and a base with no
// machine.env has none to build.
func TestATreesSpinMachineCanBeItsReleases(t *testing.T) {
	if _, err := MakeTree(filepath.Join(t.TempDir(), "r"), TreeSpec{Name: "r", From: fakeRelease(t, "k"), SpinMachine: "release"}, src(t)); err == nil ||
		!strings.Contains(err.Error(), "machine.env") {
		t.Errorf("a release with no machine.env: %v", err)
	}
}

func TestATreeFromADirectoryNamesItWhole(t *testing.T) {
	d := fakeRelease(t, "k")
	wd, _ := os.Getwd()
	rel, _ := filepath.Rel(wd, d)
	got, source, err := Sources{}.from(rel)
	if err != nil || got != d || source != d {
		t.Errorf("from(%s) = %s, %s, %v; want %s", rel, got, source, err, d)
	}
}

func TestReplaceRefusesWhatItCannotRead(t *testing.T) {
	d := t.TempDir()
	if err := replace(filepath.Join(d, "missing"), filepath.Join(d, "to"), 0o644); err == nil {
		t.Error("replaced from a file that is not there")
	}
	write(t, filepath.Join(d, "from"), []byte("x"))
	if err := replace(filepath.Join(d, "from"), filepath.Join(d, "deep/er/to"), 0o600); err != nil || read(t, filepath.Join(d, "deep/er/to")) != "x" {
		t.Errorf("replace into a new directory: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(d, "deep/er/to")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if _, err := fileSHA256(filepath.Join(d, "missing")); err == nil {
		t.Error("summed a file that is not there")
	}
	if err := run("true"); err != nil {
		t.Errorf("run(true) = %v", err)
	}
	if err := run("sh", "-c", "echo nope >&2; exit 3"); err == nil || err.Error() != "sh -c echo nope >&2; exit 3: exit status 3: nope" {
		t.Errorf("run of a failing command = %v", err)
	}
}
