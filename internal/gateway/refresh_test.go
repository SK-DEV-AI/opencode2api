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

// TestVerifyProxyAfterErrorOutlivesRequestCancel pins the v1.4.1 merge
// decision: proxy verification is gateway-scoped state, so a cancelled client
// request must not abort it. Upstream PR #80 (the request-bound WithTimeout
// variant this fork carried) was closed on exactly that reasoning, and the
// fork follows upstream again. The check is still bounded: it ends on its own,
// capped by proxyHealthCheckTimeout, and never leaks the checking flag.
func TestVerifyProxyAfterErrorOutlivesRequestCancel(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slower than the cancel below: if the check honoured cancellation the
		// flag would be released right after the cancel instead of at the end
		// of this sleep.
		time.Sleep(1 * time.Second)
	}))
	t.Cleanup(slow.Close)
	pool, err := transportPool([]string{slow.URL}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := &Gateway{transports: pool, logger: logger, monitor: telemetry.Monitor()}
	proxy := pool.items[0]
	ctx, cancel := context.WithCancel(context.Background())
	gw.verifyProxyAfterError(ctx, proxy, "upstream response", 500)
	// The check claimed the flag; cancel while it is still in-flight.
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)
	if !proxy.checking.Load() {
		t.Fatalf("request cancel aborted the proxy verification; a cancelled client request must not end a gateway-scoped health check")
	}
	// The check must still finish by itself instead of leaking the flag.
	deadline := time.Now().Add(3 * time.Second)
	for proxy.checking.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("proxy checking flag still held 3s after the check should have finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
