package chat

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Joins that need approval (§16.6): the joiner asks, and a member with
// moderate commits the Add with a signed add-approval in the members
// group's tieroute_group, sends the Welcome, and then the keys.

// ErrAskedToJoin is what JoinCommunity returns for a community whose joins
// need approval: the request went out, and a moderator has to approve it.
var ErrAskedToJoin = errors.New("chat: you asked to join. A moderator has to approve it")

// askToJoin asks a community whose joins need approval to let this
// identity in. It leaves key packages for this device first, and follows
// the community server's stream for the Welcome. The caller holds e.mu.
func (e *Engine) askToJoin(ctx context.Context, conn *client.Community, cm *Community) error {
	cm.Asking = true
	e.st.Communities[cm.Server] = cm
	if err := e.save(); err != nil {
		return err
	}
	if err := e.topUpCommunity(ctx, cm); err != nil {
		return fmt.Errorf("chat: leaving key packages with the community server: %w", err)
	}
	if err := conn.RequestJoin(ctx); err != nil {
		return err
	}
	go e.startCommunity(e.runCtx, cm.Server)
	return ErrAskedToJoin
}

// JoinRequests lists who's asking to join a community, by identifier,
// for a member with moderate. Their handles come from their chains.
func (e *Engine) JoinRequests(ctx context.Context, server string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, _, err := e.communityOf(server); err != nil {
		return nil, err
	}
	reqs, err := e.c.Community(server).JoinRequests(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range reqs {
		if !identity.ValidIdentifier(r.ID) {
			continue
		}
		if e.st.Handles[r.ID] == "" {
			e.fetchChain(ctx, r.ID, false)
		}
		out = append(out, r.ID)
	}
	return out, nil
}

// DeclineJoin declines someone's request to join.
func (e *Engine) DeclineJoin(ctx context.Context, server, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, _, err := e.communityOf(server); err != nil {
		return err
	}
	return e.c.Community(server).DeclineJoin(ctx, id)
}

// ApproveJoin lets someone who asked into the community (§16.6): it
// claims their key packages, commits the Add to the members group with
// this identity's signed approval, binding both identities' histories,
// and sends them the keys once the Welcome is out.
func (e *Engine) ApproveJoin(ctx context.Context, server, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, c, err := e.communityOf(server)
	if err != nil {
		return err
	}
	st := c.State()
	me := e.c.ID()
	if !st.Can(me, community.PermModerate) {
		return errors.New("chat: approving joins needs moderate")
	}
	if st.Banned(id, "") {
		return errors.New("chat: they're banned from this community")
	}
	g := cm.Groups[cm.MembersGroup]
	if g == nil {
		return errors.New("chat: this device isn't in the members group")
	}
	mg := e.dev.mls.Group(mustB64(g.ID))
	gst, err := mg.State()
	if err != nil {
		return err
	}
	if _, in := leavesByIdentity(gst)[id]; in {
		return errors.New("chat: they're a member already")
	}
	chain, err := e.fetchChain(ctx, id, true)
	if err != nil {
		return err
	}
	conn := e.c.Community(server)
	kps, err := conn.ApproveJoin(ctx, id)
	if err != nil {
		return err
	}
	var adds [][]byte
	var devs []wire.Bytes
	for _, kp := range kps {
		b, err := e.checkClaimed(kp, id, chain)
		if err != nil {
			e.log.Info("chat: refused a joiner's key package", "err", err)
			continue
		}
		adds = append(adds, b)
		devs = append(devs, kp.Dev)
	}
	if len(adds) == 0 {
		return errors.New("chat: none of their devices left a key package with the community server")
	}
	data, _ := gst.Extension(ExtGroup)
	old, err := ParseGroupExt(data)
	if err != nil {
		return err
	}
	next := old.Clone()
	now := e.c.Clock().Now()
	if next.Used != nil {
		// Drop approvals an hour past their time (§11.8, "Clock margins").
		for k, ts := range next.Used {
			if ts+3600 < now.Unix() {
				delete(next.Used, k)
			}
		}
	}
	a := AddApproval{Group: mustB64(g.ID), Request: newMsgID(), Who: id, By: me, Epoch: gst.Epoch, Exp: now.Add(approvalLife).Unix()}
	signed, err := e.c.SignAsDevice(TypeAddApproval, a)
	if err != nil {
		return err
	}
	next.Approval = &signed
	if next.Used == nil {
		next.Used = map[string]int64{}
	}
	next.Used[a.Request] = a.Exp
	// The commit binds the joiner's and the approver's histories (§16.6).
	next.Basis = map[string]BasisRef{id: basisFor(chain), me: basisFor(e.c.Chain())}
	o := ccommitOpts{adds: adds, welcome: []string{id}, devs: devs, ext: next, head: e.headToName(ctx, cm, c), pending: e.hasPending(g)}
	if err := e.ccommit(ctx, cm, g, o); err != nil {
		return err
	}
	// The approver is online: it sends the keys right after the Welcome
	// (§16.6, "Keys after a code join").
	e.sendKeys(ctx, cm, g)
	delete(cm.KeysDue, id)
	return e.save()
}

// checkCommunityApproval makes the members' checks on an approval join
// (§16.6, "The approval in the commit"): an add-approval in the commit's
// tieroute_group for the one identity it adds, signed by the committer,
// who holds moderate at the commit's community_head, with a device of
// theirs in the history the commit binds, and the request newly used.
func (e *Engine) checkCommunityApproval(ctx context.Context, cm *Community, g *CGroup, old, next *GroupExt, st *community.State, committer string, newPeople []string, epoch uint64, receiptTS int64) error {
	if len(newPeople) != 1 || next.Approval == nil {
		return errors.New("an Add of a new member without an approval")
	}
	a, err := parseApproval(*next.Approval)
	if err != nil {
		return err
	}
	switch {
	case a.By != committer:
		return errors.New("the approval isn't the committer's")
	case !st.Can(a.By, community.PermModerate):
		return errors.New("the approval's signer doesn't hold moderate")
	case a.Who != newPeople[0]:
		return errors.New("the approval names someone the commit doesn't add")
	case !slices.Equal(a.Group, mustB64(g.ID)):
		return errors.New("the approval is for another group")
	case a.Epoch > epoch:
		return errors.New("the approval is from a later epoch")
	case a.Exp < receiptTS:
		return errors.New("the approval had expired when the community server took the commit")
	case a.Exp > receiptTS+int64((approvalLife+time.Hour)/time.Second):
		return errors.New("the approval lasts more than 7 days")
	}
	if _, used := old.Used[a.Request]; used {
		return errors.New("the approval was used before")
	}
	if next.Used[a.Request] != a.Exp {
		return errors.New("the commit doesn't add the approval's request to used")
	}
	bound := map[string]*identity.Chain{}
	for _, id := range []string{a.Who, a.By} {
		br, ok := next.Basis[id]
		if !ok {
			return fmt.Errorf("the basis leaves out %s", e.nameOf(id))
		}
		v, c, err := identity.JudgeBasis(id, identity.Basis{Bound: boundOf(br.Ref()), Tail: br.Tail}, e.chainForBasis(ctx, id, br), nil, identity.NoCheck, nil)
		if v != identity.Valid {
			if v == identity.Pending {
				return retryable{err}
			}
			return fmt.Errorf("the history the commit binds for %s: %v", e.nameOf(id), err)
		}
		bound[id] = c
	}
	d, ok := bound[a.By].State.Device(next.Approval.K)
	if !ok {
		return errors.New("the approval isn't signed by a device of the moderator")
	}
	if err := sig.VerifyDevice(*next.Approval, d.Pub); err != nil {
		return fmt.Errorf("the approval's signature: %w", err)
	}
	return nil
}

// joinedByApproval finishes an approval join on the joiner's side, once
// the Welcome to the members group arrived. The caller holds e.mu.
func (e *Engine) joinedByApproval(cm *Community) {
	if !cm.Asking {
		return
	}
	cm.Asking = false
	e.save()
	e.refreshChannels(cm)
}

// declinedJoin forgets a community that declined this identity's request.
// The caller holds e.mu.
func (e *Engine) declinedJoin(cm *Community) {
	if !cm.Asking {
		return
	}
	delete(e.st.Communities, cm.Server)
	e.save()
	for id, ch := range maps.Clone(e.st.Chats) {
		if ch.Community == cm.Server {
			delete(e.st.Chats, id)
			e.emit(Event{Kind: EventChatRemoved, ChatID: id})
		}
	}
}

// joinRequested tells a moderator that someone asked to join, with a line
// in the community's first text channel. The caller holds e.mu.
func (e *Engine) joinRequested(ctx context.Context, cm *Community, n *client.Notice) {
	if !identity.ValidIdentifier(n.ID) {
		return
	}
	if e.st.Handles[n.ID] == "" {
		e.fetchChain(ctx, n.ID, false)
	}
	if ch := e.firstChannel(cm); ch != nil {
		e.systemLine(ch, e.nameOf(n.ID)+" asked to join. Type :approve-join or :decline-join with their handle")
	}
}

// firstChannel is the community's first text channel this device shows.
func (e *Engine) firstChannel(cm *Community) *Chat {
	var first *Chat
	for _, ch := range e.st.Chats {
		if ch.Kind == KindChannel && ch.Community == cm.Server && !ch.Voice && (first == nil || ch.Order < first.Order) {
			first = ch
		}
	}
	return first
}

// SetApproval turns on or off whether joins to a community need a
// moderator's approval, keeping whether public codes are allowed
// (§16.2.3, §16.6). It needs administrate.
func (e *Engine) SetApproval(ctx context.Context, server string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, _, err := e.communityOf(server)
	if err != nil {
		return err
	}
	return e.appendEntry(ctx, cm, func(st *community.State, _ uint64) ([]map[string]any, error) {
		if !st.Can(e.c.ID(), community.PermAdministrate) {
			return nil, errors.New("chat: changing the settings needs administrate")
		}
		return []map[string]any{{"op": community.OpSetSettings, "public": st.Public, "approval": on}}, nil
	})
}
