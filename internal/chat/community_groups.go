package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// MaxCommunityMembers is a community's cap (§16.1).
const MaxCommunityMembers = 1000

// cgroupItem handles one message for a community group, or buffers it
// for a later epoch (§11.6).
func (e *Engine) cgroupItem(ctx context.Context, cm *Community, g *CGroup, b buffered) error {
	switch {
	case b.MLS == "commit" && b.Epoch < g.Epoch:
		return nil // already applied, such as this device's own
	case b.Epoch > g.Epoch, g.Pending:
		if len(g.Buffered) >= maxBuffered {
			g.Buffered = g.Buffered[1:]
		}
		g.Buffered = append(g.Buffered, b)
		if b.Epoch > g.Epoch && !g.Pending {
			e.cfetchMissed(ctx, cm, g)
		}
		return e.save()
	case b.Epoch < g.Epoch && b.MLS == "app":
		// A late message: the epoch's secrets may still be here (§11.1).
	}
	switch b.MLS {
	case "commit":
		return e.cprocessCommit(ctx, cm, g, b)
	case "app":
		return e.cprocessApp(ctx, cm, g, b)
	case "proposal":
		return e.cprocessProposal(ctx, cm, g, b)
	}
	return nil
}

// cdrain applies buffered messages the group's epoch has reached, commits
// for an epoch first.
func (e *Engine) cdrain(ctx context.Context, cm *Community, g *CGroup) error {
	for range maxBuffered {
		if g.Pending || cm.Groups[g.ID] != g {
			return nil
		}
		i := slices.IndexFunc(g.Buffered, func(b buffered) bool { return b.Epoch == g.Epoch && b.MLS == "commit" })
		if i < 0 {
			i = slices.IndexFunc(g.Buffered, func(b buffered) bool { return b.Epoch <= g.Epoch })
		}
		if i < 0 {
			return nil
		}
		b := g.Buffered[i]
		g.Buffered = slices.Delete(g.Buffered, i, i+1)
		if err := e.cgroupItem(ctx, cm, g, b); err != nil {
			e.log.Info("chat: dropped a buffered community message", "err", err)
		}
	}
	return e.save()
}

// cfetchMissed fetches the commits the community server holds from this
// device's epoch on (§11.6).
func (e *Engine) cfetchMissed(ctx context.Context, cm *Community, g *CGroup) {
	after := uint64(0)
	if g.Epoch > 0 {
		after = g.Epoch - 1
	}
	commits, err := e.c.Community(cm.Server).Commits(ctx, mustB64(g.ID), after)
	if err != nil {
		e.log.Info("chat: fetching a community group's commits", "err", err)
		return
	}
	for _, c := range commits {
		if c.Epoch < g.Epoch {
			continue
		}
		r := c.Receipt
		b := buffered{Epoch: c.Epoch, MLS: "commit", Data: c.Data, Receipt: &r, Proof: c.Proof}
		if !slices.ContainsFunc(g.Buffered, func(x buffered) bool { return x.MLS == "commit" && bytes.Equal(x.Data, b.Data) }) {
			g.Buffered = append(g.Buffered, b)
		}
	}
}

// communityHead returns a group's community_head, if it has one.
func communityHead(exts []mls.Extension) ([]byte, bool) {
	for _, x := range exts {
		if x.Type == ExtCommunityHead {
			return x.Data, true
		}
	}
	return nil, false
}

// stateAtHead returns the community state as of the entry a
// community_head names, fetching the chain when this device doesn't have
// it yet (§16.2, "Judging with what it needs").
func (e *Engine) stateAtHead(ctx context.Context, cm *Community, head []byte) (*community.State, uint64, error) {
	c, _ := e.communityChain(cm)
	if c != nil {
		if seq, ok := c.Find(head); ok {
			st, _ := c.StateAt(seq)
			return st, seq, nil
		}
	}
	if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
		return nil, 0, err
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return nil, 0, err
	}
	seq, ok := c.Find(head)
	if !ok {
		return nil, 0, errors.New("chat: a commit names a community chain entry the community server can't produce")
	}
	st, _ := c.StateAt(seq)
	return st, seq, nil
}

// cjudged is what judging a community commit found.
type cjudged struct {
	joiner  string // an external commit's joiner
	added   []string
	removed []string
	head    []byte
	// backward is set for a commit that named an earlier community chain
	// entry, in a members group that doesn't require members_rules yet,
	// so it's taken with a line (§16.2).
	backward  bool
	committer string
}

