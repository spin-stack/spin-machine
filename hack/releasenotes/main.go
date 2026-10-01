// SPDX-License-Identifier: Apache-2.0

// Command releasenotes says what a release changed against the one before it, in Markdown for
// the release's notes: the commits that reached the machine, grouped by the part they changed;
// the pins that moved; the kernel options that were turned on, off or changed; and the patches
// added, removed or changed. Everything is read from git at the two refs, so it says what is
// in the tree, not what somebody remembered to write down.
//
// The fingerprint verdict (hack/fingerprint-diff) and the boot report's comparison are the
// notes' other two parts; this is the one that says why they moved.
//
//	go run ./hack/releasenotes -from v20260930.02 [-to HEAD]
package main

import (
	"bufio"
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
)

func main() {
	from := flag.String("from", "", "the previous release's tag")
	to := flag.String("to", "HEAD", "the commit this release is built from")
	flag.Parse()
	if *from == "" {
		fmt.Fprintln(os.Stderr, "releasenotes: -from is required")
		os.Exit(2)
	}
	if err := write(os.Stdout, ".", *from, *to); err != nil {
		fmt.Fprintln(os.Stderr, "releasenotes:", err)
		os.Exit(1)
	}
}

// parts are what a release is made of, in the order the notes list them, each with the paths
// that build it. A commit is listed under every part it touched.
var parts = []struct {
	title string
	paths []string
}{
	{"QEMU and firmware", []string{"qemu/"}},
	{"Kernel", []string{"kernel/"}},
	{"Base image", []string{"image/", "e2fsprogs/"}},
	{"The machine's definition and the spin-machine CLI", []string{"machine/", "cmd/"}},
}

// patchDirs are where the patches applied to upstream source live; each ships in the release.
var patchDirs = []string{"qemu/patches", "qemu/qboot", "kernel/patches"}

func write(w io.Writer, repo, from, to string) error {
	g := gitAt(repo)
	commits, err := g.commits(from, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "## What changed since %s\n\n", from)

	var outside []commit
	for _, p := range parts {
		var in []commit
		for _, c := range commits {
			if c.touches(p.paths...) {
				in = append(in, c)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(w, "### %s\n\n", p.title)
		for _, c := range in {
			fmt.Fprintf(w, "- %s\n", c.subject)
		}
		fmt.Fprintln(w)
	}
	for _, c := range commits {
		inside := false
		for _, p := range parts {
			inside = inside || c.touches(p.paths...)
		}
		if !inside && !c.only("versions.yaml") {
			outside = append(outside, c)
		}
	}
	if len(commits) == 0 {
		fmt.Fprintf(w, "Nothing: this is the tree %s was built from.\n\n", from)
	}

	if err := writeVersions(w, g, from, to); err != nil {
		return err
	}
	if err := writeKernelConfig(w, g, from, to); err != nil {
		return err
	}
	if err := writePatches(w, g, from, to); err != nil {
		return err
	}

	if len(outside) > 0 {
		fmt.Fprintf(w, "<details><summary>%d more, none of them in the machine: tests, the lab, CI, tooling</summary>\n\n", len(outside))
		for _, c := range outside {
			fmt.Fprintf(w, "- %s\n", c.subject)
		}
		fmt.Fprint(w, "\n</details>\n\n")
	}
	return nil
}

// writeVersions lists the pins of versions.yaml whose version moved, appeared or went.
func writeVersions(w io.Writer, g git, from, to string) error {
	old, err := g.versions(from)
	if err != nil {
		return err
	}
	cur, err := g.versions(to)
	if err != nil {
		return err
	}
	var lines []string
	for _, name := range sortedKeys(old, cur) {
		o, inOld := old[name]
		n, inNew := cur[name]
		switch {
		case !inOld:
			lines = append(lines, fmt.Sprintf("- `%s` added at `%s`", name, n))
		case !inNew:
			lines = append(lines, fmt.Sprintf("- `%s` removed (was `%s`)", name, o))
		case o != n:
			lines = append(lines, fmt.Sprintf("- `%s` `%s` → `%s`", name, o, n))
		}
	}
	section(w, "Pins (versions.yaml)", lines)
	return nil
}

// writeKernelConfig lists the options the kernel's configuration turned on, off or changed.
func writeKernelConfig(w io.Writer, g git, from, to string) error {
	old, err := g.kernelConfig(from)
	if err != nil {
		return err
	}
	cur, err := g.kernelConfig(to)
	if err != nil {
		return err
	}
	var lines []string
	for _, opt := range sortedKeys(old, cur) {
		o, n := cmp.Or(old[opt], "n"), cmp.Or(cur[opt], "n")
		if o != n {
			lines = append(lines, fmt.Sprintf("- `CONFIG_%s` %s → %s", opt, o, n))
		}
	}
	section(w, "Kernel configuration", lines)
	return nil
}

// writePatches lists the patches to upstream source added, removed or changed, by subject.
func writePatches(w io.Writer, g git, from, to string) error {
	var lines []string
	for _, dir := range patchDirs {
		changes, err := g.run("diff", "--name-status", from, to, "--", dir)
		if err != nil {
			return err
		}
		for line := range strings.Lines(changes) {
			status, file, ok := strings.Cut(strings.TrimSpace(line), "\t")
			if !ok || !strings.HasSuffix(file, ".patch") {
				continue
			}
			// A renamed patch reads "R100\told\tnew": the new name is the one that ships.
			if _, after, renamed := strings.Cut(file, "\t"); renamed {
				file = after
			}
			ref, verb := to, "changed"
			switch status[0] {
			case 'A':
				verb = "added"
			case 'D':
				ref, verb = from, "removed"
			}
			body, err := g.run("show", ref+":"+file)
			if err != nil {
				return err
			}
			about, ok := patchSubject(body)
			if !ok {
				// A bare diff, as qboot's are, says what it is in the commit that last changed it.
				last, err := g.run("log", "-1", "--format=%s", ref, "--", file)
				if err != nil {
					return err
				}
				about = strings.TrimSpace(last)
			}
			lines = append(lines, fmt.Sprintf("- %s `%s`: %s", verb, file, about))
		}
	}
	section(w, "Patches to upstream source", lines)
	return nil
}

func section(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "### %s\n\n%s\n\n", title, strings.Join(lines, "\n"))
}

// patchSubject is a patch's Subject header without its "[PATCH n/m]", continued lines joined;
// false for a patch without one.
func patchSubject(body string) (string, bool) {
	var subject []string
	in := false
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "Subject: "):
			subject, in = []string{strings.TrimPrefix(line, "Subject: ")}, true
		case in && strings.HasPrefix(line, " "):
			subject = append(subject, strings.TrimSpace(line))
		case in:
			s := strings.Join(subject, " ")
			if strings.HasPrefix(s, "[") {
				if _, after, ok := strings.Cut(s, "] "); ok {
					s = after
				}
			}
			return s, true
		}
	}
	return "", false
}

