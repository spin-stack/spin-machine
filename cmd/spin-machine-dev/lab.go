// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/spin-stack/machine/internal/lab"
)

// labCommand is measuring: trees, the probes that boot them, and the bench fleet that runs them.
// probes are the probes it knows by name: lab.Probes, or a test's own.
func labCommand(probes map[string]lab.Probe) *cobra.Command {
	c := &cobra.Command{
		Use:   "lab",
		Short: "Measure the machine: make trees, run probes over them, here or on the bench fleet",
		Long: `A tree is a whole machine in _output/trees/NAME, every part of it named in its manifest.json:
a release, with a kernel or a QEMU from a CI run, this checkout's spin-machine, an edit of every
boot's overlay. A probe boots trees and records numbers; an experiment is a probe over trees,
repetitions of each, interleaved, written to result.json.

Probes: ` + probeNames(probes) + `.

A number is only worth comparing on a quiet host of the kind the goal is measured on: the bench
fleet, through lab dispatch. Here, lab run finds leads.`,
	}
	c.AddCommand(labTree(), labRun(probes), labShow(probes), labDispatch(probes), labCheck(), labLadder(), labCompat())
	return c
}

func probeNames(probes map[string]lab.Probe) string {
	return strings.Join(slices.Sorted(maps.Keys(probes)), ", ")
}

// treeFlags are a tree spec's flags, the same for lab tree and inside a --tree NAME=SPEC.
func treeFlags(fs *pflag.FlagSet, s *lab.TreeSpec) {
	fs.StringVar(&s.From, "from", "release", "what the tree starts from: release, release:vYYYYMMDD.NN, or a directory laid out as one (_output)")
	fs.StringVar(&s.Kernel, "kernel", "", "the kernel instead: run:N (a parts.yml run) or a bzImage")
	fs.StringVar(&s.QEMU, "qemu", "", "QEMU and its firmware instead: run:N (a parts.yml run) or a directory with bin/ and qemu/")
	fs.StringVar(&s.SpinMachine, "spin-machine", "", "here: built from this checkout; release: built from the commit the --from release was made from, the machine it defined; left out, the --from tree's own when it has one, else this checkout's (a release ships none)")
	fs.StringVar(&s.Append, "append", "", "added to the kernel command line of every boot")
	fs.StringArrayVar(&s.Boot, "boot", nil, "a spin-machine boot flag for every boot of the tree, e.g. --boot --cpu=Skylake-Server-v4")
	fs.StringVar(&s.CPU, "cpu", "", "baseline: the newest named CPU model the host making the tree can run, chosen there (machine.QEMU.CPUModels, internal/lab)")
	fs.StringArrayVar(&s.Edits, "edit", nil, "an edit of every boot's overlay, by name: lane (the first login's lane as this checkout builds it), pressure (the pressure probe's guest sender as this checkout builds it into the image), floor (no systemd before sshd: a bound, with --append init=/usr/local/sbin/spin-floor), nolinger (spin not lingering, as this checkout builds the image), forwarder-early (login=spin's forwarder, spin's supervisor, without systemd's default dependencies), carry-masks, carry-generators, carry-logging and carry-coldplug (optimize-systemd.sh's boot-time changes and the coldplug filter taken back, after lane)")
	fs.StringVar(&s.Systemd, "systemd", "", "an image/systemd/experiments patch: systemd built with it here, in every overlay")
}

// parseTreeSpec reads NAME=FLAGS, the FLAGS split as a shell would.
func parseTreeSpec(arg string) (lab.TreeSpec, error) {
	name, flags, ok := strings.Cut(arg, "=")
	if !ok || name == "" {
		return lab.TreeSpec{}, fmt.Errorf("a tree is NAME=FLAGS, e.g. 'kaslr=--kernel run:44584704610', not %q", arg)
	}
	words, err := splitWords(flags)
	if err != nil {
		return lab.TreeSpec{}, fmt.Errorf("tree %s: %w", name, err)
	}
	s := lab.TreeSpec{Name: strings.TrimSpace(name)}
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	treeFlags(fs, &s)
	if err := fs.Parse(words); err != nil {
		return lab.TreeSpec{}, fmt.Errorf("tree %s: %w", name, err)
	}
	if fs.NArg() > 0 {
		return lab.TreeSpec{}, fmt.Errorf("tree %s: %q is not a flag", name, fs.Arg(0))
	}
	return s, nil
}

