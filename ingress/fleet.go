// fleet.go: multi-router attach. One
// battle-tested Client per router; a thin supervisor unions the
// operator's seed list (--router, which may be a single URL — an
// incomplete list is the point) with the root-verified entries the
// serving routers advertise (router.directory). Trust never lives here:
// entries arrive pre-verified against the node's pinned root (see
// noteDirectory), and --router-strict keeps the world seed-only. Every
// extra client shares the node identity, the serving runtime, and the
// HomeDir; each holds its own control channel, punch socket, and — the
// reason any of this exists — its own demand path.
//
// Removal: a router that disappears from the directory is
// detached only after a grace window. The point is not stubbornness —
// views all come from the same spine — but ordering: a stale push must
// be correctable by a newer one before a live session dies for it. A
// router absent past the grace is genuinely gone from the registry, and
// its session is cancelled; the router's reaper marks the node offline
// on its side.
package ingress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"remote.network/node/wire"
)

// Defaults for the attach cap and removal hysteresis; both configurable
// (tests shrink them).
const (
	DefaultMaxRouters   = 8
	DefaultRemovalGrace = 30 * time.Second
	reaperTick          = 5 * time.Second
)

// FleetConfig shapes the supervisor. Base is the per-router client
// template (identity, handler, capability closure, root pin, …) —
// RouterURL and OnDirectory are overridden per client.
type FleetConfig struct {
	Seeds        []string      // --router connect URLs; the operator's known set
	Strict       bool          // --router-strict: never attach beyond the seeds
	MaxRouters   int           // total session cap including seeds (default 8)
	RemovalGrace time.Duration // directory-absent grace before detach (default 30s)
	Base         Config
	Log          *slog.Logger
}

type attached struct {
	client  *Client
	cancel  context.CancelFunc
	seed    bool
	missing time.Time // first directory update this extra was absent from; zero = present
}

// Fleet owns one Client per attached router.
type Fleet struct {
	cfg FleetConfig
	log *slog.Logger

	ctx     context.Context // set by Run; directory attaches live under it
	mu      sync.Mutex
	clients map[string]*attached // connect URL → client
	seedErr chan error           // first terminal error from a seed client
	serving []string             // last SetServingModels list; seeds every new client
}

// NewFleet builds the supervisor.
func NewFleet(cfg FleetConfig) *Fleet {
	if cfg.MaxRouters <= 0 {
		cfg.MaxRouters = DefaultMaxRouters
	}
	if cfg.RemovalGrace <= 0 {
		cfg.RemovalGrace = DefaultRemovalGrace
	}
	if cfg.Log == nil {
		cfg.Log = cfg.Base.Log
	}
	return &Fleet{
		cfg:     cfg,
		log:     cfg.Log,
		clients: map[string]*attached{},
		seedErr: make(chan error, 1),
	}
}

// Run attaches every seed, starts the removal reaper, and supervises the
// fleet until ctx is done or a SEED client fails terminally (pin
// mismatch, refused node token — the operator must act; extras failing
// terminal are logged and dropped).
func (f *Fleet) Run(ctx context.Context) error {
	f.mu.Lock()
	f.ctx = ctx
	f.mu.Unlock()
	for i, url := range f.cfg.Seeds {
		if err := f.attach(ctx, url, i == 0); err != nil {
			return err
		}
	}
	go f.reap(ctx)
	select {
	case <-ctx.Done():
		return nil
	case err := <-f.seedErr:
		return err
	}
}