type commit struct {
	subject string
	files   []string
}

func (c commit) touches(prefixes ...string) bool {
	return slices.ContainsFunc(c.files, func(f string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(f, p) })
	})
}

func (c commit) only(file string) bool {
	return len(c.files) > 0 && !slices.ContainsFunc(c.files, func(f string) bool { return f != file })
}

type git struct{ dir string }

func gitAt(dir string) git { return git{dir} }

func (g git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return string(out), nil
}

// commits are the commits in to and not in from, oldest first, each with the files it changed.
func (g git) commits(from, to string) ([]commit, error) {
	out, err := g.run("log", "--reverse", "--no-merges", "--format=%x00%s", "--name-only", from+".."+to)
	if err != nil {
		return nil, err
	}
	var cs []commit
	for _, rec := range strings.Split(out, "\x00")[1:] {
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		c := commit{subject: lines[0]}
		for _, f := range lines[1:] {
			if f = strings.TrimSpace(f); f != "" {
				c.files = append(c.files, f)
			}
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// versions are versions.yaml's entries at ref, by name; none where the file is not there.
func (g git) versions(ref string) (map[string]string, error) {
	out := map[string]string{}
	body, err := g.run("show", ref+":versions.yaml")
	if err != nil {
		return out, nil //nolint:nilerr // a tree from before versions.yaml pins nothing to compare
	}
	name := ""
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "- name: "); ok {
			name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "  version: "); ok && name != "" {
			out[name] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out, sc.Err()
}

// kernelConfig is the kernel configuration the tree builds at ref, option by option without its
// CONFIG_ prefix: its value, or "n" for one written as not set.
func (g git) kernelConfig(ref string) (map[string]string, error) {
	names, err := g.run("ls-tree", "--name-only", ref, "kernel/")
	if err != nil {
		return nil, err
	}
	file := ""
	for n := range strings.Lines(names) {
		if n = strings.TrimSpace(n); strings.HasPrefix(path.Base(n), "config-") {
			file = n
		}
	}
	out := map[string]string{}
	if file == "" {
		return out, nil
	}
	body, err := g.run("show", ref+":"+file)
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if opt, ok := strings.CutPrefix(line, "# CONFIG_"); ok {
			if o, ok := strings.CutSuffix(opt, " is not set"); ok {
				out[o] = "n"
			}
		} else if opt, ok := strings.CutPrefix(line, "CONFIG_"); ok {
			if k, v, ok := strings.Cut(opt, "="); ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

func sortedKeys(maps ...map[string]string) []string {
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