// splitWords splits s on spaces, keeping what is inside single or double quotes together.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	var quote rune
	in := false
	for _, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words, in = append(words, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("an unclosed %c in %q", quote, s)
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}

// splitList splits each value on ';' and drops the blanks: a workflow input carries several
// trees or options in one string.
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for part := range strings.SplitSeq(v, ";") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func github() *lab.GitHub {
	return &lab.GitHub{Repo: cmp.Or(os.Getenv("GITHUB_REPOSITORY"), "spin-stack/machine")}
}

func sources(root string, log io.Writer) lab.Sources {
	return lab.Sources{Checkout: root, Cache: filepath.Join(outOf(root), "lab"), GitHub: github(), Log: log}
}

func labTree() *cobra.Command {
	var s lab.TreeSpec
	c := &cobra.Command{
		Use:   "tree NAME",
		Short: "Make the tree NAME in _output/trees: a release, with the parts asked for replaced",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			root, err := repo()
			if err != nil {
				return couldNot(err)
			}
			s.Name = args[0]
			t, err := lab.MakeTree(filepath.Join(outOf(root), "trees", s.Name), s, sources(root, c.ErrOrStderr()))
			if err != nil {
				return couldNot(err)
			}
			_, _ = fmt.Fprintln(c.OutOrStdout(), t.Dir)
			return nil
		},
	}
	treeFlags(c.Flags(), &s)
	return c
}

