// SPDX-License-Identifier: Apache-2.0

// Package versions is versions.yaml: every input the repository pins from outside it, in one
// file, read by everything that uses one and rewritten one entry at a time by a bump.
//
// It is a build tool and nothing else. The machine package does not import it and no release
// carries it: a pin decides what is built, and what was built is decided by the fingerprint.
package versions

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// File is versions.yaml's name at the repository's root.
const File = "versions.yaml"

// Kind is what sort of input an entry is, which says what its pin is.
type Kind string

// Kinds of input.
const (
	Image    Kind = "image"
	Git      Kind = "git"
	Download Kind = "download"
	Date     Kind = "date"
)

// Entry is one pinned input.
type Entry struct {
	Name    string `yaml:"name"`
	Kind    Kind   `yaml:"kind"`
	Source  string `yaml:"source,omitempty"`
	Version string `yaml:"version"`
	Pin     string `yaml:"pin,omitempty"`
	// Archive is a git entry's second check: the sha256 of what `git archive` writes for the
	// commit, for a build that compiles those bytes and has to know they are the ones that
	// were measured. Only a build can compute it, since it depends on the git that writes it.
	Archive string `yaml:"archive,omitempty"`
	Track   string `yaml:"track"`
	Note    string `yaml:"note,omitempty"`
}

var (
	name   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sum    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate is the entry whole and its pin in its kind's form.
func (e Entry) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf("versions: %s: "+format, append([]any{e.Name}, args...)...))
		}
	}
	check(name.MatchString(e.Name), "not a name: lowercase, digits and -")
	check(e.Version != "", "no version")
	check(e.Track != "", "no track: how check finds the newest")
	check(e.Archive == "" || e.Kind == Git, "only a git entry has an archive")
	switch e.Kind {
	case Image:
		check(e.Source != "", "an image with no repository")
		check(digest.MatchString(e.Pin), "the pin %q is not a sha256: digest", e.Pin)
	case Git:
		check(strings.HasPrefix(e.Source, "https://"), "a repository that is not an https URL")
		check(commit.MatchString(e.Pin), "the pin %q is not a commit", e.Pin)
		check(e.Archive == "" || sum.MatchString(e.Archive), "the archive %q is not a sha256", e.Archive)
	case Download:
		check(strings.HasPrefix(e.Source, "https://") && strings.Contains(e.Source, "{version}"),
			"a download whose source is not an https URL with {version} in it")
		check(sum.MatchString(e.Pin), "the pin %q is not a sha256", e.Pin)
	case Date:
		check(e.Pin == "", "a date has no pin")
	default:
		check(false, "kind %q is none of image, git, download, date", e.Kind)
	}
	return errors.Join(errs...)
}

// Ref is an image's reference, by its digest: what `docker run` and FROM are given.
func (e Entry) Ref() string { return e.Source + ":" + e.Version + "@" + e.Pin }

// URL is a download's, for its version.
func (e Entry) URL() string {
	major, _, _ := strings.Cut(e.Version, ".")
	return strings.NewReplacer("{version}", e.Version, "{major}", major).Replace(e.Source)
}

// Args are the build arguments a Dockerfile takes the entry as, each named for it: debian
// becomes DEBIAN_IMAGE, kernel KERNEL_VERSION and KERNEL_SHA256, qboot QBOOT_VERSION,
// QBOOT_COMMIT and QBOOT_ARCHIVE_SHA256.
func (e Entry) Args() map[string]string {
	prefix := strings.ToUpper(strings.ReplaceAll(e.Name, "-", "_"))
	switch e.Kind {
	case Image:
		return map[string]string{prefix + "_IMAGE": e.Ref()}
	case Git:
		args := map[string]string{prefix + "_VERSION": e.Version, prefix + "_COMMIT": e.Pin}
		if e.Archive != "" {
			args[prefix+"_ARCHIVE_SHA256"] = e.Archive
		}
		return args
	case Download:
		return map[string]string{prefix + "_VERSION": e.Version, prefix + "_SHA256": e.Pin}
	default:
		return map[string]string{prefix: e.Version}
	}
}

// Versions is the file, in order.
type Versions struct {
	Entries []Entry
	doc     yaml.Node
	raw     []byte
}

