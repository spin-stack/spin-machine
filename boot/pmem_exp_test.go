// SPDX-License-Identifier: Apache-2.0

package boot_test

// EXPERIMENT (exp/pmem-base, not for merge): the base image on virtio-pmem with DAX, against the
// qcow2 chain, for the sandbox survey's fifth experiment. Run as TestPageCache's "pmem" subtest,
// on the lab's qemu_run (a QEMU with virtio-pmem-pci) and kernel_run (FS_DAX), variant instead.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/boot"
)

// pmemWorkload reads the base image's /usr twice - what a build reads of the toolchain - and
// says at each step what the guest caches. On the qcow2 chain the reads fill the guest's page
// cache, which is the guest's RAM, which is QEMU's anonymous memory; on pmem with DAX they map the
// host's page cache of the one base file, shared by every VM, and the guest caches none of it.
const pmemWorkload = `#!/bin/sh
say() { echo "PMEM $1 $(awk '/^Cached:/{c=$2} /^MemFree:/{m=$2} END{print c, m}' /proc/meminfo) $2" > /dev/ttyS0; sleep 4; }
ms() { awk '{ printf "%d", $1 * 1000 }' /proc/uptime; }
usr() { find /usr -xdev -type f -size -2M 2>/dev/null | head -40000 | xargs cat > /dev/null 2>&1; }
echo "PMEM-NOTE $(findmnt -no SOURCE,FSTYPE,OPTIONS / | tr '\n' ' '); $(findmnt -no SOURCE,OPTIONS /mnt/oldroot 2>/dev/null)" > /dev/ttyS0
# Whether the pmem region itself is slow, and how the guest maps it: a 256 MB read of the raw
# device, the region in /proc/iomem, and the uncached and write-combining ranges PAT holds.
mountpoint -q /sys/kernel/debug || mount -t debugfs debugfs /sys/kernel/debug
echo "PMEM-NOTE raw $(dd if=/dev/pmem0 of=/dev/null bs=1M count=256 2>&1 | tail -1); iomem $(grep -i -E 'pmem|persistent' /proc/iomem | tr -s ' ' | tr '\n' ' '); pat $(grep -i '0x00000001[0-3]' /sys/kernel/debug/x86/pat_memtype_list 2>/dev/null | head -4 | tr '\n' ' '); pat_enabled $(grep -c . /sys/kernel/debug/x86/pat_memtype_list 2>/dev/null) entries, $(grep -o 'x86/PAT.*' /dev/null; dmesg | grep -i -m2 'PAT' | tr '\n' ' ')" > /dev/ttyS0
sync; echo 3 > /proc/sys/vm/drop_caches
say 0 0
t=$(ms); usr; say 1 $(( $(ms) - t ))
t=$(ms); usr; say 2 $(( $(ms) - t ))
echo 3 > /proc/sys/vm/drop_caches; sleep 6; say 3 0
echo PMEM-DONE > /dev/ttyS0
`

var pmemSteps = []string{"idle", "/usr read", "/usr read again", "guest dropped its cache"}

// overlayInit is PID 1 on the pmem root: the writable layer on vda under an overlayfs whose lower
// layer is the DAX root, pivoted into, and then systemd.
const overlayInit = `#!/bin/sh
fail() { echo "PMEM-FAILED overlay-init: $*" > /dev/ttyS0; exec /bin/sh; }
mount -t ext4 /dev/vda /mnt/upper || fail "mounting vda"
mkdir -p /mnt/upper/u /mnt/upper/w
mount -t overlay overlay -o lowerdir=/,upperdir=/mnt/upper/u,workdir=/mnt/upper/w /mnt/root || fail "overlayfs"
mkdir -p /mnt/root/mnt/oldroot
cd /mnt/root && pivot_root . mnt/oldroot || fail "pivot_root"
exec /sbin/init "$@"
`

var pmemLine = regexp.MustCompile(`PMEM (\d) (\d+) (\d+) (\d+)`)

type pmemStep struct {
	ms, guestCache, guestFree, rssAnon, rssFile, baseCache float64
}

