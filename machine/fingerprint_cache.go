// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// FingerprintCache reuses artifact hashes across VM creations in one process.
// Its zero value is ready to use, and calls may run concurrently. Keep one cache
// for the lifetime of the launcher; a cache created for each VM saves no work.
//
// Every call opens and stats each artifact. Device, inode, size, modification
// time and change time must all match before its hash is reused. The complete
// machine identity (including host CPU when applicable) is recomputed each time.
// Files modified within the last second are hashed without caching: ordinary
// Linux filesystems can give consecutive writes identical coarse timestamps.
//
// This is for local release files that remain unchanged while VMs use them.
// Metadata is not proof of content against a privileged writer or a filesystem
// that fails to update timestamps: use Spec.Fingerprint for an uncached content
// check. Neither method makes changing artifacts during VM creation safe.
// Do not copy a cache after its first use.
type FingerprintCache struct {
	mu    sync.Mutex
	files map[string]artifactHash
}

type artifactHash struct {
	info os.FileInfo
	sum  string
}

// Do not cache a hash taken inside the timestamp's own resolution window.
// Otherwise a second write in that window can leave all cache keys unchanged.
// A full second covers local filesystems with second-resolution timestamps as
// well as Linux's coarse clock ticks. This does not make remote timestamps or
// a clock that jumps backwards reliable content identifiers.
const artifactTimestampWindow = time.Second

func artifactOldEnough(info os.FileInfo, now time.Time) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	cutoff := now.Add(-artifactTimestampWindow)
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).Before(cutoff) &&
		time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).Before(cutoff)
}

// Fingerprint returns exactly the same identity as Spec.Fingerprint, reusing
// only hashes of artifacts whose metadata has not changed.
func (c *FingerprintCache) Fingerprint(s Spec) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.fingerprint(c.fileSum)
}

func sameArtifact(a, b os.FileInfo) bool {
	x, xok := a.Sys().(*syscall.Stat_t)
	y, yok := b.Sys().(*syscall.Stat_t)
	return xok && yok && os.SameFile(a, b) && a.Size() == b.Size() &&
		x.Mtim == y.Mtim && x.Ctim == y.Ctim
}

func (c *FingerprintCache) fileSum(path string) (string, error) {
	// Decide eligibility before hashing, not after: a long read must not promote
	// a hash taken while timestamps could still collide into a reusable entry.
	started := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular artifact file", path)
	}
	cacheable := artifactOldEnough(before, started)
	if cached, ok := c.files[path]; ok && cacheable && sameArtifact(cached.info, before) {
		return cached.sum, nil
	}
	delete(c.files, path)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !sameArtifact(before, after) {
		return "", fmt.Errorf("%s changed while hashing", path)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !cacheable {
		return sum, nil
	}
	if c.files == nil {
		c.files = make(map[string]artifactHash)
	}
	c.files[path] = artifactHash{info: after, sum: sum}
	return sum, nil
}
