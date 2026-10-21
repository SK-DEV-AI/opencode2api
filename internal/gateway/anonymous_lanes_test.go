package gateway

import (
	"testing"

	"opencode2api/internal/models"
)

// TestAnonymousUpstreamsCustomOriginNotHijacked pins the offline-safety
// guarantee: the hardcoded public legacy origin must never be prepended to a
// configured anonymous origin that lives somewhere else (mirror, self-hosted
// copy, or a test server). Prepending it made the offline probe reach the real
// internet, which both leaked free-tier traffic and defeated the
// unattempted-probe guard the laptop-boot false disable depends on.
func TestAnonymousUpstreamsCustomOriginNotHijacked(t *testing.T) {
	gw := probeFixture(t, "http://127.0.0.1:1")
	lanes := gw.anonymousUpstreams()
	if len(lanes) != 1 || lanes[0] != "http://127.0.0.1:1" {
		t.Fatalf("custom anonymous origin must stay the only lane; got %v", lanes)
	}
}

// TestAnonymousUpstreamsPublicDeploymentPrefersLegacy keeps upstream v1.4.1's
// lane order for the public deployment: the legacy /zen/v1 gate first, the
// configured /inference origin as the fallback. Both lanes accepted live
// anonymous traffic when measured, so the order is upstream's call to make.
func TestAnonymousUpstreamsPublicDeploymentPrefersLegacy(t *testing.T) {
	gw := probeFixture(t, "https://opencode.ai/inference")
	lanes := gw.anonymousUpstreams()
	want := []string{models.V1Base, "https://opencode.ai/inference"}
	if len(lanes) != len(want) || lanes[0] != want[0] || lanes[1] != want[1] {
		t.Fatalf("public deployment lanes = %v; want %v", lanes, want)
	}

	same := probeFixture(t, models.V1Base)
	lanes = same.anonymousUpstreams()
	if len(lanes) != 1 || lanes[0] != models.V1Base {
		t.Fatalf("legacy-only configuration = %v; want a single legacy lane", lanes)
	}
}

// TestSameHost covers the comparison used to decide whether the configured
// origin is the public deployment.
func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://opencode.ai/zen", "https://opencode.ai/inference", true},
		{"https://opencode.ai/zen", "https://opencode.ai:443/inference", false},
		{"https://opencode.ai/zen", "http://127.0.0.1:1", false},
		{"https://opencode.ai/zen", "https://mirror.example/zen", false},
		{"not a url", "https://opencode.ai/zen", false},
	}
	for _, tc := range cases {
		if got := sameHost(tc.a, tc.b); got != tc.want {
			t.Errorf("sameHost(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
