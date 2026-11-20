package chat

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// linkBundle handles a link delivery (§7.2 step 4). A device that was
// linked opens its own bundle once, and takes the contact list from it and
// from the homeserver. Every other device of the identity gets the same
// delivery, can't open it, and ignores it. The caller holds e.mu.
func (e *Engine) linkBundle(ctx context.Context, d *client.Delivery) error {
	if d.From != e.c.ID() || !e.c.LinkPending() {
		return nil
	}
	b, err := e.c.OpenLinkBundle(d.Data)
	if err != nil {
		e.log.Debug("chat: a link bundle for another device", "err", err)
		return nil
	}
	e.log.Debug("chat: the link bundle", "contacts", len(b.Contacts), "groups", len(b.Groups), "history", len(b.History), "communities", len(b.Communities))
	e.st.Expected = slices.Clone(b.Groups)
	if err := e.importContacts(ctx, b.Contacts); err != nil {
		return err
	}
	e.joinLinkedCommunities(ctx, b.Communities)
	if len(b.History) > 0 {
		// Earlier messages, kept until this device joins their chats
		// (§7.2, §7.3). Without them, the chats start empty.
		if err := e.importHistory(ctx, b.History); err != nil {
			e.log.Info("chat: the link bundle's history", "err", err)
		}
	}
	return nil
}

// ImportContacts takes the contact list from the homeserver
// (GET contacts), with each contact's handle from its chain. A newly
// linked device calls it once, before it processes its inbox, so Welcomes
// from contacts find them (§7.2 step 5).
func (e *Engine) ImportContacts(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.importContacts(ctx, nil)
}

// importContacts merges the homeserver's contact list, and the identifiers
// a link bundle names, into this device's contacts. Contacts this device
// already has keep their state. The caller holds e.mu.
func (e *Engine) importContacts(ctx context.Context, extra []string) error {
	list, err := e.c.Contacts(ctx)
	if err != nil {
		return retryable{err}
	}
	for _, id := range extra {
		if !slices.ContainsFunc(list, func(c client.Contact) bool { return c.ID == id }) {
			list = append(list, client.Contact{ID: id, State: client.ContactFull})
		}
	}
	for _, hc := range list {
		if c := e.st.Contacts[hc.ID]; c != nil && c.State != StateDeclined {
			continue
		}
		c := &Contact{ID: hc.ID, Since: hc.Since}
		switch hc.State {
		case client.ContactFull:
			c.State = StateContact
		case client.ContactPendingOut:
			c.State = StatePendingOut
		case client.ContactBlocked:
			c.State = StateBlocked
		case client.ContactPendingIn:
			if hc.Request == nil {
				continue
			}
			c.State, c.Request = StatePendingIn, hc.Request
		default:
			continue
		}
		chain, err := e.fetchChain(ctx, hc.ID, false)
		if err != nil {
			e.log.Info("chat: a contact's chain", "err", err)
			var w *client.ChainWarning
			if errors.As(err, &w) {
				continue
			}
		} else {
			c.Handle = chain.State.Handle
			if c.State == StatePendingIn {
				if _, err := e.c.VerifyContactRequest(*c.Request, chain); err != nil {
					continue
				}
			}
		}
		if c.Handle == "" {
			c.Handle = e.st.Handles[hc.ID]
		}
		e.st.Contacts[hc.ID] = c
		e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	}
	return e.save()
}

// AddDevice adds a device another part of Railcar approved (§7.2 steps 3
// and 4): it appends add-device to the chain, sends the link bundle with
// the contact list and chat list, and adds the device to every chat
// (§11.9). A device without key packages yet is added once it has some,
// while the live stream runs.
func (e *Engine) AddDevice(ctx context.Context, d client.NewDevice, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var b client.LinkBundle
	for _, id := range slices.Sorted(maps.Keys(e.st.Contacts)) {
		if e.st.Contacts[id].State == StateContact {
			b.Contacts = append(b.Contacts, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.st.Chats)) {
		if c := e.st.Chats[id]; c.Stopped == "" && c.Kind != KindChannel && !c.Left {
			b.Groups = append(b.Groups, id)
		}
	}
	for _, server := range slices.Sorted(maps.Keys(e.st.Communities)) {
		if cm := e.st.Communities[server]; !cm.Left && !cm.Asking && cm.MovingTo == "" {
			b.Communities = append(b.Communities, server)
		}
	}
	// History goes as files the new device downloads (§7.2, §7.3). Without
	// it, the new device starts with empty chats, so a failure here
	// doesn't stop the linking.
	if h, err := e.uploadLinkHistory(ctx); err != nil {
		e.log.Info("chat: the history for the new device", "err", err)
	} else {
		b.History = h
	}
	if err := e.c.AddLinkedDevice(ctx, d, name, b); err != nil {
		return err
	}
	e.wantSync()
	if _, err := e.syncAll(ctx); err != nil {
		e.log.Info("chat: adding the new device to chats", "err", err)
	}
	return nil
}

// RemoveDevice removes another of the identity's devices from its chain,
// and its leaves from every chat (§7.6, §11.9).
func (e *Engine) RemoveDevice(ctx context.Context, key []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.c.RemoveDevice(ctx, key); err != nil {
		return err
	}
	e.wantSync()
	if _, err := e.syncAll(ctx); err != nil {
		e.log.Info("chat: removing the device from chats", "err", err)
	}
	return nil
}