// judgeCommunityCommit judges a commit to a community group by the chain
// as of the entry its community_head names (§16.2), never by later
// entries: a code join's proof and code (§16.6), Adds of new people by a
// member with moderate or, in a restricted channel's pinned group, of
// people whose roles see it (§16.5), and Removes by leave, kick, ban, or
// lost sight of the channel. Leaves pass when their device appears in
// their identity's chain (§16.5, "A leaf's device is judged by its
// history").
func (e *Engine) judgeCommunityCommit(ctx context.Context, cm *Community, g *CGroup, before *mls.State, p *mls.Processed, b buffered, receiptTS int64) (*cjudged, error) {
	bad := func(format string, args ...any) (*cjudged, error) {
		return nil, invalid{fmt.Errorf(format, args...)}
	}
	members := g.See == nil
	data, ok := p.Commit.Extension(ExtGroup)
	if !ok {
		return bad("the commit drops tieroute_group")
	}
	next, err := ParseGroupExt(data)
	if err != nil {
		return bad("%v", err)
	}
	if old, _ := before.Extension(ExtGroup); old != nil {
		if o, err := ParseGroupExt(old); err == nil && o.Kind != next.Kind {
			return bad("the commit changes the group's kind")
		}
	}
	oldHead, hadHead := communityHead(before.Extensions)
	head, hasHead := communityHead(p.Commit.Extensions)
	j := &cjudged{head: head}
	// members_rules switches on the rules tied to it (§16.2, §16.6).
	gated := members && requires(before.Extensions, ExtMembersRules)
	if gated && !requires(p.Commit.Extensions, ExtMembersRules) {
		return bad("the commit drops members_rules from the members group's required capabilities")
	}
	var st *community.State
	var headSeq uint64
	switch {
	case members && !hasHead:
		return bad("a members group commit without community_head")
	case hadHead && !hasHead:
		return bad("the commit unpins the group")
	case hasHead:
		st, headSeq, err = e.stateAtHead(ctx, cm, head)
		if err != nil {
			var r retryable
			if errors.As(err, &r) {
				return nil, err
			}
			return bad("%v", err)
		}
		if hadHead {
			// The head never goes back: in a pinned group always (§16.5),
			// in the members group once it requires members_rules, and
			// before that it's taken with a line (§16.2).
			if _, oldSeq, err := e.stateAtHead(ctx, cm, oldHead); err == nil && headSeq < oldSeq {
				if !members || gated {
					return bad("the commit names an earlier community chain entry than the group did")
				}
				j.backward = true
			}
		}
	default:
		// An unpinned restricted group: judge by the newest chain.
		c, err := e.communityChain(cm)
		if err != nil {
			return nil, retryable{err}
		}
		st = c.State()
	}

	beforeIDs := map[string]bool{}
	leafID := map[uint32]string{}
	for _, m := range before.Members {
		if id, _, err := parseCredential(m.Identity); err == nil {
			beforeIDs[id] = true
			leafID[m.LeafIndex] = id
		}
	}
	external := p.Sender.Kind == "new_member_commit"
	committer := ""
	if external {
		if p.Commit.PathLeaf == nil {
			return bad("an external commit without the joiner's leaf")
		}
		id, _, err := parseCredential(p.Commit.PathLeaf.Identity)
		if err != nil {
			return bad("%v", err)
		}
		committer, j.joiner = id, id
	} else if p.Sender.Kind == "member" {
		committer = leafID[p.Sender.Index]
		j.committer = committer
		if committer == "" {
			return bad("a commit from outside the group")
		}
	} else {
		return bad("a commit from a %s", p.Sender.Kind)
	}

	var leaves []*mls.LeafInfo
	adds := map[string]bool{}
	removedIdx := map[uint32]bool{}
	type removal struct {
		id    string
		dev   []byte
		leave bool
	}
	var removals []removal
	for _, pr := range p.Commit.Proposals {
		switch pr.Kind {
		case "add":
			if external || pr.KeyPackage == nil {
				return bad("an Add in an external commit, or without a key package")
			}
			id, _, err := parseCredential(pr.KeyPackage.Leaf.Identity)
			if err != nil {
				return bad("%v", err)
			}
			leaves = append(leaves, &pr.KeyPackage.Leaf)
			adds[id] = true
		case "update":
			if pr.Leaf != nil {
				leaves = append(leaves, pr.Leaf)
			}
		case "remove":
			if pr.Removed == nil {
				continue
			}
			m, ok := before.Member(*pr.Removed)
			if !ok {
				return bad("a Remove of a leaf that isn't there")
			}
			id, dev, err := parseCredential(m.Identity)
			if err != nil {
				return bad("%v", err)
			}
			removedIdx[*pr.Removed] = true
			leave := pr.ByReference && pr.Sender.Kind == "member" && leafID[pr.Sender.Index] == id
			removals = append(removals, removal{id, dev, leave})
		case "group_context_extensions", "external_init":
			if pr.Kind == "external_init" && !external {
				return bad("external_init outside an external commit")
			}
		default:
			return bad("a %s proposal, which communities don't use", pr.Kind)
		}
	}
	if p.Commit.PathLeaf != nil {
		leaves = append(leaves, p.Commit.PathLeaf)
	}

	if external {
		if !members {
			return bad("an external commit to a restricted channel's group")
		}
		if hasHead && !bytes.Equal(head, oldHead) {
			return bad("a code join that changes community_head")
		}
		if b.Proof == nil {
			return bad("a code join without a join proof")
		}
		proof, key, err := community.VerifyJoinProof(*b.Proof)
		if err != nil {
			return bad("%v", err)
		}
		h := sha256.Sum256(b.Data)
		if proof.ID != j.joiner || proof.Epoch != before.Epoch || !bytes.Equal(proof.Commit, h[:]) {
			return bad("a join proof for another joiner, epoch, or commit")
		}
		code, ok := st.FindCode(key)
		hash := fmt.Sprintf("%x", community.CodeHash(key))
		// Under members_rules, the use limit isn't part of the verdict,
		// since members count uses differently (§16.6, rule 3).
		uses := cm.Uses[hash]
		if gated {
			uses = 0
		}
		if !ok || !st.CodeLive(key, receiptTS, uses) {
			return bad("a code join with a code that isn't live")
		}
		if !beforeIDs[code.Creator] {
			return bad("a code join with a code whose creator isn't a member")
		}
		if st.Banned(j.joiner, "") || st.Approval {
			return bad("a code join by a banned identity, or while joins need approval")
		}
		for _, r := range removals {
			if r.id != j.joiner {
				return bad("a code join that removes someone else")
			}
		}
		if gated {
			// Rule 7: only Removes of the joiner's own device's old
			// leaves, no other proposal, and tieroute_group as it was.
			_, newDev, _ := parseCredential(p.Commit.PathLeaf.Identity)
			for _, r := range removals {
				if !bytes.Equal(r.dev, newDev) {
					return bad("a code join that removes a leaf of another device")
				}
			}
			for _, pr := range p.Commit.Proposals {
				if pr.Kind != "remove" && pr.Kind != "external_init" {
					return bad("a code join with a %s proposal", pr.Kind)
				}
			}
			if old, _ := before.Extension(ExtGroup); !bytes.Equal(old, data) {
				return bad("a code join that changes tieroute_group")
			}
		}
		if cm.Uses == nil {
			cm.Uses = map[string]uint64{}
		}
		cm.Uses[hash]++
	} else {
		var newPeople []string
		for id := range adds {
			if !beforeIDs[id] {
				newPeople = append(newPeople, id)
			}
		}
		if members && len(newPeople) > 0 {
			// An approval join (§16.6).
			var old *GroupExt
			if data, _ := before.Extension(ExtGroup); data != nil {
				old, _ = ParseGroupExt(data)
			}
			if old == nil {
				old = &GroupExt{}
			}
			if err := e.checkCommunityApproval(ctx, cm, g, old, next, st, committer, newPeople, before.Epoch, receiptTS); err != nil {
				var r retryable
				if errors.As(err, &r) {
					return nil, err
				}
				return bad("%v", err)
			}
		}
		for id := range adds {
			if beforeIDs[id] {
				continue // another device of a member: always allowed (§16.5)
			}
			if st.Banned(id, "") {
				return bad("an Add of a banned identity")
			}
			switch {
			case members && !st.Can(committer, community.PermModerate):
				return bad("an Add of a new member by someone without moderate")
			case !members && hasHead && !st.SeesList(id, g.See):
				return bad("an Add of someone whose roles don't see the channel")
			}
		}
		for _, r := range removals {
			switch {
			case r.leave, r.id == committer:
			case st.Banned(r.id, ""):
			case st.Can(committer, community.PermModerate) && st.Outranks(committer, r.id):
			case !members && !st.SeesList(r.id, g.See):
			default:
				// A removed device: its identity's chain no longer has it
				// (§16.5, "Removed devices").
				c, err := e.identityChain(ctx, cm, r.id, true)
				if err != nil {
					return nil, retryable{err}
				}
				if c.State.IsDevice(r.dev) {
					return bad("a Remove of %s's current device without authority", e.nameOf(r.id))
				}
			}
		}
	}
	// Leaves: the device appears in its identity's chain at some point,
	// and the msg-key checks out. A device the chain never shows is
	// removed, not refused (§16.5).
	get := func(id string) (*identity.Chain, error) { return e.identityChain(ctx, cm, id, false) }
	for _, l := range leaves {
		if _, _, err := e.checkLeaf(l, get, leafEver); err != nil {
			var r retryable
			if errors.As(err, &r) {
				return nil, err
			}
			if id, _, perr := parseCredential(l.Identity); perr == nil {
				if _, ferr := e.identityChain(ctx, cm, id, true); ferr == nil {
					if _, _, err2 := e.checkLeaf(l, get, leafEver); err2 == nil {
						continue
					}
				}
			}
			return bad("%v", err)
		}
	}
	after := map[string]bool{}
	for _, m := range before.Members {
		if !removedIdx[m.LeafIndex] {
			after[leafID[m.LeafIndex]] = true
		}
	}
	for id := range adds {
		after[id] = true
	}
	if j.joiner != "" {
		after[j.joiner] = true
	}
	if members && len(after) > MaxCommunityMembers {
		return bad("the community would have more than 1,000 members")
	}
	for id := range adds {
		if !beforeIDs[id] {
			j.added = append(j.added, id)
		}
	}
	for id := range beforeIDs {
		if !after[id] {
			j.removed = append(j.removed, id)
		}
	}
	slices.Sort(j.added)
	slices.Sort(j.removed)
	return j, nil
}

