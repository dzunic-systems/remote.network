// Package backends is the node agent's backend registry: a
// persisted list of inference endpoints — the dossier provider pattern,
// plural. One agent (even a GPU-less LAN router box) fronts any number of
// heterogeneous backends (ollama + several vLLM + llama.cpp); serve turns
// each entry's probed models into service cards under one network identity.
package backends

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Entry is one configured inference endpoint.
type Entry struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // openai | openai-compatible | anthropic
	URL    string   `json:"url,omitempty"`
	APIKey string   `json:"api_key,omitempty"`
	Models []string `json:"models,omitempty"` // optional per-backend serving restriction
}

// File is the on-disk shape (~/.remote/backends.json, 0600).
type File struct {
	Backends []Entry `json:"backends"`
}

// DefaultEntry is what an agent without a registry file serves: the
// original single-backend behavior — one openai-compatible backend, URL empty so
// serve falls back to local Ollama.
func DefaultEntry() Entry {
	return Entry{ID: "default", Kind: "openai-compatible"}
}

// Path is the registry file location under the agent home.
func Path(home string) string { return filepath.Join(home, "backends.json") }

// Load reads the registry; a missing file means one default entry (the
// historical single-backend behavior), not an error.
func Load(home string) ([]Entry, error) {
	b, err := os.ReadFile(Path(home))
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{DefaultEntry()}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("backends.json: %w", err)
	}
	if len(f.Backends) == 0 {
		return []Entry{DefaultEntry()}, nil
	}
	return f.Backends, nil
}

// Save persists the registry atomically (0700 dir, 0600 file — API keys).
func Save(home string, entries []Entry) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(File{Backends: entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(home))
}

// Add appends one entry; a duplicate id is an error.
func Add(home string, e Entry) error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("backend id required")
	}
	if e.Kind == "" {
		e.Kind = "openai-compatible"
	}
	entries, err := Load(home)
	if err != nil {
		return err
	}
	for _, x := range entries {
		if x.ID == e.ID {
			return fmt.Errorf("backend %q already exists", e.ID)
		}
	}
	return Save(home, append(entries, e))
}

// Remove deletes one entry by id; unknown id is an error.
func Remove(home, id string) error {
	entries, err := Load(home)
	if err != nil {
		return err
	}
	out := entries[:0]
	found := false
	for _, x := range entries {
		if x.ID == id {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return fmt.Errorf("no backend %q", id)
	}
	return Save(home, out)
}
