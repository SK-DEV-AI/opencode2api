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

// healthyChatStream serves one complete minimal Chat stream, then EOF.
// (Named for what it is: the happy-path fixture. Reset behavior comes from
// resetReader above.)
type healthyChatStream struct {
	calls int
}

func (f *healthyChatStream) Read(data []byte) (int, error) {
	f.calls++
	const ok = "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if f.calls == 1 {
		return copy(data, ok), nil
	}
	return 0, io.EOF
}

func TestForwardStreamZeroDeliveryReset(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	_, _, _, err := ForwardStream(t.Context(), rec, &resetReader{}, Chat, "big-pickle")
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
	_, _, outcome, err := ForwardStream(t.Context(), rec, &resetReader{body: partial}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("partial delivery must not signal zero-delivery replay")
	}
	if err == nil {
		t.Fatalf("expected torn-stream error after partial delivery")
	}
	if !outcome.Delivered {
		t.Fatalf("partial outcome must claim delivery")
	}
	if body := rec.Body.String(); !strings.Contains(body, "hel") {
		t.Fatalf("partial bytes must reach downstream, got %q", body)
	}
}

func TestForwardStreamCleanEmptyCloseIsNotReplay(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, _, err := ForwardStream(t.Context(), rec, strings.NewReader(""), Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("clean empty close must not trigger a replay")
	}
}

func TestForwardStreamHealthyPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, outcome, err := ForwardStream(t.Context(), rec, &healthyChatStream{}, Chat, "big-pickle")
	if err != nil {
		t.Fatalf("healthy passthrough must not fail, got %v", err)
	}
	if outcome.Stop != "stop" {
		t.Fatalf("healthy passthrough must report stop, got %q", outcome.Stop)
	}
	if !outcome.Delivered {
		t.Fatalf("healthy passthrough must claim delivery")
	}
	if body := rec.Body.String(); !strings.Contains(body, "hi") {
		t.Fatalf("healthy stream must pass through, got %q", body)
	}
}

func TestForwardStreamKeepaliveOnlyResetReplays(t *testing.T) {
	// Upstream sent only SSE comments (keepalives) before the reset. No
	// data frame reached downstream, so the turn is safe to replay.
	rec := httptest.NewRecorder()
	_, _, _, err := ForwardStream(t.Context(), rec, &resetReader{body: ": ping\n\n"}, Chat, "big-pickle")
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
	_, _, _, err := ForwardStream(t.Context(), rec, &resetReader{body: boom}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("upstream error event must not signal replay")
	}
	if body := rec.Body.String(); !strings.Contains(body, "boom") {
		t.Fatalf("upstream error must reach downstream, got %q", body)
	}
}

func TestForwardStreamUsageOnlyResetKeepsErrorFrame(t *testing.T) {
	// Lane asymmetry with TestTranscodeStreamUsageOnlyResetStillReplays:
	// the passthrough lane counts any data frame (usage included) as
	// delivery, since the bytes already reached the client verbatim.
	rec := httptest.NewRecorder()
	usageOnly := "data: {\"id\":\"r1\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n"
	_, _, _, err := ForwardStream(t.Context(), rec, &resetReader{body: usageOnly}, Chat, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("usage-carrying reset must not signal replay")
	}
}

// Transcode-lane parity: the same reset shapes must produce the same replay
// verdicts through TranscodeStream (Chat upstream -> Anthropic client).
// These execute the lane; earlier validation only read its code.

func TestTranscodeStreamZeroDeliveryReset(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, outcome, err := TranscodeStream(t.Context(), rec, &resetReader{}, Chat, Anthropic, "big-pickle")
	if !errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("expected ErrZeroDeliveryReset, got %v", err)
	}
	if outcome.Delivered {
		t.Fatalf("zero-delivery outcome must not claim delivery")
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("zero-delivery reset must not emit downstream, got %q", body)
	}
}

func TestTranscodeStreamKeepaliveOnlyResetReplays(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, _, err := TranscodeStream(t.Context(), rec, &resetReader{body: ": ping\n\n"}, Chat, Anthropic, "big-pickle")
	if !errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("keepalive-only reset must signal replay, got %v", err)
	}
}

func TestTranscodeStreamPartialResetRescuesAsLength(t *testing.T) {
	// The transcode lane rescues partial delivery as a length finish (patch
	// 3), where the passthrough lane keeps the error frame. Both preserve
	// the partial reply; the contract differs by lane, so assert it.
	rec := httptest.NewRecorder()
	partial := "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"
	_, _, outcome, err := TranscodeStream(t.Context(), rec, &resetReader{body: partial}, Chat, Anthropic, "big-pickle")
	if err != nil {
		t.Fatalf("partial transcode reset must rescue, got %v", err)
	}
	if outcome.Stop != "length" {
		t.Fatalf("expected length rescue, got stop=%q", outcome.Stop)
	}
	if !outcome.Delivered {
		t.Fatalf("partial outcome must claim delivery")
	}
	if body := rec.Body.String(); !strings.Contains(body, "hel") {
		t.Fatalf("partial text must reach downstream, got %q", body)
	}
}

func TestTranscodeStreamStartOnlyResetDoesNotReplay(t *testing.T) {
	// The exact maintainer-reported shape: upstream sends only the start
	// signal (id, no content yet), then the connection dies. The client
	// already received message_start, so this must NOT replay.
	rec := httptest.NewRecorder()
	startOnly := "data: {\"id\":\"r1\",\"model\":\"m\"}\n\n"
	_, _, outcome, err := TranscodeStream(t.Context(), rec, &resetReader{body: startOnly}, Chat, Anthropic, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("start-only reset must not signal replay (client holds an open turn)")
	}
	if err == nil {
		t.Fatalf("expected torn-stream error after opening frame")
	}
	if !outcome.Delivered {
		t.Fatalf("start-only outcome must claim delivery")
	}
	if body := rec.Body.String(); !strings.Contains(body, "message_start") {
		t.Fatalf("opening frame must reach downstream, got %q", body)
	}
}

func TestTranscodeStreamIdlessUsageOnlyResetStillReplays(t *testing.T) {
	// A Chat usage frame without an id carries no start signal: only a
	// usage event is parsed, no downstream frame is emitted, and the reset
	// still replays. The gateway-held usage from the first attempt is
	// overwritten by the replay's, so nothing double-reports.
	rec := httptest.NewRecorder()
	usageOnly := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n"
	_, _, outcome, err := TranscodeStream(t.Context(), rec, &resetReader{body: usageOnly}, Chat, Anthropic, "big-pickle")
	if !errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("idless usage-only transcode reset must signal replay, got %v", err)
	}
	if outcome.Delivered {
		t.Fatalf("idless usage-only outcome must not claim delivery")
	}
}

func TestTranscodeStreamStartEmittingUsageResetDoesNotReplay(t *testing.T) {
	// A usage frame that also carries an id emits the downstream opening
	// frame (message_start on the Anthropic lane) before the reset: the
	// client holds an open turn, so replaying would emit a second opening
	// frame and violate the stream. The error-frame path applies instead.
	rec := httptest.NewRecorder()
	usageOnly := "data: {\"id\":\"r1\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n"
	_, _, outcome, err := TranscodeStream(t.Context(), rec, &resetReader{body: usageOnly}, Chat, Anthropic, "big-pickle")
	if errors.Is(err, ErrZeroDeliveryReset) {
		t.Fatalf("start-emitting reset must not signal replay")
	}
	if err == nil {
		t.Fatalf("expected torn-stream error after opening frame")
	}
	if !outcome.Delivered {
		t.Fatalf("start-emitting outcome must claim delivery")
	}
}
