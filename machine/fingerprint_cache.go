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
)

// FingerprintCache reuses artifact hashes across VM creations in one process.
// Its zero value is ready to use, and calls may run concurrently. Keep one cache
// for the lifetime of the launcher; a cache created for each VM saves no work.
//
// Every call opens and stats each artifact. Device, inode, size, modification
// time and change time must all match before its hash is reused. The complete
// machine identity (including host CPU when applicable) is recomputed each time.
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
	if cached, ok := c.files[path]; ok && sameArtifact(cached.info, before) {
		return cached.sum, nil
	}
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
	if c.files == nil {
		c.files = make(map[string]artifactHash)
	}
	c.files[path] = artifactHash{info: after, sum: sum}
	return sum, nil
}
