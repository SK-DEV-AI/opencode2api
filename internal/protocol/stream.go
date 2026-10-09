package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

var errStreamNormalTermination = errors.New("upstream stream terminated normally")

var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

// ErrZeroDeliveryReset signals a mid-stream upstream reset before anything
// reached downstream. The error frame is deliberately NOT emitted: the
// gateway replays the turn on a fresh attempt inside the same connection.
var ErrZeroDeliveryReset = errors.New("upstream reset before delivery")

type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

type bridgeStreamEvent struct {
	Kind       string
	ResponseID string
	Model      string
	Text       string
	Signature  string
	ToolKey    string
	ToolID     string
	ToolName   string
	Stop       string
	Error      string
	ErrorType  string
	Encrypted  string
	Usage      *Usage
}

// StreamOutcome carries the terminal state of a streamed turn for the
// request log: stop reason plus the tail already sent downstream.
type StreamOutcome struct {
	Stop string
	Tail string
	// Delivered is false when the stream died before anything reached
	// downstream. The gateway uses it for the zero-delivery replay.
	Delivered bool
}

// EmitStreamError writes a terminal upstream-error frame for a caller that
// took over a stream (e.g. after a failed zero-delivery replay). Headers
// are already sent, so this is SSE framing, not an HTTP status.
func EmitStreamError(w http.ResponseWriter, to Protocol, model string, cause error) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support streaming")
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	return emitUnexpectedStreamError(emitter, cause)
}

// TranscodeStream is the request-aware form used by the
// gateway. A cancelled client must not receive a synthetic upstream error
// after its connection has gone away.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, StreamOutcome, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, StreamOutcome{}, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	termination := streamOpen
	// heartbeat keeps jcode's idle timer alive during silent thinking on
	// the transcode lane too: upstream comments never reach readSSE's
	// handler, so without this an xhigh turn that emits no data frames for
	// minutes looks like a dead connection downstream.
	heartbeat := newStreamHeartbeat(w, flusher)
	defer heartbeat.stop()
	readErr := readSSE(reader, func(eventName, data string) error {
		// The handler fires only on data frames (readSSE skips ":"
		// comments), so this stop cannot fire on keepalive-only thinking
		// silence — it ends the heartbeat exactly when real content flows.
		heartbeat.stop()
		events, err := parser.Parse(eventName, data)
		if err != nil {
			termination = streamErrorTermination
			if emitErr := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: err.Error(), ErrorType: "upstream_error"}); emitErr != nil {
				return emitErr
			}
			return errStreamUpstreamFailure
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				termination = streamNormalTermination
			case "error":
				termination = streamErrorTermination
			}
			if err := emitter.Emit(event); err != nil {
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	outcome := func() StreamOutcome {
		stop := emitter.StopReason()
		out := StreamOutcome{Stop: stop, Delivered: emitter.Delivered()}
		if (stop == "length" || stop == "") && emitter.TextLen() > 0 {
			out.Tail = emitter.TextTail(200)
		}
		return out
	}
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, outcome(), nil
		}
		if ClientCanceled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, outcome(), readErr
		}
		// ponytail: mid-stream reset AFTER partial content already went
		// downstream. Header-phase failures never reach here (attempt loop
		// retries them). Killing the turn now strands the partial reply
		// and forces a manual continue. A length finish keeps the partial
		// text and lets the client continue from it instead.
		if termination == streamOpen && (emitter.TextLen() > 0 || emitter.ToolCount() > 0) {
			emitter.SetStop("length")
			if finishErr := emitter.Finish(); finishErr != nil {
				return emitter.usage, emitter.usageReported, outcome(), finishErr
			}
			return emitter.usage, emitter.usageReported, outcome(), nil
		}
		if termination == streamOpen {
			// ponytail: zero-delivery reset. Nothing reached downstream
			// yet, so do NOT emit the error frame: the gateway replays
			// the turn on a fresh attempt inside the same downstream
			// connection. Emitting first would poison the stream and
			// make the replay unreceivable.
			if !emitter.Delivered() {
				return emitter.usage, emitter.usageReported, outcome(), ErrZeroDeliveryReset
			}
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, outcome(), emitErr
			}
		}
		return emitter.usage, emitter.usageReported, outcome(), readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, outcome(), nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, outcome(), errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, outcome(), ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, outcome(), err
	}
	return emitter.usage, emitter.usageReported, outcome(), errSSEUnexpectedEOF
}

