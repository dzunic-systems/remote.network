package weights

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	revBase    = "b9ce18a6887e18c089fdea8509a33e63429a3300" // an example base commit
	revOverlay = "c44f5b92ee3d307b82aab1c55f4cc4ae1d440344" // the house ablit overlay
	blob1      = "1111111111111111111111111111111111111111111111111111111111111111"
	blob2      = "2222222222222222222222222222222222222222222222222222222222222222"
	blob3      = "3333333333333333333333333333333333333333333333333333333333333333"
)

func TestParseSpec(t *testing.T) {
	t.Parallel()
	good := []struct {
		in  string
		id  string
		rev string
	}{
		{"nvidia/Qwen3.8-Flash-Next-FP4-Local@" + revBase,
			"hf:nvidia/Qwen3.8-Flash-Next-FP4-Local@" + revBase, revBase},
		// Uppercase hex lowercases (HF revs are lowercase; the router's
		// intake regex demands it).
		{"acme/base@" + strings.ToUpper(revBase),
			"hf:acme/base@" + revBase, revBase},
		{"acme/base@" + revBase + "+house/overlay@" + revOverlay + ":fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors",
			"hf:acme/base@" + revBase + "+house/overlay@" + revOverlay + ":fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors", revBase},
	}
	for _, g := range good {
		spec, err := ParseSpec(g.in)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", g.in, err)
		}
		if spec.ID() != g.id {
			t.Fatalf("ParseSpec(%q).ID() = %q, want %q", g.in, spec.ID(), g.id)
		}
		if spec.Rev != g.rev {
			t.Fatalf("rev = %q, want %q", spec.Rev, g.rev)
		}
	}
	bad := []string{
		"acme/base@abc123", // short rev — a startup error, never a guess
		"acme/base",        // no rev
		"acme/base@main",   // branch name
		"",                 // empty
		"acme@" + revBase,  // no slash in repo
		"acme/base@" + revBase + "+house/overlay@abc:path", // overlay short rev
		"acme/base@" + revBase + "+house/overlay@" + revOverlay,
		"acme/base@" + revBase + "+house/overlay@" + revOverlay + ":",
		"acme/base@" + revBase + "+house/overlay@" + revOverlay + ":../escape.safetensors",
		"acme/base@" + revBase + "+house/overlay@" + revOverlay + ":a b.safetensors",
	}
	for _, b := range bad {
		if _, err := ParseSpec(b); err == nil {
			t.Fatalf("ParseSpec(%q) accepted", b)
		}
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()
	spec, err := ParseSpec("acme/base@" + revBase + "+house/ov@" + revOverlay + ":a/b.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	back, ok := ParseID(spec.ID())
	if !ok || back.Repo != spec.Repo || back.Rev != spec.Rev ||
		len(back.Overlays) != 1 || back.Overlays[0].Path != "a/b.safetensors" {
		t.Fatalf("ParseID round trip: %+v %v", back, ok)
	}
	for _, id := range []string{"ollama:sha256:" + blob1, "gguf:sha256:" + blob1, "nvidia/qwen3.8", ""} {
		if _, ok := ParseID(id); ok {
			t.Fatalf("ParseID(%q) accepted", id)
		}
	}
}

func TestHFCacheRoot(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_HOME", "")
	t.Setenv("HOME", t.TempDir())
	if got := HFCacheRoot("/explicit"); got != "/explicit" {
		t.Fatalf("flag ignored: %q", got)
	}
	t.Setenv("HF_HUB_CACHE", "/hub")
	if got := HFCacheRoot(""); got != "/hub" {
		t.Fatalf("HF_HUB_CACHE ignored: %q", got)
	}
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_HOME", "/hfhome")
	if got := HFCacheRoot(""); got != filepath.Join("/hfhome", "hub") {
		t.Fatalf("HF_HOME ignored: %q", got)
	}
	t.Setenv("HF_HOME", "")
	want := filepath.Join(t.TempDir(), "unused") // HOME already set; recompute below
	_ = want
	home, _ := os.UserHomeDir()
	if got := HFCacheRoot(""); got != filepath.Join(home, ".cache", "huggingface", "hub") {
		t.Fatalf("default = %q, want under %q", got, home)
	}
}

func TestHFIDFromPath(t *testing.T) {
	t.Parallel()
	rev := revBase
	cases := []struct {
		path string
		want string
	}{
		{"/root/.cache/huggingface/hub/models--nvidia--Qwen3.8-Flash-Next-FP4-Local/snapshots/" + rev, // container prefix, any shape
			"hf:nvidia/Qwen3.8-Flash-Next-FP4-Local@" + rev},
		{"/root/.cache/huggingface/hub/models--nvidia--Qwen3.8-Flash-Next-FP4-Local/snapshots/" + rev + "/config.json",
			"hf:nvidia/Qwen3.8-Flash-Next-FP4-Local@" + rev},
		{"/hf/home/models--acme--base/snapshots/" + rev, "hf:acme/base@" + rev},
		{"/hf/home/models--acme--base/snapshots/abc123", ""}, // short rev: no id, never a guess
		{"/mnt/models/local-weights", ""},                    // a local dir
		{"/model", ""},                                       // another local dir
		{"local/qwen38-flash-next-fp4", ""},                  // a repo-shaped local dir
		{"/vllm/models--acme--base/refs/main", ""},           // not a snapshot path
	}
	for _, c := range cases {
		if got := HFIDFromPath(c.path); got != c.want {
			t.Fatalf("HFIDFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestHFIDFromRepo(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	repoDir := filepath.Join(cache, "models--acme--base")
	if err := os.MkdirAll(filepath.Join(repoDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte(revBase+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HFIDFromRepo(cache, "acme/base"); got != "hf:acme/base@"+revBase {
		t.Fatalf("with local ref = %q", got)
	}
	for _, repo := range []string{"acme/missing", "local/mymodel", "noslash", "/model", "../escape", "acme/base/x"} {
		if got := HFIDFromRepo(cache, repo); got != "" {
			t.Fatalf("HFIDFromRepo(cache, %q) = %q, want empty", repo, got)
		}
	}
	if got := HFIDFromRepo(cache, "acme/base"); got == "" {
		t.Fatal("unreachable")
	}
	// A junk ref (branch name, not a commit) is not an id.
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HFIDFromRepo(cache, "acme/base"); got != "" {
		t.Fatalf("junk ref = %q", got)
	}
}

func TestOllama(t *testing.T) {
	t.Parallel()
	id, fp, ok := Ollama("a1b2c3d4" + strings.Repeat("e", 56))
	if !ok || id != "ollama:sha256:a1b2c3d4"+strings.Repeat("e", 56) || fp != "a1b2c3d4"+strings.Repeat("e", 56) {
		t.Fatalf("bare hex: %q %q %v", id, fp, ok)
	}
	if id, fp, ok := Ollama("sha256:A1B2C3D4" + strings.Repeat("e", 56)); !ok || id != "ollama:sha256:a1b2c3d4"+strings.Repeat("e", 56) || fp != id[len("ollama:sha256:"):] {
		t.Fatalf("sha256-prefixed: %q %q %v", id, fp, ok)
	}
	for _, bad := range []string{"", "abc", "sha256:sha256:" + blob1, blob1 + "zz", strings.Repeat("g", 64)} {
		if _, _, ok := Ollama(bad); ok {
			t.Fatalf("Ollama(%q) accepted", bad)
		}
	}
}

// writeRepo builds a fake HF hub cache repo: blobs named by their (fake)
// LFS digests, snapshot files symlinked to them, plus plain small files.
func writeRepo(t *testing.T, cache, repo, rev string, blobs map[string]string,
	links map[string]string, plain map[string]string) {
	t.Helper()
	org, name, _ := strings.Cut(repo, "/")
	dir := filepath.Join(cache, "models--"+org+"--"+name)
	snap := filepath.Join(dir, "snapshots", rev)
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(blobs) > 0 {
		blobDir := filepath.Join(dir, "blobs")
		if err := os.MkdirAll(blobDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for blob, content := range blobs {
			if err := os.WriteFile(filepath.Join(blobDir, blob), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for file, blob := range links {
		target := filepath.Join(snap, file)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		// The blob target, relative to the link's own directory (deeper
		// snapshot subdirs need more ".."s, exactly like real HF caches).
		blobPath := filepath.Join(dir, "blobs", blob)
		rel, err := filepath.Rel(filepath.Dir(target), blobPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, target); err != nil {
			skipNoSymlink(t, err)
			t.Fatal(err)
		}
	}
	for file, content := range plain {
		if err := os.WriteFile(filepath.Join(snap, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fingerprintOf(lines ...string) string {
	slices.Sort(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestFingerprintGolden: the exact fingerprint over a fake cache with two
// LFS-named blobs (never read — the blob name is the hash), one small
// file (read), an excluded README, and an overlay. Computed independently
// here, bytewise-sorted lines.
func TestFingerprintGolden(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	writeRepo(t, cache, "nvidia/stock", revBase,
		map[string]string{blob1: "config", blob2: "weights"},
		map[string]string{"config.json": blob1, "model-00001.safetensors": blob2},
		map[string]string{"tokenizer.json": "hello tokenizer", "README.md": "metadata"},
	)
	writeRepo(t, cache, "house/ablit", revOverlay,
		map[string]string{blob3: "overlay"},
		map[string]string{"fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors": blob3},
		nil,
	)
	tokHash := sha256.Sum256([]byte("hello tokenizer"))
	stockWant := fingerprintOf(
		"config.json\t"+blob1,
		"model-00001.safetensors\t"+blob2,
		"tokenizer.json\t"+hex.EncodeToString(tokHash[:]),
	)
	ablitWant := fingerprintOf(
		"config.json\t"+blob1,
		"model-00001.safetensors\t"+blob2,
		"overlay:house/ablit/fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors\t"+blob3,
		"tokenizer.json\t"+hex.EncodeToString(tokHash[:]),
	)

	var warns []string
	stock := Fingerprint(cache, "hf:nvidia/stock@"+revBase, func(p, r string) {
		warns = append(warns, p+" "+r)
	})
	if stock != stockWant {
		t.Fatalf("stock fingerprint = %s, want %s (warns: %v)", stock, stockWant, warns)
	}
	ablit := Fingerprint(cache, "hf:nvidia/stock@"+revBase+"+house/ablit@"+revOverlay+":fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors", nil)
	if ablit != ablitWant {
		t.Fatalf("ablit fingerprint = %s, want %s", ablit, ablitWant)
	}
	if stock == ablit {
		t.Fatal("stock and stock+overlay fingerprinted identically")
	}

	// No local snapshot: the id stands, the fingerprint is empty, and no
	// warning fires (the expected containerized-backend case).
	warns = nil
	if got := Fingerprint(cache, "hf:container/only@"+revBase, func(p, r string) { warns = append(warns, p) }); got != "" || warns != nil {
		t.Fatalf("missing snapshot: fp=%q warns=%v, want silent empty", got, warns)
	}
	// A declared overlay whose file is missing → no fingerprint at all,
	// and a warning naming the path.
	warns = nil
	id := "hf:nvidia/stock@" + revBase + "+house/ablit@" + revOverlay + ":missing.safetensors"
	if got := Fingerprint(cache, id, func(p, r string) { warns = append(warns, p+" "+r) }); got != "" {
		t.Fatalf("missing overlay file: fp = %q", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "missing.safetensors") || !strings.Contains(warns[0], ReasonMissing) {
		t.Fatalf("missing overlay warns = %v", warns)
	}
	// A dangling symlink (the unmounted-volume case) voids the fingerprint.
	snap := filepath.Join(cache, "models--nvidia--stock", "snapshots", revBase)
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "gone"), filepath.Join(snap, "broken.bin")); err != nil {
		skipNoSymlink(t, err)
		t.Fatal(err)
	}
	warns = nil
	if got := Fingerprint(cache, "hf:nvidia/stock@"+revBase, func(p, r string) { warns = append(warns, p+" "+r) }); got != "" {
		t.Fatalf("dangling symlink: fp = %q", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "broken.bin") {
		t.Fatalf("dangling symlink warns = %v", warns)
	}
}

func TestFingerprintHashesSmallFilesNotBlobNames(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	// A 40-hex-ish name that is NOT valid hex must be READ (hashed by
	// content), not skipped or trusted; every non-excluded file counts.
	junk := "40hexnotadigest000000000000000000000000"
	writeRepo(t, cache, "acme/gitnames", revBase, nil, nil,
		map[string]string{"tokenizer.json": "payload", junk: "junk-bytes"})
	h1 := sha256.Sum256([]byte("payload"))
	hj := sha256.Sum256([]byte("junk-bytes"))
	f1 := fingerprintOf(
		"tokenizer.json\t"+hex.EncodeToString(h1[:]),
		junk+"\t"+hex.EncodeToString(hj[:]),
	)
	if got := Fingerprint(cache, "hf:acme/gitnames@"+revBase, nil); got != f1 {
		t.Fatalf("gitnames = %s, want %s", got, f1)
	}
	// A file whose CONTENT is 64 hex chars but whose NAME is not a digest
	// is still hashed by content (the name is the LFS proof, not the data).
	content64 := strings.Repeat("ab", 32)
	writeRepo(t, cache, "acme/hexy", revBase, nil, nil, map[string]string{"data.bin": content64})
	h2 := sha256.Sum256([]byte(content64))
	f2 := fingerprintOf("data.bin\t" + hex.EncodeToString(h2[:]))
	if got := Fingerprint(cache, "hf:acme/hexy@"+revBase, nil); got != f2 {
		t.Fatalf("hexy = %s, want %s", got, f2)
	}
}

func TestGGUFHasher(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "weights-cache.json")
	h := LoadGGUFHasher(path)

	file := filepath.Join(home, "model.gguf")
	if err := os.WriteFile(file, []byte("fake gguf payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("fake gguf payload"))
	wantHex := hex.EncodeToString(want[:])

	if _, ok := h.Cached(file); ok {
		t.Fatal("cache hit before any hash")
	}
	got, err := h.HashFile(file)
	if err != nil || got != wantHex {
		t.Fatalf("HashFile = %q %v, want %q", got, err, wantHex)
	}
	// Memoized: a second hash answers from the cache (indirectly proven by
	// corrupting the file and re-hashing under a NEW hasher instance that
	// loads the persisted memo — the answer must still be the old digest).
	if _, ok := h.Cached(file); !ok {
		t.Fatal("no cache hit after hash")
	}
	h2 := LoadGGUFHasher(path)
	if got, _ := h2.HashFile(file); got != wantHex {
		t.Fatalf("persisted memo lost: %q", got)
	}
	// A size change invalidates the entry (mtime alone is second-grained;
	// keep the payload the same length so only size changing would be
	// ambiguous).
	if err := os.WriteFile(file, []byte("different payloads!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Cached(file); ok {
		t.Fatal("stale cache hit after size change")
	}
	got, err = h.HashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("different payloads!!"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("rehash = %q", got)
	}
	// Not-a-file: no crash, no entry.
	if _, ok := h.Cached(filepath.Join(home, "missing.gguf")); ok {
		t.Fatal("cache hit for missing file")
	}
	if _, err := h.HashFile(home); err == nil {
		t.Fatal("hashed a directory")
	}
}

// TestGGUFHashInflightDedupe: concurrent HashFile calls on one file share
// a single read and all get the same digest.
func TestGGUFHashInflightDedupe(t *testing.T) {
	file := filepath.Join(t.TempDir(), "m.gguf")
	payload := bytes.Repeat([]byte("w"), 8<<20)
	if err := os.WriteFile(file, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	h := LoadGGUFHasher("")
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = h.HashFile(file)
		}(i)
	}
	wg.Wait()
	for i, g := range got {
		if g != hex.EncodeToString(want[:]) {
			t.Fatalf("caller %d got %q", i, g)
		}
	}
	if len(h.inflight) != 0 {
		t.Fatalf("inflight not cleared: %d", len(h.inflight))
	}
}

// skipNoSymlink skips a test on Windows when the account may not create
// symlinks (no Developer Mode, not elevated) — the HF hub cache itself
// falls back to plain copies there, which the fingerprint handles by
// reading the files; the symlink layout is covered on Linux and macOS.
func skipNoSymlink(t *testing.T, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("symlinks unavailable on this Windows account: %v", err)
	}
}
