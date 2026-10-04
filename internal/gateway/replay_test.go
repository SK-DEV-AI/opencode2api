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

// TestZeroDeliveryReplayEndToEnd wires a fake upstream that resets the first
// SSE body before delivery and serves a complete Chat stream on the second
// hit, then drives handleInference through the real Gateway. It asserts the
// client receives one clean stream (replay, not error) on the passthrough
// lane. Anonymous mode keeps the fixture to one fake node with no keys.
func TestZeroDeliveryReplayEndToEnd(t *testing.T) {
	const okBody = "data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if hits == 1 {
			// Reset before delivery: headers sent, body dies immediately.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer upstream.Close()

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

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if hits != 2 {
		t.Fatalf("expected reset + replay = 2 upstream hits, got %d (body %q)", hits, raw)
	}
	if !strings.Contains(string(raw), `"hi"`) {
		t.Fatalf("replayed stream must deliver content, got %q", raw)
	}
	if strings.Contains(string(raw), "upstream_error") {
		t.Fatalf("replayed stream must not carry an error frame, got %q", raw)
	}
}