func emitUnexpectedStreamError(emitter *bridgeStreamEmitter, cause error) error {
	message := "upstream SSE stream ended before a terminal event"
	if cause != nil && !errors.Is(cause, errSSEUnexpectedEOF) {
		message = fmt.Sprintf("upstream SSE stream failed: %v", cause)
	}
	err := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: message, ErrorType: "upstream_error"})
	if errors.Is(err, errStreamUpstreamFailure) {
		return nil
	}
	return err
}

func ClientCanceled(ctx context.Context, streamErr error) bool {
	if errors.Is(streamErr, context.Canceled) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// sseFlushWriter preserves an upstream SSE byte stream while making each
// successful write visible to the client immediately. io.Copy is free to
// choose large writes, so flushing in Write is the only reliable place to
// keep same-protocol streams live.
type sseFlushWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

// streamHeartbeat emits SSE comments during upstream silence so downstream
// idle timers (jcode: 180s base, ×3 for xhigh) never fire while the model is
// legitimately thinking. Any forwarded byte — upstream data or our own
// comment — resets the timer; the heartbeat stops permanently once upstream
// delivers its first frame, so it can never interleave with real content.
type streamHeartbeat struct {
	writer  io.Writer
	flusher http.Flusher
	timer   *time.Timer
	done    chan struct{}
	once    sync.Once
	stopped atomic.Bool
}

func newStreamHeartbeat(w io.Writer, flusher http.Flusher) *streamHeartbeat {
	hb := &streamHeartbeat{writer: w, flusher: flusher, done: make(chan struct{})}
	hb.timer = time.AfterFunc(streamHeartbeatInterval, hb.tick)
	return hb
}

const streamHeartbeatInterval = 15 * time.Second

func (hb *streamHeartbeat) Write(data []byte) (int, error) {
	// Any upstream byte means the stream is alive: silence the heartbeat.
	hb.stop()
	return len(data), nil
}

func (hb *streamHeartbeat) tick() {
	if hb.stopped.Load() {
		return
	}
	select {
	case <-hb.done:
		return
	default:
	}
	if _, err := io.WriteString(hb.writer, ": keepalive\n\n"); err != nil {
		return
	}
	hb.flusher.Flush()
	if !hb.stopped.Load() {
		hb.timer.Reset(streamHeartbeatInterval)
	}
}

func (hb *streamHeartbeat) stop() {
	hb.once.Do(func() {
		hb.stopped.Store(true)
		hb.timer.Stop()
		close(hb.done)
	})
}

func (writer *sseFlushWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if n > 0 {
		writer.flusher.Flush()
	}
	return n, err
}

func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, StreamOutcome, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, StreamOutcome{}, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	// heartbeat keeps downstream idle timers alive while upstream thinks
	// silently: if no bytes flow either direction for the interval, emit an
	// SSE comment (a keepalive every SSE client ignores as data but counts
	// as activity). Stops at the first forwarded frame.
	heartbeat := newStreamHeartbeat(w, flusher)
	defer heartbeat.stop()
	// ponytail: the observer already parses every SSE frame for usage, so it
	// is also the delivery signal. A reset before any data frame reached
	// downstream (idle connection, keepalives only, preamble without data)
	// is safe to replay: the client saw nothing actionable. Anything with
	// data frames keeps the old error-frame path.
	_, copyErr := io.Copy(io.MultiWriter(&sseFlushWriter{writer: w, flusher: flusher}, heartbeat), io.TeeReader(reader, observer))
	heartbeat.stop()
	usage := observer.Finish()
	// ponytail: passthrough stop/tail census. Same-protocol streams are the
	// common case, yet the request log showed an empty stop for all of them
	// because only the transcode lane reported an outcome. The stop is the
	// raw upstream finish reason; the tail follows the transcode rule
	// (populated on truncated turns, where diagnosis needs it).
	outcome := StreamOutcome{Stop: observer.StopReason(), Delivered: observer.DataDelivered()}
	if (outcome.Stop == "length" || outcome.Stop == "") && observer.TextLen() > 0 {
		outcome.Tail = observer.TextTail(200)
	}
	if observer.ErrorTermination() {
		if copyErr != nil {
			return usage, observer.Reported(), outcome, copyErr
		}
		return usage, observer.Reported(), outcome, errStreamUpstreamFailure
	}
	if observer.NormalTermination() && observer.ParseError() == nil {
		return usage, observer.Reported(), outcome, copyErr
	}
	if ClientCanceled(ctx, copyErr) {
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		return usage, observer.Reported(), outcome, copyErr
	}
	if !observer.DataDelivered() && copyErr != nil {
		// ponytail: zero-delivery reset on the passthrough lane. Matches the
		// TranscodeStream contract: no error frame emitted, the gateway
		// replays the turn on a fresh attempt inside the same connection.
		// Gated on a read failure so a clean empty close keeps the old
		// error-frame behavior instead of spending a replay on it.
		return usage, observer.Reported(), outcome, ErrZeroDeliveryReset
	}
	cause := observer.ParseError()
	if copyErr != nil {
		cause = copyErr
	}
	if cause == nil {
		cause = errSSEUnexpectedEOF
	}
	emitter := newBridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), outcome, err
	}
	return usage, observer.Reported(), outcome, cause
}