// attach builds and starts one client. seed marks the operator's own
// list: only a seed's terminal error kills the fleet, only a seed is
// exempt from removal hysteresis, and only a seed honors an explicit
// --router-key (extras are root-verified, and a seed's key pin would
// only mismatch on them).
func (f *Fleet) attach(ctx context.Context, url string, seed bool) error {
	f.mu.Lock()
	if _, ok := f.clients[url]; ok {
		f.mu.Unlock()
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	cfg := f.cfg.Base
	cfg.RouterURL = url
	cfg.OnDirectory = f.onDirectory
	if !seed {
		cfg.RouterKey = "" // the pinned root governs directory attaches
	}
	client := New(cfg)
	client.SetServingModels(f.serving)
	f.clients[url] = &attached{client: client, cancel: cancel, seed: seed}
	f.mu.Unlock()

	go func() {
		err := client.Run(cctx)
		if cctx.Err() != nil {
			return // fleet shutdown or deliberate detach; not an event
		}
		if err != nil {
			if seed {
				select {
				case f.seedErr <- err:
				default:
				}
				return
			}
			f.log.Warn("directory-attached router session terminated terminally — dropping it",
				"router_url", url, "err", err)
		}
		f.mu.Lock()
		delete(f.clients, url)
		f.mu.Unlock()
	}()
	return nil
}

// onDirectory recomputes the desired set from one client's verified view.
// Entries are already root-chained (noteDirectory filtered them): new
// members attach (seeds ∪ entries, deduped, capped) and absent extras
// start their grace clock — the reaper does the detaching.
func (f *Fleet) onDirectory(entries []wire.DirectoryEntry) {
	f.mu.Lock()
	ctx := f.ctx
	urls := map[string]bool{} // live attach points
	ids := map[string]bool{}  // their router ids (URL spelling differs → still dedupes)
	for url, a := range f.clients {
		urls[url] = true
		if id := a.client.RouterID(); id != "" {
			ids[id] = true
		}
	}
	seen := func(url, routerID string) bool { // present by URL or router id
		for _, e := range entries {
			if e.URL == url || (routerID != "" && e.RouterID == routerID) {
				return true
			}
		}
		return false
	}
	now := time.Now()
	for u, a := range f.clients {
		if a.seed {
			continue // the operator's list is not subject to directory churn
		}
		if seen(u, a.client.RouterID()) {
			a.missing = time.Time{}
		} else if a.missing.IsZero() {
			a.missing = now // grace clock starts
		}
	}
	f.mu.Unlock()
	if ctx == nil {
		return // Run has not started; nothing to attach under
	}

	var want []string
	for _, url := range f.cfg.Seeds {
		if !urls[url] {
			want = append(want, url)
		}
	}
	if !f.cfg.Strict {
		for _, e := range entries {
			if urls[e.URL] || ids[e.RouterID] {
				continue
			}
			f.mu.Lock()
			capLeft := f.cfg.MaxRouters-len(f.clients) > 0
			f.mu.Unlock()
			if !capLeft {
				f.log.Warn("router cap reached — entry not attached",
					"router_id", e.RouterID, "cap", f.cfg.MaxRouters)
				continue
			}
			urls[e.URL] = true
			ids[e.RouterID] = true
			want = append(want, e.URL)
		}
	}
	for _, url := range want {
		if err := f.attach(ctx, url, false); err != nil {
			f.log.Warn("attach failed", "router_url", url, "err", err)
		}
	}
}

// reap detaches extras whose directory absence outlived the grace.
func (f *Fleet) reap(ctx context.Context) {
	t := time.NewTicker(reaperTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.mu.Lock()
			var doomed []string
			now := time.Now()
			for url, a := range f.clients {
				if !a.seed && !a.missing.IsZero() && now.Sub(a.missing) > f.cfg.RemovalGrace {
					doomed = append(doomed, url)
				}
			}
			for _, url := range doomed {
				a := f.clients[url]
				delete(f.clients, url)
				a.cancel()
			}
			f.mu.Unlock()
			for _, url := range doomed {
				f.log.Info("router left the certified directory — detaching", "router_url", url)
			}
		}
	}
}

// SetServingModels fans the heartbeat serving list out to every router,
// and remembers it for routers attached later — the node CLI sets it
// before Run, when no client exists yet, and a directory attach arrives
// long after the last change.
func (f *Fleet) SetServingModels(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serving = ids
	for _, a := range f.clients {
		a.client.SetServingModels(ids)
	}
}

// SendCapabilityUpdate re-advertises cards on every session.
func (f *Fleet) SendCapabilityUpdate() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := false
	for _, a := range f.clients {
		if a.client.SendCapabilityUpdate() {
			sent = true
		}
	}
	return sent
}

// Drain asks every session to finish in-flight work and stop.
func (f *Fleet) Drain(ctx context.Context, deadline time.Duration) {
	f.mu.Lock()
	clients := make([]*Client, 0, len(f.clients))
	for _, a := range f.clients {
		clients = append(clients, a.client)
	}
	f.mu.Unlock()
	for _, c := range clients {
		c.Drain(ctx, deadline)
	}
}

// Clients snapshots the attached clients (tests, status surfaces).
func (f *Fleet) Clients() map[string]*Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*Client, len(f.clients))
	for url, a := range f.clients {
		out[url] = a.client
	}
	return out
}
