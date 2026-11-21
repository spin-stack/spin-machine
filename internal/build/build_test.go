// SPDX-License-Identifier: Apache-2.0

package build

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spin-stack/go-tools/versions"
)

// pins is the repository's versions.yaml: what every part is really built from.
func pins(t *testing.T) *versions.Versions {
	t.Helper()
	v, err := versions.Load(filepath.Join("..", "..", versions.File))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// fakeDocker puts a docker on the PATH that records each call's arguments, one call a line, and
// answers `create` with a container id; it returns where the calls are recorded.
func fakeDocker(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	body := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestAPlanIsWhatWasNamedAndWhatItIsMadeWith(t *testing.T) {
	names := func(plan []Part) []string {
		var n []string
		for _, p := range plan {
			n = append(n, p.Name)
		}
		return n
	}
	all, err := Plan(nil)
	if err != nil || !slices.Equal(names(all), []string{"qemu", "kernel", "e2fsprogs", "systemd", "image"}) {
		t.Errorf("Plan(nil) = %v, %v; want every part, in order", names(all), err)
	}
	image, err := Plan([]string{"image", "kernel"})
	if err != nil || !slices.Equal(names(image), []string{"kernel", "e2fsprogs", "systemd", "image"}) {
		t.Errorf("Plan(image, kernel) = %v, %v; want kernel and the image with what it is made with, in order", names(image), err)
	}
	if _, err := Plan([]string{"qemu", "nope"}); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("Plan(nope) = %v, want an error naming it", err)
	}
	if !slices.Equal(Names(), names(all)) {
		t.Errorf("Names() = %v", Names())
	}
}

func TestArgsAreWhatVersionsArgsWrites(t *testing.T) {
	v := pins(t)
	args, err := Args(v, "kernel", "alpine")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := v.Get("kernel")
	a, _ := v.Get("alpine")
	want := []string{"--build-arg", "ALPINE_IMAGE=" + a.Ref(), "--build-arg", "KERNEL_SHA256=" + k.Pin, "--build-arg", "KERNEL_VERSION=" + k.Version}
	if !slices.Equal(args, want) {
		t.Errorf("Args = %v, want %v", args, want)
	}
	if _, err := Args(v, "nope"); err == nil {
		t.Error("Args of an entry versions.yaml lacks gave no error")
	}
}

func TestKernelSeriesIsMajorMinor(t *testing.T) {
	v, err := versions.Parse([]byte("- name: kernel\n  kind: download\n  source: https://k/{version}.tar.xz\n  version: 7.3.5\n  pin: " + strings.Repeat("a", 64) + "\n  track: releases https://k\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := KernelSeries(v); err != nil || s != "7.3" {
		t.Errorf("KernelSeries = %q, %v; want 7.3", s, err)
	}
	if _, err := KernelSeries(&versions.Versions{}); err == nil {
		t.Error("no kernel entry gave no error")
	}
}

func TestADigestMovesWithItsInputsAndNothingElse(t *testing.T) {
	root := t.TempDir()
	write := func(p, body string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("part/a", "a", 0o644)
	write("part/sub/b", "b", 0o644)
	write("other/c", "c", 0o644)
	digest := func(args ...string) string {
		d, err := Digest(root, args, []string{"part"})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	base := digest("x")
	if again := digest("x"); again != base {
		t.Fatal("the same inputs gave two digests")
	}
	write("other/c", "changed", 0o644)
	if digest("x") != base {
		t.Error("a file outside the part's paths moved its digest")
	}
	for _, tc := range []struct {
		name   string
		change func()
		undo   func()
	}{
		{"an argument", func() {}, func() {}},
		{"a file's content", func() { write("part/a", "A", 0o644) }, func() { write("part/a", "a", 0o644) }},
		{"a file's mode", func() { _ = os.Chmod(filepath.Join(root, "part/a"), 0o755) }, func() { _ = os.Chmod(filepath.Join(root, "part/a"), 0o644) }},
		{"a new file", func() { write("part/new", "", 0o644) }, func() { _ = os.Remove(filepath.Join(root, "part/new")) }},
		{"a renamed file", func() { _ = os.Rename(filepath.Join(root, "part/sub/b"), filepath.Join(root, "part/sub/B")) },
			func() { _ = os.Rename(filepath.Join(root, "part/sub/B"), filepath.Join(root, "part/sub/b")) }},
		{"a symlink's target", func() { _ = os.Symlink("a", filepath.Join(root, "part/l")) }, func() { _ = os.Remove(filepath.Join(root, "part/l")) }},
	} {
		tc.change()
		arg := "x"
		if tc.name == "an argument" {
			arg = "y"
		}
		if digest(arg) == base {
			t.Errorf("%s did not move the digest", tc.name)
		}
		tc.undo()
		if digest("x") != base {
			t.Fatalf("undoing %s did not bring the digest back", tc.name)
		}
	}
	_ = os.Symlink("a", filepath.Join(root, "part/l"))
	withA := digest("x")
	_ = os.Remove(filepath.Join(root, "part/l"))
	_ = os.Symlink("sub/b", filepath.Join(root, "part/l"))
	if digest("x") == withA {
		t.Error("a symlink pointed elsewhere kept its digest")
	}
	if _, err := Digest(root, nil, []string{"missing"}); err == nil {
		t.Error("a path that is not there gave a digest")
	}
	if d, err := Digest(root, []string{"x"}, nil, "extra"); err != nil || d == digest("x") {
		t.Errorf("an extra input did not move the digest: %v", err)
	}
}

func TestAManifestIsWhatWasWritten(t *testing.T) {
	out := t.TempDir()
	if _, ok, err := ReadManifest(out, "qemu"); ok || err != nil {
		t.Errorf("no manifest read as %v, %v", ok, err)
	}
	if err := WriteManifest(out, Manifest{Part: "qemu", Inputs: "abc", From: "build", Built: true}); err != nil {
		t.Fatal(err)
	}
	m, ok, err := ReadManifest(out, "qemu")
	if !ok || err != nil || m != (Manifest{Part: "qemu", Inputs: "abc", From: "build"}) {
		t.Errorf("read back %+v, %v, %v; Built is not stored", m, ok, err)
	}
	if err := os.WriteFile(ManifestPath(out, "kernel"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(out, "kernel"); err == nil {
		t.Error("a manifest that is not JSON read as one")
	}
	if err := os.Mkdir(ManifestPath(out, "image"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(out, "image"); err == nil {
		t.Error("a manifest that cannot be read gave no error")
	}
	if err := WriteManifest(filepath.Join(ManifestPath(out, "qemu"), "under-a-file"), Manifest{Part: "x"}); err == nil {
		t.Error("a manifest written under a file gave no error")
	}
	if err := WriteManifest(out, Manifest{Part: "image"}); err == nil {
		t.Error("a manifest written over a directory gave no error")
	}
}

// fakePart is a part whose making and checking a test decides.
func fakePart(made *int, checked *error, makeErr error) Part {
	return Part{Name: "fake", Paths: []string{"part"},
		make:  func(context.Context, Options, Part) error { *made++; return makeErr },
		check: func(Options) error { return *checked }}
}

func TestAPartIsBuiltWhenMissingOrStaleAndNotOtherwise(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "part"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	o := Options{Root: root, Out: out, Pins: &versions.Versions{}, Stderr: &stderr}
	made := 0
	var checked error
	p := fakePart(&made, &checked, nil)

	r, err := Make(context.Background(), o, []Part{p})
	if err != nil || made != 1 || !r[0].Built || r[0].From != "build" || len(r[0].Inputs) != 64 {
		t.Fatalf("a missing part: made %d, %+v, %v", made, r, err)
	}
	if _, ok, _ := ReadManifest(out, "fake"); !ok {
		t.Fatal("a built part left no manifest")
	}
	if r, err = Make(context.Background(), o, []Part{p}); err != nil || made != 1 || r[0].Built {
		t.Errorf("a current part was built again: made %d, %+v, %v", made, r, err)
	}
	if !strings.Contains(stderr.String(), "fake: current") {
		t.Errorf("a current part was not said to be: %q", stderr.String())
	}
	checked = errors.New("a file removed by hand")
	if _, err := Make(context.Background(), o, []Part{p}); err == nil || made != 2 {
		t.Errorf("a part that no longer verifies: made %d, %v; want it built, and its check's error", made, err)
	}
	checked = nil
	if err := os.WriteFile(filepath.Join(root, "part", "f"), []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), o, []Part{p}); err != nil || made != 3 {
		t.Errorf("a part whose inputs moved: made %d, %v; want it built", made, err)
	}
	o.Rebuild = true
	if _, err := Make(context.Background(), o, []Part{p}); err != nil || made != 4 {
		t.Errorf("a rebuild: made %d, %v; want it built", made, err)
	}
	o.Rebuild = false
	if err := WriteManifest(out, Manifest{Part: "fake", Inputs: r[0].Inputs, From: "ghcr.io/x:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), o, []Part{p}); err != nil || made != 5 {
		t.Errorf("a fetched part: made %d, %v; want it built, a fetch is not this tree's build", made, err)
	}
	broken := fakePart(&made, &checked, errors.New("compiler error"))
	if _, err := Make(context.Background(), Options{Root: root, Out: out, Pins: o.Pins, Rebuild: true}, []Part{broken}); err == nil || !strings.Contains(err.Error(), "building fake") {
		t.Errorf("a failed build gave %v", err)
	}
	if err := os.WriteFile(ManifestPath(out, "fake"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), o, []Part{p}); err == nil {
		t.Error("an unreadable manifest gave no error")
	}
	gone := Part{Name: "fake", Paths: []string{"absent"}, make: p.make, check: p.check}
	if _, err := Make(context.Background(), o, []Part{gone}); err == nil {
		t.Error("inputs that cannot be read gave no error")
	}
	unwritable := o
	unwritable.Out = t.TempDir()
	if err := os.WriteFile(filepath.Join(unwritable.Out, "manifest"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), unwritable, []Part{p}); err == nil {
		t.Error("a manifest that cannot be written gave no error")
	}
}

func TestAPartIsMadeWithTheManifestsOfWhatItNeeds(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "part"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{Root: root, Out: out, Pins: &versions.Versions{}}
	made := 0
	var checked error
	p := fakePart(&made, &checked, nil)
	p.Needs = []string{"dep"}
	if _, err := Make(context.Background(), o, []Part{p}); err == nil || !strings.Contains(err.Error(), "dep") {
		t.Errorf("a part whose need has no manifest: %v", err)
	}
	if err := WriteManifest(out, Manifest{Part: "dep", Inputs: "one", From: "build"}); err != nil {
		t.Fatal(err)
	}
	r, err := Make(context.Background(), o, []Part{p})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(out, Manifest{Part: "dep", Inputs: "two", From: "build"}); err != nil {
		t.Fatal(err)
	}
	r2, err := Make(context.Background(), o, []Part{p})
	if err != nil || r2[0].Inputs == r[0].Inputs || made != 2 {
		t.Errorf("a need rebuilt did not make the part stale: made %d, %v", made, err)
	}
	if err := os.WriteFile(ManifestPath(out, "dep"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), o, []Part{p}); err == nil {
		t.Error("an unreadable need's manifest gave no error")
	}
}

func TestVerifyIsEveryPartsCheckJoined(t *testing.T) {
	var stderr bytes.Buffer
	good := Part{Name: "good", check: func(Options) error { return nil }}
	bad := Part{Name: "bad", check: func(Options) error { return errors.New("missing x") }}
	err := Verify(Options{Stderr: &stderr}, []Part{good, bad, good})
	if err == nil || !strings.Contains(err.Error(), "bad: missing x") {
		t.Errorf("Verify = %v, want the bad part's check", err)
	}
	if strings.Count(stderr.String(), "OK: good") != 2 {
		t.Errorf("each part that verified was not said: %q", stderr.String())
	}
	if Verify(Options{}, []Part{good}) != nil {
		t.Error("a part that verifies failed Verify")
	}
}

func TestEachPartIsBuiltAsItsDockerfileWantsIt(t *testing.T) {
	v := pins(t)
	root, out := t.TempDir(), t.TempDir()
	log := fakeDocker(t, "")
	goLog := fakeGo(t, staticELF(t))
	o := Options{Root: root, Out: out, Pins: v, Jobs: 3, Registry: true, Export: true, SystemdExperiment: "exp"}
	for _, f := range []string{"bin/qemu-img", "qemu/x", "sbom/qemu.apk", "kernel/old.patch", "bin/debugfs", "e2fsprogs/x", "systemd/old.deb", "bin/keep"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(out, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range Parts {
		if err := p.make(context.Background(), o, p); err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
	}
	for _, f := range []string{"bin/qemu-img", "qemu", "sbom/qemu.apk", "kernel", "bin/debugfs", "e2fsprogs", "systemd/old.deb"} {
		if _, err := os.Stat(filepath.Join(out, f)); err == nil {
			t.Errorf("%s survived its part's build", f)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "bin/keep")); err != nil {
		t.Error("a build removed what is not its part's")
	}
	if _, err := os.Stat(filepath.Join(root, ".cache", "mkosi")); err != nil {
		t.Error("the image's build made no mkosi cache")
	}
	if got, _ := os.ReadFile(goLog); string(got) != "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 build -trimpath -ldflags -s -w -o "+filepath.Join(out, PressurePath)+" ./cmd/spin-machine-pressure\n" {
		t.Errorf("the image's sender was built with %q", got)
	}
	got := calls(t, log)
	series, _ := KernelSeries(v)
	reg := "--cache-from type=registry,ref=" + CacheRegistry
	for i, want := range []string{
		"buildx build --file qemu/Dockerfile --target extract --platform linux/amd64 " + reg + ":qemu --cache-to type=registry,ref=" + CacheRegistry + ":qemu,mode=max,image-manifest=true,oci-mediatypes=true ",
		"buildx build --file kernel/Dockerfile --target kernel --platform linux/amd64 " + reg + ":kernel",
		"buildx build --file e2fsprogs/Dockerfile --target extract --platform linux/amd64 " + reg + ":e2fsprogs",
		"buildx build --file image/systemd/Dockerfile --target debs --platform linux/amd64 " + reg + ":systemd",
		"buildx build --file image/Dockerfile --platform linux/amd64 " + reg + ":image",
		"run --rm --security-opt seccomp=unconfined --security-opt apparmor=unconfined --cap-add SYS_ADMIN -v " + out + ":/out -v " + filepath.Join(root, ".cache", "mkosi") + ":/cache -v " + filepath.Join(out, "systemd") + ":/debs:ro " + imageBuilder,
	} {
		if i >= len(got) || !strings.HasPrefix(got[i], want) {
			t.Errorf("call %d = %q, want it to start %q", i, at(got, i), want)
		}
	}
	for i, want := range [][]string{
		{"--build-arg JOBS=3", "--output type=local,dest=" + out + " ."},
		{"--build-arg KERNEL_SERIES=" + series, "--build-arg KERNEL_ARCH=x86_64", "--build-arg KERNEL_NPROC=3", "--output type=local,dest=" + out + " ."},
		{"--output type=local,dest=" + out + " ."},
		{"--build-arg SYSTEMD_EXPERIMENT=exp", "--build-arg JOBS=3", "--output type=local,dest=" + filepath.Join(out, "systemd") + " ."},
		{"--tag " + imageBuilder + " --load ."},
	} {
		for _, w := range want {
			if !strings.Contains(at(got, i), w) {
				t.Errorf("call %d = %q, want %q in it", i, at(got, i), w)
			}
		}
	}
	for i, p := range Parts {
		args, _ := Args(v, p.Pins...)
		if !strings.Contains(at(got, i), strings.Join(args, " ")) {
			t.Errorf("%s's build is not handed its pins %v", p.Name, args)
		}
	}
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

func TestTheCacheIsLocalByDefaultAndWrittenOnlyWhenAsked(t *testing.T) {
	o := Options{Root: "/r"}
	if got := cacheFlags(o, "qemu"); !slices.Equal(got, []string{"--cache-from", "type=local,src=/r/.cache/buildkit/qemu"}) {
		t.Errorf("local, not exported: %v", got)
	}
	o.Export = true
	if got := cacheFlags(o, "qemu"); !slices.Equal(got, []string{"--cache-from", "type=local,src=/r/.cache/buildkit/qemu",
		"--cache-to", "type=local,dest=/r/.cache/buildkit/qemu,mode=max,compression=zstd"}) {
		t.Errorf("local, exported: %v", got)
	}
}

func TestABuildThatFailsSaysWhich(t *testing.T) {
	fakeDocker(t, "exit 3")
	err := makeE2fsprogs(context.Background(), Options{Root: t.TempDir(), Out: t.TempDir(), Pins: pins(t)}, Parts[2])
	if err == nil || !strings.Contains(err.Error(), "docker buildx build") {
		t.Errorf("a failing docker gave %v", err)
	}
}

func TestAPartWithoutItsPinsIsNotBuilt(t *testing.T) {
	log := fakeDocker(t, "")
	o := Options{Root: t.TempDir(), Out: t.TempDir(), Pins: &versions.Versions{}}
	for _, p := range Parts {
		if err := p.make(context.Background(), o, p); err == nil {
			t.Errorf("%s built with no pins", p.Name)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("docker ran for a part with no pins")
	}
}

func TestEmptiedRefusesWhatItCannotRemove(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := emptied(filepath.Join(out, "file")); err == nil {
		t.Error("an output directory that is a file was used")
	}
	locked := filepath.Join(out, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "d", "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(locked, "d"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "d"), 0o755) })
	if os.Getuid() != 0 {
		if err := emptied(locked, "d"); err == nil {
			t.Error("what could not be removed was not said")
		}
	}
}

// fakeGo is a go on PATH that logs each call, with its environment's build switches, and copies
// built to the -o it is given.
func fakeGo(t *testing.T, built string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script(t, filepath.Join(dir, "go"), `echo "CGO_ENABLED=$CGO_ENABLED GOOS=$GOOS GOARCH=$GOARCH $*" >> `+log+`
prev=""; for a; do [ "$prev" = -o ] && cp `+built+` "$a"; prev=$a; done; exit 0`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// staticELF is an ELF whose one program header loads it, as a static binary's do, and none asks
// for an interpreter.
func staticELF(t *testing.T) string {
	t.Helper()
	return writeELF(t, []elf.Prog64{{Type: uint32(elf.PT_LOAD)}})
}

// writeELF is an x86-64 ELF executable with progs as its program headers.
func writeELF(t *testing.T, progs []elf.Prog64) string {
	t.Helper()
	var b bytes.Buffer
	h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Phoff: 64, Ehsize: 64, Phentsize: 56, Phnum: uint16(len(progs))}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS], h.Ident[elf.EI_DATA], h.Ident[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	_ = binary.Write(&b, binary.LittleEndian, h)
	for _, p := range progs {
		_ = binary.Write(&b, binary.LittleEndian, p)
	}
	path := filepath.Join(t.TempDir(), "elf")
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The image's sender is refused when it asks for a dynamic loader, or is no ELF at all.
func TestTheImagesSenderIsStatic(t *testing.T) {
	if err := static(staticELF(t)); err != nil {
		t.Errorf("a static ELF: %v", err)
	}
	dynamic := writeELF(t, []elf.Prog64{{Type: uint32(elf.PT_LOAD)}, {Type: uint32(elf.PT_INTERP)}})
	if err := static(dynamic); err == nil || !strings.Contains(err.Error(), "is linked dynamically") {
		t.Errorf("a dynamic ELF: %v", err)
	}
	if err := static(filepath.Join(t.TempDir(), "none")); err == nil || !strings.Contains(err.Error(), "reading") {
		t.Errorf("no ELF: %v", err)
	}
	fakeGo(t, dynamic)
	out := t.TempDir()
	if err := buildPressure(context.Background(), Options{Root: t.TempDir(), Out: out}); err == nil {
		t.Error("a dynamic sender was built for the image")
	}
	if err := buildPressure(context.Background(), Options{Out: filepath.Join(dynamic, "x")}); err == nil {
		t.Error("an output under a file gave no error")
	}
	dir := t.TempDir()
	script(t, filepath.Join(dir, "go"), "exit 1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := buildPressure(context.Background(), Options{Root: t.TempDir(), Out: out}); err == nil {
		t.Error("a sender that did not build gave no error")
	}
}
