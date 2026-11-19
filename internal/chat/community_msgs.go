package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/codes"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// CommunityKeys passes the metadata and history keys one group holds
// (§16.2.4).
type CommunityKeys struct {
	T        string               `json:"t"`
	TS       int64                `json:"ts"`
	Metadata []wire.Bytes         `json:"metadata"`
	History  []ChannelHistoryKeys `json:"history,omitempty"`
}

// ChannelHistoryKeys are one channel's history keys, newest first.
type ChannelHistoryKeys struct {
	Channel uint64       `json:"channel"`
	Keys    []wire.Bytes `json:"keys"`
}

// HistoryRequest asks members to upload a channel's missing history
// (§16.5).
type HistoryRequest struct {
	T       string  `json:"t"`
	TS      int64   `json:"ts"`
	Channel uint64  `json:"channel"`
	Before  *uint64 `json:"before,omitempty"`
}

// community returns a community by its server, and its chain.
func (e *Engine) communityOf(server string) (*Community, *community.Chain, error) {
	cm := e.st.Communities[server]
	if cm == nil || cm.Left {
		return nil, nil, errors.New("chat: you aren't in that community")
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return nil, nil, err
	}
	return cm, c, nil
}

// opBuilder builds an entry's ops for the seq it will have, since sealed
// names include it (§16.2.4).
type opBuilder func(st *community.State, seq uint64) ([]map[string]any, error)

// appendEntry signs and appends a community chain entry, rebuilding it on
// a new head after 409 stale-head (§16.2), then binds the new head in the
// members group. The caller holds e.mu.
func (e *Engine) appendEntry(ctx context.Context, cm *Community, build opBuilder) error {
	if cm.Stopped != "" {
		return fmt.Errorf("chat: this community is stopped: %s", cm.Stopped)
	}
	conn := e.c.Community(cm.Server)
	for attempt := 0; ; attempt++ {
		c, err := e.communityChain(cm)
		if err != nil {
			return err
		}
		st := c.State()
		seq := uint64(c.Len())
		ops, err := build(st, seq)
		if err != nil {
			return err
		}
		body := map[string]any{"community": wire.Bytes(cm.ID), "seq": seq, "prev": wire.Bytes(c.Head()),
			"ts": max(e.c.Clock().Now().Unix(), st.TS), "by": e.c.ID(), "ops": ops}
		s, err := e.c.SignAsDevice(community.TypeEntry, body)
		if err != nil {
			return err
		}
		_, err = conn.Append(ctx, s)
		if wire.IsCode(err, wire.CodeStaleHead) && attempt < 3 {
			if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
			return err
		}
		if err := e.bindHead(ctx, cm); err != nil {
			e.log.Info("chat: binding the community chain's head", "err", err)
		}
		return nil
	}
}

// ChannelOptions describe a new channel.
type ChannelOptions struct {
	Topic string
	// Voice makes a voice channel; otherwise it's a text channel that
	// keeps history.
	Voice bool
	// See restricts who sees it to these roles; nil means everyone.
	See []uint64
	// NoHistory turns off history for newcomers in a text channel.
	NoHistory bool
}

// AddChannel adds a channel at the end of a community's order (§16.2.3).
// A restricted channel gets the group for its see list if none carries it
// yet (§16.5). It returns the channel's chat ID.
func (e *Engine) AddChannel(ctx context.Context, server, name, topic string, o ChannelOptions) (string, error) {
	name = strings.TrimSpace(name)
	if err := community.CheckName(name); err != nil {
		return "", fmt.Errorf("chat: a channel's name: %w", err)
	}
	if o.Topic == "" {
		o.Topic = topic
	}
	if err := community.CheckTopic(o.Topic); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, c, err := e.communityOf(server)
	if err != nil {
		return "", err
	}
	if !c.State().Can(e.c.ID(), community.PermAdministrate) {
		return "", errors.New("chat: adding channels needs administrate")
	}
	see := []uint64{0}
	if len(o.See) > 0 {
		see = uniq64(o.See)
	}
	// A restricted channel's names go under its group's metadata key.
	var gkey []byte
	var existing *CGroup
	if see[0] != 0 {
		for _, g := range cm.Groups {
			if g.See != nil && slices.Equal(g.See, see) {
				existing = g
			}
		}
		if existing != nil && len(cm.ChannelKeys[groupKeyID(existing.ID)]) > 0 {
			gkey = cm.ChannelKeys[groupKeyID(existing.ID)][0]
		} else {
			gkey = randKey()
		}
	}
	var id uint64
	err = e.appendEntry(ctx, cm, func(st *community.State, seq uint64) ([]map[string]any, error) {
		id = st.LastChannel + 1
		key := []byte(cm.MetaKeys[0])
		if gkey != nil {
			key = gkey
		}
		n, err := community.Seal(key, cm.ID, seq, community.WhatChannel, id, community.FieldName, name)
		if err != nil {
			return nil, err
		}
		op := map[string]any{"op": community.OpAddChannel, "id": id, "name": n, "see": see, "write": see}
		if o.Topic != "" {
			t, err := community.Seal(key, cm.ID, seq, community.WhatChannel, id, community.FieldTopic, o.Topic)
			if err != nil {
				return nil, err
			}
			op["topic"] = t
		}
		if o.Voice {
			op["kind"], op["max"], op["cast"] = "voice", 25, 2
		} else {
			op["kind"], op["history"], op["slow"] = "text", !o.NoHistory, 0
		}
		return []map[string]any{op}, nil
	})
	if err != nil {
		return "", err
	}
	if !o.Voice && !o.NoHistory {
		addKey(&cm.HistoryKeys, id, randKey())
	}
	if see[0] == 0 {
		e.sendKeys(ctx, cm, cm.Groups[cm.MembersGroup])
	} else if existing != nil {
		addGroupKey(cm, existing.ID, gkey)
		e.sendKeys(ctx, cm, existing)
	} else if g, err := e.newRestrictedGroup(ctx, cm, see, gkey); err != nil {
		e.log.Info("chat: the restricted channel's group", "err", err)
	} else {
		e.sendKeys(ctx, cm, g)
	}
	e.save()
	e.refreshChannels(cm)
	return chanID(server, id), nil
}

func uniq64(a []uint64) []uint64 {
	out := slices.Clone(a)
	slices.Sort(out)
	return slices.Compact(out)
}

func randKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func addKey(m *map[uint64][]wire.Bytes, ch uint64, k []byte) {
	if *m == nil {
		*m = map[uint64][]wire.Bytes{}
	}
	if !slices.ContainsFunc((*m)[ch], func(x wire.Bytes) bool { return bytes.Equal(x, k) }) {
		(*m)[ch] = append([]wire.Bytes{k}, (*m)[ch]...)
	}
}

