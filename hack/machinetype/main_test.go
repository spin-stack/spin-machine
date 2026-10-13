// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// QEMU 11.1.2's own `-machine help`, the QEMU this tree pins.
func help1112(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "help-11.1.2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The pinned type stands when QEMU offers it and has not deprecated it, with a notice when a
// newer one is offered; it does not when QEMU deprecates it or no longer offers it.
func TestThePinnedTypeAgainstAQEMU(t *testing.T) {
	real := help1112(t)
	newer := "pc-q35-11.2          Standard PC (Q35 + ICH9, 2009)\n" + real
	for _, tc := range []struct {
		name, help, pinned, notice, err string
	}{
		{name: "this tree's QEMU and pin", help: real, pinned: machine.MachineType},
		{name: "a QEMU with a newer type", help: newer, pinned: "pc-q35-11.1", notice: "QEMU offers pc-q35-11.2; this machine stays on pc-q35-11.1"},
		{name: "a newer major", help: "pc-q35-12.0   Standard PC\n" + real, pinned: "pc-q35-11.1", notice: "QEMU offers pc-q35-12.0"},
		{name: "an older pin, still offered", help: real, pinned: "pc-q35-10.2", notice: "QEMU offers pc-q35-11.1; this machine stays on pc-q35-10.2"},
		{name: "a deprecated pin", help: real, pinned: "pc-q35-8.1", err: "deprecates pc-q35-8.1"},
		{name: "a pin QEMU removed", help: real, pinned: "pc-q35-2.4", err: "offers no pc-q35-2.4"},
		{name: "a pin that is no versioned q35", help: real, pinned: "q35", err: "is not a pc-q35-X.Y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice, err := Check(tc.help, tc.pinned)
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Check: %v, want %q", err, tc.err)
				}
			case err != nil:
				t.Fatalf("Check: %v", err)
			case tc.notice == "" && notice != "":
				t.Errorf("a notice %q with nothing newer", notice)
			case !strings.Contains(notice, tc.notice):
				t.Errorf("notice %q, want %q", notice, tc.notice)
			}
		})
	}
}

// QEMU's source as an 11.2 tarball writes it: the arrays for the pinned 11.1, each whole, and
// nothing from the array beside it.
const source = `
GlobalProperty hw_compat_11_1[] = {
    { "virtio-net-pci", "x-new-feature", "off" },
};
const size_t hw_compat_11_1_len = G_N_ELEMENTS(hw_compat_11_1);

GlobalProperty hw_compat_11_0[] = {
    { "older", "x", "off" },
};
`

const pcSource = `
GlobalProperty pc_compat_11_1[] = {
    { TYPE_X86_CPU, "x-something", "false" },
};
`

func TestCompatIsThePinnedTypesArrays(t *testing.T) {
	got, err := Compat(source+pcSource, "pc-q35-11.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GlobalProperty hw_compat_11_1[] = {", `"x-new-feature", "off"`, "GlobalProperty pc_compat_11_1[] = {", `"x-something", "false"`} {
		if !strings.Contains(got, want) {
			t.Errorf("compat\n%s\nwant %q", got, want)
		}
	}
	if strings.Contains(got, "older") {
		t.Errorf("compat\n%s\nholds the array of another type", got)
	}
	// QEMU writes an array with nothing to switch back off as one line (pc_compat_11_0 in 11.1.2).
	empty, err := Compat(source+"\nGlobalProperty pc_compat_11_1[] = {};\n", "pc-q35-11.1")
	if err != nil || !strings.HasSuffix(empty, "GlobalProperty pc_compat_11_1[] = {};") {
		t.Errorf("an empty array: %q, %v", empty, err)
	}
	if _, err := Compat(source, "pc-q35-11.1"); !errors.Is(err, errNoArray) {
		t.Errorf("no pc_compat_11_1: %v", err)
	}
	if _, err := Compat("GlobalProperty hw_compat_11_1[] = { { ", "pc-q35-11.1"); err == nil || errors.Is(err, errNoArray) {
		t.Errorf("an array with no end: %v", err)
	}
	if _, err := Compat(source, "q35"); err == nil {
		t.Error("an unversioned pin")
	}
}

// An array is its declaration to its closing brace and semicolon, and not a byte past them.
func TestAnArrayIsItsDeclarationWhole(t *testing.T) {
	got, err := array("before GlobalProperty a_1[] = { { \"x\", \"y\", \"z\" },\n};\nafter", "a_1")
	if err != nil || got != "GlobalProperty a_1[] = { { \"x\", \"y\", \"z\" },\n};" {
		t.Fatalf("array: %q, %v", got, err)
	}
}

// The command: check prints the notice and passes, fails on a pin that does not stand or a QEMU
// that cannot be asked; compat prints the arrays; anything else is no command.
func TestTheCommand(t *testing.T) {
	dir := t.TempDir()
	mc, pc := filepath.Join(dir, "machine.c"), filepath.Join(dir, "pc.c")
	if err := os.WriteFile(mc, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pc, []byte(pcSource), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.c")
	if err := os.WriteFile(short, []byte("GlobalProperty hw_compat_11_1[] = { {"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := func(string) (string, error) { return "pc-q35-11.2   Standard PC\n" + help1112(t), nil }
	broken := func(string) (string, error) { return "", errors.New("no such binary") }
	removed := func(string) (string, error) { return "pc-q35-11.2   Standard PC\n", nil }
	for _, tc := range []struct {
		name string
		args []string
		help func(string) (string, error)
		code int
		out  string
	}{
		{name: "check, a newer type", args: []string{"check", "-qemu", "q"}, help: newer, code: 0, out: "QEMU offers pc-q35-11.2"},
		{name: "check, nothing newer", args: []string{"check", "-qemu", "q"}, help: func(string) (string, error) { return help1112(t), nil }, code: 0},
		{name: "check, the pin removed", args: []string{"check", "-qemu", "q"}, help: removed, code: 1},
		{name: "check, a QEMU that does not answer", args: []string{"check", "-qemu", "q"}, help: broken, code: 1},
		{name: "check with no binary", args: []string{"check"}, help: newer, code: 2},
		{name: "compat", args: []string{"compat", mc, pc}, code: 0, out: "x-new-feature"},
		{name: "compat of a file not there", args: []string{"compat", mc, filepath.Join(dir, "none")}, code: 1},
		{name: "compat with one file", args: []string{"compat", mc}, code: 2},
		{name: "compat of a QEMU with nothing newer", args: []string{"compat", pc, pc}, code: 0, out: "is the newest type in this QEMU"},
		{name: "compat of a source cut short", args: []string{"compat", short, short}, code: 1},
		{name: "no command", args: nil, code: 2},
		{name: "another command", args: []string{"other"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := run(tc.args, &out, &errs, tc.help); code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, errs.String())
			}
			if !strings.Contains(out.String(), tc.out) {
				t.Errorf("printed %q, want %q", out.String(), tc.out)
			}
			if tc.out == "" && out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
		})
	}
}

// help asks a real binary: one that is not there is an error, not an empty answer.
func TestHelpAsksTheBinary(t *testing.T) {
	if _, err := help(filepath.Join(t.TempDir(), "no-qemu")); err == nil {
		t.Error("no binary answered")
	}
}
