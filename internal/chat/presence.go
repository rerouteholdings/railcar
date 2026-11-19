package chat

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
)

// recProfiles holds the public profiles this device has fetched (§13.1).
const recProfiles = "profiles"

// people keeps contacts' presence and everyone's public profiles (§13).
// It has a lock of its own, so the stream's presence frames don't wait
// for the inbox.
type people struct {
	mu       sync.Mutex
	presence map[string]client.Presence
	profiles map[string]client.Profile
	fetching map[string]bool
	// own is the presence the user chose; away is set while auto-away
	// holds it (§13.2).
	own  OwnPresence
	away bool
}

// OwnPresence is the presence state and status message the user chose
// (§13.2). It's kept on this device, and sent again on every connect.
type OwnPresence struct {
	State  string `json:"state"`
	Status string `json:"status,omitempty"`
}

const recOwnPresence = "presence"

func (e *Engine) loadPeople() {
	e.ppl.presence = map[string]client.Presence{}
	e.ppl.profiles = map[string]client.Profile{}
	e.ppl.fetching = map[string]bool{}
	if err := e.profile().Load(recProfiles, &e.ppl.profiles); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: the stored profiles", "err", err)
	}
	if e.ppl.profiles == nil {
		e.ppl.profiles = map[string]client.Profile{}
	}
	e.profile().Load(recOwnPresence, &e.ppl.own)
	if !client.ValidPresence(e.ppl.own.State) {
		e.ppl.own = OwnPresence{State: client.PresenceOnline}
	}
}

// Presence returns a contact's presence, as the stream last said, and
// whether there's any (§13.2).
func (e *Engine) Presence(id string) (client.Presence, bool) {
	e.ppl.mu.Lock()
	defer e.ppl.mu.Unlock()
	p, ok := e.ppl.presence[id]
	return p, ok
}

// ProfileOf returns an identity's public profile, as this device last
// fetched it (§13.1).
func (e *Engine) ProfileOf(id string) (client.Profile, bool) {
	e.ppl.mu.Lock()
	defer e.ppl.mu.Unlock()
	p, ok := e.ppl.profiles[id]
	return p, ok
}

// OwnPresence returns the presence the user chose.
func (e *Engine) OwnPresence() OwnPresence {
	e.ppl.mu.Lock()
	defer e.ppl.mu.Unlock()
	return e.ppl.own
}

// presenceFrame handles a presence frame from the stream: it keeps it, and
// fetches the profile when pv is newer than the one this device has
// (client API, Receive presence).
func (e *Engine) presenceFrame(ctx context.Context, p client.Presence) {
	e.ppl.mu.Lock()
	if old, ok := e.ppl.presence[p.ID]; ok && old.TS > p.TS {
		e.ppl.mu.Unlock()
		return
	}
	e.ppl.presence[p.ID] = p
	prof, have := e.ppl.profiles[p.ID]
	e.ppl.mu.Unlock()
	e.emit(Event{Kind: EventPresence, Who: p.ID})
	if p.PV > 0 && (!have || p.PV > prof.V) {
		go e.RefreshProfile(ctx, p.ID)
	}
}

