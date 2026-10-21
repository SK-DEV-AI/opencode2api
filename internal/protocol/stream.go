package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/jsonutil"
)

// errStreamUpstreamFailure marks a stream that failed in a way already
// reported, or, in CollapseStream, already recorded, so the caller must not
// report it a second time.
var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

// errStreamNormalTermination reports that the stream reached its own terminal
// event, so the caller treats it as a success.
var errStreamNormalTermination = errors.New("upstream stream terminated normally")

// errSSEUnexpectedEOF reports a stream that ended without a terminal event.
var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

// ErrZeroDeliveryReset signals a mid-stream upstream reset before anything
// reached downstream. The error frame is deliberately NOT emitted: the
// gateway replays the turn on a fresh attempt inside the same connection.
var ErrZeroDeliveryReset = errors.New("upstream reset before delivery")

// streamTermination records how a transcoded stream ended, so the code after
// readSSE can tell a normal terminal event from an upstream error.
type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

// bridgeStreamEvent is one protocol-neutral streaming event, parsed from an
// upstream stream and encoded for the downstream protocol. Kind selects which
// fields matter: "start" (ResponseID, Model), "text", "reasoning" (Text and/or
// Encrypted), "reasoning_signature" (Signature), "tool_start" and "tool_delta"
// (ToolKey, ToolID, ToolName, Text), "usage", "finish" (Stop), "error"
// (Error, ErrorType) and "done".
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
	emitter := bridgeStreamEmitter(w, flusher, to, model)
	return emitUnexpectedStreamError(emitter, cause)
}

// TranscodeStream parses an upstream stream of one protocol and re-encodes it
// for another, writing each downstream event as it arrives. It returns the
// usage the emitter collected and whether the upstream reported any, plus the
// terminal state of the turn for the request log.
//
// A nil error means the stream reached its terminal event. Otherwise the error
// is errStreamUpstreamFailure when the failure was already reported in-band,
// the error that ended the read, or ctx.Err() when the client was canceled
// after the stream itself ended cleanly. A stream that ends without a terminal
// event is reported in-band with an error event first, using
// errSSEUnexpectedEOF when nothing more specific is known.
//
// A canceled client gets no synthetic upstream error event: the gateway
// cancels the request context when the client goes away, and writing after
// that only wastes the remaining attempt.
//
// A stream that was reset before anything reached downstream returns
// ErrZeroDeliveryReset instead, and the gateway replays the turn.
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
	emitter := bridgeStreamEmitter(w, flusher, to, model)
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

// emitUnexpectedStreamError reports a stream that ended without a terminal
// event, as an in-band error event in the downstream protocol. It returns nil
// once that event is written, and the write error otherwise.
func emitUnexpectedStreamError(emitter *bridgeStreamEncoder, cause error) error {
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

// ClientCanceled reports whether a stream error is the client going away,
// either through the stream error itself or through the request context.
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

// ForwardStream relays a same-protocol upstream stream to the client byte for
// byte, flushing each write, while a usage tracker reads the same bytes
// through a TeeReader. It returns the tracked usage and whether the upstream
// reported any, plus the terminal state of the turn for the request log, and
// an error: the copy error, if any, for a normal termination that parsed
// cleanly; otherwise errStreamUpstreamFailure when an upstream error event was
// relayed, the copy error, or the parse error (or errSSEUnexpectedEOF) after an
// error event was emitted for a stream that ended early. A canceled client
// gets no synthetic error event.
//
// A stream that was reset before any data frame reached downstream returns
// ErrZeroDeliveryReset instead, and the gateway replays the turn.
func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, StreamOutcome, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, StreamOutcome{}, fmt.Errorf("response writer does not support streaming")
	}
	observer := streamUsageObserver(protocol)
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
	emitter := bridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), outcome, err
	}
	return usage, observer.Reported(), outcome, cause
}

// RenameStreamReader wraps an SSE stream reader and rewrites tool call names
// matching fromName back to toName before downstream parsers or writers consume
// the stream. When fromName or toName is empty (or identical), reader is
// returned unchanged.
//
// It is a pull-based transform instead of a goroutine plus io.Pipe so a client
// that disconnects mid-stream cannot strand a blocked writer. Chat frames go
// through the fragment-aware rewriter, while the other protocols use the
// stateless RenameResponseTool rewrite on each frame.
func RenameStreamReader(reader io.Reader, protocol Protocol, fromName, toName string) io.Reader {
	if fromName == "" || toName == "" || fromName == toName {
		return reader
	}
	return &renameStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		renamer: &streamToolRenamer{protocol: protocol, fromName: fromName, toName: toName, tools: map[string]*streamToolRenameState{}},
	}
}

