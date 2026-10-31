package mapping

import (
	"testing"

	"remote.network/node/catalog"
)

const testCatalog = `
models:
  - id: qwen/qwen3-4b
    family: qwen3
    aliases: ["qwen3:4b"]
  - id: openai/gpt-oss-20b
    family: gpt-oss
    aliases: ["gpt-oss:20b"]
  - id: google/gemma4-12b-it
    family: gemma4
    aliases: ["gemma4:12b-it-qat", "gemma4:12b"]
`

func newTestMapping(t *testing.T) *Mapping {
	t.Helper()
	cat, err := catalog.Parse([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	return New(cat, nil)
}

func TestResolveExactAndAlias(t *testing.T) {
	m := newTestMapping(t)
	for raw, want := range map[string]string{
		"qwen3:4b":          "qwen/qwen3-4b",
		"QWEN/QWEN3-4B":     "qwen/qwen3-4b",
		"gpt-oss:20b":       "openai/gpt-oss-20b",
		"gemma4:12b-it-qat": "google/gemma4-12b-it",
	} {
		mod, ok := m.Resolve(raw)
		if !ok || mod.ID != want {
			t.Fatalf("Resolve(%q) = %v, %v; want %s", raw, mod, ok, want)
		}
	}
}

func TestResolveHeuristicQuantStripping(t *testing.T) {
	m := newTestMapping(t)
	for raw, want := range map[string]string{
		"Qwen3-4B-Q4_K_M.gguf":      "qwen/qwen3-4b",
		"qwen3:4b-instruct-q4_k_m":  "qwen/qwen3-4b",
		"qwen3 4b":                  "qwen/qwen3-4b",
		"Qwen/Qwen3-4B-GGUF:Q4_K_M": "qwen/qwen3-4b",
	} {
		mod, ok := m.Resolve(raw)
		if !ok || mod.ID != want {
			t.Fatalf("Resolve(%q) = %v, %v; want %s", raw, mod, ok, want)
		}
	}
}

func TestResolveUnmapped(t *testing.T) {
	m := newTestMapping(t)
	for _, raw := range []string{"", "llama3:70b", "mystery-model", "bge-reranker:latest"} {
		if mod, ok := m.Resolve(raw); ok {
			t.Fatalf("Resolve(%q) unexpectedly mapped to %s", raw, mod.ID)
		}
	}
}

func TestOverrideWins(t *testing.T) {
	cat, _ := catalog.Parse([]byte(testCatalog))
	m := New(cat, map[string]string{"weird-local-name": "openai/gpt-oss-20b"})
	mod, ok := m.Resolve("Weird-Local-Name")
	if !ok || mod.ID != "openai/gpt-oss-20b" {
		t.Fatalf("override failed: %v %v", mod, ok)
	}
}