// groupKeyID keys a restricted group's metadata keys. They're kept in
// ChannelKeys under a key no channel uses: the group's place in a map of
// its own would need a new record field, so this hashes the ID instead.
func groupKeyID(gid string) uint64 {
	b := mustB64(gid)
	var n uint64
	for _, x := range b[:8] {
		n = n<<8 | uint64(x)
	}
	return n | 1<<63
}

func addGroupKey(cm *Community, gid string, k []byte) {
	if k != nil {
		addKey(&cm.ChannelKeys, groupKeyID(gid), k)
	}
}

// newRestrictedGroup creates the group for a see list, registers it, and
// adds every device of every member whose roles see it (§16.5).
func (e *Engine) newRestrictedGroup(ctx context.Context, cm *Community, see []uint64, key []byte) (*CGroup, error) {
	conn := e.c.Community(cm.Server)
	// An unpinned restricted group requires 0xF0A1 and 0xF0A3 from
	// creation on (§16.5).
	exts := []mls.Extension{mustExt(communityExt(cm.Server)), mls.RequiredCapabilities([]uint16{ExtGroup, ExtCommunityHead}, nil, nil)}
	mg, reg, err := e.newCommunityGroup(ctx, conn, exts)
	if err != nil {
		return nil, err
	}
	if err := conn.CreateGroup(ctx, reg, see, nil); err != nil {
		mg.Delete()
		e.dev.Save()
		return nil, err
	}
	gid := wire.EncodeB64(reg.Group)
	g := &CGroup{ID: gid, See: see, Epoch: 0, Members: []string{e.c.ID()}}
	cm.Groups[gid] = g
	addGroupKey(cm, gid, key)
	e.save()
	if err := e.fillRestricted(ctx, cm, g); err != nil {
		e.log.Info("chat: adding members to the restricted channel", "err", err)
	}
	return g, nil
}

// fillRestricted adds to a restricted channel's group every member, and
// every device of this identity, whose roles see it and who isn't in it
// yet. The first commit pins the group when every leaf lists 0xF0A4
// (§16.5, "Pinning a restricted group").
func (e *Engine) fillRestricted(ctx context.Context, cm *Community, g *CGroup) error {
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	st := c.State()
	mst, err := e.dev.mls.Group(mustB64(cm.MembersGroup)).State()
	if err != nil {
		return err
	}
	gst, err := e.dev.mls.Group(mustB64(g.ID)).State()
	if err != nil {
		return err
	}
	in := leavesByIdentity(gst)
	conn := e.c.Community(cm.Server)
	var adds [][]byte
	var to []string
	var devs []wire.Bytes
	allPin := true
	for _, m := range mst.Members {
		if !slices.Contains(m.Capabilities.Extensions, ExtRestrictedHead) {
			allPin = false
		}
	}
	for _, m := range gst.Members {
		if !slices.Contains(m.Capabilities.Extensions, ExtRestrictedHead) {
			allPin = false
		}
	}
	for id := range leavesByIdentity(mst) {
		if !st.SeesList(id, g.See) || st.Banned(id, "") {
			continue
		}
		chain, err := e.identityChain(ctx, cm, id, true)
		if err != nil {
			continue
		}
		var missing [][]byte
		for _, d := range chain.State.Devices {
			if !hasDevice(in[id], d.Key) && !(id == e.c.ID() && bytes.Equal(d.Key, e.c.DeviceKey())) {
				missing = append(missing, d.Key)
			}
		}
		if len(missing) == 0 {
			continue
		}
		kps, err := conn.ClaimKeyPackages(ctx, id, missing)
		if err != nil {
			e.log.Info("chat: claiming community key packages", "err", err)
			continue
		}
		got := false
		for _, kp := range kps {
			b, err := e.checkClaimed(kp, id, chain)
			if err != nil {
				e.log.Info("chat: refused a community key package", "err", err)
				continue
			}
			if info, err := mls.InspectKeyPackage(b); err == nil && !slices.Contains(info.Leaf.Capabilities.Extensions, ExtRestrictedHead) {
				allPin = false
			}
			adds = append(adds, b)
			devs = append(devs, kp.Dev)
			got = true
		}
		if got {
			to = append(to, id)
		}
	}
	if len(adds) == 0 {
		return nil
	}
	o := ccommitOpts{adds: adds, welcome: to, devs: devs, pending: e.hasPending(g)}
	if _, pinned := communityHead(gst.Extensions); pinned || allPin {
		o.head, o.pin = c.Head(), true
		o.required = []uint16{ExtGroup, ExtCommunityHead, ExtRestrictedHead}
	}
	if err := e.ccommit(ctx, cm, g, o); err != nil {
		return err
	}
	e.sendKeys(ctx, cm, g)
	return nil
}

// AddRole adds a role ranked just below this identity (§16.2.3, §16.3).
// color is #RRGGBB, or empty. It returns the role's ID.
func (e *Engine) AddRole(ctx context.Context, server, name, color string, perms []string) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return 0, err
	}
	var id uint64
	err = e.appendEntry(ctx, cm, func(st *community.State, seq uint64) ([]map[string]any, error) {
		me := e.c.ID()
		if !st.Can(me, community.PermAdministrate) {
			return nil, errors.New("chat: adding roles needs administrate")
		}
		level := st.Level(me)
		rank := level - 1
		for ; rank > 0; rank-- {
			taken := false
			for _, r := range st.Roles {
				taken = taken || r.Rank == rank
			}
			if !taken {
				break
			}
		}
		if rank == 0 {
			return nil, errors.New("chat: there's no rank left below yours")
		}
		id = st.LastRole + 1
		n, err := community.Seal(cm.MetaKeys[0], cm.ID, seq, community.WhatRole, id, community.FieldName, name)
		if err != nil {
			return nil, err
		}
		op := map[string]any{"op": community.OpAddRole, "id": id, "rank": rank, "perms": perms, "name": n}
		if color != "" {
			cs, err := community.Seal(cm.MetaKeys[0], cm.ID, seq, community.WhatRole, id, community.FieldColor, color)
			if err != nil {
				return nil, err
			}
			op["color"] = cs
		}
		return []map[string]any{op}, nil
	})
	return id, err
}

// SetRoles sets every role a member holds (§16.2.3). Afterwards this
// device adds them to the restricted channels' groups their roles see now,
// and removes them from those they don't (§16.5).
func (e *Engine) SetRoles(ctx context.Context, server, member string, roles []uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return err
	}
	roles = uniq64(roles)
	if err := e.appendEntry(ctx, cm, func(*community.State, uint64) ([]map[string]any, error) {
		return []map[string]any{{"op": community.OpAssignRoles, "member": member, "roles": roles}}, nil
	}); err != nil {
		return err
	}
	e.followRoles(ctx, cm, member)
	return nil
}

