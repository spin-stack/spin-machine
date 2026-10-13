// SPDX-License-Identifier: Apache-2.0

package boot_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// A checkpoint the release before saved resumes on this tree's machine, when the two have one
// fingerprint: machine.Spec.Fingerprint promises it, by leaving out the QEMU binary, the kernel
// and the firmware a boot reads, and this is what holds the promise to QEMU's. The guest is
// booted on the previous release's QEMU, kernel and firmware, over an overlay of its base image,
// with a counter printing on its console; it is saved, and restored with this tree's QEMU over
// the same overlay, and the counter has to go on from where it was and the disk has to answer.
//
// When the two fingerprints differ, the contract moved and no checkpoint of the previous release
// is meant to resume: the test says so and holds nothing, and the release notes say it for the
// fleet (hack/fingerprint-diff).
//
//	SPIN_COMPAT_PREVIOUS=<dir>   the previous release's usr/share/spin-stack (task boot:compat)
func TestACheckpointOfTheReleaseBeforeResumesHere(t *testing.T) {
	previous := os.Getenv("SPIN_COMPAT_PREVIOUS")
	if previous == "" {
		t.Skip("set SPIN_COMPAT_PREVIOUS to a release's usr/share/spin-stack: task boot:compat fetches the newest")
	}
	out := releaseDir(t)
	cli := filepath.Join(out, "bin", "spin-machine")
	before, here := fingerprintOf(t, cli, previous), fingerprintOf(t, cli, out)
	if before != here {
		t.Skipf("the machine's contract moved (%s, then %s): no checkpoint of %s is meant to resume here", before, here, previous)
	}

	dir := t.TempDir()
	overlay := filepath.Join(dir, "root.qcow2")
	if b, err := exec.Command(filepath.Join(out, "bin", "qemu-img"), "create", "-q", "-f", "qcow2", //nolint:gosec // this tree's qemu-img
		"-b", filepath.Join(previous, "image", "rootfs.qcow2"), "-F", "qcow2", overlay).CombinedOutput(); err != nil {
		t.Fatalf("an overlay of the previous base image: %v: %s", err, b)
	}
	disk := []string{"--disk", overlay, "--disk-format", "qcow2", "--init", "/bin/sh"}
	state := filepath.Join(dir, "state")

	// The previous release's machine, a counter on its console, saved.
	a := startGuest(t, machineArgs(t, cli, previous, append([]string{"--qmp", filepath.Join(dir, "a.sock")}, disk...)))
	a.type_(t, "mount -t proc proc /proc; ( i=0; while :; do i=$((i+1)); echo TICK-$i; sleep 0.2; done ) &")
	a.await(t, regexp.MustCompile(`TICK-5\b`), 60*time.Second)
	qa := dial(t, filepath.Join(dir, "a.sock"), &vm{})
	qa.do(t, "stop", nil)
	last := a.lastTick()
	qa.do(t, "migrate", map[string]any{"uri": "file:" + state})
	qa.until(t, "query-migrate", "completed", 60*time.Second)
	qa.close()
	a.kill()

	// This tree's machine, loading it.
	b := startGuest(t, machineArgs(t, cli, out, append([]string{"--qmp", filepath.Join(dir, "b.sock"), "--incoming", "defer"}, disk...)))
	qb := dial(t, filepath.Join(dir, "b.sock"), &vm{})
	defer qb.close()
	qb.do(t, "migrate-incoming", map[string]any{"uri": "file:" + state})
	qb.until(t, "query-migrate", "completed", 60*time.Second)
	qb.do(t, "cont", nil)
	b.await(t, regexp.MustCompile(fmt.Sprintf(`TICK-%d\b`, last+3)), 30*time.Second)
	// Read through the disk the guest had before it was saved: a file it never opened, so
	// the read reaches the restored virtio-blk and the overlay behind it.
	b.type_(t, "echo READ-$(wc -c < /etc/os-release)")
	b.await(t, regexp.MustCompile(`READ-[1-9][0-9]*`), 30*time.Second)
	rel, err := machine.OpenRelease(previous)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a checkpoint of %s, saved at TICK-%d, went on counting on this tree's machine", rel.Version(), last)
}

// fingerprintOf is `spin-machine fingerprint` of the release at dir, as this tree computes it.
func fingerprintOf(t *testing.T, cli, dir string) string {
	t.Helper()
	b, err := exec.Command(cli, "fingerprint", "--release", dir).Output() //nolint:gosec // this tree's spin-machine
	if err != nil {
		t.Fatalf("spin-machine fingerprint --release %s: %v", dir, err)
	}
	var f struct{ Fingerprint string }
	if err := json.Unmarshal(b, &f); err != nil || f.Fingerprint == "" {
		t.Fatalf("spin-machine fingerprint: %q: %v", b, err)
	}
	return f.Fingerprint
}

// guest is one QEMU whose console is its stdin and stdout.
type guest struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex
	out   bytes.Buffer
}

func startGuest(t *testing.T, args []string) *guest {
	t.Helper()
	g := &guest{cmd: exec.Command(args[0], args[1:]...)} //nolint:gosec // the machine's own command line
	stdin, err := g.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	g.stdin = stdin
	r, w := io.Pipe()
	g.cmd.Stdout, g.cmd.Stderr = w, w
	if err := g.cmd.Start(); err != nil {
		t.Fatalf("starting QEMU: %v", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			g.mu.Lock()
			g.out.Write(buf[:n])
			g.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(g.kill)
	return g
}

// type_ is a line on the console. The guest's shell is not reading it when QEMU starts, so the
// first bytes are lost: blank lines go first, each costing only a prompt.
func (g *guest) type_(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(g.stdin, "\n\n\n\n\n\n\n\n\n\n"+line+"\n"); err != nil {
		t.Fatalf("typing on the console: %v", err)
	}
}

func (g *guest) said() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.out.String()
}

// await is the console printing what matches re, or the test failing with what it did print.
func (g *guest) await(t *testing.T, re *regexp.Regexp, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !re.MatchString(g.said()) {
		if time.Now().After(deadline) {
			t.Fatalf("the console never said %s in %s:\n%s", re, timeout, tail([]byte(g.said()), 2000))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lastTick is the highest TICK the console has printed.
func (g *guest) lastTick() int {
	last := 0
	for _, m := range regexp.MustCompile(`TICK-(\d+)`).FindAllStringSubmatch(g.said(), -1) {
		if n, _ := strconv.Atoi(m[1]); n > last {
			last = n
		}
	}
	return last
}

func (g *guest) kill() {
	if g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
		_ = g.cmd.Wait()
	}
}
