// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// density is how a workspace's first login holds up with others already on the host: how many
// workspaces a host carries before the first number degrades, which is what turns a host's price
// per hour into one per workspace-hour (#273).
//
// Each row is a number of workspaces already running. A measurement boots that many from overlays
// of their own, waits for each to be logged into, lets them settle - idle, or each keeping its
// vCPUs busy with load=cpu - then boots one more and times its first login the way ssh-first
// does, and ends them all: the rows are interleaved, so none can leave its crowd to the next.
// What an idle workspace costs the host's memory is answered elsewhere (#278: about 165-170 MiB,
// nested and on metal alike).
type density struct{}

func (density) Name() string { return "density" }
func (density) About() string {
	return "ms from the moment before QEMU is exec'd to `true` run as spin over SSH, with the row's number of workspaces already running"
}

func (density) Options() []Option {
	return append(shape,
		Option{"crowds", "0 8 16 32", "the numbers of workspaces already running, a row each"},
		Option{"load", "idle", "what the running ones do: idle, or cpu (each vCPU busy)"},
		Option{"settle", "10", "seconds the crowd runs before the measured boot"})
}

func (density) Rows(o Options) []string { return o.Fields("crowds") }
func (density) Phases() int             { return 1 }

func (density) Needs(o Options) []Need {
	return append(append(bootNeeds, Need{"boot", "vsock-cid"}, Need{"boot", "console"}, Need{"boot", "append"}), plugNeeds(o)...)
}

func (density) Metrics() []Metric {
	return []Metric{
		{"first_login", "ms", "the measured workspace's `true` run as spin over SSH"},
		{"crowd_up", "ms", "the crowd booted and each logged into, before it settles"},
	}
}

// mutate-lane: TestEveryProbeMeasuresATree
func (density) Measure(_ context.Context, m *Measurement) error {
	n, err := strconv.Atoi(m.Row)
	if err != nil || n < 0 {
		return fmt.Errorf("a crowd of %q workspaces is not a number of them", m.Row)
	}
	t0 := time.Now()
	stop, err := m.bootCrowd(n, m.Options["load"])
	defer stop()
	if err != nil {
		return err
	}
	m.RecordMs("crowd_up", time.Since(t0))
	time.Sleep(time.Duration(m.Options.Int("settle")) * time.Second)

	mc, _, login, err := m.bootLoggedIn("measured.qcow2")
	if mc != nil {
		defer mc.Stop()
	}
	if err != nil {
		return err
	}
	m.RecordMs("first_login", login)
	m.Logf("crowd %d (%s): first login %v", n, m.Options["load"], login.Round(time.Millisecond))
	return nil
}

// bootCrowd boots n of the tree's workspaces, each logged into and, with load cpu, keeping its
// vCPUs busy, and returns what ends them, with any error for the ones that started. The caller ends
// them before its measurement returns, and not by m.Cleanup, which runs when the whole experiment
// has: a first run of density left every crowd up, and the rows measured a host carrying hundreds
// of machines (lab 44665519945, 2026-11-20).
// mutate-lane: TestEveryProbeMeasuresATree
func (m *Measurement) bootCrowd(n int, load string) (func(), error) {
	var crowd []*Machine
	stop := func() {
		for _, mc := range crowd {
			mc.Stop()
		}
	}
	for i := range n {
		mc, cid, _, err := m.bootLoggedIn(fmt.Sprintf("crowd%d.qcow2", i))
		if mc != nil {
			crowd = append(crowd, mc)
		}
		if err != nil {
			return stop, err
		}
		if load == "cpu" {
			// One busy loop a vCPU, left running in the background: nohup, so the session's end
			// does not take it.
			busy := `for c in $(seq "$(nproc)"); do nohup sh -c 'while :; do :; done' >/dev/null 2>&1 & done`
			if _, _, err := m.Key.Run(cid, xssh.KeyExchangeMLKEM768X25519, busy); err != nil {
				return stop, Miss("crowd workspace %d took no load: %v", i, err)
			}
		}
	}
	return stop, nil
}

// bootLoggedIn boots one more of the tree's workspaces, sized as spin sizes one (Plug), and returns
// it, for the caller to end, with its vsock CID and how long after the moment before its exec it was
// logged into. A machine that started is returned with any error.
// mutate-lane: TestEveryProbeMeasuresATree, whose density crowds boot real machines: an overlay a
// test can make here is edited through NBD, which only the VM lane has.
func (m *Measurement) bootLoggedIn(name string) (*Machine, uint32, time.Duration, error) {
	overlay, err := m.Overlay(name, m.Key.Edits()...)
	if err != nil {
		return nil, 0, 0, err
	}
	cid := NewCID()
	qmp := filepath.Join(m.Socks, name+".qmp")
	args := append([]string{"--vsock-cid", fmt.Sprint(cid), "--console", "file:" + m.Dir + "/" + name + ".console",
		"--append", m.Tree.Cmdline()}, m.qmpToPlug(qmp)...)
	t0 := time.Now()
	mc, err := m.Launch(nil, m.BootArgs(overlay, args...)...)
	if err != nil {
		return nil, 0, 0, err
	}
	plugged := m.Plug(qmp)
	login, err := m.Key.LoggedIn(mc, cid, t0)
	if err != nil {
		return mc, 0, 0, err
	}
	return mc, cid, login, plugged()
}
