// SPDX-License-Identifier: Apache-2.0

package versions

import (
	"os"
	"path/filepath"
	"testing"
)

// Whatever parse takes, a bump to what an entry already is writes the file back byte for byte, or
// is refused: never a file that says something else. (Set's read-back holds the rest: what it
// answers is what was asked, of every entry.)
func FuzzABumpChangesNothingElse(f *testing.F) {
	if raw, err := os.ReadFile(filepath.Join(root, File)); err == nil {
		f.Add(raw)
	}
	f.Add([]byte("- name: a\n  kind: date\n  version: \"1\"\n  track: today\n"))
	f.Add([]byte("- {name: a, kind: date, version: '1', track: today}\n"))
	f.Add([]byte("- name: a\n  kind: date\n  version: 1 # a comment\n  track: today\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		v, err := parse(raw)
		if err != nil {
			return
		}
		for _, e := range v.Entries {
			out, err := v.Set(e.Name, e.Version, e.Pin)
			if err != nil {
				continue
			}
			if string(out) != string(raw) {
				t.Fatalf("setting %s to what it is changed the file:\n%q\nto\n%q", e.Name, raw, out)
			}
		}
	})
}
