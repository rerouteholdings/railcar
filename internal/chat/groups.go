package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// A group chat's limits (§11.10).
const (
	MaxGroupPeople = 25
	MaxGroupLeaves = 250
)

// MaxGroupName bounds a group's name and topic as this client sends them.
// The spec sets no limit; these keep a group-meta well inside a delivery.
const (
	MaxGroupName  = 100
	MaxGroupTopic = 500
)

// TypeAddApproval is the type tag of an add approval (§11.8).
const TypeAddApproval = "add-approval"

// AddApproval is the body of an add approval, signed by a current device
// of the approving admin (§11.8).
type AddApproval struct {
	Group   wire.Bytes `json:"group"`
	Request string     `json:"request"`
	Who     string     `json:"who"`
	By      string     `json:"by"`
	Epoch   uint64     `json:"epoch"`
	Exp     int64      `json:"exp"`
}

// approvalLife is how long an approval this client issues lasts: 7 days,
// the most §11.8 allows.
const approvalLife = 7 * 24 * time.Hour

func parseApproval(s wire.Signed) (AddApproval, error) {
	var a AddApproval
	if s.T != TypeAddApproval {
		return a, fmt.Errorf("chat: an approval of type %q", s.T)
	}
	if err := s.Body(&a); err != nil {
		return a, fmt.Errorf("chat: the approval: %w", err)
	}
	if !identity.ValidIdentifier(a.Who) || !identity.ValidIdentifier(a.By) || !validMsgID(a.Request) {
		return a, errors.New("chat: a malformed approval")
	}
	return a, nil
}

// checkApproval makes §11.8's checks on the approval a commit uses to add
// a new person: every member reaches the same verdict from the commit
// alone.
func checkApproval(s wire.Signed, a AddApproval, group []byte, old, next *GroupExt, newPeople map[string]bool, epoch uint64, receiptTS int64, byChain *identity.Chain) error {
	switch {
	case !bytes.Equal(a.Group, group):
		return errors.New("the approval is for another group")
	case !newPeople[a.Who]:
		return errors.New("the approval names someone the commit doesn't add")
	case !slices.Contains(old.Admins, a.By):
		return errors.New("the approval's signer isn't an admin")
	case a.Epoch > epoch:
		return errors.New("the approval is from a later epoch")
	case a.Exp < receiptTS:
		return errors.New("the approval had expired when the hub took the commit")
	case a.Exp > receiptTS+int64((approvalLife+time.Hour)/time.Second):
		return errors.New("the approval lasts more than 7 days")
	}
	if _, used := old.Used[a.Request]; used {
		return errors.New("the approval was used before")
	}
	if next.Used[a.Request] != a.Exp {
		return errors.New("the commit doesn't add the approval's request to used")
	}
	if byChain == nil {
		return errors.New("the commit doesn't bind the approver's history")
	}
	d, ok := byChain.State.Device(s.K)
	if !ok {
		return errors.New("the approval isn't signed by a device of the admin")
	}
	if err := sig.VerifyDevice(s, d.Pub); err != nil {
		return fmt.Errorf("the approval's signature: %w", err)
	}
	return nil
}

// groupMembers lists a group's member identities from its tree.
func (e *Engine) groupState(ch *Chat) (*mls.Group, *mls.State, *GroupExt, error) {
	g := e.dev.mls.Group(mustB64(ch.ID))
	st, err := g.State()
	if err != nil {
		return nil, nil, nil, err
	}
	data, _ := st.Extension(ExtGroup)
	ext, err := ParseGroupExt(data)
	if err != nil {
		return nil, nil, nil, err
	}
	return g, st, ext, nil
}

