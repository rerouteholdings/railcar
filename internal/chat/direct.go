package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Type tags of the hub's objects (§11.6).
const (
	TypeCommitRequest = "commit-request"
	TypeCommitReceipt = "commit-receipt"
)

// CommitRequest is the body of a commit-request, signed with epoch_key_N
// (§11.6). This client always derives next itself, so it never uses reg.
type CommitRequest struct {
	Group wire.Bytes `json:"group"`
	Epoch uint64     `json:"epoch"`
	Hash  wire.Bytes `json:"hash"`
	Next  wire.Bytes `json:"next"`
}

// CommitReceipt is the body of a hub's commit-receipt (§11.6).
type CommitReceipt struct {
	Hub   string     `json:"hub"`
	Group wire.Bytes `json:"group"`
	Epoch uint64     `json:"epoch"`
	Hash  wire.Bytes `json:"hash"`
	TS    int64      `json:"ts"`
}

// createDirect creates the direct chat with a new contact (§9.3, §11.5):
// a group with this homeserver as hub, holding every current device of
// both identities. The caller holds e.mu.
func (e *Engine) createDirect(ctx context.Context, peer string) (*Chat, error) {
	me := e.c.ID()
	peerChain, err := e.fetchChain(ctx, peer, true)
	if err != nil {
		return nil, err
	}
	myChain, err := e.fetchChain(ctx, me, true)
	if err != nil {
		return nil, err
	}
	// Claim a key package for each of the contact's devices, and for this
	// identity's other devices (§9.3 step 2, §11.5 step 3).
	var adds [][]byte
	claimed, err := e.c.ClaimKeyPackages(ctx, peer, client.ClaimOptions{})
	if err != nil {
		return nil, err
	}
	for _, kp := range claimed {
		b, err := e.checkClaimed(kp, peer, peerChain)
		if err != nil {
			e.log.Info("chat: refused a claimed key package", "err", err)
			continue
		}
		adds = append(adds, b)
	}
	if len(adds) == 0 {
		return nil, errors.New("chat: the contact has no usable key packages")
	}
	var others [][]byte
	for _, d := range myChain.State.Devices {
		if !bytes.Equal(d.Key, e.c.DeviceKey()) {
			others = append(others, d.Key)
		}
	}
	ownAdded := false
	if len(others) > 0 {
		mine, err := e.c.ClaimKeyPackages(ctx, me, client.ClaimOptions{Devices: others})
		if err != nil {
			e.log.Info("chat: claiming key packages for this identity's other devices", "err", err)
		}
		for _, kp := range mine {
			if bytes.Equal(kp.Dev, e.c.DeviceKey()) {
				continue
			}
			b, err := e.checkClaimed(kp, me, myChain)
			if err != nil {
				e.log.Info("chat: refused a claimed key package", "err", err)
				continue
			}
			adds = append(adds, b)
			ownAdded = true
		}
	}

	// Create and register the group, with a new ID if it's taken (§11.5).
	var g *mls.Group
	var gid []byte
	for attempt := 0; ; attempt++ {
		gid = make([]byte, 32)
		rand.Read(gid)
		ext := &GroupExt{Kind: KindDirect, Hub: e.c.Server(), Since: map[string]uint64{me: 0}}
		x, err := ext.Extension()
		if err != nil {
			return nil, err
		}
		g, err = e.dev.mls.CreateGroup(e.dev.signer, mls.CreateOptions{
			GroupID: gid, Leaf: e.dev.Leaf(), Config: chatConfig,
			Extensions: []mls.Extension{x, mls.RequiredCapabilities([]uint16{ExtGroup}, nil, nil)},
		})
		if err != nil {
			return nil, err
		}
		ek, err := e.dev.EpochKey(g)
		if err != nil {
			return nil, err
		}
		err = e.c.CreateGroup(ctx, client.CreateGroupOptions{Group: wire.EncodeB64(gid), Kind: KindDirect, EpochKey: ek.Public()})
		if wire.IsCode(err, wire.CodeAlreadyExists) && attempt < 3 {
			g.Delete()
			continue
		}
		if err != nil {
			g.Delete()
			return nil, err
		}
		break
	}
	if err := e.dev.Save(); err != nil {
		return nil, err
	}

	// The Add commit binds both identities' chains (§7.1), records when
	// the contact joined (§11.8), and latches the group.
	st, err := g.State()
	if err != nil {
		return nil, err
	}
	data, _ := st.Extension(ExtGroup)
	old, err := ParseGroupExt(data)
	if err != nil {
		return nil, err
	}
	next := old.Clone()
	next.Since[peer] = st.Epoch + 1
	next.Basis = map[string]BasisRef{me: basisFor(myChain), peer: basisFor(peerChain)}
	next.Heads = map[string]ChainRef{me: next.Basis[me].Ref(), peer: next.Basis[peer].Ref()}
	ch := &Chat{ID: wire.EncodeB64(gid), Kind: KindDirect, Hub: e.c.Server(), Peer: peer, Members: []string{me}, Epoch: st.Epoch}
	to := []string{peer, me}
	welcomeTo := []string{peer}
	if ownAdded {
		welcomeTo = append(welcomeTo, me)
	}
	if err := e.commit(ctx, ch, g, mls.CommitOptions{Adds: adds}, next, to, welcomeTo); err != nil {
		g.Delete()
		e.dev.Save()
		return nil, err
	}
	ch.Members = []string{me, peer}
	e.adoptDirect(ch)
	if err := e.save(); err != nil {
		return nil, err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return ch, nil
}

// adoptDirect makes ch the direct chat with its peer. A direct chat
// re-created between the same two people is the same conversation
// (§11.11), so the history of the one it replaces moves over. The caller
// holds e.mu and saves the state.
func (e *Engine) adoptDirect(ch *Chat) {
	e.st.Chats[ch.ID] = ch
	c := e.st.Contacts[ch.Peer]
	if c == nil {
		c = &Contact{ID: ch.Peer, Handle: e.st.Handles[ch.Peer], State: StateContact, Since: e.c.Clock().Now().Unix()}
		e.st.Contacts[ch.Peer] = c
	}
	if c.State != StateBlocked {
		c.State, c.Request = StateContact, nil
	}
	if oldID := c.Chat; oldID != "" && oldID != ch.ID {
		if old, ok := e.st.Chats[oldID]; ok {
			if oldMsgs, err := e.messages(oldID); err == nil && len(oldMsgs) > 0 {
				msgs, _ := e.messages(ch.ID)
				if err := saveMessages(e.profile(), recordID(ch.ID), append(oldMsgs, msgs...)); err == nil {
					e.profile().Delete(messagesRecord(recordID(oldID)))
				}
				ch.LastActivity, ch.Preview, ch.Unread = old.LastActivity, old.Preview, old.Unread
			}
			delete(e.st.Chats, oldID)
			e.dev.mls.Group(mustB64(oldID)).Delete()
			e.dev.Save()
			e.emit(Event{Kind: EventChatRemoved, ChatID: oldID})
		}
	}
	if ch.LastActivity == 0 {
		ch.LastActivity = e.c.Clock().Now().Unix()
	}
	c.Chat = ch.ID
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
}

func mustB64(s string) []byte {
	b, _ := wire.DecodeB64(s)
	return b
}

// commit makes a commit with a new tieroute_group (when next isn't nil),
// has the hub sequence it with a commit-request (§11.6), and applies it
// once accepted. Welcomes go in the same submission. The caller holds
// e.mu.
func (e *Engine) commit(ctx context.Context, ch *Chat, g *mls.Group, opts mls.CommitOptions, next *GroupExt, to, welcomeTo []string) error {
	if ch.Stopped != "" || ch.Pending {
		return errors.New("chat: this chat can't change until its last commit is decided")
	}
	if next != nil {
		x, err := next.Extension()
		if err != nil {
			return err
		}
		// Every commit that changes tieroute_group sets both extensions
		// (§11.1).
		opts.Extensions = []mls.Extension{x, mls.RequiredCapabilities([]uint16{ExtGroup}, nil, nil)}
	}
	res, err := g.Commit(e.dev.signer, opts)
	if err != nil {
		return err
	}
	nextKey, err := e.dev.PendingEpochKey(g)
	if err != nil {
		g.ClearPendingCommit()
		return err
	}
	key, err := e.dev.EpochKey(g)
	if err != nil {
		g.ClearPendingCommit()
		return err
	}
	h := sha256.Sum256(res.Commit)
	req, err := sig.SignRevealing(key, TypeCommitRequest, CommitRequest{Group: g.ID(), Epoch: res.Epoch, Hash: h[:], Next: nextKey.Public()})
	if err != nil {
		g.ClearPendingCommit()
		return err
	}
	// Keep the pending commit across a crash before the answer.
	if err := e.dev.Save(); err != nil {
		return err
	}
	epoch := res.Epoch
	out := client.Outgoing{
		ID: client.NewMsgID(), Kind: "mls", To: to, Group: wire.EncodeB64(g.ID()), Epoch: &epoch, MLS: "commit",
		Data: res.Commit, Commit: &req,
	}
	if ch.Hub != e.c.Server() {
		out.Hub = ch.Hub
	}
	if len(res.Welcome) > 0 && len(welcomeTo) > 0 {
		out.Welcomes = []client.Welcome{{ID: client.NewMsgID(), To: welcomeTo, Data: res.Welcome}}
	}
	results, err := e.c.Send(ctx, out)
	if err != nil {
		g.ClearPendingCommit()
		e.dev.Save()
		return err
	}
	r := results[0]
	if r.Status != "sent" {
		g.ClearPendingCommit()
		e.dev.Save()
		return &wire.Error{Code: r.Error, Msg: "the hub refused the commit"}
	}
	if r.Receipt == nil {
		g.ClearPendingCommit()
		e.dev.Save()
		return errors.New("chat: the hub accepted the commit without a receipt")
	}
	if _, err := e.checkReceipt(ctx, ch, *r.Receipt, g.ID(), epoch, res.Commit); err != nil {
		// The commit is sequenced, so the group has moved on; a bad
		// receipt means the hub can't be trusted.
		g.ClearPendingCommit()
		ch.Stopped = "the hub's receipt for this device's commit didn't check out"
		e.dev.Save()
		return err
	}
	if err := g.MergePendingCommit(); err != nil {
		return err
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	ch.Epoch = epoch + 1
	ch.applied(epoch, h[:])
	if next != nil {
		ch.Hub = next.Hub
	}
	return nil
}

// applied records the hash of the commit applied in an epoch, and keeps
// the last 64.
func (ch *Chat) applied(epoch uint64, hash []byte) {
	if ch.Applied == nil {
		ch.Applied = map[uint64]wire.Bytes{}
	}
	ch.Applied[epoch] = hash
	for k := range ch.Applied {
		if k+64 < epoch {
			delete(ch.Applied, k)
		}
	}
}

// checkReceipt checks a hub's commit receipt (§11.6): signed by the hub
// named in tieroute_group, for this group, epoch, and commit.
func (e *Engine) checkReceipt(ctx context.Context, ch *Chat, r wire.Signed, group []byte, epoch uint64, commit []byte) (CommitReceipt, error) {
	return e.checkReceiptFrom(ctx, ch.Hub, r, group, epoch, commit)
}

// checkReceiptFrom checks a commit receipt from a named hub.
func (e *Engine) checkReceiptFrom(ctx context.Context, hub string, r wire.Signed, group []byte, epoch uint64, commit []byte) (CommitReceipt, error) {
	var b CommitReceipt
	if r.T != TypeCommitReceipt {
		return b, errors.New("chat: not a commit receipt")
	}
	if err := r.Body(&b); err != nil {
		return b, err
	}
	h := sha256.Sum256(commit)
	if b.Hub != hub || !bytes.Equal(b.Group, group) || b.Epoch != epoch || !bytes.Equal(b.Hash, h[:]) {
		return b, errors.New("chat: the receipt is for another hub, group, epoch, or commit")
	}
	if err := e.c.VerifyServerSigned(ctx, hub, r); err != nil {
		return b, retryable{fmt.Errorf("chat: the receipt's signature: %w", err)}
	}
	return b, nil
}

// welcome joins a group from a Welcome, after checking it (§7.1, §10.3).
// The caller holds e.mu.
func (e *Engine) welcome(ctx context.Context, d *client.Delivery) error {
	me := e.c.ID()
	if d.From != me {
		c := e.st.Contacts[d.From]
		if c == nil || (c.State != StateContact && c.State != StatePendingOut) {
			return fmt.Errorf("chat: a Welcome from %s, who isn't a contact", d.From)
		}
	}
	if _, ok := e.st.Chats[d.Group]; ok {
		return nil // already in it
	}
	g, res, err := e.dev.mls.JoinWelcome(d.Data, nil, chatConfig)
	if err != nil {
		e.dev.Revert()
		return err
	}
	refuse := func(err error) error {
		g.Delete()
		e.dev.Revert()
		return err
	}
	if wire.EncodeB64(res.GroupID) != d.Group {
		return refuse(errors.New("chat: the Welcome is for another group"))
	}
	st, err := g.State()
	if err != nil {
		return refuse(err)
	}
	data, ok := st.Extension(ExtGroup)
	if !ok {
		return refuse(errors.New("chat: a group without tieroute_group"))
	}
	ext, err := ParseGroupExt(data)
	if err != nil {
		return refuse(err)
	}
	if ext.Kind == KindGroup {
		if c, ok := e.st.Chats[d.Group]; ok && c.Left {
			return refuse(errors.New("chat: a Welcome to a group this identity left"))
		}
	}
	// Check every leaf on this device's own fresh copies (§7.1, Welcomes):
	// every leaf is a device its identity's chain added at some point, and
	// the sender's is a current device.
	get := e.chainGetter(ctx, true)
	ids := map[string]bool{}
	for i := range st.Members {
		m := &st.Members[i]
		kind := leafEver
		if m.LeafIndex == res.SenderLeafIndex {
			kind = leafCurrent
		}
		id, _, err := e.checkLeaf(&m.LeafInfo, get, kind)
		if err != nil {
			var r retryable
			if errors.As(err, &r) {
				g.Delete()
				e.dev.Revert()
				return err
			}
			return refuse(err)
		}
		if m.LeafIndex == res.SenderLeafIndex && id != d.From {
			return refuse(errors.New("chat: the Welcome's sender isn't who delivered it"))
		}
		ids[id] = true
	}
	for id := range ids {
		if _, ok := ext.Since[id]; !ok {
			return refuse(fmt.Errorf("chat: tieroute_group's since leaves out %s", id))
		}
	}
	if ext.Kind == KindGroup {
		return e.joinGroup(ctx, g, d, st, ext, ids)
	}
	if !ids[me] || len(ids) != 2 {
		return refuse(errors.New("chat: a direct chat that isn't this identity and one other"))
	}
	peer := ""
	for id := range ids {
		if id != me {
			peer = id
		}
	}
	if d.From != me && d.From != peer {
		return refuse(errors.New("chat: a direct chat Welcome from someone outside it"))
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if err := e.c.RegisterGroup(ctx, d.Group); err != nil {
		e.log.Info("chat: registering the group", "err", err)
	}
	ch := &Chat{ID: d.Group, Kind: KindDirect, Hub: ext.Hub, Peer: peer, Members: []string{me, peer}, Epoch: st.Epoch}
	e.adoptDirect(ch)
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	// The Welcome used up one of this device's key packages.
	if err := e.topUp(ctx); err != nil {
		e.log.Debug("chat: key packages", "err", err)
	}
	return nil
}

// joinGroup finishes joining a group chat from a checked Welcome: at most
// 25 people and 250 leaves (§11.10), admins who are members, and since
// covering exactly the members (§11.8). A Welcome whose tieroute_group
// names a group it replaces starts a separate chat: linking it to the old
// one waits for a later version of Railcar (§11.11). The caller holds
// e.mu.
func (e *Engine) joinGroup(ctx context.Context, g *mls.Group, d *client.Delivery, st *mls.State, ext *GroupExt, ids map[string]bool) error {
	refuse := func(err error) error {
		g.Delete()
		e.dev.Revert()
		return err
	}
	if len(ids) > MaxGroupPeople || len(st.Members) > MaxGroupLeaves {
		return refuse(errors.New("chat: a group over 25 people or 250 leaves"))
	}
	if len(ext.Since) != len(ids) {
		return refuse(errors.New("chat: tieroute_group's since lists someone who isn't a member"))
	}
	if len(ext.Admins) == 0 {
		return refuse(errors.New("chat: a group chat without an admin"))
	}
	for _, a := range ext.Admins {
		if !ids[a] {
			return refuse(errors.New("chat: an admin who isn't a member"))
		}
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if err := e.c.RegisterGroup(ctx, d.Group); err != nil {
		e.log.Info("chat: registering the group", "err", err)
	}
	for id := range ids {
		if e.st.Handles[id] == "" && id != e.c.ID() {
			e.fetchChain(ctx, id, false)
		}
	}
	ch := &Chat{ID: d.Group, Kind: KindGroup, Hub: ext.Hub, Members: members(st), Admins: slices.Clone(ext.Admins), Epoch: st.Epoch}
	if old, ok := e.st.Chats[d.Group]; ok {
		ch.Name, ch.Topic, ch.LastActivity, ch.Preview, ch.Unread = old.Name, old.Topic, old.LastActivity, old.Preview, old.Unread
	}
	ch.LastActivity = max(ch.LastActivity, e.c.Clock().Now().Unix())
	e.st.Chats[ch.ID] = ch
	e.st.Expected = slices.DeleteFunc(e.st.Expected, func(s string) bool { return s == ch.ID })
	if err := e.save(); err != nil {
		return err
	}
	if d.From == e.c.ID() {
		e.systemLine(ch, "This device joined the group")
	} else {
		e.systemLine(ch, e.nameOf(d.From)+" added you")
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	if err := e.topUp(ctx); err != nil {
		e.log.Debug("chat: key packages", "err", err)
	}
	return nil
}

// members returns the identities in a group's tree.
func members(st *mls.State) []string {
	var ids []string
	for _, m := range st.Members {
		if id, _, err := parseCredential(m.Identity); err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// chainForBasis returns this device's copy of an identity's chain entries
// for judging a basis, fetching it when the copy doesn't reach the tail.
func (e *Engine) chainForBasis(ctx context.Context, id string, b BasisRef) []wire.Signed {
	first := b.Seq + 1 - uint64(len(b.Tail))
	c, _ := e.c.CachedChain(id)
	if c != nil && uint64(c.Len()) >= first {
		return c.Entries
	}
	if c, err := e.fetchChain(ctx, id, true); err == nil && c != nil {
		return c.Entries
	}
	return nil
}
