package backends

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingGivesDefault(t *testing.T) {
	home := t.TempDir()
	entries, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "default" || entries[0].Kind != "openai-compatible" {
		t.Fatalf("default entries = %+v", entries)
	}
}

func TestAddListRemove(t *testing.T) {
	home := t.TempDir()

	if err := Add(home, Entry{ID: "ollama-lan", Kind: "openai-compatible",
		URL: "http://192.0.2.16:11434/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := Add(home, Entry{ID: "ollama-lan", Kind: "openai"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if err := Add(home, Entry{ID: "", Kind: "openai"}); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := Add(home, Entry{ID: "vllm", Kind: "openai", URL: "http://vllm:8000/v1",
		APIKey: "sk-test", Models: []string{"qwen3-32b"}}); err != nil {
		t.Fatal(err)
	}

	entries, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	// The synthesized default persists as entry #1 once the user starts
	// editing the registry (removable via `backend remove default`).
	if len(entries) != 3 || entries[0].ID != "default" ||
		entries[1].ID != "ollama-lan" || entries[2].APIKey != "sk-test" {
		t.Fatalf("entries = %+v", entries)
	}

	// The file on disk must not leak keys to group-reads (0600).
	if err := Add(home, Entry{ID: "third", Kind: "openai"}); err != nil {
		t.Fatal(err)
	}

	if err := Remove(home, "ollama-lan"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home, "nope"); err == nil || !strings.Contains(err.Error(), "no backend") {
		t.Fatalf("removing unknown = %v", err)
	}
	if entries, _ = Load(home); len(entries) != 3 {
		t.Fatalf("post-remove = %+v", entries)
	}
}

func TestSavePersistsRoundTrip(t *testing.T) {
	home := t.TempDir()
	want := []Entry{
		{ID: "a", Kind: "openai-compatible", URL: "http://x:11434/v1"},
		{ID: "b", Kind: "anthropic", URL: "https://api.anthropic.com", APIKey: "k"},
	}
	if err := Save(home, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].APIKey != "k" || filepath.Base(Path(home)) != "backends.json" {
		t.Fatalf("round trip = %+v", got)
	}
}
