// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The floor: what a kernel costs before anything is configured into it.
//
// Removing one thing at a time from the release config has found nothing four times running
// — CONFIG_VT, nineteen udev rule files, two device-count boot parameters, smaller TCP hash
// tables — and each answer cost a build or a bench run. This asks the question from the other
// end. If the release kernel reaches the same marker in about what a kernel with almost
// nothing in it does, then the config is not where the kernel phase goes and no amount of
// subtracting will find it. If the gap is large, the gap is the budget, and it is worth
// knowing its size before hunting inside it.
//
// The marker is a caller-supplied diagnostic initrd's, not a login prompt, because the floor
// kernel has no ext4, no virtio and no cgroups: it cannot mount this machine's base image
// and cannot run systemd. A comparison needs something both kernels can reach, and that
// bounds what this measures — the kernel phase and the launch around it, not a boot.
//
//	SPIN_FLOOR_PROBE=1     run at all
//	SPIN_PROBE_INITRD=<cpio>  the diagnostic initrd, as for task boot:firmware
//	REPS=<n>               boots per kernel (default 20)
//
// The floor kernel is read from _output/kernel-minimal/vmlinux, where `task kernel:minimal`
// writes it, and SPIN_KERNEL_FLOOR overrides that.
func TestKernelFloor(t *testing.T) {
	if os.Getenv("SPIN_FLOOR_PROBE") == "" {
		t.Skip("set SPIN_FLOOR_PROBE=1: boots dozens of VMs and needs a kernel built outside the release")
	}
	out := releaseDir(t)
	reps := envInt(t, "REPS", 20)

	floor := firmwareFile(t, "SPIN_KERNEL_FLOOR",
		filepath.Join(out, "kernel-minimal", "vmlinux"))
	p := &probe{
		out:      out,
		firmware: filepath.Join(out, "qemu"),
		kernel:   filepath.Join(out, "kernel", "vmlinux"),
		initrd:   envFile(t, "SPIN_PROBE_INITRD"),
		bios:     map[string]string{"seabios": filepath.Join(out, "qemu", "bios-256k.bin")},
		dir:      t.TempDir(),
	}

	// Same firmware, same initrd, same machine line: one argument differs. vmgenid stays on
	// because it is on the machine this is asking about, and a floor kernel that cannot see
	// the table still boots past it.
	rows := []struct{ label, kernel, initrd, cpu string }{
		{label: "release", kernel: p.kernel},
		{label: "floor", kernel: floor},
	}
	// The initrd's own decompression is inside every interval here, so it is a constant on
	// both sides and does not move the difference the floor is for. It is worth one row of its
	// own anyway: kernel/Dockerfile requires RD_LZ4 because unpacking the archive is one of the
	// largest single items in kernel boot, and this says what that choice is worth on the same
	// machine rather than on the general claim.
	if lz4 := os.Getenv("SPIN_PROBE_INITRD_LZ4"); lz4 != "" {
		rows = append(rows, struct{ label, kernel, initrd, cpu string }{
			label: "release+lz4", kernel: p.kernel, initrd: envFile(t, "SPIN_PROBE_INITRD_LZ4")})
	}
	// A named CPU model instead of the host's, which the machine already supports for a
	// different reason: `-cpu host` shows the guest this host's silicon, so a template cannot
	// move between machines, and Spec.Identity hashes the host CPU only in that case. What it
	// costs at boot was never measured, and QEMU has to enumerate the host's CPUID and build
	// the guest's from it either way — this says whether the migratable choice is also the
	// cheaper one.
	if cpu := os.Getenv("SPIN_CPU_MODEL"); cpu != "" {
		rows = append(rows, struct{ label, kernel, initrd, cpu string }{
			label: "named cpu", kernel: p.kernel, cpu: cpu})
	}
	for _, r := range rows {
		if st, err := os.Stat(r.kernel); err == nil {
			t.Logf("%-12s kernel %.1f MiB, initrd %s, cpu %s", r.label, float64(st.Size())/(1<<20),
				filepath.Base(orElse(p.initrd, r.initrd)), orElse("host", r.cpu))
		}
	}

	samples := map[string][]float64{}
	for range reps {
		for _, r := range rows {
			v := p.start(t, "seabios", withVMGenID, "", r.kernel, r.initrd, r.cpu)
			ms, err := v.wait("SPIN-READY", 15*time.Second)
			v.close()
			if err != nil {
				t.Fatalf("%s: %v", r.label, err)
			}
			samples[r.label] = append(samples[r.label], ms)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nmilliseconds from the moment before QEMU is exec'd to the initrd's "+
		"marker, p50/p95 over %d boots\n\n", reps)
	fmt.Fprintf(&b, "%-12s %10s %10s\n", "ROW", "P50", "P95")
	for _, r := range rows {
		s := append([]float64(nil), samples[r.label]...)
		sort.Float64s(s)
		fmt.Fprintf(&b, "%-12s %10.2f %10.2f\n", r.label, pct(s, 50), pct(s, 95))
	}
	rel := append([]float64(nil), samples["release"]...)
	flr := append([]float64(nil), samples["floor"]...)
	sort.Float64s(rel)
	sort.Float64s(flr)
	// Stated rather than left to the reader, because it is the number the next decision turns
	// on: it is the whole of what this machine's kernel configuration could ever give back.
	fmt.Fprintf(&b, "\nthe release kernel's configuration costs %.2f ms at p50\n",
		pct(rel, 50)-pct(flr, 50))
	t.Log(b.String())
}