// followRoles brings a member's devices in step with the restricted
// groups this device is in: Adds where their roles see the channel,
// Removes where they don't (§16.5, "Losing sight of a restricted
// channel").
func (e *Engine) followRoles(ctx context.Context, cm *Community, member string) {
	c, err := e.communityChain(cm)
	if err != nil {
		return
	}
	st := c.State()
	for _, gid := range slices.Sorted(maps.Keys(cm.Groups)) {
		g := cm.Groups[gid]
		if g.See == nil {
			continue
		}
		if st.SeesList(member, g.See) {
			if err := e.fillRestricted(ctx, cm, g); err != nil {
				e.log.Info("chat: adding to a restricted channel", "err", err)
			}
			continue
		}
		gst, err := e.dev.mls.Group(mustB64(g.ID)).State()
		if err != nil {
			continue
		}
		var removes []uint32
		for _, l := range leavesByIdentity(gst)[member] {
			removes = append(removes, l.index)
		}
		if len(removes) == 0 {
			continue
		}
		if err := e.ccommit(ctx, cm, g, ccommitOpts{removes: removes, pending: e.hasPending(g)}); err != nil {
			e.log.Info("chat: removing from a restricted channel", "err", err)
			continue
		}
		// Rotate the group's keys, which the removed member had
		// (§16.2.4, §16.5).
		addGroupKey(cm, g.ID, randKey())
		for _, ch := range st.Channels {
			if slices.Equal(ch.See, g.See) && ch.Kind == "text" && ch.History {
				addKey(&cm.HistoryKeys, ch.ID, randKey())
			}
		}
		e.sendKeys(ctx, cm, g)
	}
	e.save()
}

// MakeCode makes a join code (§16.6): 16 random characters, whose key's
// public half goes in an add-code entry. It returns SERVER::SECRET.
func (e *Engine) MakeCode(ctx context.Context, server string, days int, uses uint64) (string, error) {
	if days <= 0 {
		days = 7
	}
	if uses == 0 {
		uses = 25
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return "", err
	}
	secret := newJoinSecret()
	key, err := codes.CodeKey(secret, cm.ID)
	if err != nil {
		return "", err
	}
	exp := e.c.Clock().Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	err = e.appendEntry(ctx, cm, func(st *community.State, _ uint64) ([]map[string]any, error) {
		if !st.Can(e.c.ID(), community.PermInvite) {
			return nil, errors.New("chat: making codes needs invite")
		}
		return []map[string]any{{"op": community.OpAddCode, "key": wire.Bytes(key.Public()), "exp": exp, "uses": uses}}, nil
	})
	if err != nil {
		return "", err
	}
	return server + "::" + secret, nil
}

// newcomer schedules sending the keys to someone who joined (§16.6):
// the member with the lowest leaf index, after 0 to 2 s, or anyone else
// after 10 s if nobody has.
func (e *Engine) newcomer(cm *Community, st *mls.State, id string) {
	lowest := uint32(1 << 31)
	for _, m := range st.Members {
		if mid, _, err := parseCredential(m.Identity); err == nil && mid != id && m.LeafIndex < lowest {
			lowest = m.LeafIndex
		}
	}
	wait := 10 * time.Second
	if st.OwnLeafIndex == lowest {
		n, _ := rand.Int(rand.Reader, big.NewInt(2000))
		wait = time.Duration(n.Int64()) * time.Millisecond
	}
	if cm.KeysDue == nil {
		cm.KeysDue = map[string]int64{}
	}
	if _, ok := cm.KeysDue[id]; !ok {
		cm.KeysDue[id] = time.Now().Add(wait).UnixMilli()
	}
}

// runDue sends the keys newcomers are waiting for, once due. With force,
// as Sync runs, it sends what's due within 2 s. The caller holds e.mu.
func (e *Engine) runDue(ctx context.Context, cm *Community, force bool) {
	if cm.Left || cm.Stopped != "" || len(cm.KeysDue) == 0 {
		return
	}
	now := time.Now()
	if force {
		now = now.Add(2 * time.Second)
	}
	due := false
	for _, t := range cm.KeysDue {
		due = due || t <= now.UnixMilli()
	}
	if !due {
		return
	}
	cm.KeysDue = nil
	// The newcomer reads up to its join: upload first (§16.5, "Who
	// uploads history").
	e.uploadAll(ctx, cm)
	for _, gid := range slices.Sorted(maps.Keys(cm.Groups)) {
		e.sendKeys(ctx, cm, cm.Groups[gid])
	}
	e.save()
}

// sendKeys sends community-keys in a group: every metadata key and history
// key it holds (§16.2.4).
func (e *Engine) sendKeys(ctx context.Context, cm *Community, g *CGroup) {
	if g == nil {
		return
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return
	}
	st := c.State()
	k := CommunityKeys{T: CCommunityKeys, TS: e.c.Clock().Now().Unix()}
	if g.See == nil {
		k.Metadata = slices.Clone(cm.MetaKeys)
	} else {
		k.Metadata = slices.Clone(cm.ChannelKeys[groupKeyID(g.ID)])
	}
	if len(k.Metadata) == 0 {
		return
	}
	for _, ch := range st.OrderedChannels() {
		if e.groupFor(cm, ch) != g || len(cm.HistoryKeys[ch.ID]) == 0 {
			continue
		}
		k.History = append(k.History, ChannelHistoryKeys{Channel: ch.ID, Keys: slices.Clone(cm.HistoryKeys[ch.ID])})
	}
	if _, err := e.csend(ctx, cm, g, nil, k, false); err != nil {
		e.log.Info("chat: sending community keys", "err", err)
	}
}

