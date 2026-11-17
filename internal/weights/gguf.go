// gguf.go: digesting one GGUF file (the llama.cpp path — /props names the
// file the server loaded). Tens of GB hash in minutes, so the digest is
// memoized in ~/.remote/weights-cache.json keyed by abs path + size +
// mtime: a re-probe, restart or re-serve of the same file answers
// instantly, a changed file re-hashes. The hashing itself never runs on
// the startup path — serve calls HashFile from a goroutine and publishes
// the id when it lands (§2.4).
package weights

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// ggufBuf is the hashing read buffer — big enough to care, small enough
// to not notice.
const ggufBuf = 1 << 20

// ErrNotRegular marks a path that exists but is not a plain file.
var ErrNotRegular = errors.New("not a regular file")

// GGUFHasher is the persistent digest memo. A missing or corrupt cache
// file is an empty cache — the worst case is one extra hash. Safe for
// concurrent use (the background hashers race nothing but each other).
type GGUFHasher struct {
	mu      sync.Mutex
	path    string // "" = memory only (tests)
	entries map[string]ggufEntry
	// inflight dedupes concurrent hashes of one file (path+size+mtime):
	// a re-probe while a 40 GB hash runs waits for it instead of reading
	// the file a second time.
	inflight map[string]*ggufCall
}

type ggufCall struct {
	done chan struct{}
	hex  string
	err  error
}

// ggufCacheFile is the on-disk shape (the hasher's entries are unexported
// and guarded by mu, so marshaling goes through this DTO).
type ggufCacheFile struct {
	Entries map[string]ggufEntry `json:"entries"`
}

type ggufEntry struct {
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // unix seconds
	Hex   string `json:"hex"`
}

// LoadGGUFHasher reads the cache at path (typically ~/.remote/weights-cache.json).
func LoadGGUFHasher(path string) *GGUFHasher {
	h := &GGUFHasher{path: path, entries: map[string]ggufEntry{}, inflight: map[string]*ggufCall{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return h // first run
	}
	var f ggufCacheFile
	if json.Unmarshal(b, &f) == nil && f.Entries != nil {
		h.entries = f.Entries
	}
	return h
}

func ggufKey(file string, fi os.FileInfo) (string, bool) {
	abs, err := filepath.Abs(file)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return abs + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().Unix(), 10), true
}

// Cached returns the memoized digest for file as it sits on disk right
// now; not-ok when unreadable, irregular, or not cached under this
// path+size+mtime.
func (h *GGUFHasher) Cached(file string) (string, bool) {
	fi, err := os.Stat(file)
	if err != nil {
		return "", false
	}
	key, ok := ggufKey(file, fi)
	if !ok {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	return e.Hex, ok
}

// HashFile digests file (1 MiB buffer), stores the digest under the
// path+size+mtime it hashed, and persists the cache. The file must exist
// and be regular.
func (h *GGUFHasher) HashFile(file string) (string, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	key, ok := ggufKey(file, fi)
	if !ok {
		return "", ErrNotRegular
	}
	h.mu.Lock()
	if e, ok := h.entries[key]; ok {
		h.mu.Unlock()
		return e.Hex, nil
	}
	if c, ok := h.inflight[key]; ok {
		h.mu.Unlock()
		<-c.done
		return c.hex, c.err
	}
	c := &ggufCall{done: make(chan struct{})}
	h.inflight[key] = c
	h.mu.Unlock()

	c.hex, c.err = h.hash(file, key, fi)
	h.mu.Lock()
	delete(h.inflight, key)
	h.mu.Unlock()
	close(c.done)
	return c.hex, c.err
}

// hash reads and digests one file, then memoizes and persists the result.
func (h *GGUFHasher) hash(file, key string, fi os.FileInfo) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, f, make([]byte, ggufBuf)); err != nil {
		return "", err
	}
	hex := hex.EncodeToString(digest.Sum(nil))

	h.mu.Lock()
	h.entries[key] = ggufEntry{Size: fi.Size(), Mtime: fi.ModTime().Unix(), Hex: hex}
	h.saveLocked()
	h.mu.Unlock()
	return hex, nil
}

// saveLocked persists under mu (callers hold it).
func (h *GGUFHasher) saveLocked() {
	if h.path == "" {
		return
	}
	b, err := json.Marshal(ggufCacheFile{Entries: h.entries})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(h.path, b, 0o600)
}
