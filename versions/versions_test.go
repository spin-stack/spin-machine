// SPDX-License-Identifier: Apache-2.0

package versions

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// root is the repository's root: a test runs in its package's directory.
const root = ".."

// The repository's own file reads, and a bump that changes nothing writes it back byte for byte:
// every comment, and every other entry, is as it was.
func TestTheFileReadsAndRewritesItselfUnchanged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		t.Fatal(err)
	}
	v, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range v.Entries {
		out, err := v.Set(e.Name, e.Version, e.Pin)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if string(out) != string(raw) {
			t.Fatalf("rewriting %s as it is changed the file:\n%s", e.Name, out)
		}
	}
}

// A bump changes its entry and nothing else, and refuses a pin that is not its kind's.
func TestABumpChangesItsEntryAlone(t *testing.T) {
	raw := []byte(`# pins
- name: qemu
  kind: download
  source: https://download.qemu.org/qemu-{version}.tar.xz
  version: 11.1.1
  pin: 079ffbff8a7111bbc89022107cbabf3bbfd614d5fc9d7cc675991196aca12482
  track: tags https://gitlab.com/qemu-project/qemu.git
  # keep me
- name: alpine
  kind: image
  source: alpine
  version: '3.24'
  pin: sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
  track: digest
`)
	for _, tc := range []struct {
		name, entry, version, pin string
		// want is the file the bump writes, "" for one it refuses.
		want string
	}{
		{name: "a download pin that is not a sha256", entry: "qemu", version: "11.1.2", pin: strings.Repeat("a", 40)},
		{name: "an image pin that is not a digest", entry: "alpine", version: "3.24", pin: strings.Repeat("a", 64)},
		{name: "an entry the file does not have", entry: "kernel", version: "7.2.2", pin: strings.Repeat("a", 64)},
		{name: "a sha256", entry: "qemu", version: "11.1.2", pin: strings.Repeat("a", 64),
			want: strings.NewReplacer("11.1.1", "11.1.2", "079ffbff8a7111bbc89022107cbabf3bbfd614d5fc9d7cc675991196aca12482", strings.Repeat("a", 64)).Replace(string(raw))},
		{name: "a digest, quoted", entry: "alpine", version: "3.24", pin: "sha256:" + strings.Repeat("b", 64),
			want: strings.NewReplacer("294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6", strings.Repeat("b", 64)).Replace(string(raw))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			out, err := v.Set(tc.entry, tc.version, tc.pin)
			switch {
			case tc.want == "" && err == nil:
				t.Errorf("the bump was taken, and wrote\n%s", out)
			case tc.want != "" && err != nil:
				t.Errorf("the bump was refused: %v", err)
			case tc.want != "" && string(out) != tc.want:
				t.Errorf("the bump wrote\n%s\nwant\n%s", out, tc.want)
			}
		})
	}
}

// Each kind is handed to a Dockerfile under the names it declares.
func TestEachKindIsItsBuildArguments(t *testing.T) {
	for _, tc := range []struct {
		e    Entry
		want map[string]string
	}{
		{Entry{Name: "debian", Kind: Image, Source: "debian", Version: "trixie", Pin: "sha256:x"}, map[string]string{"DEBIAN_IMAGE": "debian:trixie@sha256:x"}},
		{Entry{Name: "qboot", Kind: Git, Version: "master", Pin: "c", Archive: "a"},
			map[string]string{"QBOOT_VERSION": "master", "QBOOT_COMMIT": "c", "QBOOT_ARCHIVE_SHA256": "a"}},
		{Entry{Name: "kernel", Kind: Download, Version: "7.2.1", Pin: "s"}, map[string]string{"KERNEL_VERSION": "7.2.1", "KERNEL_SHA256": "s"}},
		{Entry{Name: "debian-snapshot", Kind: Date, Version: "20260914T000000Z"}, map[string]string{"DEBIAN_SNAPSHOT": "20260914T000000Z"}},
	} {
		got := tc.e.Args()
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.e.Name, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s=%q, want %q", tc.e.Name, k, got[k], v)
			}
		}
	}
}

