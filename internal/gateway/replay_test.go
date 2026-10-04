package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/telemetry"
)

// replayHit scripts one upstream response: a status plus an optional SSE
// body. Status 200 with an empty body resets the connection before delivery.
type replayHit struct {
	status int
	body   string
}

// replayFixture builds a Gateway backed by scripted upstream hits.
func replayFixture(t *testing.T, hits []replayHit) (*Gateway, *telemetry.Monitor, *int) {
	t.Helper()
	n := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		script := hits[min(n-1, len(hits)-1)]
		if script.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(script.status)
			_, _ = io.WriteString(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if script.body == "" {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			return
		}
		_, _ = io.WriteString(w, script.body)
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 6, TimeoutSeconds: 30},
		Upstream:   config.UpstreamConfig{Zen: upstream.URL, Go: upstream.URL},
		Models:     config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.NewMonitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}
	return gw, monitor, &n
}

func driveInference(t *testing.T, gw *Gateway, monitor *telemetry.Monitor, model string) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// TestZeroDeliveryReplayEndToEnd wires a fake upstream that resets the first
// SSE body before delivery and serves a complete Chat stream on the second
// hit, then drives handleInference through the real Gateway. It asserts the
// client receives one clean stream (replay, not error) on the passthrough
// lane. Anonymous mode keeps the fixture to one fake node with no keys.
func TestZeroDeliveryReplayEndToEnd(t *testing.T) {
	const okBody = "data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	gw, monitor, n := replayFixture(t, []replayHit{{status: 200}, {status: 200, body: okBody}})
	raw := driveInference(t, gw, monitor, "mimo-v2.5-free")
	if *n != 2 {
		t.Fatalf("expected reset + replay = 2 upstream hits, got %d (body %q)", *n, raw)
	}
	if !strings.Contains(raw, `"hi"`) {
		t.Fatalf("replayed stream must deliver content, got %q", raw)
	}
	if strings.Contains(raw, "upstream_error") {
		t.Fatalf("replayed stream must not carry an error frame, got %q", raw)
	}
}

// TestZeroDeliveryReplayFailureEmitsErrorFrame covers the else branch: the
// first attempt resets before delivery (error frame suppressed), the replay
// comes back non-2xx, so the gateway must emit the error frame now rather
// than leave the client hanging on an idle stream.
func TestZeroDeliveryReplayFailureEmitsErrorFrame(t *testing.T) {
	gw, monitor, n := replayFixture(t, []replayHit{{status: 200}, {status: 503}})
	raw := driveInference(t, gw, monitor, "mimo-v2.5-free")
	if *n < 2 {
		t.Fatalf("expected reset + failed replay (>=2 hits), got %d (body %q)", *n, raw)
	}
	if !strings.Contains(raw, "upstream_error") {
		t.Fatalf("failed replay must emit an error frame, got %q", raw)
	}
}
