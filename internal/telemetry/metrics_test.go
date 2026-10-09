package telemetry

import (
	"testing"
	"time"
)

func TestEventRingFIFO(t *testing.T) {
	ring := newEventRing[UpstreamRequest](3)
	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		ring.Add(UpstreamRequest{RequestID: string(rune('a' + i)), Time: base.Add(time.Duration(i) * time.Minute)})
	}
	got := ring.Snapshot(base, 10)
	if len(got) != 3 || got[0].RequestID != "a" || got[2].RequestID != "c" {
		t.Fatalf("fifo order broken: %+v", got)
	}
	// Overflow evicts the oldest.
	ring.Add(UpstreamRequest{RequestID: "d", Time: base.Add(3 * time.Minute)})
	got = ring.Snapshot(base, 10)
	if len(got) != 3 || got[0].RequestID != "b" || got[2].RequestID != "d" {
		t.Fatalf("overflow eviction broken: %+v", got)
	}
	// Prune drops everything before the cutoff.
	ring.PruneBefore(base.Add(3 * time.Minute))
	got = ring.Snapshot(base, 10)
	if len(got) != 1 || got[0].RequestID != "d" {
		t.Fatalf("prune broken: %+v", got)
	}
}

func TestEventRingAttemptType(t *testing.T) {
	ring := newEventRing[UpstreamAttempt](2)
	base := time.Now().UTC()
	ring.Add(UpstreamAttempt{RequestID: "x", Time: base})
	ring.Add(UpstreamAttempt{RequestID: "y", Time: base.Add(time.Minute)})
	got := ring.Snapshot(base, 10)
	if len(got) != 2 || got[1].RequestID != "y" {
		t.Fatalf("attempt ring broken: %+v", got)
	}
}
