package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"remote.network/node/internal/schedule"
	"remote.network/node/wire"
)

type gateFixture struct {
	gate    *servingGate
	now     time.Time
	serving []string
	notices int
}

func newGateFixture(t *testing.T, specs ...string) *gateFixture {
	t.Helper()
	s, err := schedule.Parse(specs, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	f := &gateFixture{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), serving: []string{"unset"}}
	f.gate = &servingGate{
		sched:  s,
		now:    func() time.Time { return f.now },
		models: func() []string { return []string{"m1"} },
		set:    func(ids []string) { f.serving = ids },
		notify: func() bool { f.notices++; return true },
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

func TestGateFollowsTheClock(t *testing.T) {
	f := newGateFixture(t, "22:00-07:00")
	if f.gate.apply() || f.serving != nil {
		t.Fatalf("midday: open or serving %v", f.serving)
	}
	f.gate.tick()
	if f.notices != 0 {
		t.Fatalf("no boundary crossed, %d notices", f.notices)
	}

	f.now = time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC)
	f.gate.tick()
	if !reflect.DeepEqual(f.serving, []string{"m1"}) || f.notices != 1 {
		t.Fatalf("opening: serving %v, %d notices", f.serving, f.notices)
	}
	f.gate.tick()
	if f.notices != 1 {
		t.Fatalf("still open, %d notices", f.notices)
	}

	f.now = time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	f.gate.tick()
	if f.serving != nil || f.notices != 1 {
		t.Fatalf("closing: serving %v, %d notices", f.serving, f.notices)
	}
}

func TestGateHoldsAdvertisementsWhileClosed(t *testing.T) {
	f := newGateFixture(t, "22:00-07:00")
	if f.gate.advertise() || f.notices != 0 || f.serving != nil {
		t.Fatalf("closed advertise: %d notices, serving %v", f.notices, f.serving)
	}
	f.now = time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	if !f.gate.advertise() || f.notices != 1 || len(f.serving) != 1 {
		t.Fatalf("open advertise: %d notices, serving %v", f.notices, f.serving)
	}
}

func TestGateWithoutScheduleIsAlwaysOpen(t *testing.T) {
	f := newGateFixture(t, "22:00-07:00")
	f.gate.sched = nil
	if !f.gate.apply() || len(f.serving) != 1 {
		t.Fatalf("serving %v", f.serving)
	}
	if !f.gate.advertise() || f.notices != 1 {
		t.Fatalf("%d notices", f.notices)
	}
}

type countingHandler struct{ inference, embed int }

func (h *countingHandler) HandleInference(context.Context, wire.InferenceRequestData, func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	h.inference++
	return wire.InferenceDoneData{}, nil
}

func (h *countingHandler) HandleEmbed(context.Context, wire.EmbedRequestData) (wire.EmbedDoneData, error) {
	h.embed++
	return wire.EmbedDoneData{}, nil
}

func TestScheduledHandlerRefusesOutsideHours(t *testing.T) {
	f := newGateFixture(t, "22:00-07:00")
	inner := &countingHandler{}
	h := scheduledHandler{servingHandler: inner, gate: f.gate}
	ctx := context.Background()

	if _, err := h.HandleInference(ctx, wire.InferenceRequestData{}, nil); !errors.Is(err, wire.ErrBusy) {
		t.Errorf("closed inference: %v", err)
	}
	if _, err := h.HandleEmbed(ctx, wire.EmbedRequestData{}); !errors.Is(err, wire.ErrBusy) {
		t.Errorf("closed embed: %v", err)
	}
	if inner.inference != 0 || inner.embed != 0 {
		t.Fatalf("closed handler reached the backend: %+v", inner)
	}

	f.now = time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	if _, err := h.HandleInference(ctx, wire.InferenceRequestData{}, nil); err != nil {
		t.Errorf("open inference: %v", err)
	}
	if _, err := h.HandleEmbed(ctx, wire.EmbedRequestData{}); err != nil {
		t.Errorf("open embed: %v", err)
	}
	if inner.inference != 1 || inner.embed != 1 {
		t.Fatalf("open handler: %+v", inner)
	}
}
