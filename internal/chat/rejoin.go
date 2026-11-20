package chat

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// rejoinEvery is how often a device may come back as a new device on its
// own: at most once a day (§11.9).
const rejoinEvery = 24 * time.Hour

// rejoinLine is what each chat shows while this device waits to be added
// again (§11.9, "A device that's been offline too long").
const rejoinLine = "This device was offline too long to catch up. It's coming back as a new device, and the chat goes on once it's added again."

// ErrRejoinedToday reports a rejoin asked for on its own within a day of
// the last one.
var ErrRejoinedToday = errors.New("chat: this device came back as a new device less than a day ago")

// Rejoin brings this device back as a new device, for a device that can't
// catch up on its chats (§11.9): an inbox gap, a hub that no longer holds
// the commits it missed, or a commit whose key it used up. It replaces its
// device key with a self-rotation, starts a new MLS state, and uploads key
// packages for the new key. Each direct chat and group chat waits, with
// its history kept, until a member's sync rule adds the new device, and
// its Welcome brings the chat back.
func (e *Engine) Rejoin(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rejoin(ctx)
}

// maybeRejoin rejoins on its own after something this device can't catch
// up from, at most once a day. The caller holds e.mu.
func (e *Engine) maybeRejoin(ctx context.Context, why string) {
	if !e.st.LastRejoin.IsZero() && time.Since(e.st.LastRejoin) < rejoinEvery {
		e.log.Info("chat: can't catch up, and came back as a new device less than a day ago", "why", why)
		return
	}
	e.log.Info("chat: coming back as a new device", "why", why)
	if err := e.rejoin(ctx); err != nil {
		e.log.Info("chat: coming back as a new device", "err", err)
	}
}

func (e *Engine) rejoin(ctx context.Context) error {
	if err := e.c.RotateDevice(ctx); err != nil {
		return err
	}
	info, err := e.c.MLSInfo()
	if err != nil {
		return err
	}
	// The old MLS state is for the old key: start again.
	e.dev.Close()
	if err := e.c.Profile().Delete(recMLS); err != nil {
		e.log.Info("chat: deleting the old MLS state", "err", err)
	}
	dev, err := OpenDevice(e.c.Profile(), info)
	if err != nil {
		return err
	}
	e.dev, e.key = dev, ed25519.NewKeyFromSeed(info.MsgKeySeed)
	e.leaves = map[string]bool{}
	e.st.LastRejoin = time.Now()
	for _, ch := range e.st.Chats {
		if ch.Kind == KindChannel || ch.Left || ch.Stopped != "" {
			continue
		}
		ch.Rejoining, ch.Stopped = true, rejoinLine
		ch.Buffered, ch.Pending, ch.Hold = nil, false, false
		e.systemLine(ch, rejoinLine)
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	}
	if err := e.save(); err != nil {
		return err
	}
	if err := e.topUp(ctx); err != nil {
		return err
	}
	e.relinkCommunities(ctx)
	return nil
}

// relinkCommunities has a device that came back as a new device wait to be
// added to its communities again, as a newly linked device does (§7.2
// step 5): it signs in to each community server with the new key, and
// leaves key packages there. The caller holds e.mu.
func (e *Engine) relinkCommunities(ctx context.Context) {
	for _, cm := range e.st.Communities {
		if cm.Left || cm.Asking || cm.MovingTo != "" {
			continue
		}
		cm.Groups, cm.Linking, cm.Cursor = map[string]*CGroup{}, true, ""
		conn := e.c.Community(cm.Server)
		if err := conn.SignIn(ctx); err != nil {
			e.log.Info("chat: signing in to a community server with the new key", "err", err)
			continue
		}
		if err := e.topUpCommunity(ctx, cm); err != nil {
			e.log.Info("chat: community key packages for the new key", "err", err)
		}
		e.refreshChannels(cm)
	}
	e.save()
}

// rejoinedChat carries a chat's place in the list over to the record its
// Welcome makes, once this device is back in it. The caller holds e.mu.
func (e *Engine) rejoinedChat(old, ch *Chat) {
	if old == nil || !old.Rejoining {
		return
	}
	ch.LastActivity, ch.Preview, ch.Unread = old.LastActivity, old.Preview, old.Unread
	ch.ReadReceipts, ch.Picture = old.ReadReceipts, old.Picture
	e.systemLine(ch, "This device is back in the chat")
}

// rejoinIfDue rejoins when a hub said it no longer has commits this
// device missed. The caller holds e.mu.
func (e *Engine) rejoinIfDue(ctx context.Context) {
	if e.rejoinDue {
		e.rejoinDue = false
		e.maybeRejoin(ctx, "a hub no longer has the commits this device missed")
	}
}

// expired reports a hub's answer that it no longer holds the commits
// asked for (§11.9).
func expired(err error) bool { return wire.IsCode(err, wire.CodeExpired) }
