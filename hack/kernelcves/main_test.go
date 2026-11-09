// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mainline is the record's semver entry for a bug introduced in intro and fixed in mainline at
// fix, with backports to the stable releases in stable (each "x.y.z", up to its series' end) - the
// shape the kernel CNA publishes (CVE-2026-99999 is one).
func mainline(intro, fix string, stable ...string) affected {
	a := affected{DefaultStatus: "affected", Versions: []rng{
		{Version: intro, Status: "affected"},
		{Version: "0", LessThan: intro, Status: "unaffected", VersionType: "semver"},
	}}
	for _, s := range stable {
		v, _ := parseVersion(s)
		a.Versions = append(a.Versions, rng{Version: s, LessThanOrEqual: v.series() + ".*", Status: "unaffected", VersionType: "semver"})
	}
	if fix != "" {
		a.Versions = append(a.Versions, rng{Version: fix, LessThanOrEqual: "*", Status: "unaffected", VersionType: "original_commit_for_fix"})
	}
	return a
}

// gitOnly is the record's git entry, which says nothing a tarball's version can be compared with.
var gitOnly = affected{DefaultStatus: "unaffected", Versions: []rng{
	{Version: "f43798c27684ab925adde7d8acc34c78c6e50df8", LessThan: "a464d950f31c89565cbaae038cc60db04fe01956", Status: "affected", VersionType: "git"},
}}

// backport is a bug a stable backport brought into 7.3.2 and 7.3.5 took out again.
var backport = affected{DefaultStatus: "unaffected", Versions: []rng{
	{Version: "7.3.2", LessThan: "7.3.5", Status: "affected", VersionType: "semver"},
}}

// exactly names one version and no range.
var exactly = affected{DefaultStatus: "unaffected", Versions: []rng{{Version: "7.3.3", Status: "affected"}}}

func rec(id string, files []string, entries ...affected) record {
	var r record
	r.Meta.ID = id
	for _, e := range entries {
		e.ProgramFiles = files
		r.Containers.CNA.Affected = append(r.Containers.CNA.Affected, e)
	}
	return r
}

func TestEachCVEIsJudgedByTheVersionAndWhatTheKernelCompiled(t *testing.T) {
	built := map[string]bool{"net/ipv4/ip_output.c": true, "include/net/ip.h": true}
	ipv4 := []string{"net/ipv4/ip_output.c"}
	bt := []string{"net/bluetooth/hci_core.c"}
	for _, tc := range []struct {
		name    string
		version string
		r       record
		verdict verdict
		fix     string
	}{
		{"mainline fixed it in the series' first release", "7.3.3", rec("A", ipv4, gitOnly, mainline("2.6.27", "7.3", "7.2.11")), fixed, ""},
		{"a stable release of the series fixed it at this version", "7.3.3", rec("A", ipv4, mainline("6.1", "7.4", "7.3.3")), fixed, ""},
		{"a stable release of the series fixed it after this version", "7.3.2", rec("A", ipv4, mainline("6.1", "7.4", "7.3.3")), applies, "7.3.3"},
		{"another series' backport does not fix this one", "7.3.3", rec("A", ipv4, mainline("6.1", "7.4", "7.2.11")), applies, "7.4"},
		{"no fix anywhere", "7.3.3", rec("A", ipv4, mainline("6.1", "")), applies, ""},
		{"the bug came after this version", "7.3.3", rec("A", ipv4, mainline("7.4", "7.5")), fixed, ""},
		{"a backport brought the bug into a range of the series", "7.3.3", rec("A", ipv4, backport), applies, ""},
		{"a version before a backported range", "7.3.1", rec("A", ipv4, backport), fixed, ""},
		{"a version at a backported range's end", "7.3.5", rec("A", ipv4, backport), fixed, ""},
		{"a version a record names exactly", "7.3.3", rec("A", ipv4, exactly), applies, ""},
		{"a version next to one a record names exactly", "7.3.4", rec("A", ipv4, exactly), fixed, ""},
		{"the first of two later releases of the series", "7.3.3", rec("A", ipv4, mainline("6.1", "7.4", "7.3.5", "7.3.4")), applies, "7.3.4"},
		{"the first of two later series", "7.3.3", rec("A", ipv4, mainline("6.1", "7.5", "7.4.2")), applies, "7.4.2"},
		{"a range that is affected is not a fix", "7.3.3", rec("A", ipv4, mainline("6.1", ""), affected{DefaultStatus: "unaffected", Versions: []rng{
			{Version: "7.4", LessThanOrEqual: "7.4.*", Status: "affected", VersionType: "semver"},
		}}), applies, ""},
		{"files this kernel did not compile", "7.3.3", rec("A", bt, mainline("6.1", "7.4")), notBuilt, ""},
		{"one compiled file of several is enough", "7.3.3", rec("A", append(bt, "include/net/ip.h"), mainline("6.1", "7.4")), applies, "7.4"},
		{"a record that names no file applies", "7.3.3", rec("A", nil, mainline("6.1", "7.4")), applies, "7.4"},
		{"a record with only git ranges says nothing of a tarball", "7.3.3", rec("A", ipv4, gitOnly), fixed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := parseVersion(tc.version)
			got := classify([]record{tc.r}, v, built)[0]
			if got.verdict != tc.verdict {
				t.Errorf("verdict %d, want %d", got.verdict, tc.verdict)
			}
			if fix := got.fix.String(); fix != tc.fix {
				t.Errorf("fix %q, want %q", fix, tc.fix)
			}
		})
	}
}

