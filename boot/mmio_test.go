// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Who the guest's exits to QEMU are, counted: every PCI configuration access by device and
// direction, and every MMIO and port access by the memory region QEMU dispatched it to, from a
// launch to the login prompt, from QEMU's own trace (pci_cfg_read/write,
// memory_region_ops_read/write).
//
// boot:exits says how many exits each phase has and the bench fleet says what one costs -
// about 20 us for an MMIO exit there, against a few on a laptop (2026-11-12). This says whose
// they are, which is what a change to the kernel or to QEMU acts on. Counts, not times, so it
// answers the same on any host.
//
// QEMU itself also writes to some of these regions (an MSI it raises through kvm-apic-msi), so
// a region's count is accesses, not only exits; PCI configuration and the devices' BARs are the
// guest's.
//
//	SPIN_BOOT_MMIO=1    run at all
//	SPIN_KERNEL_B=<v>   a second kernel, counted beside the release's
//	TOP=<n>             regions to list (default 20)
func TestMMIOCensus(t *testing.T) {
	if os.Getenv("SPIN_BOOT_MMIO") == "" {
		t.Skip("set SPIN_BOOT_MMIO=1: boots a VM with QEMU tracing every device access")
	}
	out := releaseDir(t)
	top := envInt(t, "TOP", 20)
	cli := filepath.Join(out, "bin", "spin-machine")

	kernels := []struct{ label, path string }{{"release", ""}}
	if k := kernelB(t); k != "" {
		kernels = append(kernels, struct{ label, path string }{"kernel B", k})
	}
	counts := map[string]map[string]int{}
	var keys []string
	for _, k := range kernels {
		// Without --disk the base is under QEMU's -snapshot, so nothing is written to it and no
		// overlay needs sudo. The getty is agetty, whose second only adds what an idle guest does.
		flags := []string{"--memory", "2048", "--cpus", "2"}
		if k.path != "" {
			flags = append(flags, "--kernel", k.path)
		}
		c := mmioCensus(t, machineArgs(t, cli, out, flags))
		counts[k.label] = c
		for key := range c {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\naccesses from a launch to the login prompt, by QEMU's trace\n\n  %-52s", "")
	for _, k := range kernels {
		fmt.Fprintf(&b, " %10s", k.label)
	}
	b.WriteString("\n")
	row := func(key string) {
		fmt.Fprintf(&b, "  %-52s", key)
		for _, k := range kernels {
			fmt.Fprintf(&b, " %10d", counts[k.label][key])
		}
		b.WriteString("\n")
	}
	// Configuration accesses by device first, then regions by how busy they were on the release.
	slices.SortFunc(keys, func(a, c string) int {
		ca, cc := strings.HasPrefix(a, "config "), strings.HasPrefix(c, "config ")
		if ca != cc {
			if ca {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(counts["release"][c], counts["release"][a]), cmp.Compare(a, c))
	})
	n := 0
	for _, key := range keys {
		if strings.HasPrefix(key, "config ") || n < top {
			if !strings.HasPrefix(key, "config ") {
				n++
			}
			row(key)
		}
	}
	t.Log(b.String())
}

var (
	// "… pci_cfg_read virtio-rng-pci 00:03.0 @0x6 -> 0x10"
	rePCICfg = regexp.MustCompile(`pci_cfg_(read|write) (\S+) (\S+) @`)
	// "… memory_region_ops_read cpu 0 mr 0x… addr 0xcfc value 0x… size 4 name 'pci-conf-data'"
	reMROps = regexp.MustCompile(`memory_region_ops_(read|write) .* name '([^']*)'`)
)

// mmioCensus boots QEMU with args, its device accesses traced, until the login prompt, and
// counts them: "config <device> <read|write>" and "region <name>".
func mmioCensus(t *testing.T, args []string) map[string]int {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "trace")
	full := append(slices.Clone(args[1:]), "-D", log)
	for _, ev := range []string{"pci_cfg_read", "pci_cfg_write", "memory_region_ops_read", "memory_region_ops_write"} {
		full = append(full, "-trace", "enable="+ev)
	}
	cmd := exec.Command(args[0], full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching QEMU: %v", err)
	}
	_ = w.Close()
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = r.Close()
	}()
	var console strings.Builder
	buf := make([]byte, 65536)
	deadline := time.Now().Add(90 * time.Second)
	for !strings.Contains(console.String(), "login:") {
		if err := r.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		n, err := r.Read(buf)
		console.Write(buf[:n])
		if err != nil {
			t.Fatalf("no login prompt: %v\n%s", err, tail([]byte(console.String()), 2000))
		}
	}
	// QEMU writes its trace as it goes; what is in the file at the prompt is the boot.
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	c := map[string]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if m := rePCICfg.FindStringSubmatch(line); m != nil {
			// An empty slot is a vendor ID read or two, and there are dozens: one row.
			if m[2] == "empty" {
				m[3] = "slots"
			}
			c["config "+m[2]+" "+m[3]+" "+m[1]]++
		} else if m := reMROps.FindStringSubmatch(line); m != nil {
			c["region "+m[2]]++
		}
	}
	if len(c) == 0 {
		t.Fatalf("the trace counted nothing: QEMU without its log trace backend, or a format this does not read; its start:\n%s",
			tail(raw[:min(len(raw), 2000)], 2000))
	}
	return c
}