// pmemCompare boots the workload on the qcow2 chain and on the pmem base, reps times each,
// interleaved, and logs each step's p50.
func pmemCompare(t *testing.T, out string, reps int) {
	// A run that failed between its mount and its umount left the mount behind (run
	// 36807084946): unmounted here, before anything of this run is made.
	mustRun(t, "sudo", "sh", "-c", `findmnt -rno TARGET | grep /TestPageCachepmem | while read -r m; do umount "$m"; done; true`)
	dir := t.TempDir()
	base := filepath.Join(out, "image", "rootfs.qcow2")
	qemuImg := filepath.Join(out, "bin", "qemu-img")

	// Both variants boot the same files: an overlay of the base with the workload and the
	// overlay-init in it, which the qcow2 variant boots as it is and the pmem variant flattens
	// into a raw image - a copy, the base is never written - padded to 2 MiB.
	edit := without()
	edit.setup = `mkdir -p "$MNT/mnt/upper" "$MNT/mnt/root" && chmod 0755 "$MNT/sbin/overlay-init"`
	edit.files["/usr/local/sbin/pmem.sh"] = pmemWorkload
	edit.files["/sbin/overlay-init"] = overlayInit
	edit.files["/etc/systemd/system/pmem.service"] = strings.ReplaceAll(cacheUnit, "pagecache.sh", "pmem.sh")
	edit.links = map[string]string{"/etc/systemd/system/multi-user.target.wants/pmem.service": "../pmem.service"}
	lower := filepath.Join(dir, "lower.qcow2")
	mustRun(t, qemuImg, "create", "-f", "qcow2", "-F", "qcow2", "-b", base, lower)
	editOverlay(t, lower, edit)
	raw := filepath.Join(dir, "base.raw")
	mustRun(t, qemuImg, "convert", "-O", "raw", lower, raw)
	fi, err := os.Stat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(raw, (fi.Size()+(2<<20)-1)&^((2<<20)-1)); err != nil {
		t.Fatal(err)
	}
	// The same tree as erofs, uncompressed so DAX can map it: built from the raw image mounted
	// read-only, by mkfs.erofs in a container, since the runner's host has no erofs-utils.
	mnt := t.TempDir()
	mustRun(t, "sudo", "mount", "-o", "loop,ro", raw, mnt)
	t.Cleanup(func() { _ = exec.Command("sudo", "umount", mnt).Run() }) // before TempDir's RemoveAll
	mustRun(t, "docker", "run", "--rm", "-v", mnt+":/src:ro", "-v", dir+":/out", "alpine:3.22",
		"sh", "-c", "apk add -q erofs-utils && mkfs.erofs /out/base.erofs /src >/dev/null && mkfs.erofs -E noinline_data /out/noinline.erofs /src >/dev/null")
	erofs := filepath.Join(dir, "base.erofs")
	noinline := filepath.Join(dir, "noinline.erofs")
	for _, img := range []string{erofs, noinline} {
		mustRun(t, "sudo", "sh", "-c", fmt.Sprintf(`s=$(stat -c %%s %[1]s); truncate -s $(( (s + 2097151) / 2097152 * 2097152 )) %[1]s && chmod 0644 %[1]s`, img))
	}

	// Run 36811489671 read /dev/pmem0 at 8.3 MB/s whatever the filesystem: how QEMU maps the
	// file is the question now, so ext4 only, in each mapping.
	// Run 36816626039: QEMU maps the region as RAM in a KVM memslot, in every mapping, and it
	// still reads at 8.3 MB/s. Whether the guest maps it uncached is what nopat says: with PAT
	// off the guest cannot ask for anything but write-back.
	variants := []string{"qcow2 chain", "pmem ext4 DAX, share=on,readonly=on", "pmem ext4 DAX nopat, share=on,readonly=on"}
	got := map[string][][]pmemStep{}
	var notes = map[string]string{}
	for range reps {
		for _, v := range variants {
			steps, note := pmemBoot(t, out, v, lower, raw, erofs, noinline)
			got[v] = append(got[v], steps)
			notes[v] = note
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n/usr read twice in a 2048 MB guest, p50 over %d boots; MB but the I/O's ms.\n", reps)
	fmt.Fprintf(&b, "host: QEMU's anonymous resident memory (the guest's RAM, private to this VM) and file-backed (pmem's mapping, shared), and the base's pages in the host's cache\n\n")
	for _, v := range variants {
		fmt.Fprintf(&b, "%s\n%-26s %8s %8s %8s %9s %9s %9s\n", v, "STEP", "I/O ms", "G.CACHE", "G.FREE", "RSS ANON", "RSS FILE", "BASE")
		for i, name := range pmemSteps {
			var col [6][]float64
			for _, run := range got[v] {
				s := run[i]
				for j, x := range []float64{s.ms, s.guestCache, s.guestFree, s.rssAnon, s.rssFile, s.baseCache} {
					col[j] = append(col[j], x)
				}
			}
			fmt.Fprintf(&b, "%-26s", name)
			for j, c := range col {
				p, _ := boot.Percentile(c, 0.5)
				w := 8
				if j >= 3 {
					w = 9
				}
				fmt.Fprintf(&b, " %*.0f", w, p)
			}
			fmt.Fprintln(&b)
		}
		fmt.Fprintf(&b, "  %s\n\n", notes[v])
	}
	t.Log(b.String())
}

// pmemBoot runs the workload once on variant v and measures each step on the host.
func pmemBoot(t *testing.T, out, v, lower, raw, erofs, noinline string) ([]pmemStep, string) {
	t.Helper()
	dir := t.TempDir()
	qemuImg := filepath.Join(out, "bin", "qemu-img")
	sockDir, err := os.MkdirTemp("/tmp", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) }) // a socket QEMU made
	qmpSock := filepath.Join(sockDir, "q.sock")
	args := []string{"boot", "--release", out, "--memory", "2048", "--cpus", "2", "--console", "file:/dev/stdout", "--qmp", qmpSock}
	cached := raw
	fstype := "ext4"
	_, opts, _ := strings.Cut(v, ", ")
	extra := ""
	if strings.Contains(v, "nopat") {
		extra = " nopat"
	}
	switch v {
	case "pmem + erofs DAX":
		cached, fstype = erofs, "erofs"
	case "pmem + erofs DAX, no inline data":
		cached, fstype = noinline, "erofs"
	}
	if v == "qcow2 chain" {
		overlay := filepath.Join(dir, "overlay.qcow2")
		mustRun(t, qemuImg, "create", "-f", "qcow2", "-F", "qcow2", "-b", lower, overlay)
		args = append(args, "--disk", overlay, "--append", "init=/sbin/init")
		cached = filepath.Join(out, "image", "rootfs.qcow2")
	} else {
		upper := filepath.Join(dir, "upper.raw")
		if err := os.WriteFile(upper, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(upper, 2<<30); err != nil {
			t.Fatal(err)
		}
		mustRun(t, filepath.Join(out, "bin", "mkfs.ext4"), "-q", "-F", upper)
		args = append(args, "--pmem", cached, "--pmem-opts", opts, "--disk", upper, "--disk-format", "raw", "--root", "/dev/pmem0",
			"--append", "init=/sbin/overlay-init ro rootfstype="+fstype+" rootflags=dax=always"+extra)
	}
	cmd := exec.Command(filepath.Join(out, "bin", "spin-machine"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the machine: %v", err)
	}
	kill := func() {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	timer := time.AfterFunc(time.Duration(envInt(t, "CACHE_TIMEOUT_S", 300))*time.Second, kill)
	defer func() { timer.Stop(); kill(); _ = cmd.Wait() }()

	pid := cmd.Process.Pid
	steps := make([]pmemStep, len(pmemSteps))
	seen, note := 0, ""
	var console strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		console.WriteString(line + "\n")
		if strings.Contains(line, "PMEM-DONE") {
			break
		}
		if strings.Contains(line, "PMEM-FAILED") {
			t.Fatalf("%s: %s\n%s", v, line, tail([]byte(console.String()), 1500))
		}
		if _, n, ok := strings.Cut(line, "PMEM-NOTE "); ok {
			note += n + "; "
			continue
		}
		m := pmemLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		s := &steps[i]
		s.guestCache, s.guestFree, s.ms = kb(m[2]), kb(m[3]), num(m[4])
		// How many times the guest left for QEMU to emulate an access, idle and after the
		// first read: a region KVM does not map as memory is one exit per load.
		if i <= 1 {
			note += fmt.Sprintf("step %d kvm %s; ", i, kvmExits(pid))
		}
		if i == 1 {
			note += qemuSays(t, qmpSock) + "; "
		}
		if s.rssAnon, s.rssFile, err = rssSplit(pid); err != nil {
			t.Fatal(err)
		}
		if s.baseCache, _, err = resident(cached); err != nil {
			t.Fatal(err)
		}
		seen++
	}
	if seen != len(steps) {
		t.Fatalf("%s: the guest reported %d of %d steps; console tail:\n%s", v, seen, len(steps), tail([]byte(console.String()), 1500))
	}
	return steps, note
}

// kvmExits is the VM's exit counters from KVM's debugfs, which only root reads.
func kvmExits(pid int) string {
	out, _ := exec.Command("sudo", "sh", "-c", fmt.Sprintf(`cd /sys/kernel/debug/kvm/%d-* 2>/dev/null && for f in exits mmio_exits io_exits halt_exits; do printf "%%s=%%s " $f "$(cat $f 2>/dev/null)"; done`, pid)).Output()
	return strings.TrimSpace(string(out))
}

// qemuSays is QEMU's own account of the pmem region: the flattened memory tree's lines for it,
// which say ram or i/o, and the memory devices.
func qemuSays(t *testing.T, socket string) string {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return "no QMP: " + err.Error()
	}
	defer func() { _ = conn.Close() }() // a diagnostic connection
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	q := &qmp{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
	var greeting struct{ QMP *struct{} }
	if err := q.dec.Decode(&greeting); err != nil {
		return "no QMP greeting: " + err.Error()
	}
	q.do(t, "qmp_capabilities", nil)
	var says []string
	for _, cmd := range []string{"info mtree -f", "info memory-devices"} {
		var text string
		_ = json.Unmarshal(q.do(t, "human-monitor-command", map[string]any{"command-line": cmd}), &text)
		for l := range strings.Lines(text) {
			if strings.Contains(strings.ToLower(l), "pmem") {
				says = append(says, strings.Join(strings.Fields(l), " "))
			}
		}
	}
	return "qemu: " + strings.Join(says, " | ")
}

// rssSplit is the process pid's anonymous and file-backed resident memory in MB.
func rssSplit(pid int) (anon, file float64, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, err
	}
	for l := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(l, "RssAnon:"); ok {
			anon = kb(strings.Fields(v)[0])
		}
		if v, ok := strings.CutPrefix(l, "RssFile:"); ok {
			file = kb(strings.Fields(v)[0])
		}
	}
	return anon, file, nil
}