// csend signs content as an app-message, encrypts it in a community group,
// and sends it, with the channel for channel messages (§12, §16.5). It
// returns the sequence number a channel message got.
func (e *Engine) csend(ctx context.Context, cm *Community, g *CGroup, channel *uint64, content any, ephemeral bool) (*client.CommunitySendResult, error) {
	if cm.Stopped != "" {
		return nil, fmt.Errorf("chat: this community is stopped: %s", cm.Stopped)
	}
	gid := mustB64(g.ID)
	mg := e.dev.mls.Group(gid)
	if st, err := mg.State(); err == nil && len(st.PendingProposals) > 0 {
		// OpenMLS sends nothing while a proposal waits (§11.1).
		if err := e.ccommit(ctx, cm, g, ccommitOpts{pending: true}); err != nil {
			return nil, err
		}
	}
	c, err := wire.JCS(content)
	if err != nil {
		return nil, err
	}
	signed, err := signedApp(e.key, AppMessage{ID: e.c.ID(), Group: gid, Channel: channel, TS: e.c.Clock().Now().Unix(), C: c})
	if err != nil {
		return nil, err
	}
	ct, err := mg.Encrypt(e.dev.signer, signed.Canonical(), nil)
	if err != nil {
		return nil, err
	}
	if err := e.dev.Save(); err != nil {
		return nil, err
	}
	res, err := e.c.Community(cm.Server).Send(ctx, client.CommunityOutgoing{
		ID: client.NewMsgID(), Kind: "mls", Group: gid, Epoch: g.Epoch, MLS: "app", Channel: channel, Ephemeral: ephemeral, Data: ct,
	})
	if err != nil {
		return nil, err
	}
	r := res[0]
	if r.Status != "sent" {
		if r.Error == wire.CodeWrongHome {
			e.stopCommunity(cm, "the community moved to another community server, which this version of Railcar doesn't follow yet")
		}
		return &r, &wire.Error{Code: r.Error, Msg: "the community server refused the message", Retry: r.Retry}
	}
	if channel != nil && r.Seq != nil {
		mk, _ := wire.ParseSigned(e.dev.info.MsgKeyExtension)
		e.logAdd(cm, *channel, logEntry{Seq: *r.Seq, Msg: &signed, MsgKey: &mk, TS: time.Now().Unix()})
	}
	return &r, nil
}

func signedApp(key []byte, am AppMessage) (wire.Signed, error) {
	return sig.SignMessageKey(key, TypeAppMessage, am)
}

func (e *Engine) logAdd(cm *Community, channel uint64, le logEntry) {
	l := e.loadLog(cm, channel)
	if slices.ContainsFunc(l, func(x logEntry) bool { return x.Seq == le.Seq }) {
		return
	}
	e.saveLog(cm, channel, append(l, le))
}

// sendChannel sends content in a channel, and returns its sequence
// number.
func (e *Engine) sendChannel(ctx context.Context, ch *Chat, content any) (uint64, error) {
	cm, c, err := e.communityOf(ch.Community)
	if err != nil {
		return 0, err
	}
	cch := c.State().Channels[ch.Channel]
	if cch == nil || cch.Kind != "text" {
		return 0, errors.New("chat: that isn't a text channel")
	}
	if !c.State().Writes(e.c.ID(), cch, e.c.Clock().Now().Unix()) {
		return 0, errors.New("chat: you can't write in this channel")
	}
	g := e.groupFor(cm, cch)
	if g == nil {
		return 0, errors.New("chat: this device isn't in the channel's group yet")
	}
	channel := ch.Channel
	r, err := e.csend(ctx, cm, g, &channel, content, false)
	if err != nil {
		return 0, err
	}
	if r.Seq == nil {
		return 0, nil
	}
	e.noteSeq(ch, *r.Seq)
	return *r.Seq, nil
}

// sendChannelTyping sends a typing indicator to a text channel, as an
// ephemeral delivery (§13.3). A member who can't write there sends none.
func (e *Engine) sendChannelTyping(ctx context.Context, ch *Chat, content Typing) error {
	cm, c, err := e.communityOf(ch.Community)
	if err != nil {
		return err
	}
	cch := c.State().Channels[ch.Channel]
	if cch == nil || cch.Kind != "text" || !c.State().Writes(e.c.ID(), cch, e.c.Clock().Now().Unix()) {
		return nil
	}
	g := e.groupFor(cm, cch)
	if g == nil {
		return nil
	}
	channel := ch.Channel
	_, err = e.csend(ctx, cm, g, &channel, content, true)
	return err
}

// noteSeq records the newest sequence number a channel has.
func (e *Engine) noteSeq(ch *Chat, seq uint64) {
	if seq > ch.NewestSeq {
		ch.NewestSeq = seq
	}
}

// cprocessApp decrypts a community application message, checks it
// (§12), and applies it: a channel message with its sequence number, or
// protocol content without a channel (§16.5).
func (e *Engine) cprocessApp(ctx context.Context, cm *Community, g *CGroup, b buffered) error {
	mg := e.dev.mls.Group(mustB64(g.ID))
	p, err := mg.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		if mls.IsCode(err, mls.ErrPastEpoch) || mls.IsCode(err, mls.ErrWrongEpoch) {
			// A code joiner can't read epochs before its join (§16.6):
			// it's a number to fill from history.
			return nil
		}
		return fmt.Errorf("chat: decrypting a community message: %w", err)
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if p.Kind == mls.KindOwn {
		return nil
	}
	if p.Kind != mls.KindApplication {
		return fmt.Errorf("chat: an app delivery holding a %s", p.Kind)
	}
	st, err := mg.State()
	if err != nil {
		return err
	}
	leaf, ok := st.Member(p.Sender.Index)
	if !ok || !bytes.Equal(leaf.Identity, p.Identity) {
		return errors.New("chat: the sender's leaf is gone")
	}
	get := func(id string) (*identity.Chain, error) { return e.identityChain(ctx, cm, id, false) }
	id, _, err := e.checkLeaf(&leaf.LeafInfo, get, leafEver)
	if err != nil {
		return err
	}
	signed, err := wire.ParseSigned(p.Plaintext)
	if err != nil {
		return err
	}
	am, t, err := openAppMessage(p.Plaintext, leaf.SignatureKey)
	if err != nil {
		return err
	}
	if am.ID != id || !bytes.Equal(am.Group, mustB64(g.ID)) || b.From != "" && b.From != id {
		return errors.New("chat: an app-message whose id or group doesn't match")
	}
	switch {
	case b.Channel == nil && am.Channel != nil, b.Channel != nil && (am.Channel == nil || *am.Channel != *b.Channel):
		return errors.New("chat: an app-message whose channel isn't the delivery's") // §12
	}
	if b.Channel == nil {
		return e.protocolContent(ctx, cm, g, id, t, am)
	}
	mkData, _ := leaf.Extension(ExtMsgKey)
	mk, _ := wire.ParseSigned(mkData)
	seq := uint64(0)
	if b.Seq != nil {
		seq = *b.Seq
	}
	return e.applyChannel(ctx, cm, g, *b.Channel, seq, id, am, t, signed, mk, b.Received, true)
}

