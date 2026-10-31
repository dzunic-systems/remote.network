package catalog

import "testing"

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
	// (micro-USD per 1M tokens).
	in, out := cat.CurvePrice(4)
	if in != 100000 || out != 300000 {
		t.Fatalf("CurvePrice(4) = %d/%d, want 100000/300000", in, out)
	}
	in, out = cat.CurvePrice(26)
	if in != 300000 || out != 900000 {
		t.Fatalf("CurvePrice(26) = %d/%d, want 300000/900000", in, out)
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
