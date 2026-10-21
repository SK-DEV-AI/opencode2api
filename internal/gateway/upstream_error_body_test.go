package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	"opencode2api/internal/telemetry"
)

// The 2026-10-18 incident: an upstream 403 ("Detect Abuse Traffic") on both
// the anonymous and key lanes reached the journal as a bare status code with no
// body. The anonymous lane only logs its error responses at Debug, and the
// journal runs at Info, so the 4xx class was unobservable. These tests pin that
// the body snippet reaches an Info-level log for non-2xx responses on any lane.
func TestLogUpstreamErrorBodyEmitsSnippetAtInfo(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		gw := &Gateway{logger: logger}
		body := []byte(`{"error":{"message":"Detect Abuse Traffic","code":"abuse"}}`)

		gw.logUpstreamErrorBody(context.Background(), models.Route{ID: "space-bunny-free"},
			identity.RequestIDs{Request: "req_abc"}, status, body)

		out := buf.String()
		if !strings.Contains(out, "upstream_error_body") {
			t.Fatalf("status %d: no upstream_error_body event logged at Info: %s", status, out)
		}
		if !strings.Contains(out, "Detect Abuse Traffic") {
			t.Errorf("status %d: upstream body snippet missing from log: %s", status, out)
		}
	}
}

func TestLogUpstreamErrorBodySkipsEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gw := &Gateway{logger: logger}
	gw.logUpstreamErrorBody(context.Background(), models.Route{ID: "m"},
		identity.RequestIDs{Request: "req_abc"}, 403, nil)
	if strings.Contains(buf.String(), "upstream_error_body") {
		t.Fatalf("empty body should not produce an error-body log: %s", buf.String())
	}
}

// The snippet must never leak a configured secret: the hub redactor scrubs
// known credentials before the body is logged.
func TestLogUpstreamErrorBodyScrubsSecrets(t *testing.T) {
	const secret = "sk-super-secret-zen-key-value"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gw := &Gateway{logger: logger, cfg: config.Config{ZenKeys: []string{secret}}}
	body := []byte(`{"error":{"message":"key ` + secret + ` was rejected"}}`)

	gw.logUpstreamErrorBody(context.Background(), models.Route{ID: "m"},
		identity.RequestIDs{Request: "req_abc"}, 401, body)

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked into the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "was rejected") {
		t.Errorf("redacted log lost all context: %s", buf.String())
	}
}

// The snippet is capped so a large or hostile upstream error payload cannot
// flood the journal.
func TestLogUpstreamErrorBodyCapsSnippet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gw := &Gateway{logger: logger}
	body := bytes.Repeat([]byte("A"), 10_000)

	gw.logUpstreamErrorBody(context.Background(), models.Route{ID: "m"},
		identity.RequestIDs{Request: "req_abc"}, 500, body)

	if n := strings.Count(buf.String(), "A"); n > 240 {
		t.Fatalf("snippet not capped: %d body bytes in log", n)
	}
}

// readAndRestoreErrorBody must leave the response body readable so the error
// still reaches the client intact after the gateway logs it.
func TestReadAndRestoreErrorBodyLeavesBodyReadable(t *testing.T) {
	original := `{"error":{"message":"upstream said no"}}`
	resp := &http.Response{
		StatusCode: 403,
		Body:       io.NopCloser(strings.NewReader(original)),
	}

	got := readAndRestoreErrorBody(resp)
	if string(got) != original {
		t.Fatalf("captured body = %q, want %q", got, original)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("restored body unreadable: %v", err)
	}
	if string(rest) != original {
		t.Fatalf("restored body = %q, want %q", rest, original)
	}
}

func TestReadAndRestoreErrorBodyHandlesNil(t *testing.T) {
	if got := readAndRestoreErrorBody(nil); got != nil {
		t.Fatalf("nil response returned %q", got)
	}
	empty := &http.Response{StatusCode: 403, Body: nil}
	if got := readAndRestoreErrorBody(empty); got != nil {
		t.Fatalf("nil body returned %q", got)
	}
}

// TestAnonymousLaneLogsErrorBody is the end-to-end guard for the 2026-10-18
// incident. A fake upstream answers 403 on every anonymous attempt; the client
// sees the 403 body, and the Info-level log must carry the upstream message so
// an abuse gate is diagnosable from the journal alone. It fails if the
// anonymous lane stops calling logUpstreamErrorBody.
func TestAnonymousLaneLogsErrorBody(t *testing.T) {
	const abuseBody = `{"error":{"message":"OpenCode Free Model Gateway Detect Abuse Traffic","code":"abuse"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, abuseBody)
	}))
	t.Cleanup(upstream.Close)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 2, TimeoutSeconds: 30},
		Upstream:   config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:     config.ModelsConfig{RefreshSeconds: 300},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		},
	}
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	telemetry.Middleware(monitor, slog.New(slog.NewJSONHandler(io.Discard, nil)), gw.Handler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("client status = %d, want 403 (upstream 403 must reach the client)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Detect Abuse Traffic") {
		t.Errorf("client lost the upstream error body: %q", rec.Body.String())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "upstream_error_body") {
		t.Fatalf("anonymous 403 body never reached the Info log; the abuse gate is undiagnosable from the journal")
	}
	if !strings.Contains(logged, "Detect Abuse Traffic") {
		t.Errorf("anonymous 403 snippet missing from log: %s", logged)
	}
}