// applyChannel applies one channel message. live is false for messages
// read from history segments.
func (e *Engine) applyChannel(ctx context.Context, cm *Community, g *CGroup, channel, seq uint64, from string, am AppMessage, t string, signed, mk wire.Signed, received int64, live bool) error {
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	st := c.State()
	cch := st.Channels[channel]
	if cch == nil || e.groupFor(cm, cch) != g {
		return errors.New("chat: a message for a channel this group doesn't carry")
	}
	ch := e.st.Chats[chanID(cm.Server, channel)]
	if ch == nil {
		e.refreshChannels(cm)
		if ch = e.st.Chats[chanID(cm.Server, channel)]; ch == nil {
			return errors.New("chat: a message for a channel this device doesn't list")
		}
	}
	if live && st.TimedOut(from, am.TS) {
		return nil // a timed-out member's message that got through (§16.7)
	}
	if t == CTyping {
		// Ephemeral: shown, and never kept in history (§13.3, §7.3).
		if c, err := parseContent(t, am.C); err == nil && live {
			e.emit(Event{Kind: EventTyping, ChatID: ch.ID, Who: from, On: *c.(Typing).On})
		}
		return nil
	}
	if seq > 0 {
		e.logAdd(cm, channel, logEntry{Seq: seq, Msg: &signed, MsgKey: &mk, TS: am.TS})
		e.noteSeq(ch, seq)
	}
	content, err := parseContent(t, am.C)
	if err != nil {
		return err
	}
	if received == 0 {
		received = am.TS
	}
	switch c := content.(type) {
	case Text:
		m := Message{ID: c.ID, From: from, TS: am.TS, Received: received, Body: *c.Body, Reply: c.Reply, Status: Sent, Seq: seq, Files: attachments(c.Files), Mentioned: slices.Contains(c.Mentions, e.c.ID())}
		if c.Everyone && strings.Contains(" "+*c.Body+" ", " @everyone ") && st.Can(from, community.PermModerate) && st.SeesChannel(from, cch) {
			m.Everyone = true
		}
		return e.addChannelMessage(ch, m, live)
	case File:
		return e.addChannelMessage(ch, Message{ID: c.ID, From: from, TS: am.TS, Received: received, Status: Sent, Seq: seq, Files: []Attachment{{Ref: *c.Ref}}}, live)
	case Edit:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			if m.From != from {
				return errors.New("chat: an edit from someone other than the author")
			}
			m.Body, m.Edited = *c.Body, true
			return nil
		})
	case Delete:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			// The author, or a moderator who outranks them (§16.7).
			if m.From != from && !(st.Can(from, community.PermModerate) && st.Outranks(from, m.From)) {
				return errors.New("chat: a delete from someone who can't delete it")
			}
			if m.Seq > 0 && seq > 0 {
				e.goneInLog(cm, channel, m.Seq, seq)
			}
			m.Body, m.Deleted, m.Reactions, m.Files = "", true, nil, nil
			return nil
		})
	case React:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			react(m, from, c.Emoji, c.Remove)
			return nil
		})
	case nil:
		if !knownContent[t] {
			var u struct {
				ID string `json:"id"`
			}
			wire.Unmarshal(am.C, &u)
			if !validMsgID(u.ID) {
				u.ID = newMsgID()
			}
			return e.addChannelMessage(ch, Message{ID: u.ID, From: from, TS: am.TS, Received: received, Unsupported: t, Status: Sent, Seq: seq}, live)
		}
	}
	return nil
}

// goneInLog records that a channel message was deleted, by the delete at
// seq by.
func (e *Engine) goneInLog(cm *Community, channel, seq, by uint64) {
	l := e.loadLog(cm, channel)
	for i := range l {
		if l[i].Seq == seq {
			l[i].Msg, l[i].MsgKey, l[i].Gone = nil, nil, by
		}
	}
	e.saveLog(cm, channel, l)
}

// addChannelMessage adds a message to a channel's history in sequence
// order. A message read from history doesn't count as unread.
func (e *Engine) addChannelMessage(ch *Chat, m Message, live bool) error {
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	if findMessage(msgs, m.ID) >= 0 {
		return nil
	}
	if m.Seq > 0 && slices.ContainsFunc(msgs, func(x Message) bool { return x.Seq == m.Seq && !x.Gap }) {
		return nil
	}
	if m.Received == 0 {
		m.Received = e.c.Clock().Now().Unix()
	}
	msgs = append(msgs, m)
	slices.SortStableFunc(msgs, func(a, b Message) int {
		switch {
		case a.Seq != 0 && b.Seq != 0 && a.Seq != b.Seq:
			return cmpU(a.Seq, b.Seq)
		case a.Seq == 0 || b.Seq == 0:
			return int(a.Received - b.Received)
		}
		return 0
	})
	if err := saveMessages(e.profile(), recordID(ch.ID), msgs); err != nil {
		return err
	}
	last := msgs[len(msgs)-1]
	ch.LastActivity, ch.Preview = max(ch.LastActivity, m.Received), preview(last)
	if live && m.From != e.c.ID() && !m.System && m.Seq > ch.ReadSeq {
		ch.Unread++
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventMessage, ChatID: ch.ID, Message: &m, New: true})
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

