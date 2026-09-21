// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestFingerprintCacheTracksArtifactsAndShape(t *testing.T) {
	s := spec(t)
	s.CPU = "Broadwell-v4"
	var cache FingerprintCache
	check := func() string {
		t.Helper()
		want, err := s.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		got, err := cache.Fingerprint(s)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("cached %s != uncached %s", got, want)
		}
		return got
	}
	original := check()
	if check() != original {
		t.Fatal("cache hit changed identity")
	}
	info, err := os.Stat(s.Kernel)
	if err != nil {
		t.Fatal(err)
	}
	// Same length and restored mtime must not disguise different content.
	if err := os.WriteFile(s.Kernel, []byte("KERNEL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.Kernel, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if check() == original {
		t.Fatal("in-place change reused stale hash")
	}
	replacement := s.Kernel + ".new"
	if err := os.WriteFile(replacement, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, s.Kernel); err != nil {
		t.Fatal(err)
	}
	if check() != original {
		t.Fatal("replacement did not restore original content identity")
	}
	s.BootCPUs++
	if check() == original {
		t.Fatal("shape was cached")
	}
	s.Initrd = filepath.Join(t.TempDir(), "initrd")
	if err := os.WriteFile(s.Initrd, []byte("initrd"), 0600); err != nil {
		t.Fatal(err)
	}
	withInitrd := check()
	s.Initrd = ""
	if check() == withInitrd {
		t.Fatal("absent initrd reused hash")
	}
	if err := os.Remove(s.Kernel); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Fingerprint(s); err == nil {
		t.Fatal("missing artifact accepted from cache")
	}
}

// Force the exact condition from the reported failure: different content with
// metadata indistinguishable from a cached entry. This does not depend on the
// filesystem choosing to collide timestamps during this particular test run.
func TestFingerprintCacheRejectsRecentMetadataCollision(t *testing.T) {
	s := spec(t)
	s.CPU = "Broadwell-v4"

	// Seed precisely the metadata that the next stat will observe, but a stale
	// hash. Old code returns it; the recent-file policy must discard it.
	actual, err := os.Stat(s.Kernel)
	if err != nil {
		t.Fatal(err)
	}
	var cache FingerprintCache
	cache.files = map[string]artifactHash{s.Kernel: {info: actual, sum: "stale"}}
	want, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cache.Fingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("metadata collision reused stale hash: %s != %s", got, want)
	}
	if _, exists := cache.files[s.Kernel]; exists {
		t.Fatal("recent hash was retained for later reuse")
	}
}

type artifactInfoForTest struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i artifactInfoForTest) Sys() any { return i.stat }

func TestArtifactTimestampWindow(t *testing.T) {
	now := time.Unix(100, 0)
	for _, tt := range []struct {
		name         string
		mtime, ctime time.Time
		want         bool
	}{
		{"old", now.Add(-2 * time.Second), now.Add(-2 * time.Second), true},
		{"recent mtime", now, now.Add(-2 * time.Second), false},
		{"recent ctime", now.Add(-2 * time.Second), now, false},
		{"boundary", now.Add(-time.Second), now.Add(-time.Second), false},
		{"future", now.Add(time.Second), now.Add(time.Second), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := artifactInfoForTest{stat: &syscall.Stat_t{
				Mtim: syscall.NsecToTimespec(tt.mtime.UnixNano()),
				Ctim: syscall.NsecToTimespec(tt.ctime.UnixNano()),
			}}
			if got := artifactOldEnough(info, now); got != tt.want {
				t.Fatalf("eligible = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestFingerprintCacheConcurrent(t *testing.T) {
	s := spec(t)
	s.CPU = "Broadwell-v4"
	// Exercise concurrent cache population and hits on stable release files.
	time.Sleep(artifactTimestampWindow + time.Millisecond)
	want, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	var cache FingerprintCache
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.Fingerprint(s)
			if err != nil || got != want {
				t.Errorf("got %q, %v; want %q", got, err, want)
			}
		}()
	}
	wg.Wait()
	if len(cache.files) != 2 {
		t.Fatalf("cached %d artifacts; want 2", len(cache.files))
	}
}

func BenchmarkFingerprintArtifacts(b *testing.B) {
	dir := b.TempDir()
	s := Spec{CPU: "Broadwell-v4", BootCPUs: 2, Memory: Memory{SizeMB: 2048}}
	s.QEMU = filepath.Join(dir, "qemu")
	s.Kernel = filepath.Join(dir, "kernel")
	for _, p := range []string{s.QEMU, s.Kernel} {
		if err := os.WriteFile(p, make([]byte, 38<<20), 0600); err != nil {
			b.Fatal(err)
		}
	}
	time.Sleep(artifactTimestampWindow + time.Millisecond)
	b.Run("uncached", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.Fingerprint(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache FingerprintCache
		if _, err := cache.Fingerprint(s); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if _, err := cache.Fingerprint(s); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkFingerprintCachedHostCPU(b *testing.B) {
	dir := b.TempDir()
	artifact := filepath.Join(dir, "artifact")
	if err := os.WriteFile(artifact, make([]byte, 38<<20), 0600); err != nil {
		b.Fatal(err)
	}
	time.Sleep(artifactTimestampWindow + time.Millisecond)
	s := Spec{QEMU: artifact, Kernel: artifact, CPU: "host", BootCPUs: 2, Memory: Memory{SizeMB: 2048}}
	var cache FingerprintCache
	if _, err := cache.Fingerprint(s); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := cache.Fingerprint(s); err != nil {
			b.Fatal(err)
		}
	}
}
