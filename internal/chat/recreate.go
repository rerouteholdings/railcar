package chat

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Hub handoff and re-creating a group chat (§11.11).

// MoveHub moves a group chat's hub to this identity's homeserver. Only an
// admin can. The homeserver registers the group with itself at the epoch
// the handoff commit creates, before the old hub sequences that commit.
// If the registration fails, nothing is committed, and re-creating the
// group is the way on.
func (e *Engine) MoveHub(ctx context.Context, chat string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	me := e.c.ID()
	if !slices.Contains(ch.Admins, me) {
		return errors.New("chat: only an admin can move the hub")
	}
	if ch.Hub == e.c.Server() {
		return errors.New("chat: the hub is your homeserver already")
	}
	g, _, old, err := e.groupState(ch)
	if err != nil {
		return err
	}
	next := old.Clone()
	next.Hub = e.c.Server()
	after := map[string]bool{}
	for _, m := range ch.Members {
		after[m] = true
	}
	chains, err := e.chainsFor(ctx, ch.Members)
	if err != nil {
		return err
	}
	if err := bindFor(old, next, []string{me}, after, chains); err != nil {
		return err
	}
	register := func(epoch uint64, key []byte) error {
		err := e.c.CreateGroup(ctx, client.CreateGroupOptions{Group: ch.ID, Kind: KindGroup, EpochKey: key, Epoch: epoch})
		if err != nil {
			return &HubError{err}
		}
		return nil
	}
	oldHub := ch.Hub
	if err := e.commit(ctx, ch, g, mls.CommitOptions{}, next, ch.Members, nil, register); err != nil {
		return err
	}
	if err := e.save(); err != nil {
		return err
	}
	e.systemLine(ch, "You moved the group's hub from "+oldHub+" to "+ch.Hub)
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// HubError is a hub handoff the new hub refused. The group can be
// re-created instead (§11.11).
type HubError struct{ Err error }

func (h *HubError) Error() string {
	return "chat: the new hub didn't take the group, so nothing changed. Re-create the group instead: " + h.Err.Error()
}
func (h *HubError) Unwrap() error { return h.Err }

// RecreateGroup re-creates a stuck group chat with the same people who are
// this identity's contacts, its name, topic, and picture, and this
// identity's homeserver as hub. The new group names the old one in
// replaces, and this device moves the chat to it at once: its history
// comes along (§11.11). People who aren't this identity's contacts are
// left out, and a contact of theirs in the group can add them.
func (e *Engine) RecreateGroup(ctx context.Context, chat string) (Chat, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	old, ok := e.st.Chats[chat]
	if !ok || old.Kind != KindGroup || old.Left {
		return Chat{}, errors.New("chat: that isn't a group chat you're in")
	}
	me := e.c.ID()
	var people, left []string
	for _, id := range old.Members {
		if id == me {
			continue
		}
		if c := e.st.Contacts[id]; c != nil && c.State == StateContact {
			people = append(people, id)
		} else {
			left = append(left, id)
		}
	}
	name := old.Name
	if name == "" {
		name = "Group chat"
	}
	nc, err := e.createGroup(ctx, name, people, old.ID)
	var miss *MissingError
	if err != nil && !errors.As(err, &miss) {
		return Chat{}, err
	}
	ch := e.st.Chats[nc.ID]
	if old.Topic != "" || old.Picture {
		ch.Topic = old.Topic
		if p, ok := e.groupPicture(old.ID); ok {
			if err := e.keepGroupPicture(ch, &p); err != nil {
				e.log.Info("chat: the group's picture", "err", err)
			}
		}
		if err := e.sendMeta(ctx, ch); err != nil {
			e.log.Info("chat: the group's name", "err", err)
		}
	}
	e.switchGroup(old, ch)
	if len(left) > 0 {
		e.systemLine(ch, "Left out, since they aren't your contacts: "+joinNames(e.namesOf(left))+". A contact of theirs here can add them")
	}
	return ch.public(), err
}

func joinNames(n []string) string {
	switch len(n) {
	case 0:
		return ""
	case 1:
		return n[0]
	case 2:
		return n[0] + " and " + n[1]
	}
	return strings.Join(n[:len(n)-1], ", ") + ", and " + n[len(n)-1]
}

// offerReplacement looks at a Welcome's replaces: a group chat this device
// is in becomes an offer, when whoever sent the Welcome, every admin of
// the new group, and everyone in it were members of the old one as this
// device last saw it. The user's own re-creation, from another device,
// switches at once. Anything else stays a separate chat (§11.11). The
// caller holds e.mu.
func (e *Engine) offerReplacement(ch *Chat, ext *GroupExt, from string) {
	if len(ext.Replaces) == 0 {
		return
	}
	old, ok := e.st.Chats[wire.EncodeB64(ext.Replaces)]
	if !ok || old.Kind != KindGroup || old.Left || old.ID == ch.ID {
		return
	}
	was := func(id string) bool { return slices.Contains(old.Members, id) }
	if from == e.c.ID() {
		e.switchGroup(old, ch)
		return
	}
	if !was(from) || slices.ContainsFunc(ext.Admins, func(id string) bool { return !was(id) }) || slices.ContainsFunc(ch.Members, func(id string) bool { return !was(id) }) {
		return
	}
	ch.Replaces = old.ID
	var gone []string
	for _, id := range old.Members {
		if !slices.Contains(ch.Members, id) {
			gone = append(gone, id)
		}
	}
	line := e.nameOf(from) + " re-created " + old.Name + " here. Type :switch to move the chat to this group"
	if len(gone) > 0 {
		line += ". Not in it: " + joinNames(e.namesOf(gone))
	}
	if !sameSet(old.Admins, ext.Admins) {
		line += ". Its admins differ from the old group's"
	}
	e.systemLine(ch, line)
	e.save()
}

// SwitchGroup accepts a re-created group's offer: the old chat's history
// moves to the new group, and the old group is dropped from this device
// (§11.11).
func (e *Engine) SwitchGroup(ctx context.Context, chat string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.st.Chats[chat]
	if !ok || ch.Replaces == "" {
		return errors.New("chat: this chat doesn't re-create another")
	}
	old, ok := e.st.Chats[ch.Replaces]
	if !ok {
		ch.Replaces = ""
		return e.save()
	}
	e.switchGroup(old, ch)
	return nil
}

// switchGroup moves a chat from an old group to the new one that
// replaces it: the history first, then the new group's messages. This
// device drops the old group and sends nothing there again; it stays
// registered with the homeserver, whose deliveries for it are dropped.
// The caller holds e.mu.
func (e *Engine) switchGroup(old, ch *Chat) {
	if oldMsgs, err := e.messages(old.ID); err == nil && len(oldMsgs) > 0 {
		msgs, _ := e.messages(ch.ID)
		if err := saveMessages(e.profile(), recordID(ch.ID), append(oldMsgs, msgs...)); err == nil {
			e.profile().Delete(messagesRecord(recordID(old.ID)))
		}
	}
	e.moveHistory(old.ID, ch.ID)
	if !ch.Picture && old.Picture {
		if p, ok := e.groupPicture(old.ID); ok {
			e.keepGroupPicture(ch, &p)
		}
	}
	ch.Replaces = ""
	ch.LastActivity = max(ch.LastActivity, old.LastActivity)
	delete(e.st.Chats, old.ID)
	e.dev.mls.Group(mustB64(old.ID)).Delete()
	e.dev.Save()
	e.save()
	e.emit(Event{Kind: EventChatRemoved, ChatID: old.ID})
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
}