func TestAVersionIsComparedByItsNumbers(t *testing.T) {
	for _, tc := range []struct {
		v, bound string
		atMost   bool
	}{
		{"7.3", "7.3.0", true},
		{"7.3.10", "7.3.9", false},
		{"7.3.10", "7.3.*", true},
		{"7.4", "7.3.*", false},
		{"7.2.99", "7.3.*", true},
		{"7.3.3", "*", true},
		{"7.3.3", "not-a-version", false},
	} {
		t.Run(tc.v+" <= "+tc.bound, func(t *testing.T) {
			v, _ := parseVersion(tc.v)
			if got := v.atMost(tc.bound); got != tc.atMost {
				t.Errorf("got %v, want %v", got, tc.atMost)
			}
		})
	}
}

// The summary is what a person reads, so its counts and the list that needs a decision are
// asserted on the output of run, from files laid out as the job lays them out.
func TestTheSummaryCountsEachVerdictAndListsWhatALaterSeriesFixes(t *testing.T) {
	dir := t.TempDir()
	published := filepath.Join(dir, "published", "2026")
	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatal(err)
	}
	records := map[string]string{
		"CVE-2026-1": `{"cveMetadata":{"cveId":"CVE-2026-1"},"containers":{"cna":{"title":"fixed","affected":[` + entryJSON("net/ipv4/ip_output.c", "7.3") + `]}}}`,
		"CVE-2026-2": `{"cveMetadata":{"cveId":"CVE-2026-2"},"containers":{"cna":{"title":"bluetooth","affected":[` + entryJSON("net/bluetooth/hci_core.c", "7.4") + `]}}}`,
		"CVE-2026-3": `{"cveMetadata":{"cveId":"CVE-2026-3"},"containers":{"cna":{"title":"ipv4: later series","affected":[` + entryJSON("net/ipv4/ip_output.c", "7.4") + `]}}}`,
	}
	for id, body := range records {
		if err := os.WriteFile(filepath.Join(published, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The repository keeps more than records beside them: each CVE's .dyad and .sha1, and a README.
	for _, other := range []string{"CVE-2026-1.dyad", "README"} {
		if err := os.WriteFile(filepath.Join(published, other), []byte("not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// As the build records a file included by a relative path: the CVE names it cleaned.
	sources := filepath.Join(dir, "sources")
	if err := os.WriteFile(sources, []byte("net/core/../ipv4/ip_output.c\ninclude/net/ip.h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run([]string{"-published", filepath.Join(dir, "published"), "-version", "7.3.3", "-sources", sources}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{
		"against the kernel CNA's 3 published CVEs",
		"| Fixed in 7.3.3, or older than the bug | 1 | 33.3% |",
		"| In files this kernel does not compile | 1 | 33.3% |",
		"| Apply: fixed only in a later series, a person's decision | 1 | 33.3% |",
		"Fixed only in a later series (1)",
		"[CVE-2026-3](https://www.cve.org/CVERecord?id=CVE-2026-3) ipv4: later series - fixed in 7.4",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the summary lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "CVE-2026-2]") {
		t.Errorf("the summary lists a CVE in code the kernel does not compile:\n%s", out.String())
	}
}

func entryJSON(file, fix string) string {
	return `{"defaultStatus":"affected","programFiles":["` + file + `"],"versions":[` +
		`{"version":"6.1","status":"affected"},` +
		`{"version":"0","lessThan":"6.1","status":"unaffected","versionType":"semver"},` +
		`{"version":"` + fix + `","lessThanOrEqual":"*","status":"unaffected","versionType":"original_commit_for_fix"}]}`
}

func TestInputsThatWouldUndercountAreRefused(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "published")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "CVE-1.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(dir, "sources")
	if err := os.WriteFile(sources, []byte("a.c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "CVE-1.json"), []byte(`{"cveMetadata":{"cveId":"CVE-1"},"containers":{"cna":{"affected":[`+entryJSON("a.c", "7.4")+`]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"a sources list that names nothing", []string{"-published", bad, "-version", "7.3.3", "-sources", empty}, 1},
		{"a record that does not parse", []string{"-published", bad, "-version", "7.3.3", "-sources", sources}, 1},
		{"a directory with no record", []string{"-published", t.TempDir(), "-version", "7.3.3", "-sources", sources}, 1},
		{"a version that is not one", []string{"-published", bad, "-version", "7.3-rc1", "-sources", sources}, 2},
		{"a missing flag", []string{"-version", "7.3.3", "-sources", sources}, 2},
		{"a flag it does not know", []string{"-published", good, "-version", "7.3.3", "-sources", sources, "-since", "2026"}, 2},
		{"a sources list of one file is a list", []string{"-published", good, "-version", "7.3.3", "-sources", sources}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := run(tc.args, &out, &errs); code != tc.code {
				t.Errorf("exit %d, want %d (%s)", code, tc.code, errs.String())
			}
		})
	}
}
