// SPDX-License-Identifier: Apache-2.0

// Command machinetype watches the versioned q35 the machine is pinned to (machine.MachineType)
// against the QEMU it runs on.
//
// check asks a built QEMU for its machine types: it fails when the pinned one is missing or
// deprecated - QEMU deprecates a versioned type after three years and removes it after six, so
// this fires years before a QEMU stops loading the checkpoints it saved - and says, without
// failing, when QEMU offers a newer one, for the build log and the release's notes.
//
// compat prints what the newer types change, from QEMU's own source: the properties
// hw_compat_X_Y (hw/core/machine.c) and pc_compat_X_Y (hw/i386/pc.c) switch back off for the
// pinned X.Y. That list is what moving the pin would change under every guest, and task bump
// NAME=qemu prints it from the tarball the pin names.
//
//	go run ./hack/machinetype check -qemu _output/bin/qemu-system-x86_64
//	go run ./hack/machinetype compat <qemu>/hw/core/machine.c <qemu>/hw/i386/pc.c
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/spin-stack/spin-machine/machine"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, help)) // mutate-exempt: os.Args[0] is the program; run is what the tests drive
}

// help is a QEMU binary's `-machine help`.
func help(qemu string) (string, error) {
	out, err := exec.Command(qemu, "-machine", "help").CombinedOutput() //nolint:gosec // the QEMU this build made
	if err != nil {
		return "", fmt.Errorf("%s -machine help: %w: %s", qemu, err, out)
	}
	return string(out), nil
}

// run answers the exit status: 0 the pin stands, 1 it does not, 2 the arguments are not a check.
func run(args []string, stdout, stderr io.Writer, help func(string) (string, error)) int {
	if len(args) == 0 {
		say(stderr, "machinetype: check -qemu <binary> | compat <machine.c> <pc.c>")
		return 2
	}
	switch args[0] {
	case "check":
		if len(args) != 3 || args[1] != "-qemu" {
			say(stderr, "machinetype: check -qemu <binary>")
			return 2
		}
		text, err := help(args[2])
		if err != nil {
			say(stderr, "machinetype:", err)
			return 1
		}
		notice, err := Check(text, machine.MachineType)
		if err != nil {
			say(stderr, "machinetype:", err)
			return 1
		}
		if notice != "" {
			say(stdout, notice)
		}
		return 0
	case "compat":
		if len(args) != 3 {
			say(stderr, "machinetype: compat <machine.c> <pc.c>")
			return 2
		}
		var src []string
		for _, f := range args[1:] {
			b, err := os.ReadFile(f) //nolint:gosec // a file of the QEMU source the bump unpacked
			if err != nil {
				say(stderr, "machinetype:", err)
				return 1
			}
			src = append(src, string(b))
		}
		out, err := Compat(strings.Join(src, "\n"), machine.MachineType)
		if errors.Is(err, errNoArray) {
			say(stdout, "machinetype:", machine.MachineType, "is the newest type in this QEMU: nothing newer to differ from it")
			return 0
		}
		if err != nil {
			say(stderr, "machinetype:", err)
			return 1
		}
		say(stdout, out)
		return 0
	}
	say(stderr, "machinetype: no command", args[0])
	return 2
}

// q35 is one line of `-machine help` that names a versioned q35: its X.Y and whether it is
// deprecated.
var q35 = regexp.MustCompile(`^pc-q35-(\d+)\.(\d+)\s.*?(\(deprecated\))?\s*$`)

// version is a pc-q35-X.Y's X and Y.
type version struct{ major, minor int }

func parse(name string) (version, bool) {
	m := regexp.MustCompile(`^pc-q35-(\d+)\.(\d+)$`).FindStringSubmatch(name)
	if m == nil {
		return version{}, false
	}
	x, _ := strconv.Atoi(m[1])
	y, _ := strconv.Atoi(m[2])
	return version{x, y}, true
}

func (v version) newer(than version) bool {
	return v.major > than.major || v.major == than.major && v.minor > than.minor
}

// Check is what `-machine help` says of pinned: an error when it is not offered or is
// deprecated, and a notice when a newer pc-q35 is.
func Check(help, pinned string) (notice string, err error) {
	want, ok := parse(pinned)
	if !ok {
		return "", fmt.Errorf("%q is not a pc-q35-X.Y", pinned)
	}
	var newest version
	found := false
	for _, line := range strings.Split(help, "\n") {
		m := q35.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		v := version{x, y}
		if v == want {
			if m[3] != "" {
				return "", fmt.Errorf("this QEMU deprecates %s, the machine type every checkpoint is saved on: move machine.MachineType, a change of hardware under every guest, before QEMU removes it", pinned)
			}
			found = true
		}
		if v.newer(newest) {
			newest = v
		}
	}
	if !found {
		return "", fmt.Errorf("this QEMU offers no %s, the machine type every checkpoint is saved on", pinned)
	}
	if newest.newer(want) {
		return fmt.Sprintf("QEMU offers pc-q35-%d.%d; this machine stays on %s (machine.MachineType), and moving it is a new machine: task qemu:compat says what would change.",
			newest.major, newest.minor, pinned), nil
	}
	return "", nil
}

// Compat is QEMU's source's hw_compat_X_Y and pc_compat_X_Y for pinned's X.Y, each array as the
// source writes it: what every type newer than pinned turns on that pinned keeps off.
func Compat(src, pinned string) (string, error) {
	v, ok := parse(pinned)
	if !ok {
		return "", fmt.Errorf("%q is not a pc-q35-X.Y", pinned)
	}
	var b strings.Builder
	for _, name := range []string{fmt.Sprintf("hw_compat_%d_%d", v.major, v.minor), fmt.Sprintf("pc_compat_%d_%d", v.major, v.minor)} {
		arr, err := array(src, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\n", arr)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// errNoArray is a source in which QEMU has not yet given pinned's type a compat array: no newer
// type exists there to differ from it.
var errNoArray = errors.New("no such array: this QEMU has no machine type newer than the pinned one")

// array is the declaration `GlobalProperty <name>[] = { ... };` in src, whole.
func array(src, name string) (string, error) {
	start := regexp.MustCompile(`GlobalProperty\s+` + regexp.QuoteMeta(name) + `\[\]\s*=\s*\{`).FindStringIndex(src)
	if start == nil {
		return "", fmt.Errorf("%s: %w", name, errNoArray)
	}
	end := strings.Index(src[start[0]:], "};")
	if end < 0 { // mutate-exempt: the match starts with "GlobalProperty", so "};" is never at its offset 0
		return "", fmt.Errorf("%s has no end in the source", name)
	}
	return src[start[0] : start[0]+end+2], nil
}

func say(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }
