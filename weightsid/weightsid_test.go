package weightsid

import "testing"

// TestSparkStockFingerprint: the fingerprint the router rebuilds from
// Hugging Face for the Sparks' stock weights equals the value pinned in the
// catalog (computed 2026-11-17; the boxes' ablit fingerprint, which adds
// one overlay line, was cross-checked the same way).
func TestSparkStockFingerprint(t *testing.T) {
	var lines []string
	for _, f := range sparkStockFiles {
		if ExcludedName(lastSegment(f[0])) {
			continue
		}
		lines = append(lines, Line(f[0], f[1]))
	}
	const want = "e1d7fc58e4a0636e0720dbb020927578f61c763c0a111bf63cdae64bd03935ee"
	if got := FingerprintLines(lines); got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
	// With the house overlay the boxes report faa104f8….
	lines = append(lines, Line(OverlayPath("drowzeys/keys-Qwen3.8-Flash-Next-NVFP4-FP4-QAD-house-qsa-L3-47",
		"fp4-local/house-qsa-oproj-mxfp8-L3-47.safetensors"),
		"4fa076198d6fdcb6c4bfb541f4c78ea64490f10ecc662e0e01e62e993020a343"))
	if got := FingerprintLines(lines); got != "faa104f8093404f343314d450c0be154f92df985b813718249211d5d74c0317a" {
		t.Fatalf("ablit fingerprint = %s", got)
	}
}

func lastSegment(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func TestOllamaRef(t *testing.T) {
	for _, tc := range []struct {
		raw, ns, name, tag string
		ok                 bool
	}{
		{"gemma4:e2b-it-qat", "library", "gemma4", "e2b-it-qat", true},
		{"qwen3:0.6b", "library", "qwen3", "0.6b", true},
		{"qllama/multilingual-e5-large-instruct:latest", "qllama", "multilingual-e5-large-instruct", "latest", true},
		{"nomic-embed-text", "library", "nomic-embed-text", "latest", true},
		{"registry.ollama.ai/library/qwen3:8b", "library", "qwen3", "8b", true},
		{"hf.co/bartowski/some-GGUF:Q4_K_M", "", "", "", false},
		{"a/b/c:d", "", "", "", false},
		{"", "", "", "", false},
		{"../evil:1", "", "", "", false},
	} {
		ns, name, tag, ok := OllamaRef(tc.raw)
		if ok != tc.ok || ns != tc.ns || name != tc.name || tag != tc.tag {
			t.Errorf("OllamaRef(%q) = %q %q %q %v, want %q %q %q %v", tc.raw, ns, name, tag, ok, tc.ns, tc.name, tc.tag, tc.ok)
		}
	}
	if got := OllamaPoolID("library", "gemma4", "e2b-it-qat"); got != "ollama:gemma4:e2b-it-qat" {
		t.Errorf("library pool id = %q", got)
	}
	if got := OllamaPoolID("qllama", "e5", "latest"); got != "ollama:qllama/e5:latest" {
		t.Errorf("namespaced pool id = %q", got)
	}
	if got := OllamaKey("library", "qwen3", "8b"); got != "ollama:library/qwen3:8b" {
		t.Errorf("key = %q", got)
	}
}

func TestParseSpecAndExcluded(t *testing.T) {
	const rev = "b9ce18a6887e18c089fdea8509a33e63429a3300"
	s, ok := ParseID("hf:nvidia/x@" + rev)
	if !ok || s.Repo != "nvidia/x" || s.Rev != rev || len(s.Overlays) != 0 || s.ID() != "hf:nvidia/x@"+rev {
		t.Fatalf("ParseID = %+v %v", s, ok)
	}
	if _, ok := ParseID("hf:nvidia/x@b9ce18a"); ok {
		t.Fatal("short rev accepted")
	}
	for name, want := range map[string]bool{"README.md": true, "LICENSE": true, ".gitattributes": true,
		"logo.PNG": true, "config.json": false, "tokenizer.json": false, "chat_template.jinja": false} {
		if ExcludedName(name) != want {
			t.Errorf("ExcludedName(%q) != %v", name, want)
		}
	}
}
