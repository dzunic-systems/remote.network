package cli

import "testing"

// The observed failure mode: `ws://http://…` parses with host "http", and the
// agent asks DNS to resolve that literal name on every backoff tick.
func TestValidateRouterURL(t *testing.T) {
	cases := []struct {
		raw    string
		wantOK bool
	}{
		{"ws://localhost:8080/node/v1/connect", true},
		{"ws://203.0.113.10:38080/node/v1/connect", true},
		{"wss://router.example.com/node/v1/connect", true},
		{"ws://http://203.0.113.10:38080/node/v1/connect", false}, // double scheme
		{"wss://https://router.example.com/node/v1/connect", false},
		{"ws://203.0.113.10/:38080/node/v1/connect", false},  // port in the path
		{"http://203.0.113.10:38080/node/v1/connect", false}, // wrong scheme
		{"203.0.113.10:38080/node/v1/connect", false},        // no scheme
		{"ws://", false}, // no host
	}
	for _, tc := range cases {
		err := validateRouterURL(tc.raw)
		if tc.wantOK != (err == nil) {
			t.Errorf("validateRouterURL(%q) = %v, wantOK=%v", tc.raw, err, tc.wantOK)
		}
	}
}

// TestClientDirectOnByDefault: since v0.1.14 a node serves remote-client
// users directly unless its owner opts out with --no-client-direct.
func TestClientDirectOnByDefault(t *testing.T) {
	cmd := newStartCommand()
	on := cmd.Flags().Lookup("client-direct")
	off := cmd.Flags().Lookup("no-client-direct")
	if on == nil || on.DefValue != "true" {
		t.Fatalf("--client-direct default = %v, want true", on)
	}
	if off == nil || off.DefValue != "false" {
		t.Fatalf("--no-client-direct missing or on by default: %v", off)
	}
	// Existing service units pass --client-direct explicitly: still accepted.
	if err := cmd.Flags().Parse([]string{"--client-direct", "--no-client-direct"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
}
