// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/spin-stack/spin-machine/boot"
)

// axis is one feature of the machine and the choices on it, each as spin-machine boot flags.
// The matrix is every combination of every axis, so an axis added here is measured against
// all the others without anyone deciding which pairs matter.
type axis struct {
	name    string
	choices []choice
}

type choice struct {
	name  string
	flags []string
}

// diskFile stands in for the per-boot disk in the command line a report records, so that it
// is the same on every host and every run.
const diskFile = "/report/disk.qcow2"

// KVM only. The TCG build is for CI runners without /dev/kvm, where a boot is ~4 s of emulation:
// timing it says nothing about a host, and its rows were most of a report's minutes.
var axes = []axis{
	// RAM fixed at its boot size, or with a ceiling reached through virtio-mem.
	{"memory", []choice{
		{"anonymous", nil},
		{"ceiling", []string{"--max-memory", "4096"}},
	}},
	{"vsock", []choice{{"off", nil}, {"on", []string{"--vsock-cid", "1000"}}}},
	{"hotplug", []choice{{"off", nil}, {"on", []string{"--hotplug-disks", "4"}}}},
}

// combinations is the product of the axes, in a fixed order.
func combinations() [][]choice {
	all := [][]choice{nil}
	for _, a := range axes {
		var next [][]choice
		for _, prefix := range all {
			for _, c := range a.choices {
				next = append(next, append(append([]choice(nil), prefix...), c))
			}
		}
		all = next
	}
	return all
}

// TestReport measures the feature matrix and the image's boot variants against one release
// tree and writes a boot.Report as JSON to the path SPIN_REPORT names. `task report` runs it;
// `spin-machine compare` reads two of them.
//
// It is how a release says what it costs, and how an experiment says what it changed: build
// the tree with the change, or pass it in SPIN_REPORT_FLAGS (`--kernel /path/vmlinux`,
// `--append "..."`), and compare the report with the release's.
//
//	SPIN_REPORT=<file>         run at all, and where the JSON goes
//	SPIN_REPORT_FLAGS="..."    spin-machine boot flags added to every boot, recorded
//	SPIN_REPORT_ONLY=<regexp>  only the rows whose id matches, for iterating on one question
//	REPS=<n>                   boots per row (default 20)
//
// Needs /dev/kvm and a built release tree (SPIN_MACHINE_OUTPUT, or _output). The vsock rows
// need /dev/vhost-vsock; without it those rows are
// listed as skipped rather than reported as failing. With sudo and /dev/nbd0 the getty is
// replaced by an echo, as in TestBootCost, and every variant of the image runs; without them
// the usable column carries agetty's second and the variants that edit the image are skipped.
func TestReport(t *testing.T) {
	path := os.Getenv("SPIN_REPORT")
	if path == "" {
		t.Skip("set SPIN_REPORT=<file>: this boots every combination of the machine's features")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 20)
	flags := strings.Fields(os.Getenv("SPIN_REPORT_FLAGS"))
	only, err := regexp.Compile(os.Getenv("SPIN_REPORT_ONLY"))
	if err != nil {
		t.Fatalf("SPIN_REPORT_ONLY: %v", err)
	}

	r := boot.Report{Artifacts: map[string]string{}, Host: thisHost(t), Reps: reps, Flags: flags, Login: "agetty",
		Only: only.String()}
	env, err := os.ReadFile(filepath.Join(out, "machine.env"))
	if err == nil {
		for _, line := range strings.Split(string(env), "\n") {
			k, v, ok := strings.Cut(line, "=")
			switch {
			case !ok:
			case k == "version":
				r.Release = v
			case strings.HasSuffix(k, "_sha256"):
				r.Artifacts[k] = v
			}
		}
	}
	sudo := canEditImages()
	var login map[string]string
	if sudo {
		r.Login, login = "echo", gettyDropin(gettyEcho)
	}

	missing := map[string]string{}
	if f, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0); err != nil {
		missing["vsock=on"] = "no usable /dev/vhost-vsock: " + err.Error()
	} else {
		_ = f.Close()
	}

	// One boot nobody measures. The first after the release was written or the host started
	// reads the base image from disk and not from the page cache, and would be the slowest
	// sample of whichever row came first.
	bootOnce(t, out, variant{label: "warm-up", cpus: "2", memory: "2048", files: login})

	for _, combo := range combinations() {
		row := boot.Row{Features: map[string]string{}}
		var ids, rowFlags []string
		why := ""
		for i, c := range combo {
			row.Features[axes[i].name] = c.name
			ids = append(ids, axes[i].name+"="+c.name)
			rowFlags = append(rowFlags, c.flags...)
			if w, ok := missing[axes[i].name+"="+c.name]; ok {
				why = w
			}
		}
		row.ID = strings.Join(ids, ",")
		if !only.MatchString(row.ID) {
			continue
		}
		if why != "" {
			r.Skipped = append(r.Skipped, boot.Skipped{ID: row.ID, Why: why})
			continue
		}
		rowFlags = append(rowFlags, flags...)
		row.Args = static(t, out, "args", rowFlags)
		var fp struct {
			Fingerprint string
			Shape       any
		}
		if err := json.Unmarshal([]byte(strings.Join(static(t, out, "fingerprint", rowFlags), "\n")), &fp); err != nil {
			t.Fatalf("%s: reading spin-machine fingerprint: %v", row.ID, err)
		}
		row.Fingerprint, row.Shape = fp.Fingerprint, fp.Shape

		v := variant{label: row.ID, cpus: "2", memory: "2048", files: login,
			flags: rowFlags}
		row.Boot = measure(t, out, v, reps)
		r.Specs = append(r.Specs, row)
	}

	vs := variants
	if k := kernelB(t); k != "" {
		vs = append(vs[:len(vs):len(vs)], variant{label: "kernel B", cpus: "2", memory: "2048",
			files: gettyDropin(gettyEcho), kernel: k})
	}
	for _, v := range vs {
		if !only.MatchString(v.label) {
			continue
		}
		if !sudo && (len(v.mask) > 0 || len(v.files) > 0 || len(v.links) > 0 || v.setup != "") {
			r.Skipped = append(r.Skipped, boot.Skipped{ID: v.label, Why: "edits the image, which needs sudo and /dev/nbd0"})
			continue
		}
		v.flags = append(v.flags, flags...)
		r.Variants = append(r.Variants, boot.Row{ID: v.label, Boot: measure(t, out, v, reps)})
	}

	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows of the matrix, %d variants, %d skipped: %s", len(r.Specs), len(r.Variants), len(r.Skipped), path)
}

