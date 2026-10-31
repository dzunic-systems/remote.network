// manifest.go: the operator's half of a service card. The
// model-cards manifest supplies what no daemon can collect — display name,
// description, license, lineage, origin, version, links — keyed by raw
// backend id or canonical id. Merging happens inside buildCard after
// auto-collection, and only ever fills the descriptive fields: measured
// facts (tps/ttft/ctx/size) and collected metadata (family/quant/params)
// stay the agent's, by the "measured, not guessed" rule.
package serve

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultManifestName is the manifest file the agent looks for under Home
// when --model-cards is not given.
const DefaultManifestName = "model-cards.yaml"

// CardMeta is one manifest entry — the descriptive fields of a service
// card. YAML keys mirror the wire field names.
type CardMeta struct {
	DisplayName   string   `yaml:"display_name,omitempty"`
	Description   string   `yaml:"description,omitempty"`
	License       string   `yaml:"license,omitempty"`
	BaseModel     string   `yaml:"base_model,omitempty"`
	Origin        string   `yaml:"origin,omitempty"` // catalog | fine-tune | self-trained | quantized
	Version       string   `yaml:"version,omitempty"`
	Links         []string `yaml:"links,omitempty"`
	Languages     []string `yaml:"languages,omitempty"`
	WeightsSHA256 string   `yaml:"weights_sha256,omitempty"`
}

// manifestPath resolves the effective manifest path: the explicit
// --model-cards value, else Home's default location. Empty result means
// "no manifest configured".
func manifestPath(home, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, DefaultManifestName)
}

// loadManifest reads the model-cards manifest. A missing file is normal
// (no manifest configured); a parse error warns and yields an empty
// manifest — a typo must not brick serving, matching the router's
// clamp-never-drop intake policy.
func loadManifest(path string, log *slog.Logger) map[string]CardMeta {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("model-cards manifest unreadable — ignoring", "path", path, "err", err)
		}
		return nil
	}
	var manifest map[string]CardMeta
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		log.Warn("model-cards manifest is not valid YAML — ignoring", "path", path, "err", err)
		return nil
	}
	return manifest
}

// applyManifest folds one manifest entry into a card. Keys are tried in
// order — raw backend id first (the operator's natural handle), then the
// card's id (canonical or namespaced). Descriptive fields only: a
// manifest never overwrites a collected or measured fact, and never
// renames the card identity.
func applyManifest(card *ServedModel, manifest map[string]CardMeta, keys ...string) {
	if len(manifest) == 0 {
		return
	}
	var meta *CardMeta
	for _, k := range keys {
		if m, ok := manifest[k]; ok {
			meta = &m
			break
		}
	}
	if meta == nil {
		return
	}
	set := func(dst *string, src string) {
		if src != "" {
			*dst = src
		}
	}
	set(&card.DisplayName, meta.DisplayName)
	set(&card.Description, meta.Description)
	set(&card.License, meta.License)
	set(&card.BaseModel, meta.BaseModel)
	set(&card.Origin, meta.Origin)
	set(&card.Version, meta.Version)
	set(&card.WeightsSHA256, strings.ToLower(meta.WeightsSHA256))
	if len(meta.Links) > 0 {
		card.Links = meta.Links
	}
	if len(meta.Languages) > 0 {
		card.Languages = meta.Languages
	}
}
