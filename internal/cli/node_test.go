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
