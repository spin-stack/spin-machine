// SPDX-License-Identifier: Apache-2.0

package versions

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// client gives up on a server that has not started answering in 30 s. Not a deadline on the
// whole request: a kernel tarball over a slow link takes longer than any number worth writing
// here, and a person running a bump can interrupt one that stalls mid-body.
var client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}

// Newest is the version check says e could be at: the same for an image tracked by its digest,
// or a branch, whose pin is what moves.
func Newest(ctx context.Context, e Entry) (string, error) {
	kind, arg, _ := strings.Cut(e.Track, " ")
	switch kind {
	case "digest", "branch":
		return e.Version, nil
	case "today":
		return time.Now().UTC().Format("20060102") + "T000000Z", nil
	case "kernel-stable":
		return kernelStable(ctx, e.Version)
	case "tags":
		return newestTag(ctx, arg, strings.HasPrefix(e.Version, "v"))
	}
	return "", fmt.Errorf("versions: %s tracks %q, which check does not know", e.Name, e.Track)
}

// Resolve is the pin of e at version: an image's digest, a tag's or a branch's commit, a
// download's sha256.
func Resolve(ctx context.Context, e Entry, version string) (string, error) {
	e.Version = version
	switch e.Kind {
	case Image:
		out, err := run(ctx, "docker", "buildx", "imagetools", "inspect", e.Source+":"+version, "--format", "{{.Manifest.Digest}}")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	case Git:
		out, err := run(ctx, "git", "ls-remote", e.Source,
			"refs/tags/"+version, "refs/tags/"+version+"^{}", "refs/heads/"+version)
		if err != nil {
			return "", err
		}
		// An annotated tag is its own object: the commit is the line for the tag peeled (^{}).
		pin := ""
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			sha, ref, _ := strings.Cut(sc.Text(), "\t")
			if pin == "" || strings.HasSuffix(ref, "^{}") {
				pin = sha
			}
		}
		if pin == "" {
			return "", fmt.Errorf("versions: %s has no tag or branch %s", e.Source, version)
		}
		return pin, nil
	case Download:
		return sha256Of(ctx, e.URL())
	case Date:
		return "", nil
	}
	return "", fmt.Errorf("versions: %s is a %s, which nothing resolves", e.Name, e.Kind)
}

// Bump is v's file with name pinned at version - the newest its track finds when version is "" -
// and the entry as that file has it. Nothing is written: that is the caller's.
func (v *Versions) Bump(ctx context.Context, name, version string) (Entry, []byte, error) {
	e, err := v.Get(name)
	if err != nil {
		return Entry{}, nil, err
	}
	// A new commit with the old archive's sum is a build that fails on the checksum, at best.
	if e.Archive != "" {
		return Entry{}, nil, fmt.Errorf("versions: %s's archive is what a build's git writes of the commit, so it is bumped by hand: see its note", name)
	}
	if version == "" {
		if version, err = Newest(ctx, e); err != nil {
			return Entry{}, nil, fmt.Errorf("%w; name the version", err)
		}
	}
	pin, err := Resolve(ctx, e, version)
	if err != nil {
		return Entry{}, nil, err
	}
	out, err := v.Set(name, version, pin)
	if err != nil {
		return Entry{}, nil, err
	}
	e.Version, e.Pin = version, pin
	return e, out, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("versions: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// get is url's body when the server answered 200, which the caller closes.
func get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("versions: %s answered %s", url, resp.Status)
	}
	return resp.Body, nil
}

func sha256Of(ctx context.Context, url string) (string, error) {
	body, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }() // read-only
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return "", fmt.Errorf("versions: reading %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// release is a tag that names a release and nothing else: v11.1.1, 1.47.4, v27. A -rc, a
// -pre or a WIP tag is not one, and neither is anything with a word in it.
var release = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)$`)

// newestTag is the highest release tag of repo, spelled with a v only when the pin is: QEMU's
// tags are v11.1.1 and its tarball is qemu-11.1.1.
func newestTag(ctx context.Context, repo string, withV bool) (string, error) {
	if repo == "" {
		return "", errors.New("versions: a tags track names no repository")
	}
	out, err := run(ctx, "git", "ls-remote", "--tags", "--refs", repo)
	if err != nil {
		return "", err
	}
	var newest []int
	for line := range strings.Lines(out) {
		_, ref, _ := strings.Cut(strings.TrimSpace(line), "\t")
		m := release.FindStringSubmatch(strings.TrimPrefix(ref, "refs/tags/"))
		if m == nil {
			continue
		}
		n, err := numbers(m[1])
		if err != nil {
			return "", fmt.Errorf("versions: %s's tag %s: %w", repo, ref, err)
		}
		if slices.Compare(n, newest) > 0 {
			newest = n
		}
	}
	if newest == nil {
		return "", fmt.Errorf("versions: %s has no release tag", repo)
	}
	parts := make([]string, len(newest))
	for i, n := range newest {
		parts[i] = strconv.Itoa(n)
	}
	if withV {
		return "v" + strings.Join(parts, "."), nil
	}
	return strings.Join(parts, "."), nil
}

// numbers is a dotted version as numbers, so 11.10 comes after 11.9.
func numbers(version string) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(version, ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// kernelStable is the newest release of version's series, 7.2 for 7.2.8.
func kernelStable(ctx context.Context, version string) (string, error) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("versions: %q is not a kernel release", version)
	}
	series := parts[0] + "." + parts[1] + "."
	body, err := get(ctx, "https://www.kernel.org/releases.json")
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }() // read-only
	var doc struct {
		Releases []struct {
			Version string `json:"version"`
			Moniker string `json:"moniker"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return "", fmt.Errorf("versions: reading kernel.org's releases.json: %w", err)
	}
	for _, r := range doc.Releases {
		if r.Moniker != "mainline" && strings.HasPrefix(r.Version, series) {
			return r.Version, nil
		}
	}
	return "", fmt.Errorf("versions: kernel.org lists no %sx: the series is over, and moving to another is a decision, not a bump", series)
}

// Status is one entry as check finds it.
type Status struct {
	Entry  Entry
	Newest string
	// Behind is a newer version, or the same one at another pin.
	Behind bool
	Note   string
}

// Check is where each entry stands against its upstream. An image tracked by its digest, or a
// commit tracked by its branch, is behind when the name now points at something else; a download
// only when its version moves, since its pin is what that version was published as.
func Check(ctx context.Context, v *Versions) []Status {
	var out []Status
	for _, e := range v.Entries {
		st := Status{Entry: e}
		newest, err := Newest(ctx, e)
		if err != nil {
			st.Note = err.Error()
			out = append(out, st)
			continue
		}
		st.Newest = newest
		st.Behind = newest != e.Version
		if !st.Behind && (e.Kind == Image || e.Kind == Git) {
			pin, err := Resolve(ctx, e, newest)
			switch {
			case err != nil:
				st.Note = err.Error()
			case pin != e.Pin:
				st.Behind, st.Note = true, e.Version+" is now "+pin
			}
		}
		out = append(out, st)
	}
	return out
}
