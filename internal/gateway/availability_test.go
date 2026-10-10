package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/telemetry"
)

// probeFixture builds a Gateway whose upstream points at zenURL, with one
// -free model advertised on Zen so probeFreeModel reaches the lane loop.
func probeFixture(t *testing.T, zenURL string) *Gateway {
	t.Helper()
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		ZenKeys:    []string{"zen-key-1"},
		Retry:      config.RetryConfig{MaxAttempts: 6, TimeoutSeconds: 30},
		Upstream:   config.UpstreamConfig{Zen: zenURL, Go: zenURL},
		Models:     config.ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{"test-offline-free": "chat"}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(cfg, logger, telemetry.NewMonitor())
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}
	gw.catalog.Replace([]string{"test-offline-free"}, nil)
	return gw
}

// TestProbeOfflineIsUnattempted reproduces the laptop-boot false disable:
// with no route upstream (connection refused on every lane) the probe must
// report attempted=false so checkFreeModels skips the Record entirely and a
// working model is never disabled for 24h.
func TestProbeOfflineIsUnattempted(t *testing.T) {
	gw := probeFixture(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := gw.probeFreeModel(ctx, "test-offline-free")
	if result.attempted {
		t.Errorf("offline probe reported attempted=true (success=%v reason=%q); want unattempted so nothing is recorded", result.success, result.reason)
	}
	if result.success {
		t.Errorf("offline probe reported success=true; nothing upstream responded")
	}
	if result.reason != "no_upstream_contact" {
		t.Errorf("offline probe reason=%q; want no_upstream_contact", result.reason)
	}
}

// TestProbeHTTPErrorStillRecords proves the fix is narrow: when upstream IS
// reachable but rejects the model, the probe still reports attempted=true so
// genuinely dead models keep being disabled.
func TestProbeHTTPErrorStillRecords(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
	}))
	t.Cleanup(upstream.Close)
	gw := probeFixture(t, upstream.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := gw.probeFreeModel(ctx, "test-offline-free")
	if !result.attempted {
		t.Errorf("reachable-upstream probe reported attempted=false; want attempted=true")
	}
	if result.success {
		t.Errorf("503 probe reported success=true")
	}
	t.Logf("recorded reason=%q", result.reason)
}
