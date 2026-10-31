// Package catalog embeds and parses catalog/models.yaml — the curated set
// of servable models with canonical ids, aliases, and pinned GGUF
// artifacts. Embedded so the node binary is self-contained; the yaml file
// is the edit-here source of truth.
package catalog

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed models.yaml
var raw string

// GGUF is a pinned artifact the managed path can pull and verify.
type GGUF struct {
	Repo      string `yaml:"repo"`
	File      string `yaml:"file"`
	SHA256    string `yaml:"sha256"`
	SizeBytes int64  `yaml:"size_bytes"`
}

// CurveRow is one pricing-curve band: models with ParamsB up to MaxParamsB
// price at PriceInPer1M / PriceOutPer1M (micro-USD per 1M tokens).
type CurveRow struct {
	MaxParamsB    float64 `yaml:"max_params_b"`
	PriceInPer1M  int     `yaml:"price_in_per_1m"`
	PriceOutPer1M int     `yaml:"price_out_per_1m"`
}

// Pricing is the default-price policy: what a publisher card
// charges when its operator did not set a price.
type Pricing struct {
	Curve []CurveRow `yaml:"curve"`
}

// Model is one catalog entry.
type Model struct {
	ID                     string   `yaml:"id"`
	Family                 string   `yaml:"family"`
	DisplayName            string   `yaml:"display_name"`
	ParamsB                float64  `yaml:"params_b"`
	DefaultQuant           string   `yaml:"default_quant"`
	Ctx                    int      `yaml:"ctx"`
	Aliases                []string `yaml:"aliases"`
	MinVRAMMB              int      `yaml:"min_vram_mb"`
	SuggestedPriceInPer1M  int      `yaml:"suggested_price_in_per_1m"`  // micro-USD
	SuggestedPriceOutPer1M int      `yaml:"suggested_price_out_per_1m"` // micro-USD
	GGUF                   *GGUF    `yaml:"gguf,omitempty"`
}

// Catalog is the parsed model set.
type Catalog struct {
	Models  []Model `yaml:"models"`
	Pricing Pricing `yaml:"pricing"`
}

// Default parses the embedded catalog.
func Default() (*Catalog, error) {
	return Parse([]byte(raw))
}

// Parse reads catalog yaml.
func Parse(b []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if len(c.Models) == 0 {
		return nil, fmt.Errorf("catalog: no models")
	}
	return &c, nil
}

// ByID looks up a canonical id.
func (c *Catalog) ByID(id string) *Model {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

// CurvePrice returns the default price (micro-USD per 1M tokens) for a
// card of the given parameter size: the first curve band whose MaxParamsB
// covers paramsB. Unknown parameter counts (0) land in the cheapest band —
// an unmeasured model should not price like a frontier one. No curve
// configured → zero,zero (unpriced, which the scheduler treats as neutral).
func (c *Catalog) CurvePrice(paramsB float64) (priceIn, priceOut int) {
	for _, row := range c.Pricing.Curve {
		if paramsB <= row.MaxParamsB {
			return row.PriceInPer1M, row.PriceOutPer1M
		}
	}
	return 0, 0
}

// Resolve maps a user- or backend-supplied id (canonical id, alias, or any
// spelling that normalizes onto one) to its catalog model; nil when unknown.
// This is the alias core shared by the node's raw-id mapping (which layers
// manual overrides on top) and the consumer API's model resolution.
func (c *Catalog) Resolve(idOrAlias string) *Model {
	if idOrAlias == "" {
		return nil
	}
	// Exact alias / id match.
	for i := range c.Models {
		mod := &c.Models[i]
		if strings.EqualFold(mod.ID, idOrAlias) {
			return mod
		}
		for _, a := range mod.Aliases {
			if strings.EqualFold(a, idOrAlias) {
				return mod
			}
		}
	}
	// Heuristic: normalize and compare against normalized ids/aliases.
	n := normalize(idOrAlias)
	if n == "" {
		return nil
	}
	for i := range c.Models {
		mod := &c.Models[i]
		for _, cand := range append([]string{mod.ID}, mod.Aliases...) {
			if normalize(cand) == n {
				return mod
			}
		}
	}
	return nil
}

var (
	// noiseSegment removes separator-delimited quant/format segments
	// anywhere in the id — suffix stripping alone cannot handle HF-style
	// "Qwen3-4B-GGUF:Q4_K_M" where the quant is a tag after the name.
	noiseSegment = regexp.MustCompile(`(?i)(^|[ _:.\-])(gguf|iq[0-9]+[a-z]?|q[1-8]_?k?_?[a-z]?|qat|bf16|fp16|f16|latest|instruct|it)([ _:.\-]|$)`)
	spaces       = regexp.MustCompile(`[-_. :]+`)
)

// normalize strips quant/format noise so "Qwen3-4B-Q4_K_M.gguf",
// "Qwen/Qwen3-4B-GGUF:Q4_K_M", "qwen3:4b-instruct-q4_k_m" and "qwen3 4b"
// all land on "qwen3 4b".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:] // org/model → model
	}
	s = strings.TrimSuffix(s, ".gguf")
	for range 4 { // repeated noise: "-q4_k_m-instruct.gguf"
		next := noiseSegment.ReplaceAllString(s, "$1")
		if next == s {
			break
		}
		s = next
	}
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
