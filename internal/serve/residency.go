// residency.go: what is actually in backend memory right now. Ollama
// answers /api/ps with the loaded models and the context window each is
// truly serving — the fact the advertisement always lacked (MaxCtx is the
// training maximum from model metadata, not what a request will get).
// Backends without such an endpoint (vLLM, llama-server) keep
// ServingCtx = 0 = "resident assumed, window unknown": their models load
// once at server start and stay.
package serve

import (
	"context"

	"remote.network/node/internal/backend"
)

// ResidencyRefresh polls every residency-capable backend and rewrites each
// card's ServingCtx (0 = not resident) in both views (per-backend + flat).
// Returns whether anything changed — the caller re-advertises. Cheap enough
// for a short ticker: one GET per backend, no characterization, no bench.
func (rt *Runtime) ResidencyRefresh(ctx context.Context) bool {
	type key struct{ backend, raw string }
	rt.mu.Lock()
	var changed bool
	snapshot := map[key]int{}
	for bi := range rt.Backends {
		e := &rt.Backends[bi]
		residencer, ok := e.Client.(backend.Residencer)
		if !ok {
			continue
		}
		ps, err := residencer.ProbeResidency(ctx)
		if err != nil {
			// Unknowable residency (vLLM: no /api/ps) or a flaky endpoint —
			// keep the previous snapshot rather than flapping the cards.
			rt.log.Debug("residency probe failed — keeping previous", "backend", e.ID, "err", err)
			continue
		}
		for i := range e.Models {
			want := ps[e.Models[i].RawID]
			// The operator's --serving-ctx pin wins over residency, warm or
			// cold. Without this, the first tick after a model unloads
			// clobbers the pin to 0: the card re-advertises "window unknown"
			// and, when the model warms again, the backend's own (possibly
			// different) window — the operator's measured number must survive
			// both states, same rule applyServingCtx enforces.
			if o := rt.servingCtxOverride(e.Models[i].RawID, e.Models[i].ModelID); o > 0 {
				want = o
			}
			if e.Models[i].ServingCtx != want {
				e.Models[i].ServingCtx = want
				changed = true
			}
			snapshot[key{e.ID, e.Models[i].RawID}] = want
		}
	}
	if changed {
		for i := range rt.Models {
			if want, ok := snapshot[key{rt.Models[i].BackendID, rt.Models[i].RawID}]; ok && rt.Models[i].ServingCtx != want {
				rt.Models[i].ServingCtx = want
			}
		}
	}
	rt.mu.Unlock()
	if changed {
		rt.changed()
	}
	return changed
}

// Resident reports whether a model is loaded in its backend right now —
// the dispatch-time check behind cold-load handling. Backends without a
// residency endpoint (or a failed probe) answer "resident": an assumed
// cold load must never block a request that would have worked. A nil
// entry is the legacy hand-built runtime (tests): its Client decides.
func (rt *Runtime) Resident(ctx context.Context, entry *BackendEntry, rawID string) bool {
	rt.mu.RLock()
	var client backend.Client
	if entry != nil {
		client = entry.Client
	} else {
		client = rt.Client
	}
	rt.mu.RUnlock()
	if client == nil {
		return false
	}
	residencer, ok := client.(backend.Residencer)
	if !ok {
		return true
	}
	ps, err := residencer.ProbeResidency(ctx)
	if err != nil {
		return true
	}
	_, loaded := ps[rawID]
	return loaded
}

// applyServingCtx folds what the backend says about its own serving window
// — vLLM's max_model_len from /v1/models, llama.cpp's /props n_ctx, the
// operator's --serving-ctx pin — into the card. The explicit pin wins
// outright; the backend's own claim sets ServingCtx and fills MaxCtx only
// where nothing else knew. Returns whether the card changed (a backend
// relaunched with a different window re-advertises).
// servingCtxOverride returns the operator's --serving-ctx pin for a card,
// by raw id first, then by card id — the lookup both pin consumers share
// (applyServingCtx at characterization, ResidencyRefresh every tick).
func (rt *Runtime) servingCtxOverride(raw, modelID string) int {
	if o := rt.opts.ServingCtxOverrides[raw]; o > 0 {
		return o
	}
	return rt.opts.ServingCtxOverrides[modelID]
}

func (rt *Runtime) applyServingCtx(card *ServedModel, client backend.Client, raw string) bool {
	override := rt.servingCtxOverride(raw, card.ModelID)
	detected := 0
	if cr, ok := client.(backend.CtxReporter); ok {
		detected = cr.ServingCtxs()[raw]
	}
	ctx := override
	if ctx == 0 {
		ctx = detected
	}
	if ctx <= 0 {
		return false
	}
	changed := card.ServingCtx != ctx
	card.ServingCtx = ctx
	if card.MaxCtx == 0 || override > 0 {
		if card.MaxCtx != ctx {
			changed = true
		}
		card.MaxCtx = ctx
	}
	return changed
}
