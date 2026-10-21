package gateway

import (
	"testing"
)

// The 413 must only fire when EVERY attempt failed in the write phase. A
// mixed turnout (some attempts timed out waiting for headers, one timed out
// writing the body) is not a payload problem: the headers phase means the
// upstream was merely slow to answer, and telling the client to shrink a
// payload that was fine is worse than a plain timeout.
//
// Live evidence (2026-10-20): two real 413s were reported on turnouts whose
// legs mixed attempt_timeout_headers and attempt_timeout_write, so both were
// misclassified.
func TestPayloadTooLargeAttemptsRequiresAllWrites(t *testing.T) {
	cases := []struct {
		name       string
		legs       []string
		wantWrites int
		wantTotal  int
		wantAll    bool
	}{
		{
			name:       "all writes",
			legs:       []string{"anon:attempt_timeout_write:15000ms", "K5Gkj:attempt_timeout_write:15000ms"},
			wantWrites: 2,
			wantTotal:  2,
			wantAll:    true,
		},
		{
			name:       "mixed headers and write",
			legs:       []string{"anon:attempt_timeout_headers:15000ms", "K5Gkj:attempt_timeout_write:15000ms"},
			wantWrites: 1,
			wantTotal:  2,
			wantAll:    false,
		},
		{
			name:       "headers only",
			legs:       []string{"anon:attempt_timeout_headers:15000ms"},
			wantWrites: 0,
			wantTotal:  1,
			wantAll:    false,
		},
		{
			name:       "a successful leg is not a write timeout",
			legs:       []string{"anon:attempt_timeout_write:15000ms", "K5Gkj:200:1044ms"},
			wantWrites: 1,
			wantTotal:  2,
			wantAll:    false,
		},
		{
			name:       "no legs",
			legs:       nil,
			wantWrites: 0,
			wantTotal:  0,
			wantAll:    true,
		},
	}
	for _, tc := range cases {
		writes, total, all := payloadTooLargeAttempts(tc.legs)
		if writes != tc.wantWrites {
			t.Errorf("%s: writes = %d, want %d", tc.name, writes, tc.wantWrites)
		}
		if total != tc.wantTotal {
			t.Errorf("%s: total = %d, want %d", tc.name, total, tc.wantTotal)
		}
		if all != tc.wantAll {
			t.Errorf("%s: allWrites = %v, want %v", tc.name, all, tc.wantAll)
		}
	}
}
