// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A lock is every installed package, name=version, sorted; a package with half its record is no
// lock, and neither is none.
func TestALockIsEveryInstalledPackage(t *testing.T) {
	db := "C:Q1abc=\nP:zlib-static\nV:1.3.1-r2\nA:x86_64\n\nP:glib-static\nV:2.88.1-r1\n\n"
	got, err := Lock(db)
	if err != nil || got != "glib-static=2.88.1-r1\nzlib-static=1.3.1-r2\n" {
		t.Fatalf("lock %q, %v", got, err)
	}
	for name, db := range map[string]string{
		"no version":          "P:glib\n",
		"no name":             "V:1\n",
		"nothing":             "\n",
		"one whole, one half": "P:musl\nV:1.2.5-r10\n\nP:glib\n",
	} {
		if _, err := Lock(db); err == nil {
			t.Errorf("%s: locked", name)
		}
	}
}

// The command writes the lock of the database it is given, and nothing for none.
func TestTheCommandWritesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed")
	if err := os.WriteFile(path, []byte("P:musl\nV:1.2.5-r10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if run([]string{path}, &out, &errs) != 0 || out.String() != "musl=1.2.5-r10\n" {
		t.Fatalf("wrote %q: %s", out.String(), errs.String())
	}
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("P:musl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {filepath.Join(t.TempDir(), "none")}, {broken}} {
		if run(args, &out, &errs) != 2 {
			t.Errorf("%v: no refusal", args)
		}
	}
}

// Each builder's lock pins every package its list asks for: a name added to a list and not
// locked would be installed at whatever version the mirror has.
func TestEachListIsLocked(t *testing.T) {
	for _, part := range []string{"qemu", "e2fsprogs"} {
		list, err := os.ReadFile(filepath.Join("..", "..", part, "apk.list"))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := os.ReadFile(filepath.Join("..", "..", part, "apk.lock"))
		if err != nil {
			t.Fatal(err)
		}
		names := Listed(string(list))
		if len(names) == 0 {
			t.Fatalf("%s's list names nothing", part)
		}
		for _, n := range names {
			if !strings.Contains("\n"+string(lock), "\n"+n+"=") {
				t.Errorf("%s/apk.list asks for %s, which %s/apk.lock does not pin: task apk:lock", part, n, part)
			}
		}
	}
	if got := Listed("# a comment\nglib-static pixman-static  # why\n\nzlib-static\n"); strings.Join(got, " ") != "glib-static pixman-static zlib-static" {
		t.Errorf("listed %q", got)
	}
}