// renameStreamReader is the io.Reader RenameStreamReader returns: it reads the
// upstream stream line by line, rewrites the data lines that need it, and
// serves the rewritten bytes, carrying an error over to the next Read.
type renameStreamReader struct {
	buffer  *bufio.Reader
	renamer *streamToolRenamer
	pending []byte
	err     error
}

// Read returns the rewritten stream bytes, buffering one line at a time.
func (reader *renameStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		// ReadString either returns a non-empty chunk with a nil error or
		// returns the trailing chunk together with EOF, so one iteration
		// always either fills pending or reports the error below; the loop
		// condition only guards the entry.
		line, err := reader.buffer.ReadString('\n')
		if len(line) > 0 {
			reader.pending = reader.rewriteLine(line)
		}
		if err != nil {
			// The final chunk is drained before the error is reported so a
			// truncated stream still delivers every byte the upstream sent.
			reader.err = err
			if len(reader.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// rewriteLine transforms one complete SSE line, preserving its original bytes
// and terminator whenever nothing needs to be rewritten.
func (reader *renameStreamReader) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" || raw == "[DONE]" {
		return []byte(line)
	}
	rewritten := reader.renamer.rewriteFrame(raw)
	if string(rewritten) == raw {
		return []byte(line)
	}
	return []byte("data: " + string(rewritten) + terminator)
}

// streamToolRenameState tracks how one tool call's name was resolved while a
// stream is rewritten. Chat deltas may repeat a growing name prefix or append
// fragments, so a value that is still a prefix of fromName is held back until
// it either completes the match or diverges.
type streamToolRenameState struct {
	decided bool
	renamed bool
	pending string
}

// streamToolRenamer rewrites the tool names of one protocol's SSE frames,
// keeping per-tool state across frames so a name split over several Chat
// chunks is reassembled before it is matched.
type streamToolRenamer struct {
	protocol Protocol
	fromName string
	toName   string
	tools    map[string]*streamToolRenameState
}

// rewriteFrame rewrites one SSE data payload, which is raw JSON text. Frames
// that cannot carry a tool name are returned untouched, so only a payload that
// mentions the source name (Anthropic and Responses) or "tool_calls" (Chat) is
// decoded.
func (renamer *streamToolRenamer) rewriteFrame(raw string) []byte {
	if renamer.protocol != Chat {
		// Anthropic and Responses streams always carry a complete tool name in
		// a single event, so the stateless object rewrite is exact.
		if !strings.Contains(raw, renamer.fromName) {
			return []byte(raw)
		}
		return RenameResponseTool([]byte(raw), renamer.protocol, renamer.fromName, renamer.toName)
	}
	if !strings.Contains(raw, `"tool_calls"`) {
		return []byte(raw)
	}
	return renamer.rewriteChatFrame(raw)
}

// rewriteChatFrame rewrites the tool names of one Chat chunk. Tool calls are
// keyed by index, then id, then array position, and each name fragment is fed
// through applyNameFragment so a name split across chunks is restored as a
// whole. It returns the frame unchanged when nothing was rewritten.
func (renamer *streamToolRenamer) rewriteChatFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	changed := false
	for _, rawChoice := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := rawChoice.(map[string]any)
		for _, container := range []string{"delta", "message"} {
			message, _ := choice[container].(map[string]any)
			if message == nil {
				continue
			}
			for callIndex, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if function == nil {
					continue
				}
				state := renamer.toolState(fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id"), callIndex)))
				if name, ok := function["name"].(string); ok && name != "" {
					if renamer.applyNameFragment(state, function, name) {
						changed = true
					}
				}
				if _, ok := function["arguments"]; ok && !state.decided {
					// Arguments can only start once the name is complete: flush
					// whatever was held back, verbatim.
					if state.pending != "" {
						function["name"] = state.pending
						changed = true
					}
					state.decided = true
				}
			}
		}
	}
	if !changed {
		return []byte(raw)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(raw)
	}
	return encoded
}

