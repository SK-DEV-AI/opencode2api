package protocol

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The IdleTracker measures UPSTREAM silence, which is what separates "the
// model is thinking" from "the upstream is gone". The gateway's heartbeat
// (": keepalive" SSE comments) does not protect a client that parses SSE
// strictly: jcode's openai-compat path skips comment-only events, so a
// heartbeat never resets that client's idle timer.
func TestIdleTrackerMeasuresSilence(t *testing.T) {
	tracker := NewIdleTracker(io.NopCloser(strings.NewReader("hello")))

	if tracker.SilentFor() < 0 {
		t.Fatalf("SilentFor returned a negative duration")
	}
	if tracker.Bytes() != 0 {
		t.Fatalf("Bytes = %d before any read, want 0", tracker.Bytes())
	}

	buf := make([]byte, 5)
	n, err := tracker.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 5 {
		t.Fatalf("read %d bytes, want 5", n)
	}
	if tracker.Bytes() != 5 {
		t.Fatalf("Bytes = %d after a 5-byte read, want 5", tracker.Bytes())
	}
}

// A read that returns bytes RESETS the silence clock. That is the whole
// point: a thinking stream that keeps producing frames is never idle.
func TestIdleTrackerResetsOnActivity(t *testing.T) {
	tracker := NewIdleTracker(io.NopCloser(strings.NewReader("abcdefghij")))

	buf := make([]byte, 5)
	if _, err := tracker.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	afterFirst := tracker.SilentFor()
	time.Sleep(50 * time.Millisecond)
	if _, err := tracker.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	afterSecond := tracker.SilentFor()
	if afterSecond > afterFirst+40*time.Millisecond {
		t.Fatalf("silence clock was not reset by activity: %v then %v", afterFirst, afterSecond)
	}
}

// A tracker over a body that never produces anything stays silent, which is
// what the watchdog keys on.
func TestIdleTrackerReportsGrowingSilence(t *testing.T) {
	tracker := NewIdleTracker(io.NopCloser(strings.NewReader("")))
	time.Sleep(80 * time.Millisecond)
	silent := tracker.SilentFor()
	if silent < 60*time.Millisecond {
		t.Fatalf("SilentFor = %v after 80ms of silence, want >= 60ms", silent)
	}
}

// The tracker must not race with the reader.
func TestIdleTrackerConcurrentRead(t *testing.T) {
	tracker := NewIdleTracker(io.NopCloser(strings.NewReader(strings.Repeat("x", 4096))))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		for {
			if _, err := tracker.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = tracker.SilentFor()
			_ = tracker.Bytes()
		}
	}()
	wg.Wait()
}
