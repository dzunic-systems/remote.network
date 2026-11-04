package cli

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"remote.network/node/internal/schedule"
	"remote.network/node/wire"
)

// servingGate applies the owner's serving hours to the list of models the
// heartbeat reports. With no schedule it is always open and only passes the
// list through. Closed, the session stays up, so the node keeps its uptime and
// reopens within seconds. Requests already running finish.
type servingGate struct {
	sched  *schedule.Schedule
	now    func() time.Time
	models func() []string    // what the node could serve
	set    func(ids []string) // the heartbeat's serving list
	notify func() bool        // re-advertise the cards at once
	log    *slog.Logger

	mu     sync.Mutex
	opened bool // state the last apply left
}

func (g *servingGate) isOpen() bool { return g.sched.Open(g.now()) }

// apply sets the serving list for the current time and returns whether the
// gate is open.
func (g *servingGate) apply() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.opened = g.isOpen()
	if g.opened {
		g.set(g.models())
	} else {
		g.set(nil)
	}
	return g.opened
}

// advertise re-sends the cards after a local change. It holds off while closed:
// the router treats an advertisement as "serving these", so the cards go out
// when the gate next opens.
func (g *servingGate) advertise() bool {
	if !g.apply() {
		return false
	}
	return g.notify()
}

// tick moves the gate when the clock has crossed a boundary.
func (g *servingGate) tick() {
	g.mu.Lock()
	was := g.opened
	g.mu.Unlock()
	if g.isOpen() == was {
		return
	}
	if g.apply() {
		g.notify()
		g.log.Info("serving hours began: offering models", "until", g.nextChange())
		return
	}
	g.log.Info("serving hours ended: running requests finish, no new ones are taken", "until", g.nextChange())
}

func (g *servingGate) nextChange() string {
	next, ok := g.sched.Next(g.now())
	if !ok {
		return "no change scheduled"
	}
	return next.Format("Mon 15:04 MST")
}

// run ticks until ctx ends. The agent's first shutdown signal ends ctx, so the
// gate cannot reopen the serving list during a drain. The short period also
// covers a clock that jumped while the machine slept.
func (g *servingGate) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick()
		}
	}
}

// servingHandler is what serves dispatches: the inference and embedding surfaces.
type servingHandler interface {
	HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)
	HandleEmbed(ctx context.Context, req wire.EmbedRequestData) (wire.EmbedDoneData, error)
}

// scheduledHandler refuses work outside serving hours. The router can still
// pick the node for a few seconds, until the next heartbeat or after a
// reconnect; busy sends those requests straight to the next candidate and does
// not count against the node's reputation.
type scheduledHandler struct {
	servingHandler
	gate *servingGate
}

func (h scheduledHandler) HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	if !h.gate.isOpen() {
		return wire.InferenceDoneData{}, wire.ErrBusy
	}
	return h.servingHandler.HandleInference(ctx, req, emit)
}

func (h scheduledHandler) HandleEmbed(ctx context.Context, req wire.EmbedRequestData) (wire.EmbedDoneData, error) {
	if !h.gate.isOpen() {
		return wire.EmbedDoneData{}, wire.ErrBusy
	}
	return h.servingHandler.HandleEmbed(ctx, req)
}
