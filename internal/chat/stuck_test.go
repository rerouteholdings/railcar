package chat

import (
	"testing"
	"time"
)

// TestStuckReason checks when re-creating a chat is offered (§11.11).
func TestStuckReason(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	for _, c := range []struct {
		ch    Chat
		stuck bool
	}{
		{Chat{}, false},
		{Chat{HubDownSince: ago(23 * time.Hour)}, false},
		{Chat{HubDownSince: ago(24 * time.Hour)}, true},
		{Chat{FailingSince: ago(59 * time.Minute)}, false},
		{Chat{FailingSince: ago(time.Hour)}, true},
		{Chat{Pending: true, PendingSince: ago(25 * time.Hour)}, true},
		{Chat{Pending: false, PendingSince: ago(25 * time.Hour)}, false},
	} {
		if got := stuckReason(&c.ch, now) != ""; got != c.stuck {
			t.Errorf("%+v: stuck %v, want %v", c.ch, got, c.stuck)
		}
	}
}
