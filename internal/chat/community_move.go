package chat

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
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

// TypeCommunityMembers is the type tag of the owner's member list for a
// move (§16.6, "The member list").
const TypeCommunityMembers = "community-members"

// MoveCommunity moves a community this identity owns to another community
// server (§16.6, "Moving a community"). It asks the new server first,
// with the chain ending in the set-home it's about to append and the
// signed member list, and gets a reservation; appends the set-home on the
// old server with it, which answers with each text channel's last
// number; moves the community in with those; then hands each group's hub
// over, the members group first, and follows the move itself. code is the
// new server's creation code, if it needs one. A refusal before the
// append changes nothing.
func (e *Engine) MoveCommunity(ctx context.Context, server, to, code string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, c, err := e.communityOf(server)
	if err != nil {
		return err
	}
	to = strings.ToLower(strings.TrimSpace(to))
	me := e.c.ID()
	st := c.State()
	switch {
	case st.Owner != me:
		return errors.New("chat: only the owner can move a community")
	case to == "" || to == server:
		return errors.New("chat: name another community server")
	case cm.MovingTo != "":
		return fmt.Errorf("chat: the community is moving to %s already", cm.MovingTo)
	}
	g := cm.Groups[cm.MembersGroup]
	if g == nil {
		return errors.New("chat: this device isn't in the members group")
	}
	mst, err := e.dev.mls.Group(mustB64(g.ID)).State()
	if err != nil {
		return err
	}

	// The set-home, not appended yet.
	body := map[string]any{"community": wire.Bytes(cm.ID), "seq": uint64(c.Len()), "prev": wire.Bytes(c.Head()),
		"ts": max(e.c.Clock().Now().Unix(), st.TS), "by": me, "ops": []map[string]any{{"op": community.OpSetHome, "server": to}}}
	entry, err := e.c.SignAsDevice(community.TypeEntry, body)
	if err != nil {
		return err
	}
	m := client.Move{Chain: append(slices.Clone(c.Entries), entry), Homes: map[string]string{}, Code: code}
	// Where each signer's chain lives, for the new server to check the
	// entries (community server API, "Moving a community in").
	for _, s := range m.Chain {
		en, err := community.ParseEntry(s)
		if err != nil || m.Homes[en.By] != "" {
			continue
		}
		if ic, err := e.identityChain(ctx, cm, en.By, false); err == nil && ic.State.Home != "" {
			m.Homes[en.By] = ic.State.Home
		}
	}
	ids := slices.Sorted(maps.Keys(leavesByIdentity(mst)))
	m.Members, err = e.c.SignAsDevice(TypeCommunityMembers, map[string]any{"community": wire.Bytes(cm.ID), "members": ids, "ts": e.c.Clock().Now().Unix()})
	if err != nil {
		return err
	}

	// Ask the new server first; then the set-home with its reservation.
	newConn := e.c.Community(to)
	if err := newConn.SignIn(ctx); err != nil {
		return fmt.Errorf("chat: signing in to %s: %w", to, err)
	}
	res, err := newConn.CheckMove(ctx, m)
	if err != nil {
		return fmt.Errorf("chat: %s won't take the community: %w", to, err)
	}
	ar, err := e.c.Community(server).AppendSetHome(ctx, entry, res)
	if err != nil {
		return fmt.Errorf("chat: %s refused the move: %w", server, err)
	}
	m.Seqs = ar.Seqs
	if err := newConn.MoveIn(ctx, m); err != nil {
		return fmt.Errorf("chat: moving the community in to %s: %w", to, err)
	}
	if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
		return err
	}
	if cm.MovingTo == "" {
		e.communityMoved(cm, to)
	}

	// Hand each group's hub over: the members group first, so members
	// follow (§11.11).
	gids := []string{cm.MembersGroup}
	for _, id := range slices.Sorted(maps.Keys(cm.Groups)) {
		if id != cm.MembersGroup {
			gids = append(gids, id)
		}
	}
	for _, id := range gids {
		cg := cm.Groups[id]
		gst, err := e.dev.mls.Group(mustB64(id)).State()
		if err != nil {
			return err
		}
		data, _ := gst.Extension(ExtGroup)
		ext, err := ParseGroupExt(data)
		if err != nil {
			return err
		}
		if ext.Hub == to {
			continue
		}
		next := ext.Clone()
		next.Hub = to
		o := ccommitOpts{ext: next, handoff: to, pending: e.hasPending(cg)}
		if id == cm.MembersGroup {
			// The handoff also binds the chain's new head, the set-home
			// (§16.2).
			if c, err := e.communityChain(cm); err == nil {
				o.head = c.Head()
			}
		}
		if err := e.ccommit(ctx, cm, cg, o); err != nil {
			return fmt.Errorf("chat: handing a group's hub to %s: %w", to, err)
		}
	}
	e.followMove(ctx, cm)
	return nil
}