// bindFor sets next's basis and heads for a commit this device makes:
// the identities in scope, and every member after it when the group
// hasn't latched yet, which latches it (§7.1). chains holds a fresh chain
// for each identity named. Members who leave lose their heads.
func bindFor(old, next *GroupExt, scope []string, after map[string]bool, chains map[string]*identity.Chain) error {
	names := slices.Clone(scope)
	if len(old.Heads) == 0 {
		names = slices.Collect(maps.Keys(after))
	}
	next.Basis, next.Heads = map[string]BasisRef{}, map[string]ChainRef{}
	for id, h := range old.Heads {
		if after[id] {
			next.Heads[id] = h
		}
	}
	for _, id := range uniq(names) {
		c := chains[id]
		if c == nil {
			return fmt.Errorf("chat: no chain for %s", id)
		}
		b := basisFor(c)
		var floor *identity.Bound
		if f, ok := old.Heads[id]; ok {
			fb := boundOf(f)
			floor = &fb
		}
		// Judge it as members will (§7.1, "Committing").
		if v, _, err := identity.JudgeBasis(id, identity.Basis{Bound: boundOf(b.Ref()), Tail: b.Tail}, c.Entries, floor, identity.NoCheck, nil); v != identity.Valid {
			return fmt.Errorf("chat: %s's history conflicts with what the chat already bound: %v", id, err)
		}
		next.Basis[id] = b
		if after[id] {
			next.Heads[id] = b.Ref()
		}
	}
	if len(next.Basis) == 0 {
		next.Basis = nil
	}
	if len(next.Heads) == 0 {
		next.Heads = nil
	}
	return nil
}

// settle drops people who are no longer members from since and admins,
// and picks an admin when none is left (§11.8).
func settleExt(next *GroupExt, after map[string]bool) {
	for id := range next.Since {
		if !after[id] {
			delete(next.Since, id)
		}
	}
	if next.Kind != KindGroup {
		return
	}
	next.Admins = slices.DeleteFunc(next.Admins, func(a string) bool { return !after[a] })
	if len(next.Admins) == 0 && len(after) > 0 {
		next.Admins = []string{pickAdmin(next.Since, after)}
	}
}

