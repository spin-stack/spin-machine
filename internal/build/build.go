// SPDX-License-Identifier: Apache-2.0

// Package build makes the machine's parts - QEMU and its firmware, the guest kernel, e2fsprogs,
// the systemd the image installs, the base image and spin-machine - into _output, each from what
// versions.yaml pins and what this tree holds, and asserts what came out.
//
// A part is built when it is missing or stale: each one built leaves a manifest beside it naming
// a digest of its inputs - its Dockerfile's build arguments, which are versions.yaml's pins, and
// every file under the paths it is built from - and a part whose inputs still have that digest,
// and whose output still verifies, is left as it is. The manifest is a file under _output, so a
// build can be run again, alone, or on another machine with the same _output.
package build

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spin-stack/go-tools/versions"
)

// Arch is the one architecture this machine is built for: every build is --platform linux/amd64.
const Arch = "x86_64"

// CacheRegistry is where CI's BuildKit caches live, one tag a part: main's lanes write them, and
// pull requests, a release and compat read them. GitHub's registry and not the Actions cache,
// which the fleet's runners cannot read and which evicts a lane's layers between a merge and the
// release that needs them (10.36 GB of 10 on 2026-11-12).
const CacheRegistry = "ghcr.io/spin-stack/spin-machine/cache"

// imageBuilder is the container that builds the base image, tagged here for the run that follows.
const imageBuilder = "spin-machine/image-builder:dev"

// Options is what every part is built with.
type Options struct {
	// Root is the repository, and the build context: the Dockerfiles COPY by their path from it.
	Root string
	// Out is where the parts land, absolute: it is handed to docker -v and to a VM.
	Out string
	// Pins is versions.yaml.
	Pins *versions.Versions
	// Jobs is how many compile jobs a build runs.
	Jobs int
	// Registry reads and writes BuildKit's cache in CacheRegistry instead of under Root/.cache.
	Registry bool
	// Export writes the cache back. Only main's lanes need to: what a pull request, a release or
	// compat compiled nobody reads after, and exporting cost 88 s on a kernel build (2026-09-30).
	Export bool
	// SystemdExperiment is image/systemd/experiments/<name>.patch, applied to systemd's build.
	SystemdExperiment string
	// Rebuild builds a part whatever its manifest says.
	Rebuild bool
	// Stdout and Stderr are the builds' own output, for a person.
	Stdout, Stderr io.Writer
}

// Part is one thing build makes.
type Part struct {
	Name string
	// Needs is the parts this one is made with, built first.
	Needs []string
	// Paths are what in the tree this part is built from, as its inputs.
	Paths []string
	// Pins are the versions.yaml entries its Dockerfile is handed.
	Pins  []string
	make  func(ctx context.Context, o Options, p Part) error
	check func(o Options) error
}

// Parts is every part, in the order a whole build makes them.
var Parts = []Part{
	{Name: "qemu", Paths: []string{"qemu"}, Pins: qemuPins,
		make: makeQEMU, check: checkQEMU},
	{Name: "kernel", Paths: []string{"kernel"}, Pins: strings.Fields("debian debian-snapshot kernel moby-check-config"),
		make: makeKernel, check: VerifyKernel},
	{Name: "e2fsprogs", Paths: []string{"e2fsprogs"}, Pins: strings.Fields("e2fsprogs-builder e2fsprogs"),
		make: makeE2fsprogs, check: VerifyE2fsprogs},
	{Name: "systemd", Paths: []string{"image/systemd"}, Pins: strings.Fields("ubuntu systemd-source"),
		make: makeSystemd, check: verifySystemd},
	// The image is made with the e2fsprogs above and installs the systemd above, and the pressure
	// probe's guest half, compiled from this tree.
	{Name: "image", Needs: []string{"e2fsprogs", "systemd"}, Paths: []string{"image", "cmd/spin-machine-pressure", "internal/pressure", "go.mod", "go.sum"},
		Pins: strings.Fields("ubuntu mkosi task git-lfs buildkit"), make: makeImage, check: verifyImage},
}

// Names is every part's name, in Parts' order.
func Names() []string {
	names := make([]string, 0, len(Parts))
	for _, p := range Parts {
		names = append(names, p.Name)
	}
	return names
}

