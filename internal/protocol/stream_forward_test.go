package protocol

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// errTestReset stands in for a Cloudflare TCP reset: the upstream body dies
// with an error before (or after) producing bytes.
var errTestReset = errors.New("read: connection reset by peer")

// resetReader fails with errTestReset after yielding body once. With an empty
// body it fails on the first read: the zero-delivery case.
type resetReader struct {
	body    string
	yielded bool
}

func (r *resetReader) Read(data []byte) (int, error) {
	if !r.yielded {
		r.yielded = true
		if r.body == "" {
			return 0, errTestReset
		}
		return copy(data, r.body), nil
	}
	return 0, errTestReset
}

// flakyThenOK fails the first stream before delivery and serves a complete
// minimal Chat stream on the second call.
type flakyThenOK struct {
	calls int
}

func (f *flakyThenOK) Read(data []byte) (int, error) {
	f.calls++
	if f.calls == 1 {
		return 0, errTestReset
	}
	const ok = "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if f.calls == 2 {
		return copy(data, ok), nil
	}
	return 0, io.EOF
}

func TestForwardStreamZeroDeliveryReset(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	_, _, err := ForwardStream(t.Context(), rec, &resetReader{}, Chat, "big-pickle")
	if !errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("expected ErrZeroDeliveryReset, got %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("zero-delivery reset must not emit downstream, got %q", body)
	}
}

func TestForwardStreamPartialResetKeepsErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	partial := "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"
	_, _, err := ForwardStream(t.Context(), rec, &resetReader{body: partial}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("partial delivery must not signal zero-delivery replay")
	}
	if err == nil {
		t.Fatalf("expected torn-stream error after partial delivery")
	}
	if body := rec.Body.String(); !strings.Contains(body, "hel") {
		t.Fatalf("partial bytes must reach downstream, got %q", body)
	}
}

func TestForwardStreamCleanEmptyCloseIsNotReplay(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, err := ForwardStream(t.Context(), rec, strings.NewReader(""), Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("clean empty close must not trigger a replay")
	}
}

func TestForwardStreamHealthyPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, err := ForwardStream(t.Context(), rec, &flakyThenOK{calls: 1}, Chat, "big-pickle")
	if err != nil {
		t.Fatalf("healthy passthrough must not fail, got %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "hi") {
		t.Fatalf("healthy stream must pass through, got %q", body)
	}
}

func TestForwardStreamKeepaliveOnlyResetReplays(t *testing.T) {
	// Upstream sent only SSE comments (keepalives) before the reset. No
	// data frame reached downstream, so the turn is safe to replay.
	rec := httptest.NewRecorder()
	_, _, err := ForwardStream(t.Context(), rec, &resetReader{body: ": ping\n\n"}, Chat, "big-pickle")
	if !errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("keepalive-only reset must signal replay, got %v", err)
	}
	if body := rec.Body.String(); strings.Contains(body, `"error"`) {
		t.Fatalf("replay path must not emit an error frame, got %q", body)
	}
}

func TestForwardStreamErrorEventIsNotReplay(t *testing.T) {
	// An upstream error event is a delivered verdict, not a torn stream:
	// it must flow downstream as-is, never trigger a replay.
	rec := httptest.NewRecorder()
	boom := "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n"
	_, _, err := ForwardStream(t.Context(), rec, &resetReader{body: boom}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("upstream error event must not signal replay")
	}
	if body := rec.Body.String(); !strings.Contains(body, "boom") {
		t.Fatalf("upstream error must reach downstream, got %q", body)
	}
}

func TestForwardStreamUsageOnlyResetKeepsErrorFrame(t *testing.T) {
	// A usage-only data frame still counts as delivery: the client already
	// saw model metadata, so replaying would double-report usage.
	rec := httptest.NewRecorder()
	usageOnly := "data: {\"id\":\"r1\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n"
	_, _, err := ForwardStream(t.Context(), rec, &resetReader{body: usageOnly}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("usage-carrying reset must not signal replay")
	}
}
