package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/telemetry"
)

// The gateway's heartbeat (": keepalive" SSE comments) does not protect a
// client that parses SSE strictly: jcode's openai-compat path skips
// comment-only events, so a heartbeat never resets that client's idle timer.
// A dead upstream therefore hangs the client until the outer budget expires.
//
// With a stream idle limit configured, a stream that produces no bytes for
// longer than the limit must be abandoned promptly instead.
func TestStreamIdleWatchdogEndsDeadStream(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// One frame, so the first-event gate is satisfied and the stream is
		// established. Then silence.
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		upstream.CloseClientConnections()
		upstream.Close()
	})

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry: config.RetryConfig{
			MaxAttempts: 1, TimeoutSeconds: 60, StreamTimeoutSeconds: 60, StreamIdleTimeoutSeconds: 1,
		},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			AttemptTimeoutSeconds: 5, FirstEventTimeoutSeconds: 5,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := testLogger()
	monitor := telemetry.NewMonitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("handler did not return; the dead stream was not abandoned")
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("the frame delivered before the silence was lost: %q", rec.Body.String())
	}
}

// Without an idle limit the stream keeps waiting, so the behavior is opt-in
// and a deployment that does not configure it is unaffected.
func TestStreamIdleWatchdogDisabledKeepsStreamOpen(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		upstream.CloseClientConnections()
		upstream.Close()
	})

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 60, StreamTimeoutSeconds: 60},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			AttemptTimeoutSeconds: 5, FirstEventTimeoutSeconds: 5,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := testLogger()
	monitor := telemetry.NewMonitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
		close(done)
	}()

	// The stream budget is 60s, so the handler is still running after 2s.
	select {
	case <-done:
		t.Fatalf("handler returned early; the idle watchdog should be disabled")
	case <-time.After(2 * time.Second):
	}
	// Let the stream finish before the test ends: the recorder must not be
	// written after the test returns.
	close(release)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("handler did not return after the upstream released the stream")
	}
}