// invalid marks a verdict that a commit broke the rules, as opposed to
// one that can't be reached yet.
type invalid struct{ err error }

func (i invalid) Error() string { return i.err.Error() }
func (i invalid) Unwrap() error { return i.err }

// cprocessCommit checks a commit's receipt, judges it, and applies it, or
// refuses it and stops sending to the community (§7.1, §16.2).
func (e *Engine) cprocessCommit(ctx context.Context, cm *Community, g *CGroup, b buffered) error {
	if b.Receipt == nil {
		return errors.New("chat: a commit without a receipt")
	}
	gid := mustB64(g.ID)
	receipt, err := e.checkReceiptFrom(ctx, cm.Server, *b.Receipt, gid, b.Epoch, b.Data)
	if err != nil {
		return err
	}
	if h, ok := g.Applied[b.Epoch]; ok {
		if sum := sha256.Sum256(b.Data); !bytes.Equal(h, sum[:]) {
			e.stopCommunity(cm, "the community server sent members different changes")
		}
		return nil
	}
	mg := e.dev.mls.Group(gid)
	before, err := mg.State()
	if err != nil {
		return err
	}
	p, err := mg.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		if mls.IsCode(err, mls.ErrWrongEpoch) || mls.IsCode(err, mls.ErrPastEpoch) {
			return nil
		}
		return fmt.Errorf("chat: processing a community commit: %w", err)
	}
	if p.Kind == mls.KindOwn {
		e.dev.Revert()
		return nil
	}
	if p.Kind != mls.KindCommit || p.Commit == nil {
		e.dev.Revert()
		return errors.New("chat: a commit delivery that isn't a commit")
	}
	usesBefore := maps.Clone(cm.Uses)
	j, err := e.judgeCommunityCommit(ctx, cm, g, before, p, b, receipt.TS)
	if err != nil {
		e.dev.Revert()
		cm.Uses = usesBefore
		var inv invalid
		if errors.As(err, &inv) {
			e.log.Info("chat: refused a community commit", "err", err)
			e.stopCommunity(cm, "a member's change didn't follow the community's rules: "+err.Error())
			return nil
		}
		g.Pending = true
		g.Buffered = append([]buffered{b}, g.Buffered...)
		e.save()
		return err
	}
	if err := mg.MergeStagedCommit(); err != nil {
		e.dev.Revert()
		return err
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	after, err := mg.State()
	if err != nil {
		return err
	}
	if g.Applied == nil {
		g.Applied = map[uint64]wire.Bytes{}
	}
	sum := sha256.Sum256(b.Data)
	g.Applied[b.Epoch] = sum[:]
	for k := range g.Applied {
		if k+64 < b.Epoch {
			delete(g.Applied, k)
		}
	}
	g.Epoch, g.Members = after.Epoch, members(after)
	e.leavesGone(cm, g, before, after)
	if !after.Active {
		if g.See == nil {
			e.removedFromCommunity(ctx, cm, "You were removed from the community")
			return nil
		}
		delete(cm.Groups, g.ID)
		e.dev.mls.Group(gid).Delete()
		e.dev.Save()
	}
	if j.backward {
		if ch := e.firstChannel(cm); ch != nil {
			e.systemLine(ch, "A change named an earlier community chain entry, committed by "+e.nameOf(j.committer))
		}
	}
	if j.joiner != "" && j.joiner != e.c.ID() {
		e.newcomer(cm, after, j.joiner)
	}
	for _, id := range j.added {
		if id != e.c.ID() {
			e.newcomer(cm, after, id)
		}
	}
	e.save()
	e.refreshChannels(cm)
	return nil
}