// A download's URL is its source with the version, and the version's major number, put in.
func TestADownloadIsFetchedFromItsVersionsURL(t *testing.T) {
	e := Entry{Name: "kernel", Kind: Download, Version: "7.2.1", Source: "https://cdn.kernel.org/pub/linux/kernel/v{major}.x/linux-{version}.tar.xz"}
	if got, want := e.URL(), "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-7.2.1.tar.xz"; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// A value is replaced in the style it is written in, and whatever else is on its line - quotes, a
// comment, the rest of a flow mapping - is as it was.
func TestABumpKeepsHowTheValueIsWritten(t *testing.T) {
	pin := strings.Repeat("b", 64)
	old := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, raw, want string
	}{
		{
			name: "double quotes",
			raw:  "- name: a\n  kind: download\n  source: https://x/{version}\n  version: \"1\"\n  pin: \"" + old + "\"\n  track: tags https://x\n",
			want: "- name: a\n  kind: download\n  source: https://x/{version}\n  version: \"2\"\n  pin: \"" + pin + "\"\n  track: tags https://x\n",
		},
		{
			name: "single quotes",
			raw:  "- name: a\n  kind: download\n  source: https://x/{version}\n  version: '1'\n  pin: " + old + "\n  track: tags https://x\n",
			want: "- name: a\n  kind: download\n  source: https://x/{version}\n  version: '2'\n  pin: " + pin + "\n  track: tags https://x\n",
		},
		{
			name: "a comment after the value",
			raw:  "- name: a\n  kind: download\n  source: https://x/{version}\n  version: v1 # the LTS\n  pin: " + old + "\n  track: tags https://x\n",
			want: "- name: a\n  kind: download\n  source: https://x/{version}\n  version: 2 # the LTS\n  pin: " + pin + "\n  track: tags https://x\n",
		},
		{
			name: "a flow mapping",
			raw:  "- {name: a, kind: download, source: 'https://x/{version}', version: v1, pin: " + old + ", track: tags https://x}\n",
			want: "- {name: a, kind: download, source: 'https://x/{version}', version: 2, pin: " + pin + ", track: tags https://x}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			out, err := v.Set("a", "2", pin)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("the bump wrote\n%s\nwant\n%s", out, tc.want)
			}
		})
	}
}

// A bump pins the version it is given, or the newest its track finds, and answers the entry as the
// file it answers has it; the entry v holds is left as it was.
func TestABumpIsToTheVersionGivenOrTheNewest(t *testing.T) {
	raw := []byte("- name: debian-snapshot\n  kind: date\n  version: 20260101T000000Z\n  track: today\n")
	today := time.Now().UTC().Format("20060102") + "T000000Z"
	for _, tc := range []struct{ given, want string }{
		{"20260202T000000Z", "20260202T000000Z"},
		{"", today},
	} {
		v, err := parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		e, out, err := v.Bump(t.Context(), "debian-snapshot", tc.given)
		if err != nil {
			t.Fatal(err)
		}
		if e.Version != tc.want || !strings.Contains(string(out), "version: "+tc.want+"\n") {
			t.Errorf("bumped to %q: %+v\n%s", tc.given, e, out)
		}
		if was, _ := v.Get("debian-snapshot"); was.Version != "20260101T000000Z" {
			t.Errorf("v changed: %+v", was)
		}
	}
}

// An entry whose archive only a build can compute is not bumped, since the new commit with the
// old archive's sum is a build that fails on the checksum.
func TestAnEntryWithAnArchiveIsNotBumped(t *testing.T) {
	raw := []byte("- name: qboot\n  kind: git\n  source: https://x\n  version: master\n  pin: " + strings.Repeat("a", 40) +
		"\n  archive: " + strings.Repeat("b", 64) + "\n  track: branch\n")
	v, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, out, err := v.Bump(t.Context(), "qboot", ""); err == nil {
		t.Errorf("the bump was taken, and wrote\n%s", out)
	}
}

// Only a release tag is a version, and 11.10 comes after 11.9.
func TestTheNewestTagIsTheHighestRelease(t *testing.T) {
	for tag, want := range map[string]bool{
		"v11.1.1": true, "1.47.4": true, "v27": true,
		"v11.2.0-rc0": false, "v1.47.4-WIP": false, "stable-2.12": false,
	} {
		if got := release.MatchString(tag); got != want {
			t.Errorf("%s: a release %v, want %v", tag, got, want)
		}
	}
	a, _ := numbers("11.10")
	b, _ := numbers("11.9.9")
	if slices.Compare(a, b) <= 0 {
		t.Error("11.10 is not after 11.9.9")
	}
}
