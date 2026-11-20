package chat

import (
	"context"
	"maps"
	"slices"
)

// Following a community that moves (§16.6, "Moving a community"): the
// owner appends a set-home, and hands each group's hub to the new
// community server. A member keeps reading the old server until the
// members group's hub is the new one, then signs in there, registers its
// groups, the members group first, and carries on there.

// communityMoved notes a set-home this device saw. The caller holds e.mu.
func (e *Engine) communityMoved(cm *Community, to string) {
	cm.MovingTo = to
	e.save()
	if ch := e.firstChannel(cm); ch != nil {
		e.systemLine(ch, "The community is moving to "+to)
	}
}

// followMove switches a community to the server its set-home names, once
// the members group's hub is that server. The caller holds e.mu.
func (e *Engine) followMove(ctx context.Context, cm *Community) {
	to := cm.MovingTo
	if to == "" || to == cm.Server {
		return
	}
	g := cm.Groups[cm.MembersGroup]
	if g == nil {
		return
	}
	st, err := e.dev.mls.Group(mustB64(g.ID)).State()
	if err != nil {
		return
	}
	data, _ := st.Extension(ExtGroup)
	ext, err := ParseGroupExt(data)
	if err != nil || ext.Hub != to {
		return // the hub hasn't been handed over yet
	}
	// The other groups' handoff commits are on the old server only: apply
	// what it holds before switching (§11.11).
	for _, id := range slices.Sorted(maps.Keys(cm.Groups)) {
		if id != cm.MembersGroup {
			e.cfetchMissed(ctx, cm, cm.Groups[id])
			if err := e.cdrain(ctx, cm, cm.Groups[id]); err != nil {
				e.log.Info("chat: a restricted group's commits before the move", "err", err)
			}
		}
	}
	conn := e.c.Community(to)
	if err := conn.SignIn(ctx); err != nil {
		e.log.Info("chat: signing in to the community's new server", "err", err)
		return
	}
	// The members group first: registering for it makes this identity a
	// member there (community server API, Receive a group's traffic).
	ids := []string{cm.MembersGroup}
	for _, id := range slices.Sorted(maps.Keys(cm.Groups)) {
		if id != cm.MembersGroup {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if err := conn.RegisterGroup(ctx, mustB64(id)); err != nil {
			if id == cm.MembersGroup {
				e.log.Info("chat: registering with the community's new server", "err", err)
				return
			}
			e.log.Info("chat: registering a restricted group with the new server", "err", err)
		}
	}
	old := cm.Server
	e.stopCommunityStream(old)
	delete(e.st.Communities, old)
	cm.Server, cm.MovingTo, cm.Cursor, cm.Stopped = to, "", "", ""
	e.st.Communities[to] = cm
	if c := e.cchains[old]; c != nil {
		e.cchains[to] = c
		delete(e.cchains, old)
	}
	for id, ch := range e.st.Chats {
		if ch.Kind != KindChannel || ch.Community != old {
			continue
		}
		nid := chanID(to, ch.Channel)
		if msgs, err := e.messages(id); err == nil && len(msgs) > 0 {
			if err := saveMessages(e.profile(), recordID(nid), msgs); err == nil {
				e.profile().Delete(messagesRecord(recordID(id)))
			}
		}
		delete(e.st.Chats, id)
		ch.ID, ch.Community, ch.Hub = nid, to, to
		e.st.Chats[nid] = ch
		e.emit(Event{Kind: EventChatRemoved, ChatID: id})
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	}
	e.save()
	if err := e.topUpCommunity(ctx, cm); err != nil {
		e.log.Info("chat: key packages on the community's new server", "err", err)
	}
	e.refreshChannels(cm)
	if ch := e.firstChannel(cm); ch != nil {
		e.systemLine(ch, "The community moved to "+to)
	}
	go e.startCommunity(e.runCtx, to)
}