func cmpU(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// protocolContent handles content sent without a channel: keys and
// history requests (§16.2.4, §16.5). Clients don't show chat content that
// arrives this way.
func (e *Engine) protocolContent(ctx context.Context, cm *Community, g *CGroup, from, t string, am AppMessage) error {
	switch t {
	case CCommunityKeys:
		var k CommunityKeys
		if err := wire.Unmarshal(am.C, &k); err != nil || len(k.Metadata) == 0 {
			return errors.New("chat: malformed community-keys")
		}
		return e.takeKeys(ctx, cm, g, k)
	case CHistoryRequest:
		var r HistoryRequest
		if err := wire.Unmarshal(am.C, &r); err != nil {
			return errors.New("chat: a malformed history-request")
		}
		return e.answerHistory(ctx, cm, g, r)
	}
	return nil
}

// takeKeys keeps the keys a member sent in a group (§16.2.4): metadata
// keys, and history keys of channels the group carries. Then it reads the
// names, and the history the new keys open.
func (e *Engine) takeKeys(ctx context.Context, cm *Community, g *CGroup, k CommunityKeys) error {
	for i := len(k.Metadata) - 1; i >= 0; i-- {
		key := k.Metadata[i]
		if len(key) != 32 {
			continue
		}
		if g.See == nil {
			if !slices.ContainsFunc(cm.MetaKeys, func(x wire.Bytes) bool { return bytes.Equal(x, key) }) {
				cm.MetaKeys = append([]wire.Bytes{key}, cm.MetaKeys...)
			}
		} else {
			addGroupKey(cm, g.ID, key)
		}
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	st := c.State()
	var opened []uint64
	for _, h := range k.History {
		ch := st.Channels[h.Channel]
		if ch == nil || e.groupFor(cm, ch) != g {
			continue // a channel the group doesn't carry (§16.2.4)
		}
		for i := len(h.Keys) - 1; i >= 0; i-- {
			if len(h.Keys[i]) == 32 && !slices.ContainsFunc(cm.HistoryKeys[h.Channel], func(x wire.Bytes) bool { return bytes.Equal(x, h.Keys[i]) }) {
				addKey(&cm.HistoryKeys, h.Channel, h.Keys[i])
				opened = append(opened, h.Channel)
			}
		}
	}
	if g.See == nil {
		cm.KeysDue = nil // someone sent the keys
	}
	e.save()
	e.refreshChannels(cm)
	for _, ch := range uniq64(opened) {
		if _, err := e.readHistory(ctx, cm, ch); err != nil {
			e.log.Info("chat: reading a channel's history", "err", err)
		}
	}
	return nil
}

// HistoryResult says what reading a channel's history found.
type HistoryResult struct {
	Added int
	// Gaps are the ranges of sequence numbers no segment and no delivery
	// gave this device.
	Gaps [][2]uint64
}

// FetchHistory reads a channel's history segments (§16.5), and asks the
// members for numbers no segment holds.
func (e *Engine) FetchHistory(ctx context.Context, chat string) (HistoryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.st.Chats[chat]
	if ch == nil || ch.Kind != KindChannel {
		return HistoryResult{}, errors.New("chat: no such channel")
	}
	cm, _, err := e.communityOf(ch.Community)
	if err != nil {
		return HistoryResult{}, err
	}
	return e.readHistory(ctx, cm, ch.Channel)
}

// readHistory reads every segment of a channel this device can open,
// checking each line and verifying each message as imported history is
// (§7.3, §16.5, "Reading a segment").
func (e *Engine) readHistory(ctx context.Context, cm *Community, channel uint64) (HistoryResult, error) {
	var res HistoryResult
	c, err := e.communityChain(cm)
	if err != nil {
		return res, err
	}
	cch := c.State().Channels[channel]
	if cch == nil || cch.Kind != "text" {
		return res, nil
	}
	g := e.groupFor(cm, cch)
	ch := e.st.Chats[chanID(cm.Server, channel)]
	if g == nil || ch == nil {
		return res, nil
	}
	conn := e.c.Community(cm.Server)
	segs, err := conn.ListHistory(ctx, channel)
	if err != nil {
		return res, retryable{err}
	}
	keys := make([][]byte, 0, len(cm.HistoryKeys[channel]))
	for _, k := range cm.HistoryKeys[channel] {
		keys = append(keys, k)
	}
	gid := mustB64(g.ID)
	before := len(e.loadLog(cm, channel))
	for _, s := range segs {
		ctxt, err := conn.GetHistory(ctx, channel, s.Start)
		if err != nil {
			continue
		}
		pt, ok := community.DecryptSegment(keys, cm.ID, channel, s.Start, s.End, ctxt)
		if !ok {
			continue
		}
		items, err := community.ReadSegment(pt, gid, s.Start, s.End)
		if err != nil {
			continue
		}
		e.noteSeq(ch, s.End)
		for _, it := range items {
			if it.Gone != 0 {
				e.logAdd(cm, channel, logEntry{Seq: it.Seq, Gone: it.Gone})
				continue
			}
			if err := e.historyMsg(ctx, cm, g, channel, it); err != nil {
				e.log.Info("chat: dropped a message from a history segment", "err", err)
			}
		}
	}
	// Deleted messages from history show as deleted.
	l := e.loadLog(cm, channel)
	for _, le := range l {
		if le.Gone != 0 {
			msgs, _ := e.messages(ch.ID)
			if !slices.ContainsFunc(msgs, func(m Message) bool { return m.Seq == le.Seq }) {
				e.addChannelMessage(ch, Message{ID: "gone-" + fmt.Sprint(le.Seq), Seq: le.Seq, Deleted: true, Status: Sent, Received: le.TS}, false)
			}
		}
	}
	res.Added = len(l) - before
	res.Gaps = gaps(l, ch.NewestSeq)
	if len(res.Gaps) > 0 && cch.History {
		e.askHistory(ctx, cm, g, channel)
	}
	e.save()
	return res, nil
}

// historyMsg verifies one message from a segment: its signature under
// the msg-key its line names, the msg-key's identity, and the msg-key
// signed by a device that appears in the sender's chain (§7.3, §16.5).
func (e *Engine) historyMsg(ctx context.Context, cm *Community, g *CGroup, channel uint64, it community.ReadItem) error {
	var mk identity.MsgKeyBody
	if err := it.MsgKey.Body(&mk); err != nil {
		return err
	}
	if err := sig.VerifyMessageKey(*it.Msg, mk.Key); err != nil {
		return err
	}
	var am AppMessage
	if err := it.Msg.Body(&am); err != nil {
		return err
	}
	if am.ID != mk.ID || !bytes.Equal(am.Group, mustB64(g.ID)) || am.Channel == nil || *am.Channel != channel {
		return errors.New("chat: a history message for another identity, group, or channel")
	}
	chain, err := e.identityChain(ctx, cm, mk.ID, false)
	if err != nil {
		return err
	}
	ok := false
	for _, info := range chain.Info {
		for _, op := range info.Body.Ops {
			if op.Op == identity.OpAddDevice {
				if _, err := identity.VerifyMsgKey(*it.MsgKey, mk.ID, op.Pub); err == nil {
					ok = true
				}
			}
		}
	}
	if !ok {
		return errors.New("chat: a history message whose msg-key no device of the sender's signed")
	}
	var head struct {
		T string `json:"t"`
	}
	if err := wire.Unmarshal(am.C, &head); err != nil {
		return err
	}
	return e.applyChannel(ctx, cm, g, channel, it.Seq, am.ID, am, head.T, *it.Msg, *it.MsgKey, am.TS, false)
}

// gaps lists the ranges of sequence numbers from 1 to newest that a log
// doesn't hold.
func gaps(l []logEntry, newest uint64) [][2]uint64 {
	have := map[uint64]bool{}
	for _, le := range l {
		have[le.Seq] = true
	}
	var out [][2]uint64
	for s := uint64(1); s <= newest; s++ {
		if have[s] {
			continue
		}
		start := s
		for s+1 <= newest && !have[s+1] {
			s++
		}
		out = append(out, [2]uint64{start, s})
	}
	return out
}

// askHistory sends a history-request for a channel, at most once every
// 10 minutes (§16.5, "Asking for history").
func (e *Engine) askHistory(ctx context.Context, cm *Community, g *CGroup, channel uint64) {
	now := time.Now().Unix()
	if cm.Asked[channel]+600 > now {
		return
	}
	if cm.Asked == nil {
		cm.Asked = map[uint64]int64{}
	}
	cm.Asked[channel] = now
	r := HistoryRequest{T: CHistoryRequest, TS: e.c.Clock().Now().Unix(), Channel: channel}
	if _, err := e.csend(ctx, cm, g, nil, r, false); err != nil {
		e.log.Info("chat: asking for history", "err", err)
	}
}

// answerHistory answers a history-request: it uploads what this device
// holds that no segment holds, at most once every 10 s per channel
// (§16.5).
func (e *Engine) answerHistory(ctx context.Context, cm *Community, g *CGroup, r HistoryRequest) error {
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	cch := c.State().Channels[r.Channel]
	if cch == nil || cch.Kind != "text" || !cch.History || e.groupFor(cm, cch) != g || len(cm.HistoryKeys[r.Channel]) == 0 {
		return nil
	}
	now := time.Now().Unix()
	if r.TS+7*86400 < e.c.Clock().Now().Unix() || cm.Answered[r.Channel]+10 > now {
		return nil
	}
	if cm.Answered == nil {
		cm.Answered = map[uint64]int64{}
	}
	cm.Answered[r.Channel] = now
	before := uint64(0)
	if r.Before != nil {
		before = *r.Before
	}
	return e.uploadHistory(ctx, cm, g, r.Channel, before)
}

// uploadAll uploads, in every text channel with history on, what has
// built up past the last segment (§16.5).
func (e *Engine) uploadAll(ctx context.Context, cm *Community) {
	c, err := e.communityChain(cm)
	if err != nil {
		return
	}
	for _, ch := range c.State().OrderedChannels() {
		if ch.Kind != "text" || !ch.History || len(cm.HistoryKeys[ch.ID]) == 0 {
			continue
		}
		if g := e.groupFor(cm, ch); g != nil {
			if err := e.uploadHistory(ctx, cm, g, ch.ID, 0); err != nil {
				e.log.Info("chat: uploading history", "err", err)
			}
		}
	}
}

// uploadHistory uploads segments for what this device holds of a channel
// that no stored segment holds, below before (0: everything). Each
// segment starts at the first number this device holds there, and never
// overlaps a stored one (§16.5, "Who uploads history").
func (e *Engine) uploadHistory(ctx context.Context, cm *Community, g *CGroup, channel, before uint64) error {
	conn := e.c.Community(cm.Server)
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	st := c.State()
	for attempt := 0; attempt < 2; attempt++ {
		segs, err := conn.ListHistory(ctx, channel)
		if err != nil {
			return err
		}
		covered := func(s uint64) bool {
			return slices.ContainsFunc(segs, func(x client.Segment) bool { return s >= x.Start && s <= x.End })
		}
		var runs [][]logEntry
		var cur []logEntry
		nextSeg := func(s uint64) uint64 {
			n := uint64(0)
			for _, x := range segs {
				if x.Start > s && (n == 0 || x.Start < n) {
					n = x.Start
				}
			}
			return n
		}
		l := e.loadLog(cm, channel)
		limit := uint64(0)
		for _, le := range l {
			if before > 0 && le.Seq >= before {
				break
			}
			if covered(le.Seq) || le.Msg == nil && le.Gone == 0 || le.Msg != nil && st.HistoryWasOff(channel, le.TS) {
				if len(cur) > 0 {
					runs = append(runs, cur)
					cur = nil
				}
				continue
			}
			if len(cur) > 0 && limit != 0 && le.Seq >= limit {
				runs = append(runs, cur)
				cur = nil
			}
			if len(cur) == 0 {
				limit = nextSeg(le.Seq)
			}
			cur = append(cur, le)
		}
		if len(cur) > 0 {
			runs = append(runs, cur)
		}
		if len(runs) == 0 {
			return nil
		}
		gid := mustB64(g.ID)
		conflict := false
		for _, run := range runs {
			var items []community.SegmentMsg
			for _, le := range run {
				if le.Gone != 0 {
					items = append(items, community.SegmentMsg{Seq: le.Seq, Gone: le.Gone})
				} else {
					items = append(items, community.SegmentMsg{Seq: le.Seq, Msg: *le.Msg, MsgKey: *le.MsgKey})
				}
			}
			pt, err := community.BuildSegment(gid, items)
			if err != nil {
				return err
			}
			start, end := run[0].Seq, run[len(run)-1].Seq
			ct, err := community.EncryptSegment(cm.HistoryKeys[channel][0], cm.ID, channel, start, end, pt)
			if err != nil {
				return err
			}
			err = conn.PutHistory(ctx, channel, start, end, ct, false)
			if wire.IsCode(err, wire.CodeAlreadyExists) {
				conflict = true
				continue
			}
			if err != nil {
				return err
			}
		}
		if !conflict {
			return nil
		}
	}
	return nil
}

// MarkChannelRead clears a channel's unread count, and sends how far this
// member has read it (§16.4, "Read positions").
func (e *Engine) markChannelRead(ctx context.Context, ch *Chat) {
	msgs, _ := e.messages(ch.ID)
	var newest uint64
	for _, m := range msgs {
		newest = max(newest, m.Seq)
	}
	if newest > ch.ReadSeq {
		ch.ReadSeq = newest
		if err := e.c.Community(ch.Community).PutRead(ctx, ch.Channel, newest); err != nil && !wire.IsCode(err, wire.CodeNotFound) {
			e.log.Info("chat: sending a read position", "err", err)
		}
	}
}

// pullReadPositions takes the member's read positions from the community
// server where they're ahead of this device's (§16.4).
func (e *Engine) pullReadPositions(ctx context.Context, cm *Community) {
	if cm.Left {
		return
	}
	pos, err := e.c.Community(cm.Server).ReadPositions(ctx)
	if err != nil {
		return
	}
	for channel, seq := range pos {
		ch := e.st.Chats[chanID(cm.Server, channel)]
		if ch == nil || seq <= ch.ReadSeq {
			continue
		}
		ch.ReadSeq = seq
		msgs, _ := e.messages(ch.ID)
		unread := 0
		for _, m := range msgs {
			if m.Seq > seq && m.From != e.c.ID() && !m.System {
				unread++
			}
		}
		ch.Unread = unread
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	}
	e.save()
}

// ReadPosition returns how far this member has read a channel.
func (e *Engine) ReadPosition(chat string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch := e.st.Chats[chat]; ch != nil {
		return ch.ReadSeq
	}
	return 0
}

// deleteChannel deletes a channel message for everyone: the signed
// delete, then POST messages/delete so the community server drops the
// ciphertext (§16.7). The author can, and so can a moderator who outranks
// them.
func (e *Engine) deleteChannel(ctx context.Context, ch *Chat, target string) error {
	cm, c, err := e.communityOf(ch.Community)
	if err != nil {
		return err
	}
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	i := findMessage(msgs, target)
	if i < 0 || msgs[i].Deleted {
		return errors.New("chat: no such message")
	}
	st := c.State()
	me := e.c.ID()
	if msgs[i].From != me && !(st.Can(me, community.PermModerate) && st.Outranks(me, msgs[i].From)) {
		return errors.New("chat: you can only delete your own messages here")
	}
	seq, err := e.sendChannel(ctx, ch, Delete{T: CDelete, ID: newMsgID(), TS: e.c.Clock().Now().Unix(), Target: target})
	if err != nil {
		return err
	}
	if msgs[i].Seq > 0 && seq > 0 {
		if err := e.c.Community(cm.Server).DeleteMessage(ctx, ch.Channel, msgs[i].Seq, seq); err != nil {
			e.log.Info("chat: dropping a deleted message's ciphertext", "err", err)
		}
		e.goneInLog(cm, ch.Channel, msgs[i].Seq, seq)
	}
	return e.changeMessage(ch, target, func(m *Message) error {
		m.Body, m.Deleted, m.Reactions, m.Files = "", true, nil, nil
		return nil
	})
}

// Kick removes someone from the community (§16.7): the community server
// stops delivering to them, their roles are cleared, and this device
// removes their leaves from every group it's in. They can rejoin with a
// code.
func (e *Engine) Kick(ctx context.Context, server, member string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, c, err := e.communityOf(server)
	if err != nil {
		return err
	}
	st := c.State()
	me := e.c.ID()
	if !st.Can(me, community.PermModerate) || !st.Outranks(me, member) {
		return errors.New("chat: kicking needs moderate, and to outrank them")
	}
	if err := e.c.Community(server).Kick(ctx, member); err != nil {
		return err
	}
	if len(st.Holds[member]) > 0 {
		if err := e.appendEntry(ctx, cm, func(*community.State, uint64) ([]map[string]any, error) {
			return []map[string]any{{"op": community.OpAssignRoles, "member": member, "roles": []uint64{}}}, nil
		}); err != nil {
			e.log.Info("chat: clearing a kicked member's roles", "err", err)
		}
	}
	e.removeEverywhere(ctx, cm, member)
	if err := e.rotateMetaKey(ctx, cm); err != nil {
		e.log.Info("chat: rotating the community metadata key", "err", err)
	}
	return nil
}

// Ban bans someone by identifier (§16.7): a ban entry, which the community
// server applies as a kick, and this device's Removes of their leaves.
func (e *Engine) Ban(ctx context.Context, server, member string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return err
	}
	if err := e.appendEntry(ctx, cm, func(st *community.State, _ uint64) ([]map[string]any, error) {
		if !st.Can(e.c.ID(), community.PermAdministrate) || !st.Outranks(e.c.ID(), member) {
			return nil, errors.New("chat: banning needs administrate, and to outrank them")
		}
		return []map[string]any{{"op": community.OpBan, "member": member}}, nil
	}); err != nil {
		return err
	}
	e.removeEverywhere(ctx, cm, member)
	if err := e.rotateMetaKey(ctx, cm); err != nil {
		e.log.Info("chat: rotating the community metadata key", "err", err)
	}
	return nil
}

// Timeout times a member out for a while, or ends it with zero (§16.7).
func (e *Engine) Timeout(ctx context.Context, server, member string, d time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return err
	}
	return e.appendEntry(ctx, cm, func(st *community.State, _ uint64) ([]map[string]any, error) {
		until := max(e.c.Clock().Now().Unix(), st.TS) + int64(d/time.Second)
		return []map[string]any{{"op": community.OpTimeout, "member": member, "until": until}}, nil
	})
}

