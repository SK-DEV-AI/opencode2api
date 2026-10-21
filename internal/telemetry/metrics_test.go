package telemetry

import (
	"testing"
	"time"
)

// The recent-request ring backs the monitor's request history. Upstream
// rewrote it as a fixed-capacity ring buffer (upstreamRequestBuffer); this
// test pins the behaviour that matters: FIFO order, overflow eviction of the
// oldest entry, and pruning by age.
func TestRequestRingFIFO(t *testing.T) {
	ring := upstreamRequestRing()
	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		ring.Add(UpstreamRequest{RequestID: string(rune('a' + i)), Time: base.Add(time.Duration(i) * time.Minute)})
	}
	got := ring.Snapshot(base.Add(-time.Minute), 10)
	if len(got) != 3 || got[0].RequestID != "a" || got[2].RequestID != "c" {
		t.Fatalf("fifo order broken: %+v", got)
	}
}

// Overflow evicts the oldest entry.
func TestRequestRingOverflow(t *testing.T) {
	ring := upstreamRequestRing()
	base := time.Now().UTC()
	for i := 0; i < monitorRecentRequestCapacity+1; i++ {
		ring.Add(UpstreamRequest{RequestID: string(rune('a' + i%26)), Time: base.Add(time.Duration(i) * time.Minute)})
	}
	got := ring.Snapshot(base.Add(-time.Minute), monitorRecentRequestCapacity+1)
	if len(got) != monitorRecentRequestCapacity {
		t.Fatalf("ring holds %d entries, want the capacity %d", len(got), monitorRecentRequestCapacity)
	}
	// The newest entry must be last, and the oldest must be gone.
	if got[len(got)-1].Time.Before(base.Add(time.Duration(monitorRecentRequestCapacity) * time.Minute)) {
		t.Fatalf("the newest entry was not retained: %+v", got[len(got)-1])
	}
}

// Prune drops everything before the cutoff.
func TestRequestRingPrune(t *testing.T) {
	ring := upstreamRequestRing()
	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		ring.Add(UpstreamRequest{RequestID: string(rune('a' + i)), Time: base.Add(time.Duration(i) * time.Minute)})
	}
	// Cut off everything except the newest entry.
	ring.PruneBefore(base.Add(2 * time.Minute))
	got := ring.Snapshot(base.Add(-time.Minute), 10)
	if len(got) != 1 || got[0].RequestID != "c" {
		t.Fatalf("prune broken: %+v", got)
	}
	// A cutoff that drops everything leaves the ring empty.
	ring.PruneBefore(base.Add(time.Hour))
	if got := ring.Snapshot(base.Add(-time.Minute), 10); len(got) != 0 {
		t.Fatalf("prune left %d entries: %+v", len(got), got)
	}
}
