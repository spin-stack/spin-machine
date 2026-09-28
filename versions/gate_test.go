// SPDX-License-Identifier: Apache-2.0

package versions

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Where a digest or a commit may be written besides versions.yaml, because it is a record of
// something that happened rather than a pin a bump should move.
var elsewhere = []string{
	// What Go writes of its own modules.
	"go.sum",
	// The qboot commit the patch was cut against and the licence it carries. A bump of qboot
	// has to rebase the patch and restate both; versions.yaml's note says so.
	"NOTICE",
	"qemu/qboot/README.md",
	// The hashes of the artefacts a measurement was taken with.
	"qemu/qboot/measurements.json",
}

var (
	pinned = regexp.MustCompile(`\b[0-9a-f]{64}\b|\b[0-9a-f]{40}\b`)
	// A Dockerfile's build argument that is one of an entry's (Entry.Args).
	pinArg  = regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+(?:_IMAGE|_VERSION|_COMMIT|_SHA256|_SNAPSHOT))(=.*)?$`)
	usedArg = regexp.MustCompile(`(?:versions|\{\{\.VERSIONS\}\}) (?:args|env|version)((?: [a-z][a-z0-9-]*)+)`)
)

// files is every file of the repository a pin could be written in. Tests are fixtures, and
// _output and the dot-directories but .github are what a build or a tool made.
func files(t *testing.T) map[string]string {
	t.Helper()
	top, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = top.Close() }()
	out := map[string]string{}
	err = fs.WalkDir(top.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "_output" || (strings.HasPrefix(d.Name(), ".") && rel != "." && rel != ".github") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || rel == File || slices.Contains(elsewhere, rel) || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		raw, err := top.ReadFile(rel)
		if err != nil {
			return err
		}
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A version is written once: in versions.yaml. A digest or a commit anywhere else is a pin a
// bump does not move and check does not see.
func TestNoPinIsWrittenOutsideVersionsYAML(t *testing.T) {
	for rel, body := range files(t) {
		for i, line := range strings.Split(body, "\n") {
			if m := pinned.FindString(line); m != "" {
				t.Errorf("%s:%d pins %s: it belongs in %s", rel, i+1, m, File)
			}
		}
	}
}

// Every Dockerfile build argument that is a pin is one versions.yaml hands in, with no default of
// its own to fall back on; every entry is read by something, and everything read is an entry.
func TestEveryPinIsAnEntryAndEveryEntryIsUsed(t *testing.T) {
	v, err := Load(filepath.Join(root, File))
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]bool{}
	for _, e := range v.Entries {
		for k := range e.Args() {
			args[k] = true
		}
	}
	used := map[string]bool{}
	for rel, body := range files(t) {
		if filepath.Base(rel) == "Dockerfile" {
			for _, m := range pinArg.FindAllStringSubmatch(body, -1) {
				if m[2] != "" {
					t.Errorf("%s: ARG %s has a default; %s hands it in", rel, m[1], File)
				}
				if !args[m[1]] {
					t.Errorf("%s: ARG %s is no entry's: %s has nothing to hand it", rel, m[1], File)
				}
			}
		}
		for _, m := range usedArg.FindAllStringSubmatch(body, -1) {
			for _, n := range strings.Fields(m[1]) {
				used[n] = true
			}
		}
	}
	for n := range used {
		if _, err := v.Get(n); err != nil {
			t.Error(err)
		}
	}
	for _, e := range v.Entries {
		if !used[e.Name] {
			t.Errorf("%s pins %s, and nothing reads it", File, e.Name)
		}
	}
}

// mkosi.conf's MinimumVersion is the version image/Dockerfile installs, not a floor with room
// above it: mkosi decides what ends up in the tree. It cannot be handed in, so it is held to the
// pin here instead.
func TestMkosiConfIsThePinnedMkosi(t *testing.T) {
	v, err := Load(filepath.Join(root, File))
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
		t.Errorf("image/mkosi.conf says MinimumVersion=%s, and %s pins mkosi %s", m[1], File, e.Version)
	}
}
