package chat

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Extension types communities add, in MLS's private-use range. Both are
// provisional (§16.2, §16.5).
const (
	ExtCommunityHead  uint16 = 0xF0A3 // community_head, a GroupContext extension
	ExtRestrictedHead uint16 = 0xF0A4 // restricted_head, a capability only
	ExtMembersRules   uint16 = 0xF0A5 // members_rules, a capability only (§16.2)
)

// requires reports whether a group's extensions require a capability in
// required_capabilities.
func requires(exts []mls.Extension, t uint16) bool {
	for _, x := range exts {
		if x.Type == mls.ExtRequiredCapabilities {
			return slices.Contains(mls.RequiredExtensions(x.Data), t)
		}
	}
	return false
}

// KindChannel is the chat kind of a community text or voice channel, as
// this device lists it beside direct chats and group chats.
const KindChannel = "channel"

// communityConfig is the OpenMLS configuration of every community group:
// no ratchet tree in Welcomes and GroupInfos, which the community server
// serves instead; PublicMessages accepted, for code joins; and 5 past
// epochs kept (§11.1, §16.5).
var communityConfig = mls.Config{RatchetTreeInWelcome: false, AcceptPublicMessages: true, MaxPastEpochs: 5}

// Community is a community this identity is in (§16).
type Community struct {
	Server string     `json:"server"`
	ID     wire.Bytes `json:"id"`
	// MembersGroup is the members group's ID (§16.5).
	MembersGroup string `json:"members_group"`
	// Groups are the community's groups this device is in, by ID: the
	// members group, and restricted channels' groups.
	Groups map[string]*CGroup `json:"groups"`
	// Cursor is the last community inbox item this device acked.
	Cursor string `json:"cursor,omitempty"`
	// MetaKeys are the community metadata keys, newest first, and
	// ChannelKeys each restricted channel's (§16.2.4).
	MetaKeys    []wire.Bytes            `json:"meta_keys,omitempty"`
	ChannelKeys map[uint64][]wire.Bytes `json:"channel_keys,omitempty"`
	// HistoryKeys are each text channel's history keys, newest first
	// (§16.5).
	HistoryKeys map[uint64][]wire.Bytes `json:"history_keys,omitempty"`
	// Uses counts the joins this device has seen with each code, by the
	// code's hash in hex (§16.6, "Use limits").
	Uses map[string]uint64 `json:"uses,omitempty"`
	// KeysDue lists newcomers waiting for keys, with when this device
	// sends them unless someone else does first (§16.6).
	KeysDue map[string]int64 `json:"keys_due,omitempty"`
	// Asked records when this device last asked for each channel's
	// history, and Answered when it last answered a request (§16.5).
	Asked    map[uint64]int64 `json:"asked,omitempty"`
	Answered map[uint64]int64 `json:"answered,omitempty"`
	// Stopped is why this device stopped sending to the community, such as
	// a commit it refused, or being removed (§16.2, §7.1).
	Stopped string `json:"stopped,omitempty"`
	// Left is set once this identity left or was removed.
	Left bool `json:"left,omitempty"`
	// Asking is set while this identity waits for a moderator to approve
	// its request to join (§16.6).
	Asking bool `json:"asking,omitempty"`
	// MovingTo is the community server a set-home names, until this
	// device follows the community there (§16.6).
	MovingTo string `json:"moving_to,omitempty"`
	// Linking is set on a newly linked device until another device of
	// this identity adds it to the members group (§7.2 step 5).
	Linking bool `json:"linking,omitempty"`
	// MembersRulesKPs is set once this device's key packages at the
	// community server list 0xF0A5 (§16.2).
	MembersRulesKPs bool `json:"members_rules_kps,omitempty"`
	// Warned keeps warnings this device has shown once.
	Warned map[string]bool `json:"warned,omitempty"`
}