// chainsFor fetches a fresh chain for each identity.
func (e *Engine) chainsFor(ctx context.Context, ids []string) (map[string]*identity.Chain, error) {
	out := map[string]*identity.Chain{}
	for _, id := range uniq(ids) {
		c, err := e.syncChain(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

// claimFor claims and checks a key package for each current device of an
// identity, leaving out this device. It returns the packages to add.
func (e *Engine) claimFor(ctx context.Context, id string, chain *identity.Chain) ([][]byte, error) {
	opts := client.ClaimOptions{}
	if id == e.c.ID() {
		for _, d := range chain.State.Devices {
			if !bytes.Equal(d.Key, e.c.DeviceKey()) {
				opts.Devices = append(opts.Devices, d.Key)
			}
		}
		if len(opts.Devices) == 0 {
			return nil, nil
		}
	}
	claimed, err := e.c.ClaimKeyPackages(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, kp := range claimed {
		if bytes.Equal(kp.Dev, e.c.DeviceKey()) {
			continue
		}
		b, err := e.checkClaimed(kp, id, chain)
		if err != nil {
			e.log.Info("chat: refused a claimed key package", "err", err)
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// CreateGroup creates a group chat with some of this identity's contacts
// (§11.5): a new MLS group with this homeserver as hub and this identity
// as its first admin, every current device of each person, and a
// group-meta with its name (§12). People none of whose devices has a key
// package are left out, and come back as an error beside the chat.
func (e *Engine) CreateGroup(ctx context.Context, name string, people []string) (Chat, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createGroup(ctx, name, people, "")
}

// createGroup creates a group chat; replaces names the group it
// re-creates, if any (§11.11). The caller holds e.mu.
func (e *Engine) createGroup(ctx context.Context, name string, people []string, replaces string) (Chat, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxGroupName {
		return Chat{}, errors.New("chat: a group needs a name of 1 to 100 characters")
	}
	people = uniq(people)
	if len(people) == 0 {
		return Chat{}, errors.New("chat: a group needs at least one other person")
	}
	if len(people)+1 > MaxGroupPeople {
		return Chat{}, errors.New("chat: a group chat has at most 25 people")
	}
	me := e.c.ID()
	for _, id := range people {
		c := e.st.Contacts[id]
		if id == me || c == nil || c.State != StateContact {
			return Chat{}, fmt.Errorf("chat: %s isn't a contact", e.nameOf(id))
		}
	}
	chains, err := e.chainsFor(ctx, append([]string{me}, people...))
	if err != nil {
		return Chat{}, err
	}
	var adds [][]byte
	var welcomeTo []string
	var missing []string
	for _, id := range append([]string{me}, people...) {
		kps, err := e.claimFor(ctx, id, chains[id])
		if err != nil {
			e.log.Info("chat: claiming key packages", "err", err)
		}
		if len(kps) == 0 {
			if id != me {
				missing = append(missing, id)
			}
			continue
		}
		adds = append(adds, kps...)
		welcomeTo = append(welcomeTo, id)
	}
	if len(slices.DeleteFunc(slices.Clone(welcomeTo), func(id string) bool { return id == me })) == 0 {
		return Chat{}, errors.New("chat: nobody in the group has a key package Railcar can use")
	}

	// Create and register the group, with a new ID if it's taken (§11.5).
	ext := &GroupExt{Kind: KindGroup, Hub: e.c.Server(), Admins: []string{me}, Since: map[string]uint64{me: 0}}
	if replaces != "" {
		ext.Replaces = mustB64(replaces)
	}
	g, gid, err := e.newGroup(ctx, KindGroup, ext)
	if err != nil {
		return Chat{}, err
	}
	st, err := g.State()
	if err != nil {
		return Chat{}, err
	}
	data, _ := st.Extension(ExtGroup)
	old, _ := ParseGroupExt(data)
	next := old.Clone()
	after := map[string]bool{me: true}
	for _, id := range welcomeTo {
		after[id] = true
		if id != me {
			next.Since[id] = st.Epoch + 1
		}
	}
	if err := bindFor(old, next, slices.Collect(maps.Keys(after)), after, chains); err != nil {
		return Chat{}, err
	}
	ch := &Chat{ID: wire.EncodeB64(gid), Kind: KindGroup, Hub: e.c.Server(), Members: []string{me}, Admins: []string{me}, Epoch: st.Epoch, Name: name}
	// Before the creating commit, the group is this identity alone; the
	// rest get only the Welcome (§10.2).
	to := []string{me}
	if err := e.commit(ctx, ch, g, mls.CommitOptions{Adds: adds}, next, to, welcomeTo); err != nil {
		g.Delete()
		e.dev.Save()
		return Chat{}, err
	}
	after2, _ := g.State()
	ch.Members = members(after2)
	ch.LastActivity = e.c.Clock().Now().Unix()
	e.st.Chats[ch.ID] = ch
	if err := e.save(); err != nil {
		return Chat{}, err
	}
	e.systemLine(ch, "You created the group")
	if err := e.sendMeta(ctx, ch); err != nil {
		e.log.Info("chat: the group's name", "err", err)
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	if len(missing) > 0 {
		return ch.public(), &MissingError{People: e.namesOf(missing)}
	}
	return ch.public(), nil
}

// MissingError reports people left out of a group chat because none of
// their devices had a key package.
type MissingError struct{ People []string }

func (m *MissingError) Error() string {
	return "Railcar couldn't add " + strings.Join(m.People, ", ") + ": none of their devices had a key package"
}

// newGroup creates an MLS group with a tieroute_group and registers it
// with this homeserver as hub, picking a new ID if it's taken (§11.5).
func (e *Engine) newGroup(ctx context.Context, kind string, ext *GroupExt) (*mls.Group, []byte, error) {
	for attempt := 0; ; attempt++ {
		gid := make([]byte, 32)
		rand.Read(gid)
		x, err := ext.Extension()
		if err != nil {
			return nil, nil, err
		}
		g, err := e.dev.mls.CreateGroup(e.dev.signer, mls.CreateOptions{
			GroupID: gid, Leaf: e.dev.Leaf(), Config: chatConfig,
			Extensions: []mls.Extension{x, mls.RequiredCapabilities([]uint16{ExtGroup}, nil, nil)},
		})
		if err != nil {
			return nil, nil, err
		}
		ek, err := e.dev.EpochKey(g)
		if err != nil {
			g.Delete()
			return nil, nil, err
		}
		err = e.c.CreateGroup(ctx, client.CreateGroupOptions{Group: wire.EncodeB64(gid), Kind: kind, EpochKey: ek.Public()})
		if wire.IsCode(err, wire.CodeAlreadyExists) && attempt < 3 {
			g.Delete()
			continue
		}
		if err != nil {
			g.Delete()
			return nil, nil, err
		}
		if err := e.dev.Save(); err != nil {
			return nil, nil, err
		}
		return g, gid, nil
	}
}

// groupChat returns a group chat this device can change now.
func (e *Engine) groupChat(chat string) (*Chat, error) {
	ch, err := e.sendable(chat)
	if err != nil {
		return nil, err
	}
	if ch.Kind != KindGroup {
		return nil, errors.New("chat: that's a direct chat, not a group chat")
	}
	return ch, nil
}

// AddMember adds someone to a group chat. An admin adds any of their own
// contacts directly; another member sends an add-request, and the Add
// follows once an admin approves (§11.8). It reports whether the person
// was added now.
func (e *Engine) AddMember(ctx context.Context, chat, id string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return false, err
	}
	if slices.Contains(ch.Members, id) {
		return false, errors.New("chat: they're already in the group")
	}
	if c := e.st.Contacts[id]; c == nil || c.State != StateContact {
		// The Welcome comes from the member who asked, and their
		// homeserver takes it only from a contact (§10.3, §11.8).
		return false, fmt.Errorf("chat: %s isn't your contact", e.nameOf(id))
	}
	if len(ch.Members) >= MaxGroupPeople {
		return false, errors.New("chat: a group chat has at most 25 people")
	}
	if slices.Contains(ch.Admins, e.c.ID()) {
		return true, e.commitAdd(ctx, ch, id, nil)
	}
	now := e.c.Clock().Now().Unix()
	req := AddRequest{T: CAddRequest, ID: newMsgID(), TS: now, Who: id}
	if err := e.sendContent(ctx, ch, req, false); err != nil {
		return false, err
	}
	ch.Requests = append(ch.Requests, AddPending{ID: req.ID, From: e.c.ID(), Who: id, TS: now})
	e.systemLine(ch, "You asked the admins to add "+e.nameOf(id))
	return false, e.save()
}

// commitAdd commits the Add of a new person to a group chat, with an
// approval when this identity isn't an admin (§11.8), then sends the
// group's metadata so they get its name (§12).
func (e *Engine) commitAdd(ctx context.Context, ch *Chat, id string, approval *wire.Signed) error {
	g, st, old, err := e.groupState(ch)
	if err != nil {
		return err
	}
	me := e.c.ID()
	scope := []string{me, id}
	var appr AddApproval
	if approval != nil {
		if appr, err = parseApproval(*approval); err != nil {
			return err
		}
		if time.Unix(appr.Exp, 0).Sub(e.c.Clock().Now()) < time.Hour {
			// An approval this close to expiring may expire before the
			// hub's receipt (§11.8, "Clock margins").
			return errors.New("chat: the approval expires within the hour; ask again")
		}
		scope = append(scope, appr.By)
	}
	after := map[string]bool{}
	for _, m := range ch.Members {
		after[m] = true
	}
	after[id] = true
	ids := slices.Collect(maps.Keys(after))
	if approval != nil {
		ids = append(ids, appr.By)
	}
	chains, err := e.chainsFor(ctx, append(ids, scope...))
	if err != nil {
		return err
	}
	kps, err := e.claimFor(ctx, id, chains[id])
	if err != nil {
		return err
	}
	if len(kps) == 0 {
		return fmt.Errorf("chat: none of %s's devices has a key package", e.nameOf(id))
	}
	next := old.Clone()
	next.Since[id] = st.Epoch + 1
	now := e.c.Clock().Now().Unix()
	if next.Used != nil {
		// Drop approvals an hour past their time (§11.8, "Clock margins").
		for k, ts := range next.Used {
			if ts+3600 < now {
				delete(next.Used, k)
			}
		}
	}
	if approval != nil {
		next.Approval = approval
		if next.Used == nil {
			next.Used = map[string]int64{}
		}
		next.Used[appr.Request] = appr.Exp
	} else {
		next.Approval = nil
	}
	if len(next.Used) == 0 {
		next.Used = nil
	}
	if err := bindFor(old, next, scope, after, chains); err != nil {
		return err
	}
	// The commit goes to the members before it; the new person gets
	// only the Welcome (§10.2).
	to := slices.Sorted(slices.Values(ch.Members))
	if err := e.commit(ctx, ch, g, mls.CommitOptions{Adds: kps}, next, to, []string{id}); err != nil {
		return err
	}
	afterSt, _ := g.State()
	ch.Members, ch.Admins = members(afterSt), slices.Clone(next.Admins)
	ch.Requests = slices.DeleteFunc(ch.Requests, func(r AddPending) bool { return r.Who == id })
	if err := e.save(); err != nil {
		return err
	}
	e.systemLine(ch, "You added "+e.nameOf(id))
	if err := e.sendMeta(ctx, ch); err != nil {
		e.log.Info("chat: the group's metadata for a new member", "err", err)
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// AnswerAdd answers an add-request as an admin: with a signed approval, or
// declined (§11.8). The member who asked commits the Add.
func (e *Engine) AnswerAdd(ctx context.Context, chat, request string, approve bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	me := e.c.ID()
	if !slices.Contains(ch.Admins, me) {
		return errors.New("chat: only an admin can answer an add-request")
	}
	i := slices.IndexFunc(ch.Requests, func(r AddPending) bool { return r.ID == request })
	if i < 0 {
		return errors.New("chat: no such add-request")
	}
	r := ch.Requests[i]
	now := e.c.Clock().Now()
	ans := AddAnswer{T: CAddAnswer, TS: now.Unix(), Target: r.ID}
	if approve {
		a, err := e.c.SignAsDevice(TypeAddApproval, AddApproval{
			Group: mustB64(ch.ID), Request: r.ID, Who: r.Who, By: me, Epoch: ch.Epoch, Exp: now.Add(approvalLife).Unix(),
		})
		if err != nil {
			return err
		}
		ans.Approval = &a
	}
	if err := e.sendContent(ctx, ch, ans, false); err != nil {
		return err
	}
	ch.Requests = slices.Delete(ch.Requests, i, i+1)
	if approve {
		e.systemLine(ch, "You approved adding "+e.nameOf(r.Who))
	} else {
		e.systemLine(ch, "You declined adding "+e.nameOf(r.Who))
	}
	return e.save()
}

// RemoveMember removes someone from a group chat: every leaf they have
// (§11.8). Only an admin can.
func (e *Engine) RemoveMember(ctx context.Context, chat, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	me := e.c.ID()
	if !slices.Contains(ch.Admins, me) {
		return errors.New("chat: only an admin can remove someone")
	}
	if id == me {
		return errors.New("chat: to leave the group, leave it")
	}
	g, st, old, err := e.groupState(ch)
	if err != nil {
		return err
	}
	var removes []uint32
	for _, l := range leavesByIdentity(st)[id] {
		removes = append(removes, l.index)
	}
	if len(removes) == 0 {
		return errors.New("chat: they aren't in the group")
	}
	after := map[string]bool{}
	for _, m := range ch.Members {
		if m != id {
			after[m] = true
		}
	}
	next := old.Clone()
	settleExt(next, after)
	chains, err := e.chainsFor(ctx, slices.Collect(maps.Keys(after)))
	if err != nil {
		return err
	}
	if err := bindFor(old, next, []string{me}, after, chains); err != nil {
		return err
	}
	if err := e.commit(ctx, ch, g, mls.CommitOptions{Removes: removes}, next, ch.Members, nil); err != nil {
		return err
	}
	afterSt, _ := g.State()
	ch.Members, ch.Admins = members(afterSt), slices.Clone(next.Admins)
	if err := e.save(); err != nil {
		return err
	}
	e.systemLine(ch, "You removed "+e.nameOf(id))
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// SetAdmin grants or revokes admin for a member of a group chat (§11.8).
// Only an admin can, and the group always keeps one.
func (e *Engine) SetAdmin(ctx context.Context, chat, id string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	return e.setAdmins(ctx, ch, func(admins []string) []string {
		if on {
			return append(admins, id)
		}
		return slices.DeleteFunc(admins, func(a string) bool { return a == id })
	}, id, on)
}

func (e *Engine) setAdmins(ctx context.Context, ch *Chat, change func([]string) []string, who string, on bool) error {
	me := e.c.ID()
	if !slices.Contains(ch.Admins, me) {
		return errors.New("chat: only an admin can change who's an admin")
	}
	if !slices.Contains(ch.Members, who) {
		return errors.New("chat: they aren't in the group")
	}
	g, _, old, err := e.groupState(ch)
	if err != nil {
		return err
	}
	next := old.Clone()
	next.Admins = uniq(change(slices.Clone(old.Admins)))
	if len(next.Admins) == 0 {
		return errors.New("chat: a group always keeps an admin")
	}
	if sameSet(next.Admins, old.Admins) {
		return nil
	}
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
	if err := e.commit(ctx, ch, g, mls.CommitOptions{}, next, ch.Members, nil); err != nil {
		return err
	}
	ch.Admins = slices.Clone(next.Admins)
	if err := e.save(); err != nil {
		return err
	}
	switch {
	case who == me && !on:
		e.systemLine(ch, "You're no longer an admin")
	case on:
		e.systemLine(ch, "You made "+e.nameOf(who)+" an admin")
	default:
		e.systemLine(ch, e.nameOf(who)+" is no longer an admin")
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// Leave leaves a group chat (§11.7). The last admin first hands admin to
// the longest-standing member (§11.8). Then this device proposes removing
// each of the identity's leaves, which the next committer includes, and
// stops receiving the group. The history stays on this device.
func (e *Engine) Leave(ctx context.Context, chat string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	me := e.c.ID()
	others := map[string]bool{}
	for _, m := range ch.Members {
		if m != me {
			others[m] = true
		}
	}
	if len(others) > 0 && slices.Equal(ch.Admins, []string{me}) {
		_, _, ext, err := e.groupState(ch)
		if err != nil {
			return err
		}
		heir := pickAdmin(ext.Since, others)
		if err := e.setAdmins(ctx, ch, func([]string) []string { return []string{heir} }, heir, true); err != nil {
			return fmt.Errorf("chat: handing admin over before leaving: %w", err)
		}
	}
	g, st, _, err := e.groupState(ch)
	if err != nil {
		return err
	}
	// This device's own leaf first, then the identity's other leaves.
	leaves := leavesByIdentity(st)[me]
	slices.SortFunc(leaves, func(a, b leafOf) int {
		return boolInt(b.index == st.OwnLeafIndex) - boolInt(a.index == st.OwnLeafIndex)
	})
	var outs []client.Outgoing
	for _, l := range leaves {
		p, err := g.ProposeRemove(e.dev.signer, l.index, nil)
		if err != nil {
			return err
		}
		epoch := ch.Epoch
		outs = append(outs, client.Outgoing{ID: client.NewMsgID(), Kind: "mls", To: ch.Members, Group: ch.ID, Epoch: &epoch, MLS: "proposal", Data: p.Message})
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	res, err := e.c.Send(ctx, outs...)
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Status != "sent" {
			return &wire.Error{Code: r.Error, Msg: "the homeserver refused the leave"}
		}
	}
	e.leftGroup(ctx, ch, "You left the group")
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// leftGroup stops following a group chat this identity left or was
// removed from: it stops receiving it and deletes its MLS state, and keeps
// the history.
func (e *Engine) leftGroup(ctx context.Context, ch *Chat, line string) {
	if err := e.c.UnregisterGroup(ctx, ch.ID); err != nil {
		e.log.Info("chat: unregistering a group", "err", err)
	}
	e.dev.mls.Group(mustB64(ch.ID)).Delete()
	e.dev.Save()
	ch.Left, ch.Stopped, ch.Buffered, ch.Requests = true, line, nil, nil
	e.systemLine(ch, line)
	e.save()
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
}

// commitPending commits the standalone proposals waiting in a group: a
// member's leave (§11.7). Some MLS libraries, OpenMLS among them, send
// nothing while one waits, so members commit them promptly (§11.1).
func (e *Engine) commitPending(ctx context.Context, ch *Chat) error {
	g, st, old, err := e.groupState(ch)
	if err != nil {
		return err
	}
	if len(st.PendingProposals) == 0 {
		return nil
	}
	gone := map[uint32]bool{}
	for _, p := range st.PendingProposals {
		if p.Kind == "remove" && p.Removed != nil {
			gone[*p.Removed] = true
		}
	}
	if gone[st.OwnLeafIndex] {
		// This identity is leaving from another device: stop here too.
		e.leftGroup(ctx, ch, "You left the group from another device")
		return nil
	}
	after := map[string]bool{}
	for _, m := range st.Members {
		if !gone[m.LeafIndex] {
			if id, _, err := parseCredential(m.Identity); err == nil {
				after[id] = true
			}
		}
	}
	next := old.Clone()
	settleExt(next, after)
	scope := []string{}
	if !sameSet(old.Admins, next.Admins) {
		scope = append(scope, e.c.ID())
	}
	chains, err := e.chainsFor(ctx, slices.Collect(maps.Keys(after)))
	if err != nil {
		return err
	}
	if err := bindFor(old, next, scope, after, chains); err != nil {
		return err
	}
	before := ch.Members
	if err := e.commit(ctx, ch, g, mls.CommitOptions{PendingProposals: true}, next, before, nil); err != nil {
		if wire.IsCode(err, wire.CodeStaleEpoch) {
			// Another member committed first; theirs comes through the
			// inbox, or from the hub.
			e.fetchMissed(ctx, ch)
			return nil
		}
		return err
	}
	afterSt, _ := g.State()
	ch.Members, ch.Admins = members(afterSt), slices.Clone(next.Admins)
	if err := e.save(); err != nil {
		return err
	}
	e.membershipLines(ch, "", before, ch.Members, nil, map[string]bool{})
	for _, id := range before {
		if !after[id] {
			e.systemLine(ch, e.nameOf(id)+" left")
		}
	}
	e.adminLines(ch, old.Admins, next.Admins)
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// SetGroupMeta renames a group chat or changes its topic, and sends the
// whole state as group-meta (§12). Any member can.
func (e *Engine) SetGroupMeta(ctx context.Context, chat string, name, topic *string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" || utf8.RuneCountInString(n) > MaxGroupName {
			return errors.New("chat: a group's name is 1 to 100 characters")
		}
		ch.Name = n
	}
	if topic != nil {
		t := strings.TrimSpace(*topic)
		if utf8.RuneCountInString(t) > MaxGroupTopic {
			return errors.New("chat: a group's topic is at most 500 characters")
		}
		ch.Topic = t
	}
	if err := e.sendMeta(ctx, ch); err != nil {
		return err
	}
	if name != nil {
		e.systemLine(ch, "You renamed the group to "+ch.Name)
	}
	if topic != nil {
		e.systemLine(ch, "You changed the topic")
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// sendMeta sends a group chat's name and topic as group-meta. The picture
// key is left out, which keeps the current one (§12).
func (e *Engine) sendMeta(ctx context.Context, ch *Chat) error {
	name, topic := ch.Name, ch.Topic
	m := GroupMeta{T: CGroupMeta, TS: e.c.Clock().Now().Unix(), Name: &name}
	if topic != "" {
		m.Topic = &topic
	}
	if ch.Picture {
		m.Avatar = e.metaPicture(ch)
	} else if ch.PictureRemoved {
		// Only a removal says null: a member who hasn't seen another's
		// picture yet leaves it out, so it doesn't clear it.
		m.Avatar = []byte("null")
	}
	return e.sendContent(ctx, ch, m, false)
}

// applyGroupMeta applies a group-meta from a member.
func (e *Engine) applyGroupMeta(ch *Chat, from string, m GroupMeta) error {
	if ch.Kind != KindGroup {
		return nil
	}
	who := e.nameOf(from)
	if m.Name != nil && *m.Name != ch.Name {
		renamed := ch.Name != ""
		ch.Name = *m.Name
		if renamed {
			e.systemLine(ch, who+" renamed the group to "+ch.Name)
		}
	}
	topic := ""
	if m.Topic != nil {
		topic = *m.Topic
	}
	if topic != ch.Topic {
		ch.Topic = topic
		e.systemLine(ch, who+" changed the topic")
	}
	if len(m.Avatar) > 0 {
		var pic *GroupPicture
		if string(m.Avatar) != "null" {
			var p GroupPicture
			if err := wire.Unmarshal(m.Avatar, &p); err != nil || checkGroupPicture(p) != nil {
				return errors.New("chat: a group picture that isn't a PNG, JPEG, WebP, or GIF of at most 256 KiB")
			}
			pic = &p
		}
		old, had := e.groupPicture(ch.ID)
		changed := had != (pic != nil) || pic != nil && !bytes.Equal(old.Data, pic.Data)
		if err := e.keepGroupPicture(ch, pic); err != nil {
			return err
		}
		if changed && pic != nil {
			e.systemLine(ch, who+" changed the group's picture")
		} else if changed {
			e.systemLine(ch, who+" removed the group's picture")
		}
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// applyAddRequest records a member's add-request (§11.8).
func (e *Engine) applyAddRequest(ctx context.Context, ch *Chat, from string, r AddRequest) error {
	if ch.Kind != KindGroup || slices.ContainsFunc(ch.Requests, func(p AddPending) bool { return p.ID == r.ID }) {
		return nil
	}
	ch.Requests = append(ch.Requests, AddPending{ID: r.ID, From: from, Who: r.Who, TS: r.TS})
	e.fetchChain(ctx, r.Who, false)
	e.systemLine(ch, e.nameOf(from)+" wants to add "+e.nameOf(r.Who))
	return e.save()
}

// applyAddAnswer handles an admin's answer to an add-request. When it
// approves this identity's request, this device commits the Add (§11.8).
func (e *Engine) applyAddAnswer(ctx context.Context, ch *Chat, from string, a AddAnswer) error {
	if ch.Kind != KindGroup {
		return nil
	}
	i := slices.IndexFunc(ch.Requests, func(p AddPending) bool { return p.ID == a.Target })
	if i < 0 {
		return nil
	}
	r := ch.Requests[i]
	ch.Requests = slices.Delete(ch.Requests, i, i+1)
	if err := e.save(); err != nil {
		return err
	}
	if a.Approval == nil {
		if r.From == e.c.ID() {
			e.systemLine(ch, e.nameOf(from)+" declined adding "+e.nameOf(r.Who))
		}
		return nil
	}
	appr, err := parseApproval(*a.Approval)
	if err != nil || appr.Request != r.ID || appr.Who != r.Who || appr.By != from {
		return errors.New("chat: an add-answer whose approval doesn't match the request")
	}
	if r.From != e.c.ID() || slices.Contains(ch.Members, r.Who) {
		return nil
	}
	if err := e.commitAdd(ctx, ch, r.Who, a.Approval); err != nil {
		e.systemLine(ch, "Railcar couldn't add "+e.nameOf(r.Who)+": "+err.Error())
		return nil
	}
	return nil
}

// membershipLines adds the system lines for people a commit added or
// removed (§11.9, "System lines").
func (e *Engine) membershipLines(ch *Chat, committer string, before, after []string, added []string, leaving map[string]bool) {
	me := e.c.ID()
	by := e.nameOf(committer)
	for _, id := range added {
		switch {
		case id == me:
			e.systemLine(ch, by+" added you")
		default:
			e.systemLine(ch, by+" added "+e.nameOf(id))
		}
	}
	for _, id := range before {
		if slices.Contains(after, id) || committer == "" {
			continue
		}
		switch {
		case leaving[id]:
			e.systemLine(ch, e.nameOf(id)+" left")
		case id == me:
			e.systemLine(ch, by+" removed you")
		default:
			e.systemLine(ch, by+" removed "+e.nameOf(id))
		}
	}
}

// adminLines adds the system lines for a change of admins.
func (e *Engine) adminLines(ch *Chat, before, after []string) {
	me := e.c.ID()
	for _, id := range after {
		if !slices.Contains(before, id) {
			if id == me {
				e.systemLine(ch, "You're an admin now")
			} else {
				e.systemLine(ch, e.nameOf(id)+" is an admin now")
			}
		}
	}
	for _, id := range before {
		if !slices.Contains(after, id) && slices.Contains(ch.Members, id) {
			if id == me {
				e.systemLine(ch, "You're no longer an admin")
			} else {
				e.systemLine(ch, e.nameOf(id)+" is no longer an admin")
			}
		}
	}
}

// nameOf is how system lines name an identity: @handle, or "someone"
// when this device doesn't know it.
func (e *Engine) nameOf(id string) string {
	if id == "" {
		return "Someone"
	}
	if id == e.c.ID() {
		return "You"
	}
	if h := e.handleOf(id); h != "" {
		return "@" + h
	}
	return "someone"
}

func (e *Engine) namesOf(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = e.nameOf(id)
	}
	return out
}
