// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const rel = "v20261014.01"

var (
	usn    = finding{"USN-8927-1", "curl@8.18.0-1ubuntu2.7", "curl vulnerabilities"}
	curl1  = finding{"UBUNTU-CVE-2026-92386", "curl@8.18.0-1ubuntu2.7", ""}
	jq1    = finding{"UBUNTU-CVE-2026-40612", "jq@1.8.1-4ubuntu2", ""}
	legacy = issue{Number: 194, Title: "osv: USN-8927-1 in curl@8.18.0-1ubuntu2.7", State: "OPEN"}
)

// verbs is a plan as "verb #number title", the part of an action a reader checks first.
func verbs(as []action) []string {
	var out []string
	for _, a := range as {
		out = append(out, strings.TrimSpace(a.describe()))
	}
	return out
}

func TestAPackageHasOneIssueThatFollowsItsFindings(t *testing.T) {
	both := body(rel, []finding{usn, curl1})
	for _, tc := range []struct {
		name     string
		findings []finding
		issues   []issue
		want     []string
	}{
		{"a package with findings and no issue gets one", []finding{usn, curl1, jq1}, nil,
			[]string{`create "osv: curl"`, `create "osv: jq"`}},
		{"an open issue that lists the findings is left", []finding{usn, curl1},
			[]issue{{Number: 3, Title: "osv: curl", State: "OPEN", Body: both}}, nil},
		{"an open issue is rewritten when the findings change", []finding{usn},
			[]issue{{Number: 3, Title: "osv: curl", State: "OPEN", Body: both}},
			[]string{`edit #3 "osv: curl"`}},
		{"an open issue whose package has no finding is closed", []finding{jq1},
			[]issue{{Number: 3, Title: "osv: curl", State: "OPEN", Body: both}},
			[]string{`create "osv: jq"`, `close #3 "osv: curl"`}},
		{"a closed issue stays closed while it listed every finding", []finding{usn},
			[]issue{{Number: 3, Title: "osv: curl", State: "CLOSED", Body: both}}, nil},
		{"a finding a closed issue did not list reopens it", []finding{usn, curl1},
			[]issue{{Number: 3, Title: "osv: curl", State: "CLOSED", Body: body(rel, []finding{usn})}},
			[]string{`reopen #3 "osv: curl"`, `edit #3 "osv: curl"`}},
		{"the newest issue of a title is the package's", []finding{usn, curl1},
			[]issue{{Number: 9, Title: "osv: curl", State: "OPEN", Body: both}, {Number: 3, Title: "osv: curl", State: "CLOSED"}}, nil},
		{"the newest issue of a title is the package's, listed after an older one", []finding{usn, curl1},
			[]issue{{Number: 3, Title: "osv: curl", State: "CLOSED"}, {Number: 9, Title: "osv: curl", State: "OPEN", Body: both}}, nil},
		{"an open issue of one word that is not osv's is left", nil,
			[]issue{{Number: 120, Title: "flaky", State: "OPEN"}}, nil},
		{"an issue of one finding, as osv.yml opened them, is neither the package's nor closed", nil,
			[]issue{legacy}, nil},
		{"an issue that is not osv's is left", nil,
			[]issue{{Number: 113, Title: "Checkpoints resume across machine releases", State: "OPEN"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verbs(plan(rel, tc.findings, tc.issues)); !slices.Equal(got, tc.want) {
				t.Errorf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnIssueListsEachFindingByTheLinkItIsReadBackBy(t *testing.T) {
	curl2 := finding{"UBUNTU-CVE-2026-92387", "curl@8.18.0-1ubuntu2.7", ""}
	b := body(rel, []finding{usn, curl1, curl2})
	for _, want := range []string{rel, "`curl@8.18.0-1ubuntu2.7`", "[USN-8927-1](https://osv.dev/vulnerability/USN-8927-1): curl vulnerabilities\n"} {
		if !strings.Contains(b, want) {
			t.Errorf("body does not say %q:\n%s", want, b)
		}
	}
	if got := (issue{Body: b}).ids(); !slices.Equal(got, []string{usn.id, curl1.id, curl2.id}) {
		t.Errorf("ids read back = %q, want the three written", got)
	}
}

func TestFindingsAreReadAsTaskOSVWritesThem(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []finding
		err      bool
	}{
		{"id, package and summary", "USN-8927-1\tcurl@8.18.0-1ubuntu2.7\tcurl vulnerabilities\n", []finding{usn}, false},
		{"no summary", "UBUNTU-CVE-2026-92386\tcurl@8.18.0-1ubuntu2.7\n", []finding{curl1}, false},
		{"a summary is the rest of the line", "USN-8927-1\tcurl@8.18.0-1ubuntu2.7\tcurl\tvulnerabilities\n",
			[]finding{{usn.id, usn.pkg, "curl\tvulnerabilities"}}, false},
		{"an empty line is nothing", "\n", nil, false},
		{"a package without its version", "USN-8927-1\tcurl\tcurl vulnerabilities\n", nil, true},
		{"no id", "\tcurl@8.18.0-1ubuntu2.7\tcurl vulnerabilities\n", nil, true},
		{"one column", "USN-8927-1\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFindings(strings.NewReader(tc.in))
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want an error: %v", err, tc.err)
			}
			if err != nil && !strings.Contains(err.Error(), "line 1:") {
				t.Errorf("err = %v, want it to name line 1", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("findings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRunTakesThePlanThroughGH(t *testing.T) {
	path := filepath.Join(t.TempDir(), "osv.tsv")
	if err := os.WriteFile(path, []byte("USN-8927-1\tcurl@8.18.0-1ubuntu2.7\tcurl vulnerabilities\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	existing, err := json.Marshal([]issue{{Number: 7, Title: "osv: jq", State: "OPEN"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"acts", []string{"-release", rel, "-findings", path},
			[]string{"issue list", "issue create --title osv: curl", "issue close 7"}},
		{"with -n only reads", []string{"-n", "-release", rel, "-findings", path},
			[]string{"issue list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			cli := func(args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				if args[1] == "list" {
					return existing, nil
				}
				return nil, nil
			}
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, cli, &stdout, &stderr); code != 0 {
				t.Fatalf("run = %d: %s", code, stderr.String())
			}
			if len(calls) != len(tc.want) {
				t.Fatalf("gh was called %d times, want %d: %q", len(calls), len(tc.want), calls)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(calls[i], w) {
					t.Errorf("call %d = %q, want it to start %q", i, calls[i], w)
				}
			}
		})
	}
}

func TestRunFailsWhenItCannotReadOrAct(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "osv.tsv")
	bad := filepath.Join(dir, "bad.tsv")
	if err := os.WriteFile(good, []byte("USN-8927-1\tcurl@8.18.0-1ubuntu2.7\tcurl vulnerabilities\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("USN-8927-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fails := errors.New("gh failed")
	for _, tc := range []struct {
		name     string
		findings string
		list     []byte
		listErr  error
		actErr   error
	}{
		{"no findings file", filepath.Join(dir, "missing"), []byte("[]"), nil, nil},
		{"findings it cannot read", bad, []byte("[]"), nil, nil},
		{"gh cannot list", good, nil, fails, nil},
		{"gh lists something that is not issues", good, []byte("not json"), nil, nil},
		{"gh cannot act", good, []byte("[]"), nil, fails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := func(args ...string) ([]byte, error) {
				if args[1] == "list" {
					return tc.list, tc.listErr
				}
				return nil, tc.actErr
			}
			var stderr bytes.Buffer
			if code := run([]string{"-release", rel, "-findings", tc.findings}, cli, &bytes.Buffer{}, &stderr); code != 1 {
				t.Errorf("run = %d, want 1: %s", code, stderr.String())
			}
		})
	}
}

func TestRunRefusesACommandLineWithoutItsInputs(t *testing.T) {
	for _, args := range [][]string{{"-release", rel}, {"-findings", "x"}, {"-bogus"}} {
		var stderr bytes.Buffer
		if code := run(args, func(...string) ([]byte, error) { t.Fatal("gh called"); return nil, nil }, &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("run %q = %d, want 2", args, code)
		}
	}
}