// measure boots v reps times and summarises the boots.
func measure(t *testing.T, out string, v variant, reps int) boot.Boot {
	t.Helper()
	runs := make([]boot.Run, 0, reps)
	failure := ""
	for range reps {
		run := bootOnce(t, out, v)
		if !run.Reached(boot.Usable) && failure == "" {
			failure = tail(run.Output, 800)
		}
		runs = append(runs, run)
	}
	b := boot.Summarise(runs, failure)
	t.Logf("%s: %d/%d boots reached a login prompt", v.label, b.Boots-b.Failed, b.Boots)
	return b
}

// static runs a spin-machine command that builds the machine without starting it, over the
// fixed file names a report records, and returns its output lines with the release
// directory written as $RELEASE.
func static(t *testing.T, out, command string, flags []string) []string {
	t.Helper()
	args := append([]string{command, "--release", out, "--disk", diskFile}, flags...)
	b, err := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...).Output()
	if err != nil {
		t.Fatalf("spin-machine %s: %v", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if command == "args" {
			for _, f := range strings.Fields(strings.ReplaceAll(l, out, "$RELEASE")) {
				if f != "\\" {
					lines = append(lines, f)
				}
			}
			continue
		}
		lines = append(lines, strings.ReplaceAll(l, out, "$RELEASE"))
	}
	return lines
}

func thisHost(t *testing.T) boot.Host {
	t.Helper()
	h := boot.Host{CPUs: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, ok := strings.CutPrefix(sc.Text(), "model name"); ok {
			_, h.CPU, _ = strings.Cut(name, ":")
			h.CPU = strings.TrimSpace(h.CPU)
			break
		}
	}
	return h
}
