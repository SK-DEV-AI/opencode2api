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

func testPerformanceConfig() config.PerformanceConfig {
	return config.PerformanceConfig{
		MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1,
	}
}

// TestVerifyProxyAfterErrorRespectsCancel proves the 2026-10-09 fix: a proxy
// verification must not outlive its caller's context. Before the fix the
// check ran on context.WithoutCancel, so every client disconnect left a 10s
// health check running against a dead network while holding the checking
// flag and serializing later verifications.
func TestVerifyProxyAfterErrorRespectsCancel(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slower than the test's cancel: if the check ignores cancellation
		// it will still be in-flight when we assert.
		time.Sleep(5 * time.Second)
	}))
	t.Cleanup(slow.Close)
	pool, err := newTransportPool([]string{slow.URL}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := &Gateway{transports: pool, logger: logger, monitor: telemetry.NewMonitor()}
	proxy := pool.items[0]
	ctx, cancel := context.WithCancel(context.Background())
	gw.verifyProxyAfterError(ctx, proxy, 500)
	// The check claimed the flag; cancel while it is still in-flight.
	time.Sleep(200 * time.Millisecond)
	cancel()
	// A cancel-respecting check releases the flag quickly. The old detached
	// check held it for the full 5s server sleep.
	deadline := time.Now().Add(3 * time.Second)
	for proxy.checking.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("proxy checking flag still held 3s after cancel; verification ignores context cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
