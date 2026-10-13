// SPDX-License-Identifier: Apache-2.0

// Command apklock writes the lock of an Alpine builder: every package an apk database says is
// installed, as name=version, sorted - what the builder's `apk add` is then held to, so the static
// libraries QEMU and e2fsprogs link are the versions a commit names, not whatever the mirror held
// that day. task apk:lock runs it over a fresh install of the part's apk.list.
//
//	go run ./hack/apklock <installed db> > <part>/apk.lock
package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) // mutate-exempt: the process's own arguments, past its name; run is what the tests drive
}

// run answers the exit status: 0 the lock written, 2 no database to read.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "apklock: <installed db>")
		return 2
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "apklock:", err)
		return 2
	}
	lock, err := Lock(string(raw))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "apklock:", err)
		return 2
	}
	_, _ = io.WriteString(stdout, lock)
	return 0
}

// Lock is an apk installed database's packages as name=version lines, sorted.
func Lock(db string) (string, error) {
	var pins []string
	for _, block := range strings.Split(db, "\n\n") {
		var name, version string
		for _, line := range strings.Split(block, "\n") {
			switch k, v, _ := strings.Cut(line, ":"); k {
			case "P":
				name = v
			case "V":
				version = v
			}
		}
		switch {
		case name == "" && version == "":
			continue
		case name == "" || version == "":
			return "", fmt.Errorf("a package with name %q and version %q", name, version)
		}
		pins = append(pins, name+"="+version)
	}
	if len(pins) == 0 {
		return "", fmt.Errorf("no package installed")
	}
	slices.Sort(pins)
	return strings.Join(pins, "\n") + "\n", nil
}

// Listed is the package names of an apk.list: one or more a line, # comments and blank lines left
// out.
func Listed(list string) []string {
	var names []string
	for _, line := range strings.Split(list, "\n") {
		line, _, _ = strings.Cut(line, "#")
		names = append(names, strings.Fields(line)...)
	}
	return names
}
