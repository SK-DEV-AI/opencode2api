package config

import "testing"

// ParseProxyEntry decides whether a configured proxy entry is a rotating
// "#pool" entry and strips the marker. The gateway treats a pool entry
// differently (no keep-alives, HTTP/1.1 ALPN, a fresh tunnel per attempt), so
// the parsing rules are worth pinning.
func TestParseProxyEntry(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		pool bool
	}{
		{"direct", "direct", false},
		{"", "", false},
		{"  http://127.0.0.1:7890  ", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890#pool", "http://127.0.0.1:7890", true},
		{"http://127.0.0.1:7890#POOL", "http://127.0.0.1:7890", true},
		{"http://user:pass@127.0.0.1:7890#pool", "http://user:pass@127.0.0.1:7890", true},
		{"socks5://127.0.0.1:1080#pool", "socks5://127.0.0.1:1080", true},
		{"socks5h://127.0.0.1:1080#pool", "socks5h://127.0.0.1:1080", true},
		{"pool+http://127.0.0.1:7890", "http://127.0.0.1:7890", true},
		{"POOL+http://127.0.0.1:7890", "http://127.0.0.1:7890", true},
	}
	for _, tc := range cases {
		got, pool := ParseProxyEntry(tc.raw)
		if got != tc.want || pool != tc.pool {
			t.Errorf("ParseProxyEntry(%q) = (%q, %v), want (%q, %v)", tc.raw, got, pool, tc.want, tc.pool)
		}
	}
}
