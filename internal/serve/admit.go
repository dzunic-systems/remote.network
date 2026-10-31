// admit.go: late backend admission. A backend that was down when the agent
// started (its vLLM service boots later, the operator starts and stops
// backends on their own schedule) used to stay invisible until the next
// agent restart — the startup probe was the only admission gate, and every
// later refresh path (Refresh, ResidencyRefresh, node.reprobe) walks only
// the already-admitted set. The admit loop keeps those specs and re-probes
// them on a timer: the moment the endpoint answers, its cards join the
// serving set and ride the normal changed() → SendCapabilityUpdate path to
// the router. A spec that fails with a config error (unparseable URL,
// unknown kind) can never succeed and is dropped instead of retried.
package serve

import (
	"context"
	"time"
)

// admitTimeout bounds one admission attempt: it may characterize (and
// bench, 10 min) the backend's whole model list — the same budget a
// startup probe effectively has.
const admitTimeout = 15 * time.Minute

// admitLoop re-probes the down-at-startup backends until they answer or
// the process ends. One attempt per tick: admission characterizes and
// benches, and two concurrent admissions would race the probe cache and
// the bench for nothing.
func (rt *Runtime) admitLoop(ctx context.Context) {
	t := time.NewTicker(rt.opts.AdmitRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			actx, cancel := context.WithTimeout(ctx, admitTimeout)
			if rt.admitPending(actx) {
				rt.log.Info("late backend admitted — capability update follows")
			}
			cancel()
		}
	}
}

// admitPending re-probes every backend that failed at startup and admits
// the ones that answer now. Returns whether the serving set changed —
// the caller re-advertises (admitLoop fires changed(), the CLI's OnChange
// wiring, same as every other card mutation).
func (rt *Runtime) admitPending(ctx context.Context) bool {
	rt.mu.RLock()
	pending := make([]BackendSpec, len(rt.pending))
	copy(pending, rt.pending)
	rt.mu.RUnlock()
	if len(pending) == 0 {
		return false
	}

	changed := false
	for _, spec := range pending {
		if ctx.Err() != nil {
			return changed
		}
		entry, err := rt.prepareBackend(ctx, spec)
		if err != nil {
			// Config errors are deterministic — the spec already parsed its
			// kind at startup, so a failure here is a bad URL. Retry would
			// never succeed: drop the spec instead.
			rt.mu.Lock()
			rt.dropPendingLocked(spec.ID)
			rt.mu.Unlock()
			rt.log.Warn("backend config error — dropped from retry", "backend", spec.ID, "err", err)
			continue
		}
		if entry == nil {
			rt.log.Debug("backend still down — keeping in retry", "backend", spec.ID)
			continue
		}
		rt.mu.Lock()
		rt.Backends = append(rt.Backends, *entry)
		rt.dropPendingLocked(spec.ID)
		rt.rebuildFlatLocked()
		rt.mu.Unlock()
		changed = true
		rt.log.Info("backend came up — admitted", "backend", spec.ID, "models", len(entry.Models))
	}
	if changed {
		if rt.cache != nil {
			rt.cache.Save()
		}
		rt.changed()
	}
	return changed
}

// dropPendingLocked removes one spec from the retry list (no-op when the
// id is not pending). Caller holds rt.mu for writing.
func (rt *Runtime) dropPendingLocked(id string) {
	out := rt.pending[:0]
	for _, p := range rt.pending {
		if p.ID != id {
			out = append(out, p)
		}
	}
	rt.pending = out
}

// specIDs renders a spec list for log lines.
func specIDs(specs []BackendSpec) []string {
	ids := make([]string, len(specs))
	for i, s := range specs {
		ids[i] = s.ID
	}
	return ids
}
