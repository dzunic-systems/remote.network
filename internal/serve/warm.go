// warm.go: keep-warm — the node loads its pinned models into backend
// memory at startup (and keeps them resident with keep_alive=-1), so the
// cold-load window happens before traffic, not during a request. Scope is
// deliberately narrow: "pinned" (the default) warms exactly the --model
// list / managed model the operator sized to the machine — never the whole
// advertisement. While a load runs the card advertises state=loading, which
// the router reads as "stretch the first-token window, skip the reputation
// penalty" (chat.go).
package serve

import (
	"context"
	"errors"
	"time"

	"remote.network/node/internal/backend"
)

// Keep-warm modes (Options.KeepWarm).
const (
	KeepWarmPinned = "pinned" // default: the --model list / managed model only
	KeepWarmNone   = "none"
	KeepWarmAll    = "all" // every advertised card — big machines only
)

// OnChange registers the callback fired whenever a card's state flips —
// the CLI wires it to Client.SendCapabilityUpdate so the router sees the
// loading→ready transitions live. Must be set before Warm runs.
func (rt *Runtime) OnChange(fn func()) {
	rt.onChange = fn
}

func (rt *Runtime) changed() {
	if rt.onChange != nil {
		rt.onChange()
	}
}

// setState flips one card's state in both views (flat + per-backend).
// Returns whether anything changed (no update storms on no-ops).
func (rt *Runtime) setState(modelID, state string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	changed := false
	for i := range rt.Models {
		if rt.Models[i].ModelID == modelID && rt.Models[i].State != state {
			rt.Models[i].State = state
			changed = true
		}
	}
	for bi := range rt.Backends {
		for i := range rt.Backends[bi].Models {
			if rt.Backends[bi].Models[i].ModelID == modelID && rt.Backends[bi].Models[i].State != state {
				rt.Backends[bi].Models[i].State = state
				changed = true
			}
		}
	}
	return changed
}

// cardStates snapshots the current state of every warmable card keyed by
// model id, with the backend that serves it.
type warmTarget struct {
	backend *BackendEntry
	card    ServedModel
}

func (rt *Runtime) warmTargets() []warmTarget {
	mode := rt.opts.KeepWarm
	if mode == "" {
		mode = KeepWarmPinned
	}
	if mode == KeepWarmNone {
		return nil
	}
	pinned := map[string]bool{}
	for _, m := range rt.opts.Models {
		pinned[m] = true
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var out []warmTarget
	for bi := range rt.Backends {
		if _, ok := rt.Backends[bi].Client.(backend.Warmer); !ok {
			continue // managed llama-server loads at start; anthropic kind has no loader
		}
		for i := range rt.Backends[bi].Models {
			card := rt.Backends[bi].Models[i]
			if mode == KeepWarmAll || pinned[card.RawID] || pinned[card.ModelID] {
				out = append(out, warmTarget{backend: &rt.Backends[bi], card: card})
			}
		}
	}
	return out
}

// Warm loads the keep-warm set, one model at a time (a load is memory- and
// disk-bound; parallel loads just thrash). Announces state=loading per card
// before its load and restores the prior state after — success or not, the
// card stays servable either way.
func (rt *Runtime) Warm(ctx context.Context) {
	targets := rt.warmTargets()
	if len(targets) == 0 {
		return
	}
	rt.log.Info("keep-warm loading pinned models", "count", len(targets))
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		rt.warmOne(ctx, t)
	}
}

func (rt *Runtime) warmOne(ctx context.Context, t warmTarget) {
	w, ok := t.backend.Client.(backend.Warmer)
	if !ok {
		return
	}
	prev := t.card.State
	if rt.setState(t.card.ModelID, "loading") {
		rt.changed()
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	err := w.WarmModel(wctx, t.card.RawID)
	cancel()
	switch {
	case err == nil:
		rt.log.Info("keep-warm loaded", "model", t.card.ModelID, "raw", t.card.RawID)
	case errors.Is(ctx.Err(), context.Canceled):
		rt.log.Warn("keep-warm interrupted", "model", t.card.ModelID)
	default:
		rt.log.Warn("keep-warm load failed — card stays servable, first request pays the load",
			"model", t.card.ModelID, "err", err)
	}
	if rt.setState(t.card.ModelID, prev) {
		rt.changed()
	}
}
