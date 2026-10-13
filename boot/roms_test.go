// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// The fingerprint sizes one ROM, the BIOS, and names the other read-only blocks the machine's
// own (machine.BIOSBlock, machine.MachineBlocks): a resume refuses a ROM block of another
// length, so a ROM a file sizes and the fingerprint does not is a checkpoint that matches a
// machine it cannot be loaded into. This starts the machine `spin-machine args` describes, at
// its smallest and at a workspace's whole shape, paused before the guest runs, and holds every
// read-only block QEMU mapped to those two lists, and the BIOS block to its file's size. A QEMU
// bump or a device that maps another ROM fails here, and is answered in machine.go.
func TestEveryROMIsTheFingerprints(t *testing.T) {
	if os.Getenv("SPIN_ROM_TEST") == "" {
		t.Skip("set SPIN_ROM_TEST=1: this starts the machine")
	}
	out := releaseDir(t)
	bios, err := os.Stat(filepath.Join(out, "qemu", machine.DefaultBIOS))
	if err != nil {
		t.Fatal(err)
	}
	for name, flags := range map[string][]string{
		"smallest":        nil,
		"workspace-sized": {"--max-memory", "8192", "--max-cpus", "8", "--hotplug-disks", "2", "--vsock-cid", "5"},
	} {
		t.Run(name, func(t *testing.T) {
			ro := readOnlyBlocks(t, ramBlocks(t, out, flags...))
			if got := ro[machine.BIOSBlock]; got != bios.Size() {
				t.Errorf("%s is %d bytes and %s is %d: the fingerprint sizes the BIOS by its file", machine.BIOSBlock, got, machine.DefaultBIOS, bios.Size())
			}
			for block := range ro {
				if block != machine.BIOSBlock && !slices.Contains(machine.MachineBlocks, block) {
					t.Errorf("QEMU maps a read-only block %s the fingerprint does not account for: size it there, or name it a machine block with why", block)
				}
			}
			for _, block := range machine.MachineBlocks {
				if _, ok := ro[block]; !ok {
					t.Errorf("machine.MachineBlocks names %s, which this machine no longer maps", block)
				}
			}
		})
	}
}

// readOnlyBlocks is `info ramblock`'s read-only blocks by name, each at its used length.
func readOnlyBlocks(t *testing.T, table string) map[string]int64 {
	t.Helper()
	ro := map[string]int64{}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		// Block Name, PSize as a number and a unit, Offset, Used, Total, HVA, RO.
		if len(f) != 8 || f[7] != "ro" {
			continue
		}
		used, err := strconv.ParseInt(strings.TrimPrefix(f[4], "0x"), 16, 64)
		if err != nil {
			t.Fatalf("info ramblock's used length %q: %v", f[4], err)
		}
		ro[f[0]] = used
	}
	if len(ro) == 0 {
		t.Fatalf("no read-only block in info ramblock:\n%s", table)
	}
	return ro
}

// ramBlocks is QEMU's `info ramblock` of the machine `spin-machine args` describes, started paused:
// every block of memory the machine maps, by name, before the guest runs a byte.
func ramBlocks(t *testing.T, out string, flags ...string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "qmp.sock")
	args := machineArgs(t, filepath.Join(out, "bin", "spin-machine"), out,
		append([]string{"--qmp", socket, "--init", "/bin/sh", "--console", ""}, flags...))
	cmd := exec.Command(args[0], append(args[1:], "-S")...) //nolint:gosec // the machine's own command line
	log, err := os.CreateTemp(t.TempDir(), "qemu-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting QEMU: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	said := func() []byte { b, _ := os.ReadFile(log.Name()); return b }
	q := dial(t, socket, &vm{out: said()})
	defer q.close()
	var text string
	if err := json.Unmarshal(q.do(t, "human-monitor-command", map[string]any{"command-line": "info ramblock"}), &text); err != nil {
		t.Fatalf("reading info ramblock: %v", err)
	}
	return text
}
