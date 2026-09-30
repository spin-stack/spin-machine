// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKernelPagesWritten asks what a guest writes of the kernel image QEMU loads into its memory:
// page by page, the loaded segments as they stand once the machine is at a login prompt, against
// the ELF they were copied from, grouped by the section each page belongs to.
//
// It decides whether mapping the kernel into guest memory could replace QEMU's copy of it (6.7 ms
// of every start, measured by boot:firmware-stages on 2026-09-30). A mapped page the guest never
// writes costs nothing; one it writes costs a copy-on-write fault, dearer page for page than the
// bulk copy it replaced. The kernel writes its own text as it boots - alternatives, static keys,
// ftrace's call sites - so the answer is in how many pages that touches, and where.
//
// Measured 2026-09-30 on v20260930.01 (7.2.8): 53% of the 8749 loaded pages written, .text 86%
// of its 3670 (return thunks and ENDBR sealing reach nearly every function), .data 55%; .rodata
// 5%, ORC 6% and .BTF none. So the kernel is copied, not mapped: what no guest writes is the
// ~12.7 MB of read-only tables, and the cheaper way to stop copying those is not loading them
// (BTF as a module) rather than a mapping shared between VMs.
//
//	SPIN_KERNEL_PAGES=1   run at all
//	FLAGS=<flags>         the machine (default: a workspace's shape)
//
// Needs /dev/kvm and a built release; no sudo.
func TestKernelPagesWritten(t *testing.T) {
	if os.Getenv("SPIN_KERNEL_PAGES") == "" {
		t.Skip("set SPIN_KERNEL_PAGES=1: this boots a VM and reads its memory back")
	}
	out := releaseDir(t)
	kernel := filepath.Join(out, "kernel", "vmlinux")
	f, err := elf.Open(kernel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	dir, err := os.MkdirTemp("", "kpages")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, console := filepath.Join(dir, "qmp"), filepath.Join(dir, "console")
	flags := strings.Fields(orElse("--memory 512 --max-memory 8192 --cpus 1 --max-cpus 16", os.Getenv("FLAGS")))
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), append([]string{"boot", "--release", out,
		"--qmp", socket, "--console", "file:" + console}, flags...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	end := time.Now().Add(60 * time.Second)
	for {
		b, _ := os.ReadFile(console)
		if bytes.Contains(b, []byte("login:")) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("no login prompt within 60 s:\n%s", tail(b, 3000))
		}
		time.Sleep(200 * time.Millisecond)
	}
	q := dial(t, socket, &vm{})
	defer q.close()
	// Stopped, so what is read is one moment of the guest.
	q.do(t, "stop", nil)

	const page = 4096
	counts := map[string][2]int{} // section group: pages, pages written
	var total, written int
	for i, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		dump := filepath.Join(dir, fmt.Sprintf("seg%d", i))
		q.do(t, "pmemsave", map[string]any{"val": p.Paddr, "size": p.Filesz, "filename": dump})
		mem, err := os.ReadFile(dump)
		if err != nil {
			t.Fatal(err)
		}
		file := make([]byte, p.Filesz)
		if _, err := p.ReadAt(file, 0); err != nil {
			t.Fatal(err)
		}
		for off := uint64(0); off < p.Filesz; off += page {
			n := min(page, p.Filesz-off)
			group := sectionGroup(f, p.Vaddr+off)
			c := counts[group]
			c[0]++
			total++
			if !bytes.Equal(mem[off:off+n], file[off:off+n]) {
				c[1]++
				written++
			}
			counts[group] = c
		}
	}

	var groups []string
	for g := range counts {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return counts[groups[i]][0] > counts[groups[j]][0] })
	var b strings.Builder
	fmt.Fprintf(&b, "\nthe kernel's loaded pages once the guest is at a login prompt, against %s\n\n", filepath.Base(kernel))
	fmt.Fprintf(&b, "%-22s %8s %8s %8s\n", "SECTIONS", "PAGES", "WRITTEN", "SHARE")
	for _, g := range groups {
		c := counts[g]
		fmt.Fprintf(&b, "%-22s %8d %8d %7.1f%%\n", g, c[0], c[1], 100*float64(c[1])/float64(c[0]))
	}
	fmt.Fprintf(&b, "%-22s %8d %8d %7.1f%%\n", "all", total, written, 100*float64(written)/float64(total))
	t.Log(b.String())
}

// sectionGroup names the part of the kernel a virtual address is in: its section, with the
// sections that behave alike together.
func sectionGroup(f *elf.File, addr uint64) string {
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_ALLOC == 0 || addr < s.Addr || addr >= s.Addr+s.Size {
			continue
		}
		switch {
		case strings.HasPrefix(s.Name, ".init") || strings.HasPrefix(s.Name, ".exit") || strings.HasPrefix(s.Name, ".altinstr") ||
			s.Name == ".smp_locks" || s.Name == ".parainstructions" || strings.HasPrefix(s.Name, ".retpoline") ||
			strings.HasPrefix(s.Name, ".return_sites") || strings.HasPrefix(s.Name, ".call_sites") || strings.HasPrefix(s.Name, ".ibt"):
			return "init and patch sites"
		case strings.HasPrefix(s.Name, ".orc_"):
			return ".orc_unwind*"
		default:
			return s.Name
		}
	}
	return "between sections"
}
