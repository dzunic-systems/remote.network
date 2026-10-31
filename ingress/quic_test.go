package ingress

import (
	"testing"

	"remote.network/node/wire"
)

func TestClassifyNAT(t *testing.T) {
	cases := []struct {
		local, main, alt string
		wantType         string
		wantCandidate    bool
	}{
		// No NAT: router sees exactly our socket on both probes.
		{"127.0.0.5:51000", "127.0.0.5:51000", "127.0.0.5:51000", wire.NATOpen, true},
		// Cone NAT: one public mapping for both destinations.
		{"10.0.0.4:51000", "203.0.113.7:49152", "203.0.113.7:49152", wire.NATCone, true},
		// Symmetric: per-destination mappings → relay fallback.
		{"10.0.0.4:51000", "203.0.113.7:49152", "203.0.113.7:49153", wire.NATSymmetric, false},
		// Missing observation → unknown, not a candidate.
		{"10.0.0.4:51000", "203.0.113.7:49152", "", "unknown", false},
		{"10.0.0.4:51000", "", "203.0.113.7:49153", "unknown", false},
	}
	for _, c := range cases {
		gotType, gotCand := classifyNAT(c.local, c.main, c.alt)
		if gotType != c.wantType || gotCand != c.wantCandidate {
			t.Errorf("classifyNAT(%q,%q,%q) = %s,%v; want %s,%v",
				c.local, c.main, c.alt, gotType, gotCand, c.wantType, c.wantCandidate)
		}
	}
}
