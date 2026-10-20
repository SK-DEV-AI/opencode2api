package protocol

import (
	"io"
	"sync"
	"time"
)

// IdleTracker wraps an upstream stream and records when the upstream last
// produced bytes.
//
// Why this exists: the gateway's heartbeat (": keepalive" SSE comments) does
// NOT protect a client that parses SSE strictly. jcode's openai-compat path
// uses OpenRouterStream, whose parse_next_event skips comment-only events, so
// a heartbeat produces no StreamEvent and never resets that client's idle
// timer. A client that counts only real events therefore sees a dead
// connection while the model is thinking.
//
// The tracker gives the gateway a way to tell "the upstream is thinking" from
// "the upstream is gone": a thinking stream keeps its silence short (measured
// 8-56s on this box), while a dead stream stays silent until the outer budget
// expires — which is the multi-minute hang a client sees.
//
// The tracker does not decide anything by itself: the caller supplies the
// limit and acts on it.
type IdleTracker struct {
	body io.ReadCloser

	mu       sync.Mutex
	lastRead time.Time
	bytes    int64
}

// NewIdleTracker wraps body and starts the silence clock at the first read.
func NewIdleTracker(body io.ReadCloser) *IdleTracker {
	return &IdleTracker{body: body, lastRead: time.Now()}
}

// Read records activity: any byte from the upstream means the stream is alive.
func (t *IdleTracker) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.lastRead = time.Now()
		t.bytes += int64(n)
		t.mu.Unlock()
	}
	return n, err
}

// Close closes the underlying body.
func (t *IdleTracker) Close() error { return t.body.Close() }

// SilentFor reports how long the upstream has produced no bytes.
func (t *IdleTracker) SilentFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.lastRead)
}

// Bytes reports how many bytes the upstream produced.
func (t *IdleTracker) Bytes() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes
}
