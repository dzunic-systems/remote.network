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

// NetworkPrice is one network_prices entry: per-model overrides for raw
// tags the catalog does not map. Match strings are raw backend tags;
// in/out are micro-USD per 1M tokens (50% of the model's market price).
// Source/AsOf are documentation only.
type NetworkPrice struct {
	Match  []string `yaml:"match"`
	In     int      `yaml:"in"`
	Out    int      `yaml:"out"`
	Source string   `yaml:"source"`
	AsOf   string   `yaml:"as_of"`
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
	Models        []Model        `yaml:"models"`
	Pricing       Pricing        `yaml:"pricing"`
	NetworkPrices []NetworkPrice `yaml:"network_prices"`
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
	if err := c.checkNetworkPrices(); err != nil {
		return nil, err
	}
	return &c, nil
}

// checkNetworkPrices refuses a list where one match string appears twice —
// a duplicated match would make the charged price depend on list order.
func (c *Catalog) checkNetworkPrices() error {
	seen := map[string]bool{}
	for i := range c.NetworkPrices {
		np := &c.NetworkPrices[i]
		for _, m := range np.Match {
			key := strings.ToLower(strings.TrimSpace(m))
			if key == "" {
				return fmt.Errorf("catalog: network_prices entry %d has an empty match", i)
			}
			if seen[key] {
				return fmt.Errorf("catalog: network_prices match %q duplicated (%s)", m, np.Source)
			}
			seen[key] = true
		}
	}
	return nil
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

// NetworkPrice looks a raw backend tag up in the network price list:
// exact match after lower-casing, also trying with a trailing ":latest"
// stripped or added (Ollama tags use it inconsistently), then the same
// for the tag without its Ollama user namespace. Miss → ok=false.
func (c *Catalog) NetworkPrice(rawTag string) (priceIn, priceOut int, ok bool) {
	if len(c.NetworkPrices) == 0 {
		return 0, 0, false
	}
	key := strings.ToLower(strings.TrimSpace(rawTag))
	var try []string
	add := func(k string) {
		try = append(try, k)
		if base, has := strings.CutSuffix(k, ":latest"); has {
			try = append(try, base)
		} else {
			try = append(try, k+":latest")
		}
	}
	add(key)
	// Ollama user namespaces ("qllama/multilingual-e5-large-instruct") name
	// the same weights as the bare tag the card id is built from; the
	// namespaced spelling is tried first, so a list entry can still single
	// one namespace out.
	if i := strings.LastIndex(key, "/"); i >= 0 && i < len(key)-1 {
		add(key[i+1:])
	}
	for _, t := range try {
		for i := range c.NetworkPrices {
			np := &c.NetworkPrices[i]
			for _, m := range np.Match {
				if strings.ToLower(strings.TrimSpace(m)) == t {
					return np.In, np.Out, true
				}
			}
		}
	}
	return 0, 0, false
}

// legacyCurve is the pricing curve as it stood on 2026-11-17, before the
// 50%-of-market halving. Nodes released before then bake these rows into
// their cards; the router uses LegacyCurvePrice to recognize such a price
// as "node default, not operator-set" and re-price it from the network
// price list. Delete once every node reports price_source.
var legacyCurve = []CurveRow{
	{MaxParamsB: 4, PriceInPer1M: 100000, PriceOutPer1M: 300000},
	{MaxParamsB: 8, PriceInPer1M: 120000, PriceOutPer1M: 400000},
	{MaxParamsB: 16, PriceInPer1M: 200000, PriceOutPer1M: 700000},
	{MaxParamsB: 1000, PriceInPer1M: 300000, PriceOutPer1M: 900000},
}

// LegacyCurvePrice is CurvePrice over legacyCurve.
func LegacyCurvePrice(paramsB float64) (priceIn, priceOut int) {
	for _, row := range legacyCurve {
		if paramsB <= row.MaxParamsB {
			return row.PriceInPer1M, row.PriceOutPer1M
		}
	}
	return 0, 0
}

// legacySuggested holds the catalog suggested prices as they stood before
// 1d86bdb halved them (and before the 2026-11-17 list re-banded
// qwen3-4b/gemma4-12b-it): what a pre-halving node prices its
// catalog-mapped cards with. Same purpose as legacyCurve — recognize an
// old node's default as not-operator-set. Delete once every node reports
// price_source.
var legacySuggested = map[string][2]int{
	"qwen/qwen3-4b":                         {100000, 300000},
	"qwen/qwen3-14b":                        {200000, 700000},
	"qwen/qwen3-30b":                        {300000, 900000},
	"openai/gpt-oss-20b":                    {150000, 600000},
	"google/gemma4-12b-it":                  {100000, 400000},
	"google/gemma4-26b-a4b-it":              {200000, 700000},
	"nvidia/qwen3.8-flash-next-fp4":         {100000, 300000},
	"drowzeys/qwen3.8-flash-next-fp4-ablit": {100000, 300000},
}

// LegacySuggestedPrice returns the pre-1d86bdb suggested price of a
// catalog id; ok=false for ids with no legacy entry.
func LegacySuggestedPrice(id string) (priceIn, priceOut int, ok bool) {
	p, ok := legacySuggested[id]
	return p[0], p[1], ok
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