// CGroup is one of a community's MLS groups.
type CGroup struct {
	ID string `json:"id"`
	// See is the see list of the restricted channels it carries, or nil
	// for the members group.
	See     []uint64              `json:"see,omitempty"`
	Epoch   uint64                `json:"epoch"`
	Members []string              `json:"members"`
	Applied map[uint64]wire.Bytes `json:"applied,omitempty"`
	// Buffered holds deliveries for a later epoch (§11.6).
	Buffered []buffered `json:"buffered,omitempty"`
	// Pending is set while a commit waits for chain entries this device
	// can't get yet (§16.5, "Judging with what it needs").
	Pending bool `json:"pending,omitempty"`
}

// chanID names a channel as a chat: the community server, then the
// channel ID.
func chanID(server string, channel uint64) string {
	return "c:" + server + ":" + strconv.FormatUint(channel, 10)
}

// parseChanID reads a channel's chat ID.
func parseChanID(id string) (string, uint64, bool) {
	rest, ok := strings.CutPrefix(id, "c:")
	if !ok {
		return "", 0, false
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(rest[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return rest[:i], n, true
}

// Records for communities.
const (
	recCommunityChain = "cchain." // followed by the community ID in hex
	recChannelLog     = "clog."   // followed by the community ID in hex, ".", and the channel
)

func chainRecord(id []byte) string { return recCommunityChain + hex.EncodeToString(id) }

func logRecord(id []byte, channel uint64) string {
	return recChannelLog + hex.EncodeToString(id) + "." + strconv.FormatUint(channel, 10)
}

// logEntry is one channel message as received, kept to build history
// segments (§16.5): the signed app-message and the msg-key that signed
// it, or a deleted message's place.
type logEntry struct {
	Seq    uint64       `json:"seq"`
	Msg    *wire.Signed `json:"msg,omitempty"`
	MsgKey *wire.Signed `json:"mk,omitempty"`
	TS     int64        `json:"ts"`
	Gone   uint64       `json:"gone,omitempty"`
}

func (e *Engine) loadLog(cm *Community, channel uint64) []logEntry {
	var l []logEntry
	if err := e.profile().Load(logRecord(cm.ID, channel), &l); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: a channel's log", "err", err)
	}
	return l
}

func (e *Engine) saveLog(cm *Community, channel uint64, l []logEntry) error {
	slices.SortFunc(l, func(a, b logEntry) int { return int(a.Seq) - int(b.Seq) })
	if len(l) > maxHistory {
		l = l[len(l)-maxHistory:]
	}
	return e.profile().Save(logRecord(cm.ID, channel), l)
}

// communityChain returns a community's chain, checked, from this device's
// copy.
func (e *Engine) communityChain(cm *Community) (*community.Chain, error) {
	if c := e.cchains[cm.Server]; c != nil {
		return c, nil
	}
	var entries []wire.Signed
	if err := e.profile().Load(chainRecord(cm.ID), &entries); err != nil {
		return nil, err
	}
	c, err := community.Validate(entries, e.chainChecker(context.Background(), cm))
	if err != nil {
		return nil, err
	}
	e.cchains[cm.Server] = c
	return c, nil
}

// chainChecker checks community chain entries' signers: a key an
// add-device in the signer's chain added at some point (§16.2.2). It
// takes the identity chain from this device's copy, the homeserver, or
// the community server's copy (GET members/{id}/chain).
func (e *Engine) chainChecker(ctx context.Context, cm *Community) community.Checker {
	return community.Checker{
		Now: e.c.Clock().Now().Unix(),
		DeviceKey: func(id string, key []byte) ([]byte, error) {
			c, err := e.identityChain(ctx, cm, id, false)
			if err != nil {
				return nil, community.ErrNoChain
			}
			if pub, ok := devicePub(c, key, true); ok {
				return pub, nil
			}
			c, err = e.identityChain(ctx, cm, id, true)
			if err != nil {
				return nil, community.ErrNoChain
			}
			if pub, ok := devicePub(c, key, true); ok {
				return pub, nil
			}
			return nil, fmt.Errorf("chat: %s's chain never added the key that signed the entry", id)
		},
	}
}

// identityChain gets an identity's chain for community checks: this
// device's copy, then the community server's copy, which serves every
// member and former member, then the homeserver (§16.1). fresh skips the
// copy.
func (e *Engine) identityChain(ctx context.Context, cm *Community, id string, fresh bool) (*identity.Chain, error) {
	if id == e.c.ID() {
		return e.c.Chain(), nil
	}
	if !fresh {
		if c, err := e.c.CachedChain(id); err == nil && c != nil {
			return c, nil
		}
	}
	if cm != nil {
		if entries, err := e.c.Community(cm.Server).MemberChain(ctx, id, e.joinCode[cm.Server]); err == nil {
			if c, err := e.c.AcceptChain(id, entries); err == nil {
				e.st.Handles[id] = c.State.Handle
				return c, nil
			}
		}
	}
	return e.fetchChain(ctx, id, fresh)
}

// syncChain fetches what's new in a community's chain, checks it, and
// keeps it. It reports whether anything changed.
func (e *Engine) syncCommunityChain(ctx context.Context, cm *Community, code []byte) (bool, error) {
	c, _ := e.communityChain(cm)
	var after *uint64
	if c != nil {
		n := uint64(c.Len() - 1)
		after = &n
	}
	page, err := e.c.Community(cm.Server).Chain(ctx, after, code)
	if err != nil {
		return false, retryable{err}
	}
	if len(page.Entries) == 0 {
		return false, nil
	}
	ck := e.chainChecker(ctx, cm)
	if c == nil {
		c, err = community.Validate(page.Entries, ck)
		if err != nil {
			return false, err
		}
		if len(cm.ID) > 0 && !bytes.Equal(c.ID(), cm.ID) {
			return false, errors.New("chat: the community server served another community's chain")
		}
		cm.ID = c.ID()
	} else {
		for _, s := range page.Entries {
			if err := c.Append(s, ck); err != nil {
				// A refused entry: keep the state before it, and accept
				// nothing built on it (§16.2.2).
				e.log.Info("chat: refused a community chain entry", "err", err)
				break
			}
		}
	}
	e.cchains[cm.Server] = c
	if err := e.profile().Save(chainRecord(cm.ID), c.Entries); err != nil {
		return false, err
	}
	e.refreshChannels(cm)
	if to := c.State().Server; to != "" && to != cm.Server && cm.MovingTo != to {
		e.communityMoved(cm, to)
		e.followMove(ctx, cm)
	}
	return true, nil
}

// refreshChannels updates the chat list's channels from the chain, with
// the names this device can open (§16.2.4).
func (e *Engine) refreshChannels(cm *Community) {
	c, err := e.communityChain(cm)
	if err != nil {
		return
	}
	st := c.State()
	me := e.c.ID()
	seen := map[string]bool{}
	for i, ch := range st.OrderedChannels() {
		id := chanID(cm.Server, ch.ID)
		if !st.SeesChannel(me, ch) {
			continue
		}
		seen[id] = true
		chat := e.st.Chats[id]
		if chat == nil {
			chat = &Chat{ID: id, Kind: KindChannel, Hub: cm.Server, LastActivity: e.c.Clock().Now().Unix()}
			e.st.Chats[id] = chat
		}
		keys := e.channelMetaKeys(cm, ch)
		name, _ := community.Open(keys, ch.Name, cm.ID, ch.NameSeq, community.WhatChannel, ch.ID, community.FieldName)
		if community.CheckName(name) != nil {
			name = ""
		}
		topic, _ := community.Open(keys, ch.Topic, cm.ID, ch.TopicSeq, community.WhatChannel, ch.ID, community.FieldTopic)
		changed := chat.Name != name || chat.Topic != topic || chat.Order != i || chat.Voice != (ch.Kind == "voice")
		chat.Name, chat.Topic, chat.Order, chat.Voice = name, topic, i, ch.Kind == "voice"
		chat.Community, chat.CommunityName, chat.Channel = cm.Server, st.Name, ch.ID
		chat.Members = nil
		if g := e.groupFor(cm, ch); g != nil {
			chat.Members = slices.Clone(g.Members)
		}
		if cm.Stopped != "" {
			chat.Stopped = cm.Stopped
		} else {
			chat.Stopped = ""
		}
		if changed {
			e.emit(Event{Kind: EventChat, Chat: ptr(chat.public())})
		}
	}
	for id, chat := range e.st.Chats {
		if chat.Kind == KindChannel && chat.Community == cm.Server && !seen[id] {
			delete(e.st.Chats, id)
			e.emit(Event{Kind: EventChatRemoved, ChatID: id})
		}
	}
	e.save()
}

// channelMetaKeys returns the metadata keys a channel's name may be
// sealed under: the community's, or a restricted channel's own.
func (e *Engine) channelMetaKeys(cm *Community, ch *community.Channel) [][]byte {
	var out [][]byte
	if ch.Restricted() {
		for _, k := range cm.ChannelKeys[ch.ID] {
			out = append(out, k)
		}
		// Keys from community-keys are kept by the group that carries the
		// channel (§16.2.4).
		if g := e.groupFor(cm, ch); g != nil {
			for _, k := range cm.ChannelKeys[groupKeyID(g.ID)] {
				out = append(out, k)
			}
		}
	}
	for _, k := range cm.MetaKeys {
		out = append(out, k)
	}
	return out
}

// groupFor returns the group this device holds that carries a channel.
func (e *Engine) groupFor(cm *Community, ch *community.Channel) *CGroup {
	if !ch.Restricted() {
		return cm.Groups[cm.MembersGroup]
	}
	for _, g := range cm.Groups {
		if g.See != nil && slices.Equal(g.See, ch.See) {
			return g
		}
	}
	return nil
}

// RoleName returns a role's name, if this device can open it.
func (e *Engine) roleName(cm *Community, st *community.State, id uint64) string {
	r := st.Roles[id]
	if r == nil {
		return ""
	}
	keys := e.metaKeys(cm)
	n, _ := community.Open(keys, r.Name, cm.ID, r.NameSeq, community.WhatRole, id, community.FieldName)
	return n
}

func (e *Engine) metaKeys(cm *Community) [][]byte {
	out := make([][]byte, len(cm.MetaKeys))
	for i, k := range cm.MetaKeys {
		out[i] = k
	}
	return out
}

// runCommunities keeps a live stream open to every community server this
// identity is in, and starts one for each community it joins.
func (e *Engine) runCommunities(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	servers := slices.Collect(maps.Keys(e.st.Communities))
	e.mu.Unlock()
	for _, s := range servers {
		e.startCommunity(ctx, s)
	}
}

// startCommunity starts a community's stream, once. The caller doesn't
// hold e.mu.
func (e *Engine) startCommunity(ctx context.Context, server string) {
	e.crunMu.Lock()
	defer e.crunMu.Unlock()
	if ctx == nil || e.cruns[server] != nil {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	e.cruns[server] = cancel
	go e.streamCommunity(sctx, server)
}

// stopCommunityStream ends a community server's stream, such as the old
// one's after a move.
func (e *Engine) stopCommunityStream(server string) {
	e.crunMu.Lock()
	defer e.crunMu.Unlock()
	if cancel := e.cruns[server]; cancel != nil {
		cancel()
		delete(e.cruns, server)
	}
}

func (e *Engine) streamCommunity(ctx context.Context, server string) {
	e.mu.Lock()
	cm := e.st.Communities[server]
	if cm == nil || cm.Left {
		e.mu.Unlock()
		return
	}
	conn := e.c.Community(server)
	conn.SetCursor(cm.Cursor)
	e.mu.Unlock()
	events := conn.Stream(ctx, client.StreamOptions{})
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastRead := time.Time{}
	for {
		select {
		case <-tick.C:
			e.mu.Lock()
			if cm := e.st.Communities[server]; cm != nil {
				e.runDue(ctx, cm, false)
				if time.Since(lastRead) > 30*time.Second {
					lastRead = time.Now()
					e.pullReadPositions(ctx, cm)
				}
			}
			e.mu.Unlock()
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case client.EventConnected:
				e.voice.connected(server, ev.Send)
				go func() {
					e.mu.Lock()
					defer e.mu.Unlock()
					if cm := e.st.Communities[server]; cm != nil {
						e.catchUpCommunity(ctx, cm)
					}
				}()
			case client.EventItem:
				e.handleCommunity(ctx, server, ev.Item)
			case client.EventVoice:
				e.voiceFrame(server, ev.Frame)
				e.voice.frame(server, ev.Frame)
			case client.EventError:
				e.voice.frame(server, ev.Frame)
			case client.EventDisconnected:
				e.voice.disconnected(server)
			case client.EventStopped:
				e.log.Info("chat: a community stream stopped", "server", server, "err", ev.Err)
				return
			}
		}
	}
}

// syncCommunities fetches and processes every community's waiting inbox
// items, for Sync.
func (e *Engine) syncCommunities(ctx context.Context) error {
	e.mu.Lock()
	servers := slices.Sorted(maps.Keys(e.st.Communities))
	e.mu.Unlock()
	for _, server := range servers {
		e.mu.Lock()
		cm := e.st.Communities[server]
		if cm == nil || cm.Left {
			e.mu.Unlock()
			continue
		}
		cursor := cm.Cursor
		e.mu.Unlock()
		conn := e.c.Community(server)
		for range 100 {
			page, err := conn.Inbox(ctx, cursor, 0)
			if err != nil {
				return err
			}
			for i := range page.Items {
				e.handleCommunity(ctx, server, &page.Items[i])
				if page.Items[i].Cursor != "" {
					cursor = page.Items[i].Cursor
				}
			}
			if page.Next == "" || len(page.Items) == 0 {
				break
			}
		}
		e.mu.Lock()
		if cm := e.st.Communities[server]; cm != nil {
			e.runDue(ctx, cm, true)
		}
		e.mu.Unlock()
	}
	return nil
}

// handleCommunity processes one community inbox item and acks it.
func (e *Engine) handleCommunity(ctx context.Context, server string, it *client.Item) {
	if it.Delivery != nil && it.Delivery.ID != "" && e.seen["c:"+it.Delivery.ID] {
		e.ackCommunity(ctx, server, it)
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		e.mu.Lock()
		cm := e.st.Communities[server]
		var err error
		if cm != nil {
			err = e.processCommunity(ctx, cm, it)
		}
		e.mu.Unlock()
		var r retryable
		if err == nil || !errors.As(err, &r) {
			if err != nil {
				e.log.Info("chat: dropped a community item", "server", server, "err", err)
			}
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second << attempt):
		}
	}
	if it.Delivery != nil && it.Delivery.ID != "" {
		e.seen["c:"+it.Delivery.ID] = true
	}
	e.ackCommunity(ctx, server, it)
}

func (e *Engine) ackCommunity(ctx context.Context, server string, it *client.Item) {
	if it.Cursor == "" {
		return
	}
	if err := e.c.Community(server).Ack(ctx, it.Cursor); err != nil {
		e.log.Debug("chat: a community ack", "err", err)
		return
	}
	e.mu.Lock()
	if cm := e.st.Communities[server]; cm != nil {
		cm.Cursor = it.Cursor
		e.save()
	}
	e.mu.Unlock()
}

// processCommunity handles one item. The caller holds e.mu.
func (e *Engine) processCommunity(ctx context.Context, cm *Community, it *client.Item) error {
	if n := it.Notice; n != nil {
		switch n.T {
		case "community-chain":
			_, err := e.syncCommunityChain(ctx, cm, nil)
			return err
		case "removed":
			e.removedFromCommunity(ctx, cm, "You were removed from the community")
		case "join-declined":
			e.declinedJoin(cm)
		case "join-request":
			e.joinRequested(ctx, cm, n)
		}
		return nil
	}
	d := it.Delivery
	if d.Kind != "mls" {
		return nil
	}
	gid, err := wire.DecodeB64(d.Group)
	if err != nil {
		return err
	}
	if d.MLS == "welcome" {
		return e.communityWelcome(ctx, cm, d)
	}
	g := cm.Groups[d.Group]
	if g == nil {
		return fmt.Errorf("chat: a community message for a group this device isn't in")
	}
	if d.Epoch == nil {
		return errors.New("chat: an mls delivery without an epoch")
	}
	b := buffered{Epoch: *d.Epoch, MLS: d.MLS, From: d.From, Data: d.Data, Receipt: d.Receipt, Received: it.TS, Channel: d.Channel, Seq: d.Seq, Proof: d.Proof}
	_ = gid
	if err := e.cgroupItem(ctx, cm, g, b); err != nil {
		return err
	}
	err = e.cdrain(ctx, cm, g)
	if cm.MovingTo != "" && d.MLS == "commit" {
		e.followMove(ctx, cm)
	}
	return err
}

// removedFromCommunity stops following a community this identity was
// removed from. Its channels' history stays.
func (e *Engine) removedFromCommunity(ctx context.Context, cm *Community, why string) {
	for id := range cm.Groups {
		e.dev.mls.Group(mustB64(id)).Delete()
	}
	e.dev.Save()
	cm.Left, cm.Stopped, cm.Groups = true, why, map[string]*CGroup{}
	for _, ch := range e.st.Chats {
		if ch.Kind == KindChannel && ch.Community == cm.Server {
			ch.Stopped = why
			e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
		}
	}
	e.save()
}

// catchUpCommunity fetches what changed while this device wasn't
// connected: the chain, and each group's missed commits (§11.6).
func (e *Engine) catchUpCommunity(ctx context.Context, cm *Community) {
	if cm.Left {
		return
	}
	if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
		e.log.Info("chat: a community's chain", "err", err)
	}
	for _, id := range slices.Sorted(maps.Keys(cm.Groups)) {
		g := cm.Groups[id]
		e.cfetchMissed(ctx, cm, g)
		if err := e.cdrain(ctx, cm, g); err != nil {
			e.log.Info("chat: catching up a community group", "err", err)
		}
	}
	e.pullReadPositions(ctx, cm)
}

