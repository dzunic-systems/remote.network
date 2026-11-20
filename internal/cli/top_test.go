package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §5 N6: `remote node top` reads live.json — fresh sample renders, a
// stale one says the agent is not running (last sample + age), never a
// live-looking number from a dead agent.
func TestTopReadsLiveFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "live.json")
	body := `{
	  "at": "` + time.Now().UTC().Format(time.RFC3339) + `",
	  "cpu": {"load1": 2.5, "cores": 16},
	  "mem": {"total_mb": 125952, "used_mb": 94208, "available_mb": 31744},
	  "gpus": [{"index": 0, "util": 0.31, "power_w": 11.6, "temp_c": 50}],
	  "backends": [
	    {"id": "tf-gb10", "kind": "tensorfold", "source": "metrics", "models": ["m"],
	     "slots": 10, "held": 3, "busy_1m": 2, "running": 3, "waiting": 0, "kv": 0.38,
	     "decode_tps": 75.5, "mem_used_mb": 82791},
	    {"id": "agent-only", "kind": "agent", "source": "agent", "models": [],
	     "slots": 1, "held": 0, "busy_1m": 0}
	  ],
	  "conns": [{"name": "c1", "connected": true, "in_flight_conn": 2}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// printTop writes to stdout; capture via os.Pipe is overkill here —
	// assert the parse and freshness logic through the error paths and a
	// tiny refactor seam (renderTop returns the text).
	out, err := renderTop(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"load 2.50 ×16", "92/123 GiB", "31%", "11.6W", "50°C",
		"tf-gb10", "running 3 waiting 0", "kv 0.38", "75.5 tok/s", "82791 MiB",
		"agent-only", "c1  connected · in-flight 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("top output missing %q:\n%s", want, out)
		}
	}

	// Stale: the agent is not running, the file says so honestly.
	stale := strings.Replace(body,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().Add(-30*time.Second).UTC().Format(time.RFC3339), 1)
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = renderTop(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	// Stale means the honest dead-agent line — and none of the live
	// numbers (the exact age string wobbles by a second at boundaries).
	if !strings.Contains(out, "agent not running") || strings.Contains(out, "backend tf-gb10") {
		t.Fatalf("stale sample not reported honestly: %s", out)
	}

	// No file at all: a clear error naming the path.
	if _, err := renderTop(filepath.Join(home, "none.json"), time.Now); err == nil {
		t.Fatal("missing file accepted")
	}
}