// applyNameFragment folds one name fragment into a tool's rename state and
// reports whether the frame changed. Once the accumulated name completes
// fromName, it is replaced by toName; while it is still a prefix of fromName
// the name is held back; on divergence the accumulated name is written out as
// is. After a resolved rename, any further fragment is dropped, because it
// belongs to a longer name the client never declared.
func (renamer *streamToolRenamer) applyNameFragment(state *streamToolRenameState, function map[string]any, fragment string) bool {
	if state.decided {
		if !state.renamed {
			return false
		}
		// A resolved alias accepts no further fragments: any continuation
		// belongs to a longer name the client never declared.
		if _, exists := function["name"]; exists {
			delete(function, "name")
			return true
		}
		return false
	}
	state.pending = mergeToolName(state.pending, fragment)
	switch {
	case state.pending == renamer.fromName:
		function["name"] = renamer.toName
		state.decided, state.renamed = true, true
		return true
	case strings.HasPrefix(renamer.fromName, state.pending):
		// Still a prefix of the alias: hold the fragment until a later frame
		// either completes the match or diverges.
		delete(function, "name")
		return true
	default:
		function["name"] = state.pending
		state.decided = true
		return true
	}
}

// toolState returns the rename state for a tool key, creating it on first
// sight.
func (renamer *streamToolRenamer) toolState(key string) *streamToolRenameState {
	if state := renamer.tools[key]; state != nil {
		return state
	}
	state := &streamToolRenameState{}
	renamer.tools[key] = state
	return state
}

// streamUsageTracker is an io.Writer that splits a relayed stream into SSE
// frames and parses them for usage, while recording whether the stream reached
// a terminal or error event and whether any frame failed to parse. It never
// modifies the bytes it is given.
type streamUsageTracker struct {
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

// streamUsageObserver builds a usage tracker whose parser reads the given
// protocol.
func streamUsageObserver(protocol Protocol) *streamUsageTracker {
	return &streamUsageTracker{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

// Write buffers the relayed bytes and consumes every complete SSE frame they
// contain, keeping the incomplete tail for the next call. It never fails.
func (observer *streamUsageTracker) Write(data []byte) (int, error) {
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

// Finish returns the merged usage and discards the buffered tail.
func (observer *streamUsageTracker) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageTracker) Reported() bool { return observer.reported }

func (observer *streamUsageTracker) NormalTermination() bool { return observer.normal }

func (observer *streamUsageTracker) ErrorTermination() bool { return observer.error }

func (observer *streamUsageTracker) ParseError() error { return observer.parseErr }

// StopReason reports the raw upstream finish reason observed on the
// stream ("", "stop", "length", "tool_calls", ...), or "" when the
// stream never carried a terminal event.
func (observer *streamUsageTracker) StopReason() string { return observer.stop }

// TextLen reports how much text passed through on this stream.
func (observer *streamUsageTracker) TextLen() int { return observer.text.Len() }

// TextTail returns the last n characters streamed.
func (observer *streamUsageTracker) TextTail(n int) string {
	full := observer.text.String()
	if len(full) > n {
		full = full[len(full)-n:]
	}
	return full
}

// DataDelivered reports whether any SSE data frame arrived upstream. A
// stream that died on keepalives alone replays safely: comments carry no
// model content, so the client saw nothing actionable.
func (observer *streamUsageTracker) DataDelivered() bool { return observer.dataFrames > 0 }

// consume parses one complete SSE frame and records its terminal events and
// usage. A parse error is remembered but not propagated, because the relay
// must keep copying bytes.
func (observer *streamUsageTracker) consume(frame []byte) {
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

// nextSSEBoundary finds the end of the first complete SSE frame in data: it
// returns the index where the frame's terminator starts and the terminator's
// width, preferring whichever of a blank line ("\n\n") or a CRLF blank line
// ("\r\n\r\n") comes first. No complete frame yields -1.
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

// readSSE reads a stream and calls handler once per complete SSE record, with
// the record's event name and its data lines joined by newlines. A record ends
// at a blank line; comment lines are skipped, and a record without any data
// line is not delivered. A final record whose data is not terminated by a
// blank line is an unexpected EOF rather than a complete event. Scanner
// errors, and errors returned by handler, end the read.
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