type streamUsageObserver struct {
	parser   *bridgeStreamParser
	buffer   []byte
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
	stop     string
	// text accumulates streamed text for the tail census. Bounded at
	// 4 KiB: only the last 200 characters are ever reported, so keeping
	// the whole reply in memory would be waste on long turns.
	text strings.Builder
	// dataFrames counts SSE data frames observed upstream. Comment-only
	// traffic (keepalives like ": ping") does not count: the client saw
	// nothing actionable, so a reset there is still safe to replay.
	dataFrames int
}

func newStreamUsageObserver(protocol Protocol) *streamUsageObserver {
	return &streamUsageObserver{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

func (observer *streamUsageObserver) Write(data []byte) (int, error) {
	observer.buffer = append(observer.buffer, data...)
	for {
		index, width := nextSSEBoundary(observer.buffer)
		if index < 0 {
			break
		}
		frame := append([]byte(nil), observer.buffer[:index+width]...)
		observer.buffer = observer.buffer[index+width:]
		observer.consume(frame)
	}
	return len(data), nil
}

func (observer *streamUsageObserver) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageObserver) Reported() bool { return observer.reported }

func (observer *streamUsageObserver) NormalTermination() bool { return observer.normal }

func (observer *streamUsageObserver) ErrorTermination() bool { return observer.error }

func (observer *streamUsageObserver) ParseError() error { return observer.parseErr }

// StopReason reports the raw upstream finish reason observed on the
// stream ("", "stop", "length", "tool_calls", ...), or "" when the
// stream never carried a terminal event.
func (observer *streamUsageObserver) StopReason() string { return observer.stop }

// TextLen reports how much text passed through on this stream.
func (observer *streamUsageObserver) TextLen() int { return observer.text.Len() }

// TextTail returns the last n characters streamed.
func (observer *streamUsageObserver) TextTail(n int) string {
	full := observer.text.String()
	if len(full) > n {
		full = full[len(full)-n:]
	}
	return full
}

// DataDelivered reports whether any SSE data frame arrived upstream. A
// stream that died on keepalives alone replays safely: comments carry no
// model content, so the client saw nothing actionable.
func (observer *streamUsageObserver) DataDelivered() bool { return observer.dataFrames > 0 }

func (observer *streamUsageObserver) consume(frame []byte) {
	_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		observer.dataFrames++
		events, err := observer.parser.Parse(eventName, data)
		if err != nil {
			if observer.parseErr == nil {
				observer.parseErr = err
			}
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				observer.normal = true
			case "error":
				observer.error = true
			case "finish":
				observer.stop = event.Stop
			case "text":
				if observer.text.Len() < 4096 {
					remaining := 4096 - observer.text.Len()
					if len(event.Text) > remaining {
						observer.text.WriteString(event.Text[len(event.Text)-remaining:])
					} else {
						observer.text.WriteString(event.Text)
					}
				}
			}
			if event.Usage != nil {
				observer.reported = true
				mergeBridgeUsage(&observer.usage, *event.Usage)
			}
		}
		return nil
	})
}

func nextSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var eventName string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 {
		// A blank line is the SSE record delimiter. Do not parse a final
		// unterminated record as a complete frame; an EOF without a terminal
		// event is handled by the caller as a truncated stream.
		return errSSEUnexpectedEOF
	}
	return nil
}