// trees are the trees an experiment measures: each NAME=SPEC made, each bare NAME opened.
func trees(root string, names, specs []string, log io.Writer) ([]*lab.Tree, error) {
	var out []*lab.Tree
	for _, spec := range splitList(specs) {
		s, err := parseTreeSpec(spec)
		if err != nil {
			return nil, err
		}
		t, err := lab.MakeTree(filepath.Join(outOf(root), "trees", s.Name), s, sources(root, log))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	for _, name := range names {
		t, err := lab.OpenTree(filepath.Join(outOf(root), "trees", name))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// releaseTrees are the published releases tags as trees, each booted by the spin-machine of the
// commit it was made from: a ladder's, and a record's.
//
// mutate-lane: TestLabLadderMeasuresReleases
func releaseTrees(root string, tags []string, log io.Writer) ([]*lab.Tree, error) {
	var specs []string
	for _, tag := range tags {
		specs = append(specs, tag+"=--from release:"+tag+" --spin-machine release")
	}
	return trees(root, nil, specs, log)
}

// options reads k=v pairs.
func options(pairs []string) (lab.Options, error) {
	o := lab.Options{}
	for _, p := range splitList(pairs) {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("an option is NAME=VALUE, not %q", p)
		}
		o[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return o, nil
}

func labRun(probes map[string]lab.Probe) *cobra.Command {
	var specs, opts []string
	var reps int
	var out string
	c := &cobra.Command{
		Use:   "run PROBE [TREE...]",
		Short: "Run a probe over trees, interleaved, and write result.json",
		Long: `Run PROBE over every tree, REPS times each, interleaved, and write what it recorded to
OUT/result.json and its table to stdout. Trees are names in _output/trees, or made here with
--tree NAME=SPEC (lab tree's flags: --tree 'kaslr=--kernel run:44584704610'); ';' separates
several in one value. Exits 1 when a measurement was lost, 2 when the experiment could not run.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, ok := probes[args[0]]
			if !ok {
				return couldNot(fmt.Errorf("no probe %q; the probes are: %s", args[0], probeNames(probes)))
			}
			root, err := repo()
			if err != nil {
				return couldNot(err)
			}
			o, err := options(opts)
			if err != nil {
				return couldNot(err)
			}
			ts, err := trees(root, args[1:], specs, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			scratch, err := lab.Scratch(filepath.Join(outOf(root), "lab"))
			if err != nil {
				return couldNot(err)
			}
			if out == "" {
				out = filepath.Join(outOf(root), "lab", "results", time.Now().UTC().Format("20060102-150405")+"-"+p.Name())
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return couldNot(fmt.Errorf("making %s: %w", out, err))
			}
			r, err := lab.Experiment{Probe: p, Trees: ts, Reps: reps, Options: o, Scratch: scratch, Log: c.ErrOrStderr()}.Run(c.Context())
			if err != nil {
				return couldNot(err)
			}
			if err := r.Write(filepath.Join(out, "result.json")); err != nil {
				return couldNot(err)
			}
			return report(c.OutOrStdout(), r, probes)
		},
	}
	c.Flags().StringArrayVar(&specs, "tree", nil, "a tree made for this run, NAME=SPEC")
	c.Flags().StringArrayVar(&opts, "opt", nil, "a probe option, NAME=VALUE (the shape, spin's unless given: cpus, max-cpus, memory, max-memory, hotplug-disks, flags; and the probe's own)")
	c.Flags().IntVar(&reps, "reps", 10, "repetitions of each tree")
	c.Flags().StringVar(&out, "out", "", "where result.json goes (default _output/lab/results/TIME-PROBE)")
	return c
}

// report prints r's table and the probe's detail, on the run's summary too in a workflow, and
// says 1 when a measurement was lost.
func report(w io.Writer, r *lab.Result, probes map[string]lab.Probe) error {
	text := r.Table()
	if d, ok := probes[r.Probe].(lab.Detailer); ok {
		text += d.Detail(r)
	}
	_, _ = fmt.Fprintln(w, text)
	summarise("```\n" + text + "\n```")
	for _, s := range r.Samples {
		if s.Missed != "" {
			return exitError{code: 1}
		}
	}
	return nil
}

// mutate-lane: TestLabLadderMeasuresReleases
func labLadder() *cobra.Command {
	var releases, reps int
	var newest, out string
	c := &cobra.Command{
		Use:   "ladder",
		Short: "Measure the newest releases together, each with its own spin-machine, and write ladder.json",
		Long: `Measure the --releases newest published releases up to --newest, interleaved on this host:
the two goal numbers (a cold boot and a restore, each to the first SSH login), both again into a
QEMU started ahead, and beside them save, state, kernel and QEMU RSS as diagnostics. Each release
is a tree booted by the spin-machine of the commit it was made from; a release whose spin-machine
cannot run a probe is a cell that says why. Writes OUT/ladder.json and OUT/ladder.md, and prints
the table. Exits 1 when a measurement was lost, 2 when the ladder could not run.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			root, err := repo()
			if err != nil {
				return couldNot(err)
			}
			all, err := github().Releases(max(releases, 30))
			if err != nil {
				return couldNot(err)
			}
			tags, err := lab.LadderReleases(all, newest, releases)
			if err != nil {
				return couldNot(err)
			}
			ts, err := releaseTrees(root, tags, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			scratch, err := lab.Scratch(filepath.Join(outOf(root), "lab"))
			if err != nil {
				return couldNot(err)
			}
			l, err := lab.RunLadder(c.Context(), ts, reps, scratch, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			if out == "" {
				out = filepath.Join(outOf(root), "lab", "ladder")
			}
			return writeLadder(c.OutOrStdout(), l, out)
		},
	}
	c.Flags().IntVar(&releases, "releases", 5, "how many releases, the newest up to --newest")
	c.Flags().StringVar(&newest, "newest", "", "the newest release measured (default the newest published)")
	c.Flags().IntVar(&reps, "reps", 20, "boots of each release for each probe")
	c.Flags().StringVar(&out, "out", "", "where ladder.json and ladder.md go (default _output/lab/ladder)")
	return c
}

// writeLadder writes l's JSON and Markdown under out, prints the Markdown, on the run's summary
// too in a workflow, and says 1 when a measurement was lost.
func writeLadder(w io.Writer, l *lab.Ladder, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return couldNot(fmt.Errorf("making %s: %w", out, err))
	}
	if err := l.Write(filepath.Join(out, "ladder.json")); err != nil {
		return couldNot(err)
	}
	md := lab.Tables(l.Records())
	if err := os.WriteFile(filepath.Join(out, "ladder.md"), []byte(md), 0o644); err != nil {
		return couldNot(fmt.Errorf("writing %s: %w", filepath.Join(out, "ladder.md"), err))
	}
	_, _ = fmt.Fprintln(w, md)
	summarise(md)
	for _, r := range l.Results {
		for _, s := range r.Samples {
			if s.Missed != "" {
				return exitError{code: 1}
			}
		}
	}
	return nil
}

// summarise appends text to the run's summary in a workflow.
func summarise(text string) {
	if f := os.Getenv("GITHUB_STEP_SUMMARY"); f != "" {
		if s, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintln(s, text)
			_ = s.Close()
		}
	}
}

// mutate-lane: TestLabCompatResumesEveryRelease
func labCompat() *cobra.Command {
	var here string
	var releases int
	c := &cobra.Command{
		Use:   "compat",
		Short: "Resume, on this machine, a checkpoint of every release whose machine is this one",
		Long: `boot:compat over the newest --releases published releases, newest first, each fetched and
checked against its sha256 and its signature: a checkpoint each saves, at spin's shape, goes on
running on the machine at --here (a built release: _output, or boot:here's tree in CI). It stops
at the first release whose checkpoint the machine refuses (machine.CanResume), naming every
difference: its checkpoints - and every older one's - are not meant to resume. For each, a
checkpoint of --here's QEMU is one CanResume refuses on an older QEMU. Exits 1 when one did not
resume.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			root, err := repo()
			if err != nil {
				return couldNot(err)
			}
			tags, err := github().Releases(releases)
			if err != nil {
				return couldNot(err)
			}
			abs, err := filepath.Abs(here)
			if err != nil {
				return couldNot(err)
			}
			// A machine that is not this one is an answer, not a failure: a row that says so.
			resume := func(previous, previousCLI string) (string, error) {
				return lab.ResumeHere(c.Context(), abs, previous, previousCLI, c.ErrOrStderr())
			}
			rows, err := lab.Compat(abs, tags, sources(root, c.ErrOrStderr()), resume, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			table := lab.CompatTable(rows)
			_, _ = fmt.Fprintln(c.OutOrStdout(), table)
			summarise(table)
			for _, r := range rows {
				if !r.Resumed && !r.Moved && !r.NoBzImage {
					return exitError{code: 1}
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&here, "here", "_output", "the machine checkpoints resume on: a built release")
	c.Flags().IntVar(&releases, "releases", 10, "how many of the newest releases at most")
	return c
}

func labShow(probes map[string]lab.Probe) *cobra.Command {
	return &cobra.Command{
		Use:   "show RUN_ID|DIR",
		Short: "Print the result of a lab.yml run, or of a result.json in DIR",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := resultOf(args[0])
			if err != nil {
				return couldNot(err)
			}
			// A ladder's run left ladder.json where a lab run leaves result.json.
			if ladder := filepath.Join(filepath.Dir(path), "ladder.json"); exists(ladder) {
				l, err := lab.ReadLadder(ladder)
				if err != nil {
					return couldNot(err)
				}
				_, _ = fmt.Fprintln(c.OutOrStdout(), lab.Tables(l.Records()))
				return nil
			}
			r, err := lab.ReadResult(path)
			if err != nil {
				return couldNot(err)
			}
			return report(c.OutOrStdout(), r, probes)
		},
	}
}

// resultOf is the result.json arg names: one in a directory, or a lab.yml run's, downloaded.
//
// mutate-lane: TestLabDispatchMeasuresOnTheBench, which reads its run's result through it.
func resultOf(arg string) (string, error) {
	if _, err := strconv.ParseInt(arg, 10, 64); err != nil {
		return filepath.Join(arg, "result.json"), nil
	}
	root, err := repo()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(outOf(root), "lab", "runs", arg, "result")
	if !exists(filepath.Join(dir, "result.json")) && !exists(filepath.Join(dir, "ladder.json")) {
		if err := github().Artifact(arg, "lab-"+arg, dir); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "result.json"), nil
}

// exists says whether there is a file at path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mutate-lane: TestLabDispatchMeasuresOnTheBench
func labDispatch(probes map[string]lab.Probe) *cobra.Command {
	var specs, opts []string
	var reps int
	var host, ref string
	c := &cobra.Command{
		Use:   "dispatch PROBE",
		Short: "Run a probe on the bench fleet through lab.yml, one lab at a time, and print its result",
		Long: `Dispatch lab.yml with PROBE over the trees given, once no other lab run is queued or running -
the fleet measures one at a time, and two runs side by side measure each other - then wait for it
and print its result. Each --tree is NAME=SPEC, made on the runner.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if _, ok := probes[args[0]]; !ok {
				return couldNot(fmt.Errorf("no probe %q; the probes are: %s", args[0], probeNames(probes)))
			}
			if len(specs) == 0 {
				return couldNot(errors.New("dispatch measures the trees it is given: --tree NAME=SPEC"))
			}
			for _, s := range splitList(specs) {
				if _, err := parseTreeSpec(s); err != nil {
					return couldNot(err)
				}
			}
			if _, err := options(opts); err != nil {
				return couldNot(err)
			}
			gh := github()
			run, err := gh.Dispatch(c.Context(), ref, map[string]string{
				"probe": args[0], "trees": strings.Join(splitList(specs), "; "), "reps": strconv.Itoa(reps),
				"options": strings.Join(splitList(opts), "; "), "host": host,
			}, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "lab run %d: %s\n", run.ID, run.URL)
			if run, err = gh.Wait(c.Context(), run.ID); err != nil {
				return couldNot(err)
			}
			path, err := resultOf(strconv.FormatInt(run.ID, 10))
			if err != nil {
				return couldNot(fmt.Errorf("lab run %d %s and left no result: %w", run.ID, run.Conclusion, err))
			}
			r, err := lab.ReadResult(path)
			if err != nil {
				return couldNot(err)
			}
			return report(c.OutOrStdout(), r, probes)
		},
	}
	c.Flags().StringArrayVar(&specs, "tree", nil, "a tree to measure, NAME=SPEC (lab tree's flags)")
	c.Flags().StringArrayVar(&opts, "opt", nil, "a probe option, NAME=VALUE")
	c.Flags().IntVar(&reps, "reps", 20, "repetitions of each tree")
	c.Flags().StringVar(&host, "host", "bench", "bench (nested KVM) or bench-metal (bare metal, one at a time across the org)")
	c.Flags().StringVar(&ref, "ref", "main", "the branch whose lab.yml and probes run")
	return c
}

// mutate-lane: TestLabCheckRunsABootTarget
func labCheck() *cobra.Command {
	var spec string
	c := &cobra.Command{
		Use:   "check TARGET",
		Short: "Run a boot/ Task target - a pass/fail check, or a probe not yet a lab probe - on one tree",
		Long: `Run boot:TARGET with the tree as the release it boots (SPIN_MACHINE_OUTPUT): the functional
checks (firmware, record, compat, hotadd, logind, lockdown) and the probes still Go tests
(firmware-stages, waits), on a tree made like any other. A second tree is
SPIN_MACHINE_OUTPUT_B, which firmware-stages boots interleaved with the first. Exits with the
target's status.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			root, err := repo()
			if err != nil {
				return couldNot(err)
			}
			ts, err := trees(root, nil, []string{spec}, c.ErrOrStderr())
			if err != nil {
				return couldNot(err)
			}
			cmd := exec.CommandContext(c.Context(), "go", "tool", "task", "boot:"+args[0], "OUTPUT_DIR="+ts[0].Dir)
			cmd.Dir, cmd.Stdout, cmd.Stderr = root, c.OutOrStdout(), c.ErrOrStderr()
			cmd.Env = append(os.Environ(), "SPIN_MACHINE_OUTPUT="+ts[0].Dir)
			if len(ts) > 1 {
				// The tree a probe still a Go test boots interleaved with the first
				// (firmware-stages); the checks boot one.
				cmd.Env = append(cmd.Env, "SPIN_MACHINE_OUTPUT_B="+ts[1].Dir)
			}
			if err := cmd.Run(); err != nil {
				return exitError{code: 1, err: fmt.Errorf("boot:%s on tree %s: %w", args[0], ts[0].Name(), err)}
			}
			return nil
		},
	}
	c.Flags().StringVar(&spec, "tree", "release=--from release", "the tree, NAME=SPEC")
	return c
}