// RefreshProfile fetches an identity's profile and keeps it, if it's newer
// than the one this device has. One fetch per identity runs at a time.
func (e *Engine) RefreshProfile(ctx context.Context, id string) (client.Profile, error) {
	e.ppl.mu.Lock()
	if e.ppl.fetching[id] {
		e.ppl.mu.Unlock()
		return client.Profile{}, nil
	}
	e.ppl.fetching[id] = true
	e.ppl.mu.Unlock()
	defer func() {
		e.ppl.mu.Lock()
		delete(e.ppl.fetching, id)
		e.ppl.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, err := e.c.FetchProfile(ctx, id)
	if err != nil {
		if !errors.Is(err, client.ErrNoProfile) {
			e.log.Debug("chat: a profile", "err", err)
		}
		return client.Profile{}, err
	}
	e.keepProfile(p)
	return p, nil
}

// keepProfile stores a profile unless the one stored is newer.
func (e *Engine) keepProfile(p client.Profile) {
	e.ppl.mu.Lock()
	if old, ok := e.ppl.profiles[p.ID]; ok && old.V > p.V {
		e.ppl.mu.Unlock()
		return
	}
	e.ppl.profiles[p.ID] = p
	err := e.profile().Save(recProfiles, e.ppl.profiles)
	e.ppl.mu.Unlock()
	if err != nil {
		e.log.Info("chat: saving profiles", "err", err)
	}
	e.emit(Event{Kind: EventPresence, Who: p.ID})
}

// ProfileEdit changes some of this identity's profile fields. Nil fields
// stay as they are.
type ProfileEdit struct {
	Name   *string
	Bio    *string
	Links  []string // nil keeps them; empty clears them
	Avatar *client.PublicMedia
	// NoAvatar removes the picture.
	NoAvatar bool
}

// EditProfile publishes this identity's profile with the edit applied to
// the one published now (§13.1).
func (e *Engine) EditProfile(ctx context.Context, edit ProfileEdit) (client.Profile, error) {
	cur, err := e.c.FetchProfile(ctx, e.c.ID())
	if err != nil && !errors.Is(err, client.ErrNoProfile) {
		return client.Profile{}, err
	}
	if edit.Name != nil {
		cur.Name = *edit.Name
	}
	if edit.Bio != nil {
		cur.Bio = *edit.Bio
	}
	if edit.Links != nil {
		cur.Links = edit.Links
	}
	if edit.Avatar != nil {
		cur.Avatar = edit.Avatar
	}
	if edit.NoAvatar {
		cur.Avatar = nil
	}
	p, err := e.c.PublishProfile(ctx, cur)
	if err != nil {
		return client.Profile{}, err
	}
	e.keepProfile(p)
	return p, nil
}

// SetPresence sets the presence state and status message the user chose,
// sends it (§13.2), and keeps it for the next connect.
func (e *Engine) SetPresence(ctx context.Context, state, status string) error {
	if err := e.c.SetPresence(ctx, state, status); err != nil {
		return err
	}
	e.ppl.mu.Lock()
	e.ppl.own, e.ppl.away = OwnPresence{State: state, Status: status}, false
	err := e.profile().Save(recOwnPresence, e.ppl.own)
	e.ppl.mu.Unlock()
	e.emit(Event{Kind: EventPresence, Who: e.c.ID()})
	return err
}

// AutoAway moves available and online to away while the user is idle, and
// back to whichever they had when they return (§13.2). Other states stay.
func (e *Engine) AutoAway(ctx context.Context, idle bool) error {
	e.ppl.mu.Lock()
	own, away := e.ppl.own, e.ppl.away
	if own.State != client.PresenceAvailable && own.State != client.PresenceOnline || idle == away {
		e.ppl.mu.Unlock()
		return nil
	}
	e.ppl.away = idle
	e.ppl.mu.Unlock()
	state := own.State
	if idle {
		state = client.PresenceAway
	}
	err := e.c.SetPresence(ctx, state, own.Status)
	e.emit(Event{Kind: EventPresence, Who: e.c.ID()})
	return err
}

// Away reports whether auto-away holds the user's presence now.
func (e *Engine) Away() bool {
	e.ppl.mu.Lock()
	defer e.ppl.mu.Unlock()
	return e.ppl.away
}

// announcePresence sends the user's presence after the stream connects.
func (e *Engine) announcePresence(ctx context.Context) {
	e.ppl.mu.Lock()
	own := e.ppl.own
	if e.ppl.away {
		own.State = client.PresenceAway
	}
	e.ppl.mu.Unlock()
	if err := e.c.SetPresence(ctx, own.State, own.Status); err != nil {
		e.log.Debug("chat: presence", "err", err)
	}
}

// fetchMissingProfiles fetches the profiles of contacts, people asking to
// be contacts, and chat members that this device has none of, once per
// connect. Presence only reaches mutual contacts, so this is how a group
// chat's other members and a request's sender get their names (§13.1).
func (e *Engine) fetchMissingProfiles(ctx context.Context) {
	ids := map[string]bool{}
	e.mu.Lock()
	for id := range e.st.Contacts {
		ids[id] = true
	}
	for _, ch := range e.st.Chats {
		if ch.Kind == KindChannel {
			continue
		}
		for _, id := range ch.Members {
			ids[id] = true
		}
	}
	e.mu.Unlock()
	ids[e.c.ID()] = true
	n := 0
	for id := range ids {
		if _, ok := e.ProfileOf(id); ok {
			continue
		}
		if n++; n > 200 || ctx.Err() != nil {
			return
		}
		e.RefreshProfile(ctx, id)
	}
}
