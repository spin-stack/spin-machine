// SPDX-License-Identifier: Apache-2.0

// jobwatch records what PID 1 says about its jobs while a guest boots, as they happen: the
// live half of task boot:waits, which reads systemd's timestamps after the fact. It is what
// bootvine (codeberg.org/wvhulle/bootvine) reads to draw its tree, without the tree.
//
// What it sees that the timestamps do not keep: when a job went from waiting to running
// (systemd dispatched it), how each job ended, and jobs queued after the initial transaction
// - socket and path activation, the first login's - with the moment they were queued.
//
// It talks to PID 1 on /run/systemd/private, which exists from PID 1's first moment and before
// any bus daemon; systemd sends every signal to every connection on that socket, Subscribe or
// not, and Subscribe is called anyway because a systemd that changed that would otherwise send
// nothing and this would report a boot with no jobs. Each event is stamped with
// CLOCK_MONOTONIC, systemd's own clock, when it is received, and kept in memory: writing to
// the serial console during the boot is a VM exit per byte inside what is being measured. The
// whole record is written once, after the boot.
//
// Diagnostic input, never part of a release: boot/jobwatch_probe_test.go builds it into a
// boot's own overlay.
//
//	jobwatch <console> <linger> <after-unit>
//
// writes the record to console once startup has finished, linger has passed since, and
// after-unit (another probe's writer, which shares the console) has gone inactive.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: jobwatch <console> <linger> <after-unit>")
		os.Exit(2)
	}
	linger, err := time.ParseDuration(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "jobwatch: linger %q: %v\n", os.Args[2], err)
		os.Exit(2)
	}
	rec, err := watch(linger, os.Args[3])
	// What was recorded is written even when watching failed: a record that ends early says
	// where it ended, and the error is its last line.
	if err != nil {
		rec = append(rec, fmt.Sprintf("%d E %v", now(), err))
	}
	if err := os.WriteFile(os.Args[1], []byte("SPIN-JOBS-BEGIN\n"+strings.Join(rec, "\n")+"\nSPIN-JOBS-END\n"), 0); err != nil {
		fmt.Fprintf(os.Stderr, "jobwatch: writing %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// now is CLOCK_MONOTONIC in microseconds, which is what systemd's *TimestampMonotonic are.
func now() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano() / 1000
}

func watch(linger time.Duration, after string) ([]string, error) {
	var rec []string
	add := func(format string, a ...any) { rec = append(rec, fmt.Sprintf("%d ", now())+fmt.Sprintf(format, a...)) }

	conn, err := dbus.Dial("unix:path=/run/systemd/private")
	if err != nil {
		return rec, fmt.Errorf("connecting to /run/systemd/private: %w", err)
	}
	defer conn.Close()
	// No Hello: there is no bus daemon on this socket to say it to.
	if err := conn.Auth(nil); err != nil {
		return rec, fmt.Errorf("authenticating to PID 1: %w", err)
	}
	// Large, because a channel godbus finds full is a signal lost, and a lost JobRemoved is a
	// job that reads as never finished.
	signals := make(chan *dbus.Signal, 1<<14)
	conn.Signal(signals)

	manager := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	// A message that arrives in the same read as the handshake's BEGIN is left unread by PID 1
	// until something else arrives on the socket, so the first message is a ping nobody waits
	// for and the second is the one that matters. Without it Subscribe never returned and the
	// boot finished around it (2026-11-12); bootvine works around the same thing with libdbus.
	if err := manager.
		Go("org.freedesktop.DBus.Peer.Ping", dbus.FlagNoReplyExpected, nil).Err; err != nil {
		return rec, fmt.Errorf("pinging PID 1: %w", err)
	}
	if err := manager.Call("org.freedesktop.systemd1.Manager.Subscribe", 0).Err; err != nil {
		return rec, fmt.Errorf("subscribing to PID 1: %w", err)
	}
	add("C")

	// The jobs queued before this connected, which is the initial transaction: when they were
	// queued is not known, only that it was before now.
	var jobs [][]any
	if err := manager.Call("org.freedesktop.systemd1.Manager.ListJobs", 0).Store(&jobs); err != nil {
		return rec, fmt.Errorf("listing PID 1's jobs: %w", err)
	}
	for _, j := range jobs {
		add("L %v %v %v %v", j[0], j[1], j[2], j[3])
	}
	var finish uint64
	if err := manager.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.systemd1.Manager", "FinishTimestampMonotonic").Store(&finish); err != nil {
		return rec, fmt.Errorf("reading FinishTimestampMonotonic: %w", err)
	}
	finished := finish != 0
	if finished {
		add("F already")
	}
	var lingerUntil <-chan time.Time
	if finished {
		lingerUntil = time.After(linger)
	}
	lingered, afterDone := false, false

	for !(lingered && afterDone) {
		select {
		case <-lingerUntil:
			lingered, lingerUntil = true, nil
		case s, ok := <-signals:
			if !ok {
				return rec, fmt.Errorf("PID 1 closed the connection")
			}
			switch s.Name {
			case "org.freedesktop.systemd1.Manager.JobNew":
				add("N %v %v", s.Body[0], s.Body[2])
			case "org.freedesktop.systemd1.Manager.JobRemoved":
				add("R %v %v %v", s.Body[0], s.Body[2], s.Body[3])
			case "org.freedesktop.systemd1.Manager.StartupFinished":
				add("F")
				if !finished {
					finished, lingerUntil = true, time.After(linger)
				}
			case "org.freedesktop.DBus.Properties.PropertiesChanged":
				iface, _ := s.Body[0].(string)
				changed, _ := s.Body[1].(map[string]dbus.Variant)
				p := string(s.Path)
				switch {
				case iface == "org.freedesktop.systemd1.Job":
					if v, ok := changed["State"]; ok {
						add("S %s %v", strings.TrimPrefix(p, "/org/freedesktop/systemd1/job/"), v.Value())
					}
				case iface == "org.freedesktop.systemd1.Unit":
					if v, ok := changed["ActiveState"]; ok {
						unit := unescape(strings.TrimPrefix(p, "/org/freedesktop/systemd1/unit/"))
						add("A %s %v", unit, v.Value())
						if unit == after && (v.Value() == "inactive" || v.Value() == "failed") {
							afterDone = true
						}
					}
				}
			}
		}
	}
	return rec, nil
}

// unescape reads a unit's name back from its object path, where systemd writes every byte that
// is not a letter or a digit as _ and two hex digits: dev-vda.device is dev_2dvda_2edevice.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '_' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
