package chat

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// When a client offers to re-create a stuck chat (§11.11).
const (
	hubDownFor = 24 * time.Hour // the hub hasn't answered
	failingFor = time.Hour      // every commit has failed
	pendingFor = 24 * time.Hour // a commit has waited for a check (§7.1)
)

// unreachable reports an error that means a server didn't answer.
func unreachable(err error) bool {
	var ne net.Error
	return wire.IsCode(err, wire.CodeUnavailable) || errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

// noteHub records how a commit went: nil for one the hub took. A commit
// that lost a race to another isn't a failure (§11.6). The caller holds
// e.mu.
func (e *Engine) noteHub(ch *Chat, err error) {
	switch {
	case err == nil:
		ch.FailingSince = 0
		e.noteHubAnswer(ch, true)
	case wire.IsCode(err, wire.CodeStaleEpoch):
		e.noteHubAnswer(ch, true)
	default:
		if ch.FailingSince == 0 {
			ch.FailingSince = e.c.Clock().Now().Unix()
		}
		if unreachable(err) {
			e.noteHubAnswer(ch, false)
		}
	}
}

// noteHubAnswer records whether the hub answered. The caller holds e.mu.
func (e *Engine) noteHubAnswer(ch *Chat, answered bool) {
	switch {
	case answered:
		ch.HubDownSince = 0
	case ch.HubDownSince == 0:
		ch.HubDownSince = e.c.Clock().Now().Unix()
	}
}

// stuckReason says why a chat is stuck enough to offer re-creating it,
// or "" (§11.11).
func stuckReason(ch *Chat, now time.Time) string {
	since := func(t int64) time.Duration { return now.Sub(time.Unix(t, 0)) }
	switch {
	case ch.HubDownSince != 0 && since(ch.HubDownSince) >= hubDownFor:
		return "Its hub, " + ch.Hub + ", hasn't answered for a day"
	case ch.FailingSince != 0 && since(ch.FailingSince) >= failingFor:
		return "Every change to it has failed for an hour"
	case ch.Pending && ch.PendingSince != 0 && since(ch.PendingSince) >= pendingFor:
		return "A change to it has waited a day for a check that can't finish"
	}
	return ""
}

// checkStuck offers to re-create each chat that's stuck, once each time it
// gets stuck, with a line in the chat. Re-creating stays the user's
// choice. The caller holds e.mu.
func (e *Engine) checkStuck() {
	now := e.c.Clock().Now()
	changed := false
	for _, ch := range e.st.Chats {
		if ch.Kind == KindChannel || ch.Left || ch.Rejoining {
			continue
		}
		switch {
		case ch.Pending && ch.PendingSince == 0:
			ch.PendingSince, changed = now.Unix(), true
		case !ch.Pending && ch.PendingSince != 0:
			ch.PendingSince, changed = 0, true
		}
		why := stuckReason(ch, now)
		if why == ch.Stuck {
			continue
		}
		ch.Stuck, changed = why, true
		if why != "" {
			e.systemLine(ch, "This chat seems stuck. "+why+". Type :recreate to re-create it with the same people, as a new chat that carries on this one")
		}
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	}
	if changed {
		if err := e.save(); err != nil {
			e.log.Info("chat: saving", "err", err)
		}
	}
}
