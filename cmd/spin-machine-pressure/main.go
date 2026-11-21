// SPDX-License-Identifier: Apache-2.0

// spin-machine-pressure is the guest's half of the pressure probe: it arms PSI triggers on the
// guest's CPU, memory and IO, watches the guest's OOM kills, and writes a line to the host over
// vsock each time one fires (internal/pressure has the format, machine.PressureListener the host's
// half). It runs in every guest from spin-machine-pressure.service, after the first login's path,
// at the lowest priority the guest has (image/.../spin-machine-pressure.service).
//
// It wakes only when the kernel says something happened: no timer, no wake in an idle guest. While
// the host is not listening, or after a checkpoint's restore has reset the connection, it dials
// again with a backoff, and the hello it sends then carries the OOM kills it could not report.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spin-stack/machine/internal/pressure"
)

const (
	// firstRedial and lastRedial bound the wait between two dials of a host that is not there.
	firstRedial = time.Second
	lastRedial  = 30 * time.Second
	// writeWait bounds a write to a host that stopped reading: the triggers wait while it blocks.
	writeWait = time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spin-machine-pressure:", err)
		os.Exit(1)
	}
}

func run() error {
	var triggers []pressure.Trigger
	for _, r := range pressure.Resources() {
		t, err := pressure.Arm(filepath.Join("/proc/pressure", r.String()), r)
		if err != nil {
			return err
		}
		triggers = append(triggers, t)
	}
	events, err := memoryEvents("/sys/fs/cgroup")
	if err != nil {
		return err
	}
	s := sender{fd: -1}
	kills, err := oomKills("/proc/vmstat")
	if err != nil {
		return err
	}
	fds := make([]unix.PollFd, 0, len(triggers)+len(events)+1)
	for {
		fds = fds[:0]
		for _, t := range triggers {
			fds = append(fds, unix.PollFd{Fd: int32(t.File.Fd()), Events: unix.POLLPRI})
		}
		for _, f := range events {
			fds = append(fds, unix.PollFd{Fd: int32(f.Fd()), Events: unix.POLLPRI})
		}
		timeout := -1
		if s.fd >= 0 {
			fds = append(fds, unix.PollFd{Fd: int32(s.fd), Events: unix.POLLIN})
		} else {
			// Rounded up: a wait cut to 0 ms would spin until the dial is due.
			timeout = int((max(0, time.Until(s.next)) + time.Millisecond - 1) / time.Millisecond)
		}
		if _, err := unix.Poll(fds, timeout); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("waiting for the guest's pressure: %w", err)
		}
		if s.fd < 0 && !time.Now().Before(s.next) {
			s.dial(kills)
		}
		for i, t := range triggers {
			if fds[i].Revents&unix.POLLPRI == 0 {
				continue
			}
			if fds[i].Revents&unix.POLLERR != 0 {
				return fmt.Errorf("the %s trigger ended", t.Resource)
			}
			total, err := t.Total()
			if err != nil {
				return err
			}
			s.send(pressure.Stall{Resource: t.Resource, Threshold: pressure.Threshold, Window: pressure.Window, Total: total})
		}
		woke := false
		for i, f := range events {
			if fds[len(triggers)+i].Revents&unix.POLLPRI == 0 {
				continue
			}
			// A kernfs file polls as changed until it is read again: unread, every poll after
			// the first change returned at once, and the sender spun.
			if _, err := f.ReadAt(make([]byte, 512), 0); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("reading %s: %w", f.Name(), err)
			}
			woke = true
		}
		if woke {
			n, err := oomKills("/proc/vmstat")
			if err != nil {
				return err
			}
			if n != kills {
				kills = n
				s.send(pressure.OOM{Kills: kills})
			}
		}
		// The host writes nothing: anything readable on the connection is its end.
		if s.fd >= 0 && len(fds) > len(triggers)+len(events) && fds[len(fds)-1].Revents != 0 {
			s.drop()
		}
	}
}

// sender is the connection to the host, or the time to dial it again.
type sender struct {
	fd     int
	next   time.Time
	redial time.Duration
}

func (s *sender) dial(kills uint64) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		err = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: pressure.Port})
		if err == nil {
			tv := unix.NsecToTimeval(writeWait.Nanoseconds())
			err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
		}
		if err != nil {
			_ = unix.Close(fd)
		}
	}
	if err != nil {
		s.redial = min(max(2*s.redial, firstRedial), lastRedial)
		s.next = time.Now().Add(s.redial)
		return
	}
	s.fd, s.redial = fd, 0
	s.send(pressure.Hello{OOMKills: kills})
}

// send writes l to the host, and drops the connection when the host does not take it.
func (s *sender) send(l pressure.Line) {
	if s.fd < 0 {
		return
	}
	// A part of a line would run into the next one, which the host refuses: a short write is a
	// failed one.
	b := l.Append(nil)
	if n, err := unix.Write(s.fd, b); err != nil || n != len(b) {
		s.drop()
	}
}

func (s *sender) drop() {
	_ = unix.Close(s.fd)
	s.fd = -1
	s.redial = firstRedial
	s.next = time.Now().Add(firstRedial)
}

// memoryEvents opens memory.events of every cgroup at the top of the hierarchy under root. Every
// OOM kill, the kernel's own as well as a cgroup's, counts in the victim's cgroup and each one
// above it, and notifies the file (memcg_memory_event_mm in mm/oom_kill.c); the root has no
// memory.events, so its children's stand for it. A cgroup made at the top after the sender started
// is not watched, and its kills are reported with the next one elsewhere.
func memoryEvents(root string) ([]*os.File, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "memory.events"))
	if err != nil {
		return nil, fmt.Errorf("listing the guest's cgroups: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no cgroup under %s has memory.events", root)
	}
	files := make([]*os.File, 0, len(paths))
	for _, p := range paths {
		// #nosec G304 -- a cgroup's memory.events
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", p, err)
		}
		files = append(files, f)
	}
	return files, nil
}

// oomKills is the guest's oom_kill since its boot, from vmstat, which is /proc/vmstat.
func oomKills(vmstat string) (uint64, error) {
	b, err := os.ReadFile(vmstat) // #nosec G304 -- /proc/vmstat
	if err != nil {
		return 0, fmt.Errorf("reading the guest's OOM kills: %w", err)
	}
	for line := range bytes.Lines(b) {
		if v, ok := bytes.CutPrefix(line, []byte("oom_kill ")); ok {
			n, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("reading the guest's OOM kills: %q is not a count", line)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("reading the guest's OOM kills: %s has no oom_kill", vmstat)
}