// stopCommunity stops sending to a community after a refused commit
// (§7.1, §16.5).
func (e *Engine) stopCommunity(cm *Community, why string) {
	cm.Stopped = why
	e.save()
	e.refreshChannels(cm)
}

// cprocessProposal handles a standalone proposal in a community group: a
// device leaving (§11.7). The next commit this device makes includes it.
func (e *Engine) cprocessProposal(ctx context.Context, cm *Community, g *CGroup, b buffered) error {
	mg := e.dev.mls.Group(mustB64(g.ID))
	st, err := mg.State()
	if err != nil {
		return err
	}
	p, err := mg.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		return err
	}
	if p.Kind == mls.KindOwn {
		e.dev.Revert()
		return nil
	}
	ok := p.Proposal != nil && p.Proposal.Kind == "remove" && p.Proposal.Removed != nil && p.Sender.Kind == "member"
	if ok {
		from, _ := st.Member(p.Sender.Index)
		target, _ := st.Member(*p.Proposal.Removed)
		ok = from != nil && target != nil && bytes.Equal(credID(from.Identity), credID(target.Identity))
	}
	if !ok {
		if p.Proposal != nil {
			mg.RemovePendingProposal(p.Proposal.Reference)
		}
		e.dev.Save()
		return errors.New("chat: a standalone proposal other than a device leaving")
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if cm.Stopped == "" {
		if err := e.ccommit(ctx, cm, g, ccommitOpts{pending: true}); err != nil {
			e.log.Info("chat: committing a leave in a community group", "err", err)
		}
	}
	return nil
}

