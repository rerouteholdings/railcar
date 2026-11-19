package chat

import (
	"context"
	"maps"
	"slices"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// Moving to another homeserver (§6.6). The identifier, contacts, and
// chats stay the same; the homeserver and the handle change.

// Move moves the identity to another homeserver, with name as its handle
// under the new server's handle domain, and invite as its registration
// code. Contacts' homeservers learn of it from the chain the new
// homeserver pushes.
func (e *Engine) Move(ctx context.Context, server, name, invite string) error {
	e.mu.Lock()
	r := client.MoveRequest{Server: server, Name: name, Invite: invite}
	for _, id := range slices.Sorted(maps.Keys(e.st.Contacts)) {
		if s := e.st.Contacts[id].State; s == StateContact || s == StatePendingOut {
			r.Contacts = append(r.Contacts, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.st.Chats)) {
		if c := e.st.Chats[id]; c.Kind != KindChannel && !c.Left {
			r.Groups = append(r.Groups, id)
		}
	}
	e.mu.Unlock()
	if err := e.c.Move(ctx, r); err != nil {
		return err
	}
	return e.afterMove(ctx)
}

// FinishMove finishes a move that stopped partway, if there is one. Run
// calls it before anything else (§6.6).
func (e *Engine) FinishMove(ctx context.Context) error {
	if e.c.PendingMove() == nil {
		return nil
	}
	if err := e.c.FinishMove(ctx); err != nil {
		return err
	}
	return e.afterMove(ctx)
}

// afterMove gives the new homeserver this device's key packages.
func (e *Engine) afterMove(ctx context.Context) error {
	if err := e.EnsureKeyPackages(ctx); err != nil {
		e.log.Info("chat: key packages on the new homeserver", "err", err)
	}
	e.emit(Event{Kind: EventPresence, Who: e.c.ID()}) // the handle and homeserver changed
	return nil
}