// communityLeaf is this device's leaf node in community groups: the chat
// leaf, plus community_head and restricted_head in its capabilities
// (§16.2, §16.5).
func (d *Device) communityLeaf() mls.Leaf {
	l := d.Leaf()
	l.Capabilities.Extensions = append(l.Capabilities.Extensions, ExtCommunityHead, ExtRestrictedHead, ExtMembersRules)
	return l
}

// communityMaker makes key packages for a community server's pool, with
// the community leaf (§16.1, §16.5).
type communityMaker struct{ d *Device }

func (m communityMaker) KeyPackages(n int, lastResort bool) ([][]byte, error) {
	d := m.d
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, 0, n)
	for range n {
		kp, err := d.mls.NewKeyPackage(d.signer, mls.KeyPackageOptions{Leaf: d.communityLeaf(), LastResort: lastResort, Lifetime: keyPackageLifetime})
		if err != nil {
			return nil, err
		}
		out = append(out, kp.Raw)
	}
	if err := d.Save(); err != nil {
		return nil, err
	}
	return out, nil
}

// communityKeyPackages keeps about 10 key packages, and a last-resort
// one, in a community server's pool (community server API).
const communityKeyPackages = 10

func (e *Engine) topUpCommunity(ctx context.Context, cm *Community) error {
	conn := e.c.Community(cm.Server)
	n, lastResort, err := conn.KeyPackageCount(ctx)
	if err != nil {
		return err
	}
	if !cm.MembersRulesKPs {
		// Key packages from before members_rules don't list 0xF0A5:
		// replace them all once (§16.2).
		if _, err := conn.UploadKeyPackages(ctx, communityMaker{e.dev}, communityKeyPackages, client.UploadOptions{LastResort: true, Replace: true}); err != nil {
			return err
		}
		cm.MembersRulesKPs = true
		return e.save()
	}
	if n >= communityKeyPackages/2 && lastResort {
		return nil
	}
	_, err = conn.UploadKeyPackages(ctx, communityMaker{e.dev}, communityKeyPackages-n, client.UploadOptions{LastResort: !lastResort})
	return err
}