// Lookup is the part named name.
func Lookup(name string) (Part, error) {
	for _, p := range Parts {
		if p.Name == name {
			return p, nil
		}
	}
	return Part{}, fmt.Errorf("no part %q: the parts are %s", name, strings.Join(Names(), ", "))
}

// Plan is the parts named, and what they need, in Parts' order: every part when none is named.
func Plan(names []string) ([]Part, error) {
	if len(names) == 0 {
		return slices.Clone(Parts), nil
	}
	want := map[string]bool{}
	var add func(string) error
	add = func(name string) error {
		p, err := Lookup(name)
		if err != nil {
			return err
		}
		want[name] = true
		for _, n := range p.Needs {
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range names {
		if err := add(n); err != nil {
			return nil, err
		}
	}
	var plan []Part
	for _, p := range Parts {
		if want[p.Name] {
			plan = append(plan, p)
		}
	}
	return plan, nil
}

// Manifest is what a built part leaves beside it, under _output/manifest.
type Manifest struct {
	Part string `json:"part"`
	// Inputs is the digest of what the part was made from: Digest.
	Inputs string `json:"inputs"`
	// From is "build", or the image a fetched part was copied out of.
	From string `json:"from"`
	// Built says whether this run made it, or found it current: not stored.
	Built bool `json:"built"`
}

// ManifestPath is where part's manifest is.
func ManifestPath(out, part string) string { return filepath.Join(out, "manifest", part+".json") }

// ReadManifest is part's manifest, and false when it has none.
func ReadManifest(out, part string) (Manifest, bool, error) {
	b, err := os.ReadFile(ManifestPath(out, part))
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("reading %s's manifest: %w", part, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("reading %s's manifest %s: %w", part, ManifestPath(out, part), err)
	}
	return m, true, nil
}

// WriteManifest records m beside its part.
func WriteManifest(out string, m Manifest) error {
	m.Built = false
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s's manifest: %w", m.Part, err)
	}
	path := ManifestPath(out, m.Part)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("writing %s's manifest: %w", m.Part, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil { //nolint:gosec // a manifest, read by whoever reads _output
		return fmt.Errorf("writing %s's manifest: %w", m.Part, err)
	}
	return nil
}

// Args are the build arguments p's Dockerfile is handed from the pins, sorted, as
// `--build-arg NAME=VALUE` pairs: what `versions args` writes.
func Args(v *versions.Versions, names ...string) ([]string, error) {
	all := map[string]string{}
	for _, n := range names {
		e, err := v.Get(n)
		if err != nil {
			return nil, err
		}
		maps.Copy(all, e.Args())
	}
	var args []string
	for _, k := range slices.Sorted(maps.Keys(all)) {
		args = append(args, "--build-arg", k+"="+all[k])
	}
	return args, nil
}

// Digest is a digest of p's inputs: its build arguments, extra, and every file under its paths
// by name, mode and content. A file that moves, changes or appears changes it; nothing else does.
func Digest(root string, args []string, paths []string, extra ...string) (string, error) {
	h := sha256.New()
	for _, a := range append(slices.Clone(args), extra...) {
		fmt.Fprintf(h, "arg %q\n", a)
	}
	for _, p := range paths {
		err := filepath.WalkDir(filepath.Join(root, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "file %q %o\n", filepath.ToSlash(rel), info.Mode())
			if info.Mode()&fs.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "-> %q\n", target)
				return nil
			}
			f, err := os.Open(path) //nolint:gosec // a file of this tree
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(h, f)
			return err
		})
		if err != nil {
			return "", fmt.Errorf("reading the inputs under %s: %w", p, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inputs is p's digest, with the digests of the parts it needs folded in: an image made with
// another e2fsprogs is another image.
func inputs(o Options, p Part) (string, error) {
	args, err := partArgs(o, p)
	if err != nil {
		return "", err
	}
	var extra []string
	for _, n := range p.Needs {
		m, ok, err := ReadManifest(o.Out, n)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%s is made with %s, which has no manifest in %s", p.Name, n, o.Out)
		}
		extra = append(extra, n+"="+m.Inputs)
	}
	return Digest(o.Root, args, p.Paths, extra...)
}

// partArgs is everything p's Dockerfile is handed, pins and the rest.
func partArgs(o Options, p Part) ([]string, error) {
	args, err := Args(o.Pins, p.Pins...)
	if err != nil {
		return nil, err
	}
	switch p.Name {
	case "kernel":
		series, err := KernelSeries(o.Pins)
		if err != nil {
			return nil, err
		}
		args = append(args, "--build-arg", "KERNEL_SERIES="+series, "--build-arg", "KERNEL_ARCH="+Arch)
	case "systemd":
		args = append(args, "--build-arg", "SYSTEMD_EXPERIMENT="+o.SystemdExperiment)
	}
	return args, nil
}

// KernelSeries is the pinned kernel's major.minor, which names its configuration: every release of
// a series builds from one file, so a stable release is a versions.yaml line and nothing else.
func KernelSeries(v *versions.Versions) (string, error) {
	e, err := v.Get("kernel")
	if err != nil {
		return "", err
	}
	major, rest, _ := strings.Cut(e.Version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	if major == "" || minor == "" {
		return "", fmt.Errorf("the kernel's version %q has no major.minor", e.Version)
	}
	return major + "." + minor, nil
}

// Result is what a build did with one part.
type Result = Manifest

// Make builds plan's parts that are missing or stale, in order, and verifies each one.
func Make(ctx context.Context, o Options, plan []Part) ([]Result, error) {
	var results []Result
	for _, p := range plan {
		r, err := makeOne(ctx, o, p)
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}

func makeOne(ctx context.Context, o Options, p Part) (Result, error) {
	digest, err := inputs(o, p)
	if err != nil {
		return Result{}, err
	}
	if !o.Rebuild {
		m, ok, err := ReadManifest(o.Out, p.Name)
		if err != nil {
			return Result{}, err
		}
		// Current only when what is there is still whole: a manifest outlives a file removed by hand.
		if ok && m.Inputs == digest && m.From == "build" && p.check(o) == nil {
			say(o.Stderr, "%s: current (%s…)", p.Name, digest[:12])
			return m, nil
		}
	}
	say(o.Stderr, "%s: building", p.Name)
	if err := p.make(ctx, o, p); err != nil {
		return Result{}, fmt.Errorf("building %s: %w", p.Name, err)
	}
	if err := p.check(o); err != nil {
		return Result{}, fmt.Errorf("verifying %s: %w", p.Name, err)
	}
	m := Manifest{Part: p.Name, Inputs: digest, From: "build"}
	if err := WriteManifest(o.Out, m); err != nil {
		return Result{}, err
	}
	m.Built = true
	return m, nil
}

// Verify asserts what is in o.Out for each part of plan, without building anything.
func Verify(o Options, plan []Part) error {
	var errs []error
	for _, p := range plan {
		if err := p.check(o); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		say(o.Stderr, "OK: %s", p.Name)
	}
	return errors.Join(errs...)
}

// cacheFlags are a part's BuildKit cache, read and, when o.Export, written.
func cacheFlags(o Options, part string) []string {
	from := "type=local,src=" + filepath.Join(o.Root, ".cache", "buildkit", part)
	to := "type=local,dest=" + filepath.Join(o.Root, ".cache", "buildkit", part) + ",mode=max,compression=zstd"
	if o.Registry {
		from = "type=registry,ref=" + CacheRegistry + ":" + part
		to = "type=registry,ref=" + CacheRegistry + ":" + part + ",mode=max,image-manifest=true,oci-mediatypes=true"
	}
	flags := []string{"--cache-from", from}
	if o.Export {
		flags = append(flags, "--cache-to", to)
	}
	return flags
}

// buildx is a `docker buildx build` of dockerfile's target, linux/amd64, with part's cache and
// args, from the repository root; output is where it goes (--output type=local,dest=... or
// --load and a tag).
func buildx(o Options, part, dockerfile, target string, args []string, output ...string) []string {
	cmd := []string{"buildx", "build", "--file", dockerfile}
	if target != "" {
		cmd = append(cmd, "--target", target)
	}
	cmd = append(cmd, "--platform", "linux/amd64")
	cmd = append(cmd, cacheFlags(o, part)...)
	cmd = append(cmd, args...)
	cmd = append(cmd, output...)
	return append(cmd, ".")
}

// emptied removes what a part writes before it writes it again: `--output type=local` writes files
// and removes none, so a binary or a firmware blob a build stopped producing would stay on disk,
// indistinguishable from one it does produce. That is how a 331 MB firmware tree outlived the
// build that stopped shipping it.
func emptied(out string, globs ...string) error {
	for _, g := range globs {
		found, err := filepath.Glob(filepath.Join(out, g))
		if err != nil {
			return fmt.Errorf("emptying %s: %w", g, err)
		}
		for _, f := range found {
			if err := os.RemoveAll(f); err != nil {
				return fmt.Errorf("emptying %s: %w", f, err)
			}
		}
	}
	return os.MkdirAll(out, 0o755)
}

func makeQEMU(ctx context.Context, o Options, p Part) error {
	if err := emptied(o.Out, "bin/qemu-*", "qemu", "sbom/qemu.apk"); err != nil {
		return err
	}
	args, err := partArgs(o, p)
	if err != nil {
		return err
	}
	args = append(args, "--build-arg", fmt.Sprintf("JOBS=%d", o.Jobs))
	return docker(ctx, o, buildx(o, "qemu", "qemu/Dockerfile", "extract", args, "--output", "type=local,dest="+o.Out)...)
}

func makeKernel(ctx context.Context, o Options, p Part) error {
	if err := emptied(o.Out, "kernel"); err != nil {
		return err
	}
	args, err := partArgs(o, p)
	if err != nil {
		return err
	}
	args = append(args, "--build-arg", fmt.Sprintf("KERNEL_NPROC=%d", o.Jobs))
	return docker(ctx, o, buildx(o, "kernel", "kernel/Dockerfile", "kernel", args, "--output", "type=local,dest="+o.Out)...)
}

func makeE2fsprogs(ctx context.Context, o Options, p Part) error {
	if err := emptied(o.Out, "bin/mkfs.ext4", "bin/e2fsck", "bin/dumpe2fs", "bin/debugfs", "e2fsprogs", "sbom/e2fsprogs.apk"); err != nil {
		return err
	}
	args, err := partArgs(o, p)
	if err != nil {
		return err
	}
	return docker(ctx, o, buildx(o, "e2fsprogs", "e2fsprogs/Dockerfile", "extract", args, "--output", "type=local,dest="+o.Out)...)
}

func makeSystemd(ctx context.Context, o Options, p Part) error {
	dest := filepath.Join(o.Out, "systemd")
	if err := emptied(o.Out, "systemd"); err != nil {
		return err
	}
	args, err := partArgs(o, p)
	if err != nil {
		return err
	}
	args = append(args, "--build-arg", fmt.Sprintf("JOBS=%d", o.Jobs))
	return docker(ctx, o, buildx(o, "systemd", "image/systemd/Dockerfile", "debs", args, "--output", "type=local,dest="+dest)...)
}

// makeImage builds the container that builds the image, then runs it. `docker run` and not
// `docker buildx build`, as image/Dockerfile says: mkosi assembles a root filesystem by unsharing a
// user and a mount namespace, which BuildKit forbids. The flags are what it needs and nothing more -
// not --privileged.
func makeImage(ctx context.Context, o Options, p Part) error {
	args, err := partArgs(o, p)
	if err != nil {
		return err
	}
	if err := buildPressure(ctx, o); err != nil {
		return err
	}
	if err := docker(ctx, o, buildx(o, "image", "image/Dockerfile", "", args, "--tag", imageBuilder, "--load")...); err != nil {
		return err
	}
	// mkosi's package cache, which is not BuildKit's: a rebuild of the image is ~1.5 GB of apt.
	cache := filepath.Join(o.Root, ".cache", "mkosi")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return fmt.Errorf("making mkosi's cache: %w", err)
	}
	return docker(ctx, o, "run", "--rm",
		"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--cap-add", "SYS_ADMIN",
		"-v", o.Out+":/out", "-v", cache+":/cache", "-v", filepath.Join(o.Out, "systemd")+":/debs:ro",
		imageBuilder)
}

// docker runs docker with args in the repository root.
func docker(ctx context.Context, o Options, args ...string) error {
	return run(ctx, o, "docker", args...)
}

// run runs name in the repository root with the build's output.
func run(ctx context.Context, o Options, name string, args ...string) error {
	return runEnv(ctx, o, nil, name, args...)
}

// runEnv is run with env added to this process's environment.
func runEnv(ctx context.Context, o Options, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = o.Root
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout, cmd.Stderr = cmp.Or[io.Writer](o.Stdout, io.Discard), cmp.Or[io.Writer](o.Stderr, io.Discard)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args[:min(len(args), 2)], " "), err)
	}
	return nil
}

func say(w io.Writer, format string, a ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format+"\n", a...)
	}
}