func credID(cred []byte) []byte {
	id, _, err := parseCredential(cred)
	if err != nil {
		return nil
	}
	return []byte(id)
}

// communityWelcome joins a community group from a Welcome: a restricted
// channel's group, or the members group after an approval or for a new
// device of this identity. The tree comes from the community server, and
// every leaf's device appears in its identity's chain (§7.1, §16.5).
func (e *Engine) communityWelcome(ctx context.Context, cm *Community, d *client.Delivery) error {
	if _, ok := cm.Groups[d.Group]; ok {
		return nil
	}
	info, err := mls.InspectMessage(d.Data)
	if err != nil || info.WireFormat != "welcome" {
		return errors.New("chat: a Welcome that isn't one")
	}
	gid := mustB64(d.Group)
	if d.Epoch == nil {
		return errors.New("chat: a community Welcome without an epoch")
	}
	conn := e.c.Community(cm.Server)
	tree, err := conn.Tree(ctx, gid, *d.Epoch, nil)
	if err != nil {
		return retryable{err}
	}
	mg, res, err := e.dev.mls.JoinWelcome(d.Data, tree, communityConfig)
	if err != nil {
		e.dev.Revert()
		return err
	}
	refuse := func(err error) error {
		mg.Delete()
		e.dev.Revert()
		return err
	}
	if !bytes.Equal(res.GroupID, gid) {
		return refuse(errors.New("chat: the Welcome is for another group"))
	}
	st, err := mg.State()
	if err != nil {
		return refuse(err)
	}
	get := func(id string) (*identity.Chain, error) { return e.identityChain(ctx, cm, id, false) }
	for i := range st.Members {
		if _, _, err := e.checkLeaf(&st.Members[i].LeafInfo, get, leafEver); err != nil {
			var r retryable
			if errors.As(err, &r) {
				mg.Delete()
				e.dev.Revert()
				return err
			}
			return refuse(err)
		}
	}
	// Which group: the members group, or the one carrying a see list.
	var see []uint64
	if d.Group != cm.MembersGroup {
		// The Welcome can come before the entries that let this identity
		// see the group's channels: bring the chain up to date first.
		if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
			mg.Delete()
			e.dev.Revert()
			return retryable{err}
		}
		c, err := e.communityChain(cm)
		if err != nil {
			return refuse(err)
		}
		cst := c.State()
		for _, list := range cst.SeeLists() {
			if cst.SeesList(e.c.ID(), list) {
				if see == nil || len(list) < len(see) {
					see = list
				}
			}
		}
		if see == nil {
			see = []uint64{}
		}
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	g := &CGroup{ID: d.Group, See: see, Epoch: st.Epoch, Members: members(st)}
	cm.Groups[d.Group] = g
	if err := e.save(); err != nil {
		return err
	}
	if d.Group == cm.MembersGroup {
		e.joinedByApproval(cm)
		e.linkedIn(cm)
	}
	e.refreshChannels(cm)
	if err := e.topUpCommunity(ctx, cm); err != nil {
		e.log.Debug("chat: community key packages", "err", err)
	}
	return nil
}

