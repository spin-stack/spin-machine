// SPDX-License-Identifier: Apache-2.0

// Package versions holds this repository to its versions.yaml, which go-tools reads and moves.
package versions_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/spin-stack/go-tools/versions"
	"github.com/spin-stack/go-tools/versions/gate"
)

// root is the repository's: a test runs in its package's directory.
const root = ".."

// A version is written once: in versions.yaml. The gate is go-tools': no digest or commit
// anywhere else, no Dockerfile pin with a default of its own, no entry nothing reads.
func TestVersionsYAMLIsTheOnlyPin(t *testing.T) {
	g := gate.Gate{Root: root, Elsewhere: []string{
		// What Go writes of its own modules.
		"go.sum",
		// The qboot commit the patch was cut against and the licence it carries. A bump of qboot
		// has to rebase the patch and restate both; versions.yaml's note says so.
		"NOTICE",
		"qemu/qboot/README.md",
	}}
	if err := g.Check(); err != nil {
		t.Error(err)
	}
}

// mkosi.conf's MinimumVersion is the version image/Dockerfile installs, not a floor with room
// above it: mkosi decides what ends up in the tree. It cannot be handed in, so it is held to the
// pin here instead.
func TestMkosiConfIsThePinnedMkosi(t *testing.T) {
	v, err := versions.Load(filepath.Join(root, versions.File))
	if err != nil {
		t.Fatal(err)
	}
	e, err := v.Get("mkosi")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "image", "mkosi.conf"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^MinimumVersion=(.*)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("image/mkosi.conf has no MinimumVersion")
	}
	if string(m[1]) != e.Version {
		t.Errorf("image/mkosi.conf says MinimumVersion=%s, and %s pins mkosi %s", m[1], versions.File, e.Version)
	}
}