// Load reads and validates path.
func Load(path string) (*Versions, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	return parse(raw)
}

// parse reads and validates raw: every entry whole, and no name twice.
func parse(raw []byte) (*Versions, error) {
	v := &Versions{raw: raw}
	if err := yaml.Unmarshal(raw, &v.doc); err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&v.Entries); err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	seen := map[string]bool{}
	var errs []error
	for _, e := range v.Entries {
		if seen[e.Name] {
			errs = append(errs, fmt.Errorf("versions: %s is there twice", e.Name))
		}
		seen[e.Name] = true
		errs = append(errs, e.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return v, nil
}

// Get is the entry named name.
func (v *Versions) Get(name string) (Entry, error) {
	for _, e := range v.Entries {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("versions: there is no %s in %s", name, File)
}

// Set answers the file with name's version and pin changed: those two values are replaced where
// they are written, in the style they are written in, and every other byte - comments, notes,
// blank lines, quotes - is as it was. v is left as it is; the file Set answers is what a Load of
// it would read, and a value Set cannot write in place is refused rather than written otherwise.
func (v *Versions) Set(name, version, pin string) ([]byte, error) {
	e, err := v.Get(name)
	if err != nil {
		return nil, err
	}
	e.Version, e.Pin = version, pin
	if err := e.Validate(); err != nil {
		return nil, err
	}
	lines := strings.SplitAfter(string(v.raw), "\n")
	type edit struct {
		line, start, end int
		text             string
	}
	var edits []edit
	for _, item := range v.doc.Content[0].Content {
		if n := field(item, "name"); n == nil || n.Value != name {
			continue
		}
		for _, kv := range [][2]string{{"version", version}, {"pin", pin}} {
			n := field(item, kv[0])
			if n == nil {
				continue
			}
			start, end, err := span(lines[n.Line-1], n)
			if err != nil {
				return nil, fmt.Errorf("versions: %s's %s: %w", name, kv[0], err)
			}
			edits = append(edits, edit{n.Line - 1, start, end, render(n.Style, kv[1])})
		}
	}
	// Right to left, so an edit does not move the next one on a line that has both.
	slices.SortFunc(edits, func(a, b edit) int { return cmp.Or(cmp.Compare(a.line, b.line), cmp.Compare(b.start, a.start)) })
	for _, ed := range edits {
		l := lines[ed.line]
		lines[ed.line] = l[:ed.start] + ed.text + l[ed.end:]
	}
	out := []byte(strings.Join(lines, ""))
	// Read back: what the file now says is what was asked, of this entry and of every other.
	reread, err := parse(out)
	if err != nil {
		return nil, err
	}
	want := slices.Clone(v.Entries)
	for i := range want {
		if want[i].Name == name {
			want[i] = e
		}
	}
	if !slices.Equal(reread.Entries, want) {
		return nil, fmt.Errorf("versions: %s could not be rewritten in place", name)
	}
	return out, nil
}

// span is where the scalar n is written in line, its quotes included: n.Column is where it starts,
// counted in characters. A scalar that goes on past its line is not one Set rewrites.
func span(line string, n *yaml.Node) (int, int, error) {
	start := 0
	for range n.Column - 1 {
		if start >= len(line) {
			return 0, 0, errors.New("not where the file says it is")
		}
		_, size := utf8.DecodeRuneInString(line[start:])
		start += size
	}
	rest := line[start:]
	switch n.Style {
	case 0:
		if strings.HasPrefix(rest, n.Value) {
			return start, start + len(n.Value), nil
		}
	case yaml.SingleQuotedStyle:
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\'' {
				if i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				return start, start + i + 1, nil
			}
		}
	case yaml.DoubleQuotedStyle:
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				return start, start + i + 1, nil
			}
		}
	}
	return 0, 0, errors.New("not written on one line as a plain or quoted value")
}

// render is value written in style: quoted as the old value was, or plain.
func render(style yaml.Style, value string) string {
	switch style {
	case yaml.SingleQuotedStyle:
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	case yaml.DoubleQuotedStyle:
		return strconv.Quote(value)
	}
	return value
}

// field is key's value in the mapping m, nil when it has none.
func field(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
