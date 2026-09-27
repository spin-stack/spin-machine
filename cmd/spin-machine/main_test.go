// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
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
			`{"error": {"class": "GenericError", "desc": "Bus 'rp0' not found"}}`,
		},
		do: func(q *qmpConn) error {
			return q.run("device_add", map[string]any{"id": "rp0-disk"}, nil)
		},
		wantErr: "device_add: GenericError: Bus 'rp0' not found",
	}, {
		// A DEVICE_DELETED that arrives before device_del's own reply is kept, or
		// detach waits out its timeout for an event it already read.
		name: "an event before the reply is not lost",
		replies: []string{
			`{"return": {}}`,
			`{"event": "DEVICE_DELETED", "data": {"device": "rp0-disk"}}` + "\n" + `{"return": {}}`,
		},
		do: func(q *qmpConn) error {
			if err := q.run("device_del", map[string]any{"id": "rp0-disk"}, nil); err != nil {
				return err
			}
			return q.waitDeleted("rp0-disk", time.Second)
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
		name:    "a refused handshake",
		replies: []string{`{"error": {"class": "CommandNotFound", "desc": "not in this mode"}}`},
		wantErr: "qmp_capabilities: CommandNotFound: not in this mode",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client, monitor := net.Pipe()
			released := make(chan error, 1)
			go func() { released <- serve(monitor, tc.replies) }()

			q, err := newQMP(client)
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

// serve is the scripted monitor: a greeting, then one reply per request read. It
// returns nil once the client has closed the connection, and an error if the client
// is still holding it a second after the script ran out.
func serve(c net.Conn, replies []string) error {
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, `{"QMP": {"version": {}, "capabilities": []}}`+"\n"); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	for _, reply := range replies {
		if _, err := r.ReadString('\n'); err != nil {
			return err
		}
		if _, err := io.WriteString(c, reply+"\n"); err != nil {
			return err
		}
	}
	// A pipe the client has already closed refuses a deadline with ErrClosedPipe,
	// which is the answer being waited for.
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); errors.Is(err, io.ErrClosedPipe) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		return errors.New("the client kept the monitor after the conversation ended")
	}
	return nil
}

// How a command ends. Only main ends the process: a command that exited on its own
// took the test binary with it, and skipped whatever its caller would have done next.
func TestHowACommandEnds(t *testing.T) {
	// A QEMU that exits with a status of its own, as one that refused its arguments
	// does after saying why.
	dir := t.TempDir()
	qemu := filepath.Join(dir, "qemu")
	if err := os.WriteFile(qemu, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failing := machine.Spec{QEMU: qemu, Kernel: qemu, Firmware: dir, BootCPUs: 1, Memory: machine.Memory{SizeMB: 64}}

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
		name: "QEMU's exit status is passed on as it was",
		do:   func() error { return boot(failing, false) },
		is: func(err error) bool {
			var code exitCode
			return errors.As(err, &code) && code == 7
		},
	}, {
		name: "an unknown command is an error",
		do:   func() error { return run([]string{"frobnicate"}) },
		is: func(err error) bool {
			var code exitCode
			return err != nil && !errors.As(err, &code) && !errors.Is(err, flag.ErrHelp)
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(); !tc.is(err) {
				t.Errorf("returned %v (%T)", err, err)
			}
		})
	}
}
