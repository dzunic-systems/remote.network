package serve

import (
	"context"
	"strings"
	"testing"

	"remote.network/node/internal/backend"
)

// ctxBackend is a fakeBackend that reports its own serving windows.
type ctxBackend struct {
	fakeBackend
	ctxs map[string]int
}

func (c *ctxBackend) ServingCtxs() map[string]int { return c.ctxs }

var _ backend.CtxReporter = (*ctxBackend)(nil)

func TestApplyServingCtx(t *testing.T) {
	cb := &ctxBackend{ctxs: map[string]int{"chat": 131072, "e5": 512}}
	rt := &Runtime{opts: Options{ServingCtxOverrides: map[string]int{"vendor/pinned": 8192}}}

	// Backend claim: sets ServingCtx, fills an unknown MaxCtx.
	card := ServedModel{ModelID: "vendor/chat", RawID: "chat"}
	if !rt.applyServingCtx(&card, cb, "chat") || card.ServingCtx != 131072 || card.MaxCtx != 131072 {
		t.Fatalf("detected: %+v", card)
	}
	// Known training max is left alone by a backend claim.
	card = ServedModel{ModelID: "vendor/e5", RawID: "e5", MaxCtx: 514}
	rt.applyServingCtx(&card, cb, "e5")
	if card.ServingCtx != 512 || card.MaxCtx != 514 {
		t.Fatalf("max kept: %+v", card)
	}
	// Unchanged → no re-advertise.
	if rt.applyServingCtx(&card, cb, "e5") {
		t.Fatal("second apply reported a change")
	}
	// Operator pin (by card id) wins outright, on a backend that can't say.
	card = ServedModel{ModelID: "vendor/pinned", RawID: "d4", MaxCtx: 262144}
	if !rt.applyServingCtx(&card, &fakeBackend{}, "d4") || card.ServingCtx != 8192 || card.MaxCtx != 8192 {
		t.Fatalf("override: %+v", card)
	}
	// Nothing known → untouched.
	card = ServedModel{ModelID: "x/y", RawID: "y"}
	if rt.applyServingCtx(&card, &fakeBackend{}, "y") || card.ServingCtx != 0 {
		t.Fatalf("unknown: %+v", card)
	}
}

// residBackend is a fakeBackend that reports Ollama-style residency: the
// loaded models and the context window each is serving with.
type residBackend struct {
	fakeBackend
	ps map[string]int
}

func (r *residBackend) ProbeResidency(ctx context.Context) (map[string]int, error) {
	return r.ps, nil
}

var _ backend.Residencer = (*residBackend)(nil)

// TestResidencyRefreshKeepsThePin: the --serving-ctx pin must survive the
// residency tick in both states — a model going cold must not rewrite the
// operator's measured window to 0, and a warm backend window must not
// outrank the pin either (same rule applyServingCtx enforces).
func TestResidencyRefreshKeepsThePin(t *testing.T) {
	build := func(pin map[string]int) *Runtime {
		return &Runtime{
			opts: Options{ServingCtxOverrides: pin},
			Backends: []BackendEntry{{ID: "b", Client: &residBackend{
				ps: map[string]int{"warm": 4096}, // "cold" is absent from /api/ps
			}, Models: []ServedModel{
				{ModelID: "vendor/pinned", RawID: "pinned", ServingCtx: 8192},
				{ModelID: "vendor/warm", RawID: "warm", ServingCtx: 8192},
				{ModelID: "vendor/cold", RawID: "cold", ServingCtx: 8192},
			}}},
			Models: []ServedModel{
				{BackendID: "b", ModelID: "vendor/pinned", RawID: "pinned", ServingCtx: 8192},
				{BackendID: "b", ModelID: "vendor/warm", RawID: "warm", ServingCtx: 8192},
				{BackendID: "b", ModelID: "vendor/cold", RawID: "cold", ServingCtx: 8192},
			},
		}
	}
	ctx := context.Background()

	// (a) warm + pin → pin (4096 would be the backend's claim);
	// (b) cold + pin → pin (the bug: the tick zeroed it);
	// (c) cold + no pin → 0 (residency still reported honestly);
	// (d) warm + no pin → the probe value.
	rt := build(map[string]int{"pinned": 8192, "warm": 8192, "cold": 16384})
	rt.ResidencyRefresh(ctx)
	for raw, want := range map[string]int{"pinned": 8192, "warm": 8192, "cold": 16384} {
		if got := rt.Backends[0].Models[0:3][idxRaw(raw)].ServingCtx; got != want {
			t.Errorf("%s: ServingCtx = %d, want %d", raw, got, want)
		}
		if got := rt.Models[idxRaw(raw)].ServingCtx; got != want {
			t.Errorf("flat %s: ServingCtx = %d, want %d", raw, got, want)
		}
	}

	// No pins at all: honest residency both ways.
	rt = build(nil)
	rt.ResidencyRefresh(ctx)
	if got := rt.Backends[0].Models[0:3][idxRaw("warm")].ServingCtx; got != 4096 {
		t.Errorf("unpinned warm: ServingCtx = %d, want 4096", got)
	}
	if got := rt.Backends[0].Models[0:3][idxRaw("cold")].ServingCtx; got != 0 {
		t.Errorf("unpinned cold: ServingCtx = %d, want 0", got)
	}
}

// idxRaw finds a card's index in the fixed three-card test slice by raw id.
func idxRaw(raw string) int {
	switch raw {
	case "pinned":
		return 0
	case "warm":
		return 1
	default:
		return 2
	}
}

// TestApplyCapabilityPins: the --tool-capable / --vision-capable /
// --thinking-capable pins force capabilities on models whose backend cannot
// self-report them (vLLM, llama-server), by raw id or card id; unpinned and
// already-capable cards are untouched, and each capability lands in the
// list exactly once.
func TestApplyCapabilityPins(t *testing.T) {
	opts := Options{
		ToolCapable:     []string{"flash", "vendor/other"},
		VisionCapable:   []string{"flash"},
		ThinkingCapable: []string{"flash"},
	}

	// All three pins land on the raw-pinned card.
	card := ServedModel{ModelID: "vendor/mine", RawID: "flash"}
	opts.applyCapabilityPins(&card, "flash")
	if !card.Tools || !card.Vision || !card.Reasoning {
		t.Fatalf("pins missing: %+v", card)
	}
	if len(card.Capabilities) != 3 {
		t.Fatalf("capabilities = %v, want the three pins", card.Capabilities)
	}

	// By card id: only the tool pin names this one.
	card = ServedModel{ModelID: "vendor/other", RawID: "d4"}
	opts.applyCapabilityPins(&card, "d4")
	if !card.Tools || card.Vision || card.Reasoning {
		t.Fatalf("card-id pin wrong: %+v", card)
	}

	// Unpinned stays untouched.
	card = ServedModel{ModelID: "vendor/x", RawID: "x"}
	opts.applyCapabilityPins(&card, "x")
	if card.Tools || card.Vision || card.Reasoning || card.Capabilities != nil {
		t.Fatalf("unpinned changed: %+v", card)
	}

	// Already capable: no duplicate capability entry.
	card = ServedModel{ModelID: "vendor/mine", RawID: "flash", Tools: true,
		Capabilities: []string{"completion", "tools"}}
	opts.applyCapabilityPins(&card, "flash")
	if n := strings.Count(strings.Join(card.Capabilities, ","), "tools"); n != 1 {
		t.Fatalf("duplicate tools capability: %v", card.Capabilities)
	}
}
