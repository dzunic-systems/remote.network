package livestats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"remote.network/node/wire"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// wireBackendStats is the wire type, aliased for the assertions.
type wireBackendStats = wire.BackendStats

// §5 N2/N6: one round publishes the newest reading behind the pointer,
// writes live.json (0600, atomic rename) with the connection lines, and
// a backend whose dialect is unreadable still reports its agent-side
// counts (slots/held/busy) with no metrics fields.
func TestSamplerRound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(llamacppGolden))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	s := New(func() []BackendLive {
		return []BackendLive{
			{ID: "lc", Kind: "llamacpp", URL: srv.URL, Models: []string{"stories"}, Slots: 4, Held: 2, Busy1m: 7},
			{ID: "dead", Kind: "openai-compatible", URL: "http://127.0.0.1:1", Slots: 1},
		}
	}, home+"/live.json", func() []ConnState {
		return []ConnState{{Name: "c1", Connected: true, InFlightConn: 3}}
	}, nil)
	// One round: Run's loop blocks on the ticker, so tests drive the round
	// directly (the immediate first round inside Run is the same call).
	s.round(context.Background())

	st := s.Latest()
	if st == nil {
		t.Fatal("no stats published")
	}
	if st.CPU == nil || st.CPU.Cores <= 0 {
		t.Fatalf("cpu missing: %+v", st.CPU)
	}
	if len(st.Backends) != 2 {
		t.Fatalf("backends = %d", len(st.Backends))
	}
	var lc, dead *wireBackendStats
	for i := range st.Backends {
		if st.Backends[i].ID == "lc" {
			lc = &st.Backends[i]
		}
		if st.Backends[i].ID == "dead" {
			dead = &st.Backends[i]
		}
	}
	if lc == nil || lc.Source != "metrics" || lc.Held != 2 || lc.Slots != 4 || lc.Busy1m != 7 {
		t.Fatalf("lc = %+v", lc)
	}
	if lc.Running == nil || *lc.Running != 1 {
		t.Fatalf("lc running = %v", lc.Running)
	}
	if dead == nil || dead.Source != "agent" || dead.Running != nil || dead.KV != nil {
		t.Fatalf("dead backend must stay agent-sourced with nil metrics: %+v", dead)
	}

	// live.json exists, is 0600 (Unix only: Windows has no permission
	// bits and reports every writable file as 0666), and carries the
	// connection line.
	info, err := os.Stat(home + "/live.json")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("live.json mode = %v", info.Mode().Perm())
	}
	doc := struct {
		At    string `json:"at"`
		Conns []struct {
			Name         string `json:"name"`
			Connected    bool   `json:"connected"`
			InFlightConn int    `json:"in_flight_conn"`
		} `json:"conns"`
	}{}
	b, err := os.ReadFile(home + "/live.json")
	if err != nil {
		t.Fatal(err)
	}
	if jsonUnmarshal(b, &doc) != nil {
		t.Fatalf("bad json: %s", b)
	}
	if len(doc.Conns) != 1 || doc.Conns[0].Name != "c1" || !doc.Conns[0].Connected || doc.Conns[0].InFlightConn != 3 {
		t.Fatalf("conns: %+v", doc.Conns)
	}
	if _, err := time.Parse(time.RFC3339, doc.At); err != nil {
		t.Fatalf("sample time: %v", err)
	}
	// No leftover temp file (atomic rename).
	if _, err := os.Stat(home + "/live.json.tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file survived: %v", err)
	}
}
