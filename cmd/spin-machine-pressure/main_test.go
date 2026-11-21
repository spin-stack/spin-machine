// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/machine/internal/pressure"
)

// A host that is not there is dialled again later and later, up to lastRedial.
func TestAHostThatIsNotThereIsDialledLessOften(t *testing.T) {
	s := sender{fd: -1}
	var waits []time.Duration
	for range 7 {
		before := time.Now()
		s.dial(0)
		if s.fd >= 0 {
			t.Skip("something answers on the host's pressure port here")
		}
		waits = append(waits, s.next.Sub(before).Round(time.Second))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if waits[i] != want[i]*time.Second {
			t.Fatalf("the waits between dials are %v", waits)
		}
	}
}

// What the sender sends is one whole line each; a host that is gone is dropped, and dialled again
// a second later.
func TestTheSenderSendsWholeLinesAndDropsAHostThatIsGone(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	host := os.NewFile(uintptr(fds[1]), "host")
	defer func() { _ = host.Close() }()
	s := sender{fd: fds[0], redial: 8 * time.Second}
	s.send(pressure.OOM{Kills: 2})
	b := make([]byte, 64)
	n, _ := host.Read(b)
	if string(b[:n]) != "oom 2\n" {
		t.Errorf("the host read %q", b[:n])
	}
	_ = host.Close()
	before := time.Now()
	s.send(pressure.OOM{Kills: 3})
	if s.fd != -1 || s.redial != firstRedial || s.next.Sub(before).Round(time.Second) != firstRedial {
		t.Errorf("a host gone left %+v", s)
	}
	s.send(pressure.OOM{Kills: 4}) // nothing to send it to, and nothing breaks
}

func TestMemoryEventsAreTheTopCgroups(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"system.slice", "user.slice", "init.scope/x"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"system.slice/memory.events", "user.slice/memory.events", "init.scope/x/memory.events"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("oom_kill 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := memoryEvents(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimPrefix(f.Name(), root+"/"))
		_ = f.Close()
	}
	if strings.Join(names, " ") != "system.slice/memory.events user.slice/memory.events" {
		t.Errorf("watched %v", names)
	}
	if _, err := memoryEvents(t.TempDir()); err == nil || !strings.Contains(err.Error(), "has memory.events") {
		t.Errorf("a hierarchy with none: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "user.slice/memory.events"), 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, err := memoryEvents(root); err == nil {
			t.Error("a memory.events that cannot be opened was watched")
		}
	}
}

func TestOOMKillsAreVmstatsCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmstat")
	for text, want := range map[string]string{
		"pgfault 9\noom_kill 3\nfoo 1\n": "",
		"pgfault 9\n":                    "has no oom_kill",
		"oom_kill x\n":                   "is not a count",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		n, err := oomKills(path)
		switch {
		case want == "" && (err != nil || n != 3):
			t.Errorf("%q: %d, %v", text, n, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: %v, not %q", text, err, want)
		}
	}
	if _, err := oomKills(filepath.Join(dir, "none")); err == nil {
		t.Error("no vmstat read")
	}
}
