// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PublishedQEMU is where the runtime image is published, tagged with the pinned version: asking
// for this repository's QEMU and asking for the one this commit says it runs are one question. It
// is a package name, not the repository's: the repository was renamed spin-stack/machine on
// 2026-11-21, and its images, pinned by digest, kept their names.
const PublishedQEMU = "ghcr.io/spin-stack/spin-machine/qemu"

// qemuPins are the entries qemu/Dockerfile is handed, for the build and the published image alike.
var qemuPins = strings.Fields("qemu-builder alpine qemu debian debian-snapshot qboot")

// QEMUVersion is the pinned QEMU's version.
func QEMUVersion(o Options) (string, error) {
	e, err := o.Pins.Get("qemu")
	if err != nil {
		return "", err
	}
	return e.Version, nil
}

// FetchQEMU puts the published QEMU of the pinned version in o.Out, the same tree a build writes,
// and verifies it: seconds against tens of minutes, and the same binaries, which are static.
func FetchQEMU(ctx context.Context, o Options) error {
	version, err := QEMUVersion(o)
	if err != nil {
		return err
	}
	image := PublishedQEMU + ":" + version
	if err := emptied(o.Out, "bin/qemu-*", "qemu"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.Out, "bin"), 0o755); err != nil {
		return fmt.Errorf("making %s: %w", filepath.Join(o.Out, "bin"), err)
	}
	if err := docker(ctx, o, "pull", "--quiet", "--platform", "linux/amd64", image); err != nil {
		return err
	}
	// create and cp, not `docker run cat`: the image's entrypoint is qemu-system-x86_64, and
	// nothing has to start for a file to be copied out of a container that never ran.
	var id strings.Builder
	create := o
	create.Stdout = &id
	if err := docker(ctx, create, "create", "--platform", "linux/amd64", image); err != nil {
		return err
	}
	cid := strings.TrimSpace(id.String())
	defer func() { _ = docker(context.WithoutCancel(ctx), Options{Root: o.Root}, "rm", "--force", cid) }()
	for _, b := range QEMUBinaries {
		if err := docker(ctx, o, "cp", cid+":/usr/local/bin/"+b, filepath.Join(o.Out, "bin", b)); err != nil {
			return err
		}
	}
	if err := docker(ctx, o, "cp", cid+":/usr/share/spin-stack/qemu", filepath.Join(o.Out, "qemu")); err != nil {
		return err
	}
	if err := VerifyQEMU(o); err != nil {
		return fmt.Errorf("verifying the QEMU fetched from %s: %w", image, err)
	}
	// Not "build": a fetched QEMU is the pinned version's, built from main's qemu/ and not
	// necessarily this tree's, so a build that wants this tree's builds it.
	return WriteManifest(o.Out, Manifest{Part: "qemu", From: image})
}

// PublishQEMU builds and pushes the runtime image to PublishedQEMU, tagged with the pinned
// version: what FetchQEMU pulls. It reads the builder stage the build compiled from the qemu cache and
// writes no cache: that cache holds these layers already.
func PublishQEMU(ctx context.Context, o Options) error {
	version, err := QEMUVersion(o)
	if err != nil {
		return err
	}
	args, err := Args(o.Pins, qemuPins...)
	if err != nil {
		return err
	}
	cmd := []string{"buildx", "build", "--file", "qemu/Dockerfile", "--target", "runtime", "--platform", "linux/amd64",
		"--cache-from", "type=registry,ref=" + CacheRegistry + ":qemu"}
	cmd = append(cmd, args...)
	cmd = append(cmd, "--build-arg", "JOBS=4",
		"--label", "org.opencontainers.image.source=https://github.com/"+sourceRepo(PublishedQEMU),
		"--tag", PublishedQEMU+":"+version, "--push", ".")
	return docker(ctx, o, cmd...)
}

// Tools builds spin-machine into o.Out/bin, statically. A release does not ship it: the lab and
// the probes boot with it.
func Tools(ctx context.Context, o Options) error {
	if err := os.MkdirAll(filepath.Join(o.Out, "bin"), 0o755); err != nil {
		return fmt.Errorf("making %s: %w", filepath.Join(o.Out, "bin"), err)
	}
	return runEnv(ctx, o, []string{"CGO_ENABLED=0"}, "go", "build", "-ldflags", "-s -w",
		"-o", filepath.Join(o.Out, "bin", "spin-machine"), "./cmd/spin-machine")
}

// Summary is what each built part is, by its hashes: for the run's summary in CI.
func Summary(o Options, part string) ([]string, error) {
	var lines []string
	add := func(label, path string) error {
		sum, err := FileSHA256(filepath.Join(o.Out, path))
		if err != nil {
			return err
		}
		lines = append(lines, label+" sha256: "+sum)
		return nil
	}
	var err error
	switch part {
	case "qemu":
		err = add("qemu", "bin/qemu-system-x86_64")
		if err == nil {
			var notice string
			notice, err = MachineType(filepath.Join(o.Out, "bin", "qemu-system-x86_64"))
			if notice != "" {
				lines = append(lines, notice)
			}
		}
	case "kernel":
		for _, f := range []struct{ label, path string }{{"kernel", "kernel/vmlinux"}, {"bzImage", "kernel/bzImage"}, {"config", "kernel/kernel-config"}} {
			if err = add(f.label, f.path); err != nil {
				break
			}
		}
	case "image":
		err = add("rootfs.qcow2", "image/rootfs.qcow2")
	}
	return lines, err
}

// FileSHA256 is path's sha256, in hex.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a file this build made
	if err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PressurePath is where the image's build finds the pressure probe's guest half, under o.Out. A
// release ships none of guest/: the binary is in the image.
const PressurePath = "guest/spin-machine-pressure"

// buildPressure builds the pressure probe's guest half into o.Out for image/build.sh, which
// installs it, and refuses one that asks for a dynamic loader: the guest's libc is not the one it
// would have been linked against.
func buildPressure(ctx context.Context, o Options) error {
	out := filepath.Join(o.Out, PressurePath)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("making %s: %w", filepath.Dir(out), err)
	}
	if err := runEnv(ctx, o, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, "go", "build", "-trimpath",
		"-ldflags", "-s -w", "-o", out, "./cmd/spin-machine-pressure"); err != nil {
		return err
	}
	return static(out)
}

// static is whether the ELF at path asks for no interpreter.
func static(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is linked dynamically", path)
		}
	}
	return nil
}
