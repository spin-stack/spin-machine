// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// A QMP conversation against a monitor that answers from a script: one reply per
// command the client sends, each reply one or more lines, events included.
//
// Every row also asks whether the client let the monitor go. A monitor accepts one
// client at a time, so a connection left open after a failed handshake is a monitor
// the next command cannot reach.
func TestQMP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []string
		// silent is a monitor that accepted and never greets: one whose single
		// connection is somebody else's.
		silent bool
		// do is what the client does once the handshake is through.
		do      func(*qmpConn) error
		wantErr string
	}{{
		// QEMU's refusal in its own words. It was printed as a Go map —
		// "map[class:GenericError desc:...]" — at the moment somebody most needs to
		// read it.
		name: "a refusal names its class and what went wrong",
		replies: []string{
			`{"return": {}}`,
			`{"error": {"class": "GenericError", "desc": "Bus 'scsi0.0' not found"}}`,
		},
		do: func(q *qmpConn) error {
			return q.run("device_add", map[string]any{"id": "hd0"}, nil)
		},
		wantErr: "device_add: GenericError: Bus 'scsi0.0' not found",
	}, {
		// A DEVICE_DELETED that arrives before device_del's own reply is kept, or
		// detach waits out its timeout for an event it already read.
		name: "an event before the reply is not lost",
		replies: []string{
			`{"return": {}}`,
			`{"event": "DEVICE_DELETED", "data": {"device": "hd0"}}` + "\n" + `{"return": {}}`,
		},
		do: func(q *qmpConn) error {
			if err := q.run("device_del", map[string]any{"id": "hd0"}, nil); err != nil {
				return err
			}
			return q.waitDeleted("hd0", time.Second)
		},
	}, {
		name: "a return value is decoded",
		replies: []string{
			`{"return": {}}`,
			`{"return": {"status": "failed", "error-desc": "No space left on device"}}`,
		},
		do: func(q *qmpConn) error {
			var m struct {
				Status    string `json:"status"`
				ErrorDesc string `json:"error-desc"`
			}
			if err := q.run("query-migrate", nil, &m); err != nil {
				return err
			}
			return errors.New(m.Status + ": " + m.ErrorDesc)
		},
		wantErr: "failed: No space left on device",
	}, {
		// x-ignore-shared skips only shared RAM: a template of anything else writes its
		// memory into the state and restores nothing a template restore expects.
		name: "a template of RAM that is not shared is refused before it stops the VM",
		replies: []string{
			`{"return": {}}`,
			`{"return": [{"id": "pc.ram", "share": false, "size": 536870912}]}`,
		},
		do:      freeze,
		wantErr: "the VM's RAM is not a shared pc.ram",
	}, {
		name:    "a refused handshake",
		replies: []string{`{"error": {"class": "CommandNotFound", "desc": "not in this mode"}}`},
		wantErr: "qmp_capabilities: CommandNotFound: not in this mode",
	}, {
		// A monitor serves one client and queues the next without a word. Waiting on
		// its greeting without a deadline was a command that hung forever and said
		// nothing.
		name:    "a monitor that never greets",
		silent:  true,
		wantErr: "no greeting within",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client, monitor := net.Pipe()
			released := make(chan error, 1)
			if tc.silent {
				go func() {
					// Holds the connection until the client lets it go.
					_, err := io.Copy(io.Discard, monitor)
					_ = monitor.Close()
					released <- err
				}()
			} else {
				go func() {
					_, err := serve(monitor, tc.replies)
					released <- err
				}()
			}

			q, err := newQMP(client, 100*time.Millisecond)
			if err == nil && tc.do != nil {
				err = tc.do(q)
			}
			if q != nil {
				_ = q.Close()
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
			if err := <-released; err != nil {
				t.Error(err)
			}
		})
	}
}

type qmpRequest struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments"`
}

// serve is the scripted monitor: a greeting, then one reply per request read. It
// returns what it was asked once the client has closed the connection, and an error
// if the client is still holding it a second after the script ran out.
func serve(c net.Conn, replies []string) ([]qmpRequest, error) {
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, `{"QMP": {"version": {}, "capabilities": []}}`+"\n"); err != nil {
		return nil, err
	}
	var asked []qmpRequest
	r := bufio.NewReader(c)
	for _, reply := range replies {
		line, err := r.ReadString('\n')
		if err != nil {
			return asked, err
		}
		var req qmpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			return asked, err
		}
		asked = append(asked, req)
		if _, err := io.WriteString(c, reply+"\n"); err != nil {
			return asked, err
		}
	}
	// A pipe the client has already closed refuses a deadline with ErrClosedPipe,
	// which is the answer being waited for.
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); errors.Is(err, io.ErrClosedPipe) {
		return asked, nil
	} else if err != nil {
		return asked, err
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		return asked, errors.New("the client kept the monitor after the conversation ended")
	}
	return asked, nil
}

// How a command ends. Only main ends the process: a command that exited on its own
// took the test binary with it, and skipped whatever its caller would have done next.
func TestHowACommandEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func() error
		// is reports whether the error is the one this row expects.
		is func(error) bool
	}{{
		name: "help is asked for, not a failure",
		do:   func() error { return run([]string{"save", "-h"}) },
		is:   func(err error) bool { return errors.Is(err, flag.ErrHelp) },
	}, {
		name: "an unknown command is an error",
		do:   func() error { return run([]string{"frobnicate"}) },
		is:   func(err error) bool { return err != nil && !errors.Is(err, flag.ErrHelp) },
	}, {
		// Returned once, naming the command, and not also printed by the flag package
		// under a usage listing.
		name: "a flag nobody defined names its command",
		do:   func() error { return run([]string{"save", "--nope"}) },
		is: func(err error) bool {
			return err != nil && strings.HasPrefix(err.Error(), "save: flag provided but not defined")
		},
	}, {
		// Refused before a monitor is dialled: the socket here does not exist, so an
		// error about it would mean the target was never looked at.
		name: "a target no machine has",
		do: func() error {
			return run([]string{"attach", "--qmp", "/nonexistent", "--disk", "d", "--target", "999"})
		},
		is: func(err error) bool { return err != nil && strings.Contains(err.Error(), "target 999") },
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(); !tc.is(err) {
				t.Errorf("returned %v (%T)", err, err)
			}
		})
	}
}

// boot becomes QEMU. Run as a child of this test, because a process that execs is not
// there afterwards to report: the child's pid is the one QEMU writes down, and its exit
// status is QEMU's, with nothing in between to lose either.
func TestBootBecomesQEMU(t *testing.T) {
	if qemu := os.Getenv("SPIN_MACHINE_TEST_QEMU"); qemu != "" {
		dir := filepath.Dir(qemu)
		err := boot(machine.Spec{QEMU: qemu, Kernel: qemu, Firmware: dir, BootCPUs: 1,
			Memory: machine.Memory{SizeMB: 64}}, false)
		// Reached only if the exec failed.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(100)
	}

	dir := t.TempDir()
	qemu := filepath.Join(dir, "qemu")
	pidFile := filepath.Join(dir, "pid")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nexit 7\n"
	if err := os.WriteFile(qemu, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBootBecomesQEMU$")
	cmd.Env = append(os.Environ(), "SPIN_MACHINE_TEST_QEMU="+qemu)
	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("boot ended with %v, want QEMU's own exit status 7", err)
	}
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(pid)), strconv.Itoa(cmd.Process.Pid); got != want {
		t.Errorf("QEMU ran as pid %s and spin-machine as %s: a child, not an exec", got, want)
	}
}
