package catalog

import (
	"strings"
	"testing"
)

func TestDefaultParsesCurve(t *testing.T) {
	cat, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Pricing.Curve) == 0 {
		t.Fatal("embedded models.yaml carries no pricing curve")
	}
	// Curve must track the network-standard entries: qwen3-4b sits in the
	// first band, gemma4-26b above the 16B line. Values are real money
	// (micro-USD per 1M tokens, 50% of market since 2026-11-17).
	in, out := cat.CurvePrice(4)
	if in != 10000 || out != 20000 {
		t.Fatalf("CurvePrice(4) = %d/%d, want 10000/20000", in, out)
	}
	in, out = cat.CurvePrice(26)
	if in != 60000 || out != 200000 {
		t.Fatalf("CurvePrice(26) = %d/%d, want 60000/200000", in, out)
	}
	// The pre-halving rows stay reachable for old-node detection.
	if in, out := LegacyCurvePrice(4); in != 100000 || out != 300000 {
		t.Fatalf("LegacyCurvePrice(4) = %d/%d, want 100000/300000", in, out)
	}
	if in, out := LegacyCurvePrice(26); in != 300000 || out != 900000 {
		t.Fatalf("LegacyCurvePrice(26) = %d/%d, want 300000/900000", in, out)
	}
	// The 2026-11-17 network price list parses with no duplicate matches.
	if len(cat.NetworkPrices) == 0 {
		t.Fatal("embedded models.yaml carries no network_prices list")
	}
	if in, out, ok := cat.NetworkPrice("qwen3:0.6b"); !ok || in != 10000 || out != 10000 {
		t.Fatalf("NetworkPrice(qwen3:0.6b) = %d/%d %v", in, out, ok)
	}
}

func TestNetworkPriceLookup(t *testing.T) {
	cat, err := Parse([]byte(`models:
  - id: x/y
    family: y
network_prices:
  - {match: ["nomic-embed-text:latest"], in: 4000, out: 0}
  - {match: ["qwen3:8b"], in: 55000, out: 120000}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Exact hit, :latest added, :latest stripped, miss.
	if in, out, ok := cat.NetworkPrice("Nomic-Embed-Text:latest"); !ok || in != 4000 || out != 0 {
		t.Fatalf("exact = %d/%d %v", in, out, ok)
	}
	if in, out, ok := cat.NetworkPrice("nomic-embed-text"); !ok || in != 4000 {
		t.Fatalf(":latest stripped = %d/%d %v", in, out, ok)
	}
	if in, out, ok := cat.NetworkPrice("qwen3:8b:latest"); !ok || in != 55000 || out != 120000 {
		t.Fatalf(":latest added = %d/%d %v", in, out, ok)
	}
	if _, _, ok := cat.NetworkPrice("qwen3:14b"); ok {
		t.Fatal("miss returned ok")
	}
	if _, _, ok := cat.NetworkPrice(""); ok {
		t.Fatal("empty tag returned ok")
	}
}

func TestNetworkPriceDuplicateIsLoadError(t *testing.T) {
	_, err := Parse([]byte(`models:
  - id: x/y
    family: y
network_prices:
  - {match: ["qwen3:8b"], in: 1, out: 1}
  - {match: ["QWEN3:8B"], in: 2, out: 2}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate match accepted: %v", err)
	}
}

func TestCurvePriceEdges(t *testing.T) {
	cat, err := Parse([]byte(`models:
  - id: x/y
    family: y
pricing:
  curve:
    - {max_params_b: 4, price_in_per_1m: 10, price_out_per_1m: 25}
    - {max_params_b: 1000, price_in_per_1m: 60, price_out_per_1m: 150}
`))
	if err != nil {
		t.Fatal(err)
	}
	// 0 params (unmeasured) → cheapest band; exactly on a boundary → that
	// band; beyond the last band → zero (unpriced, not top-banded).
	if in, out := cat.CurvePrice(0); in != 10 || out != 25 {
		t.Fatalf("CurvePrice(0) = %d/%d", in, out)
	}
	if in, out := cat.CurvePrice(4); in != 10 || out != 25 {
		t.Fatalf("CurvePrice(4) = %d/%d", in, out)
	}
	if in, out := cat.CurvePrice(4.1); in != 60 || out != 150 {
		t.Fatalf("CurvePrice(4.1) = %d/%d", in, out)
	}
	if in, out := cat.CurvePrice(5000); in != 0 || out != 0 {
		t.Fatalf("CurvePrice(5000) = %d/%d, want unpriced", in, out)
	}
}

func TestNoCurveIsUnpriced(t *testing.T) {
	cat, err := Parse([]byte("models:\n  - id: x/y\n    family: y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if in, out := cat.CurvePrice(7); in != 0 || out != 0 {
		t.Fatalf("no-curve CurvePrice = %d/%d, want 0/0", in, out)
	}
}

// TestResolveQwen38 pins the 2026-11-16 dogfood entries: the Sparks' raw
// backend id resolves to the stock entry (never the abliterated one), the
// house alias resolves to the ablit entry (never stock), and near-miss
// spellings that name DIFFERENT weights resolve to nothing — the
// normalizer must not fuse them.
func TestResolveQwen38(t *testing.T) {
	cat, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	const stockID = "nvidia/qwen3.8-flash-next-fp4"
	const ablitID = "drowzeys/qwen3.8-flash-next-fp4-ablit"

	if m := cat.Resolve("qwen38-flash-next-fp4"); m == nil || m.ID != stockID {
		t.Fatalf("stock alias = %+v, want %s", m, stockID)
	}
	// The exact backend id the Sparks advertise.
	if m := cat.Resolve("nvidia/Qwen3.8-Flash-Next-FP4-Local"); m == nil || m.ID != stockID {
		t.Fatalf("backend id = %+v, want %s", m, stockID)
	}
	if m := cat.Resolve("qwen38-flash-next-fp4-ablit"); m == nil || m.ID != ablitID {
		t.Fatalf("ablit alias = %+v, want %s (NOT stock)", m, ablitID)
	}
	for _, miss := range []string{
		"qwen38-flash-next-uncensored", // another overlay's name — nothing here
		"Qwen3.8-Flash-Next-NVFP4",     // RadixArk's build: different weights
	} {
		if m := cat.Resolve(miss); m != nil {
			t.Fatalf("Resolve(%q) = %+v, want nil (distinct weights must not fuse)", miss, m)
		}
	}
}