// removeEverywhere commits the Removes of every leaf a member has, in
// every community group this device is in.
func (e *Engine) removeEverywhere(ctx context.Context, cm *Community, member string) {
	for _, gid := range slices.Sorted(maps.Keys(cm.Groups)) {
		g := cm.Groups[gid]
		gst, err := e.dev.mls.Group(mustB64(gid)).State()
		if err != nil {
			continue
		}
		var removes []uint32
		for _, l := range leavesByIdentity(gst)[member] {
			removes = append(removes, l.index)
		}
		if len(removes) == 0 {
			continue
		}
		o := ccommitOpts{removes: removes, pending: e.hasPending(g)}
		if err := e.ccommit(ctx, cm, g, o); err != nil {
			e.log.Info("chat: removing a member's leaves", "err", err)
		}
	}
	e.save()
	e.refreshChannels(cm)
}

// LeaveCommunity leaves a community (§16.6): this device clears the
// identity's roles, tells the community server, and proposes removing its
// leaves. The owner hands ownership over first.
func (e *Engine) LeaveCommunity(ctx context.Context, server string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, c, err := e.communityOf(server)
	if err != nil {
		return err
	}
	st := c.State()
	me := e.c.ID()
	if st.Owner == me {
		return errors.New("chat: the owner can't leave without handing ownership over first")
	}
	if len(st.Holds[me]) > 0 {
		if err := e.appendEntry(ctx, cm, func(*community.State, uint64) ([]map[string]any, error) {
			return []map[string]any{{"op": community.OpAssignRoles, "member": me, "roles": []uint64{}}}, nil
		}); err != nil {
			return err
		}
	}
	conn := e.c.Community(server)
	for _, gid := range slices.Sorted(maps.Keys(cm.Groups)) {
		g := cm.Groups[gid]
		mg := e.dev.mls.Group(mustB64(gid))
		p, err := mg.Leave(e.dev.signer)
		if err != nil {
			continue
		}
		e.dev.Save()
		conn.Send(ctx, client.CommunityOutgoing{ID: client.NewMsgID(), Kind: "mls", Group: mustB64(gid), Epoch: g.Epoch, MLS: "proposal", Data: p.Message})
	}
	if err := conn.Leave(ctx); err != nil {
		e.log.Info("chat: leaving the community", "err", err)
	}
	e.removedFromCommunity(ctx, cm, "You left the community")
	return nil
}

// voiceFrame notes who's in a community's voice channels, from
// voice-state (community server API, "Voice"). Railcar lists voice
// channels and who's in them, and can't join them.
func (e *Engine) voiceFrame(server string, frame jsontext.Value) {
	var f struct {
		T       string `json:"t"`
		Channel uint64 `json:"channel"`
		In      []struct {
			ID string `json:"id"`
		} `json:"in"`
	}
	if wire.Unmarshal(frame, &f) != nil || f.T != "voice-state" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.st.Chats[chanID(server, f.Channel)]
	if ch == nil {
		return
	}
	ch.InVoice = nil
	for _, in := range f.In {
		if !slices.Contains(ch.InVoice, in.ID) {
			ch.InVoice = append(ch.InVoice, in.ID)
		}
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
}