// ccommitOpts describe a commit to a community group.
type ccommitOpts struct {
	adds     [][]byte
	removes  []uint32
	pending  bool // commit standalone proposals waiting
	head     []byte
	welcome  []string     // identities a Welcome goes to
	devs     []wire.Bytes // the devices it goes to
	ext      *GroupExt    // a new tieroute_group, or nil to keep it
	pin      bool         // set community_head in a restricted group
	required []uint16     // required_capabilities, when the extensions change
	// handoff is the community server a hub handoff registers the group
	// with, at the epoch the commit creates, before the old hub
	// sequences it (§11.11, §16.6).
	handoff string
}

// ccommit makes a commit to a community group, sends it to the community
// server with the GroupInfo and ratchet tree of the epoch it creates, and
// applies it once accepted (§11.6, community server API "Commits").
func (e *Engine) ccommit(ctx context.Context, cm *Community, g *CGroup, o ccommitOpts) error {
	if cm.Stopped != "" || g.Pending {
		return errors.New("chat: the community can't change until its last commit is decided")
	}
	gid := mustB64(g.ID)
	mg := e.dev.mls.Group(gid)
	st, err := mg.State()
	if err != nil {
		return err
	}
	opts := mls.CommitOptions{Adds: o.adds, Removes: o.removes, PendingProposals: o.pending, UpdatePath: true, GroupInfo: true}
	head, hasHead := communityHead(st.Extensions)
	if o.head != nil {
		head, hasHead = o.head, true
	}
	if o.head != nil || o.ext != nil || o.pin {
		ext := o.ext
		if ext == nil {
			data, _ := st.Extension(ExtGroup)
			if ext, err = ParseGroupExt(data); err != nil {
				return err
			}
		}
		x, err := ext.Extension()
		if err != nil {
			return err
		}
		// The members group and an unpinned restricted group require
		// 0xF0A1 and 0xF0A3; a pinned restricted group 0xF0A4 too (§16.5).
		req := []uint16{ExtGroup, ExtCommunityHead}
		if g.See != nil && hasHead {
			req = append(req, ExtRestrictedHead)
		}
		if o.required != nil {
			req = o.required
		}
		if g.See == nil && (requires(st.Extensions, ExtMembersRules) || allList(st, ExtMembersRules)) {
			// members_rules goes on once every leaf lists it, and never
			// comes off (§16.2).
			req = append(slices.DeleteFunc(slices.Clone(req), func(t uint16) bool { return t == ExtMembersRules }), ExtMembersRules)
		}
		opts.Extensions = []mls.Extension{x, mls.RequiredCapabilities(req, nil, nil)}
		if hasHead {
			opts.Extensions = append(opts.Extensions, mls.Extension{Type: ExtCommunityHead, Data: head})
		}
	}
	res, err := mg.Commit(e.dev.signer, opts)
	if err != nil {
		return err
	}
	clear := func() { mg.ClearPendingCommit(); e.dev.Save() }
	nextKey, err := e.dev.PendingEpochKey(mg)
	if err != nil {
		clear()
		return err
	}
	key, err := e.dev.EpochKey(mg)
	if err != nil {
		clear()
		return err
	}
	h := sha256.Sum256(res.Commit)
	req, err := sig.SignRevealing(key, TypeCommitRequest, CommitRequest{Group: gid, Epoch: res.Epoch, Hash: h[:], Next: nextKey.Public()})
	if err != nil {
		clear()
		return err
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	// The new epoch's tree: merge, export, and go back to the pending
	// commit, which the save above kept.
	if err := mg.MergePendingCommit(); err != nil {
		e.dev.Revert()
		return err
	}
	tree, err := mg.ExportRatchetTree()
	e.dev.Revert()
	if err != nil {
		return err
	}
	conn := e.c.Community(cm.Server)
	treeHash, err := conn.UploadTree(ctx, tree)
	if err != nil {
		clear()
		return err
	}
	if o.handoff != "" {
		to := e.c.Community(o.handoff)
		th, err := to.UploadTree(ctx, tree)
		if err == nil {
			reg := client.GroupReg{Group: gid, Epoch: res.Epoch + 1, EpochKey: nextKey.Public(), Info: res.GroupInfo, Tree: th}
			err = to.CreateGroup(ctx, reg, g.See, nil)
		}
		if err != nil {
			clear()
			return fmt.Errorf("chat: registering the group with %s: %w", o.handoff, err)
		}
	}
	out := client.CommunityOutgoing{ID: client.NewMsgID(), Kind: "mls", Group: gid, Epoch: res.Epoch, MLS: "commit",
		Data: res.Commit, Commit: &req, Info: res.GroupInfo, Tree: treeHash}
	if len(res.Welcome) > 0 && len(o.welcome) > 0 {
		out.Welcomes = []client.CommunityWelcome{{ID: client.NewMsgID(), To: o.welcome, Devs: o.devs, Data: res.Welcome}}
	}
	results, err := conn.Send(ctx, out)
	if err != nil {
		clear()
		return err
	}
	r := results[0]
	if r.Status != "sent" || r.Receipt == nil {
		clear()
		if r.Error == wire.CodeStaleEpoch {
			e.cfetchMissed(ctx, cm, g)
			e.cdrain(ctx, cm, g)
		}
		return &wire.Error{Code: r.Error, Msg: "the community server refused the commit"}
	}
	if _, err := e.checkReceiptFrom(ctx, cm.Server, *r.Receipt, gid, res.Epoch, res.Commit); err != nil {
		clear()
		e.stopCommunity(cm, "the community server's receipt didn't check out")
		return err
	}
	if err := mg.MergePendingCommit(); err != nil {
		return err
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	after, err := mg.State()
	if err != nil {
		return err
	}
	if g.Applied == nil {
		g.Applied = map[uint64]wire.Bytes{}
	}
	g.Applied[res.Epoch] = h[:]
	g.Epoch, g.Members = after.Epoch, members(after)
	e.leavesGone(cm, g, st, after)
	return e.save()
}

// bindHead commits the members group's new community_head after this
// device appends a chain entry, so members who agree on the epoch agree on
// the chain, and judge commits by it (§16.2).
func (e *Engine) bindHead(ctx context.Context, cm *Community) error {
	g := cm.Groups[cm.MembersGroup]
	c, err := e.communityChain(cm)
	if g == nil || err != nil {
		return errors.New("chat: no members group")
	}
	var lastErr error
	for range 3 {
		err := e.ccommit(ctx, cm, g, ccommitOpts{head: c.Head(), pending: e.hasPending(g)})
		if err == nil || !wire.IsCode(err, wire.CodeStaleEpoch) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (e *Engine) hasPending(g *CGroup) bool {
	st, err := e.dev.mls.Group(mustB64(g.ID)).State()
	return err == nil && len(st.PendingProposals) > 0
}

// allList reports whether every leaf in a group lists a capability.
func allList(st *mls.State, t uint16) bool {
	for _, m := range st.Members {
		if !slices.Contains(m.Capabilities.Extensions, t) {
			return false
		}
	}
	return len(st.Members) > 0
}
