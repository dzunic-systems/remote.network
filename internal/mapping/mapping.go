// Package mapping resolves raw backend model ids (ollama tags, gguf file
// names, vendor ids) to catalog canonical ids. Alias table first, then a
// normalizing heuristic; a manual override always wins.
package mapping

import (
	"strings"

	"remote.network/node/catalog"
)

// Mapping answers "what canonical model is this raw id?".
type Mapping struct {
	cat      *catalog.Catalog
	override map[string]string // raw id (lowercased) → canonical id
}

// New builds a mapping with optional manual overrides (raw → canonical).
func New(cat *catalog.Catalog, overrides map[string]string) *Mapping {
	o := make(map[string]string, len(overrides))
	for k, v := range overrides {
		o[strings.ToLower(k)] = v
	}
	return &Mapping{cat: cat, override: o}
}

// Resolve maps a raw backend id to its catalog model. Second return is
// false when nothing in the catalog matches (the model is not servable).
// Manual overrides win; otherwise this is catalog.Resolve (alias table +
// normalizing heuristic).
func (m *Mapping) Resolve(raw string) (*catalog.Model, bool) {
	if raw == "" {
		return nil, false
	}
	if canon, ok := m.override[strings.ToLower(raw)]; ok {
		if mod := m.cat.ByID(canon); mod != nil {
			return mod, true
		}
	}
	if mod := m.cat.Resolve(raw); mod != nil {
		return mod, true
	}
	return nil, false
}

// ResolveAll maps a probed id list; unmapped ids are reported separately so
// the CLI can show the operator what the catalog is missing.
func (m *Mapping) ResolveAll(rawIDs []string) (mapped []struct {
	Raw, Canonical string
	Model          *catalog.Model
}, unmapped []string) {
	for _, raw := range rawIDs {
		if mod, ok := m.Resolve(raw); ok {
			mapped = append(mapped, struct {
				Raw, Canonical string
				Model          *catalog.Model
			}{raw, mod.ID, mod})
		} else {
			unmapped = append(unmapped, raw)
		}
	}
	return mapped, unmapped
}
