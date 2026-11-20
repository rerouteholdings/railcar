package chat

import (
	"testing"
	"time"
)

// TestCallList checks a group call's participant list (§15.2, "Calls
// going on"): ring and accept count for 6 hours, state for 120 seconds,
// and a call nobody has sent state for in 120 seconds is over.
func TestCallList(t *testing.T) {
	now := time.Now()
	k := &knownCall{in: map[string]string{}}
	k.note("a", "kat", opRing)
	k.note("b", "ravi", opAccept)
	k.prune(now.Add(5 * time.Hour))
	if len(k.in) != 2 {
		t.Fatalf("after 5 hours, without state: %v", k.in)
	}
	k.prune(now.Add(7 * time.Hour))
	if len(k.in) != 0 {
		t.Fatalf("after 7 hours, without state: %v", k.in)
	}

	k = &knownCall{in: map[string]string{}}
	k.note("a", "kat", opRing)
	k.note("c", "lee", opState)
	k.prune(now.Add(time.Minute))
	if len(k.in) != 2 {
		t.Fatalf("a minute in: %v", k.in)
	}
	k.prune(now.Add(3 * time.Minute))
	if len(k.in) != 0 {
		t.Fatalf("no state for 3 minutes, and the call goes on: %v", k.in)
	}
}
