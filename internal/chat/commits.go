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
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// groupMessage handles an MLS message for a group this device is in: a
// commit, an application message, or a proposal. Messages for a later
// epoch wait until the group gets there (§11.6). The caller holds e.mu.
func (e *Engine) groupMessage(ctx context.Context, d *client.Delivery, received int64) error {
	ch, ok := e.st.Chats[d.Group]
	if !ok {
		return fmt.Errorf("chat: a message for a group this device isn't in")
	}
	if ch.Stopped != "" {
		return nil
	}
	if d.Epoch == nil {
		return errors.New("chat: an mls delivery without an epoch")
	}
	b := buffered{Epoch: *d.Epoch, MLS: d.MLS, From: d.From, Data: d.Data, Receipt: d.Receipt, Received: received}
	if err := e.groupItem(ctx, ch, b); err != nil {
		return err
	}
	return e.drain(ctx, ch)
}

// groupItem handles one message, or buffers it for a later epoch.
func (e *Engine) groupItem(ctx context.Context, ch *Chat, b buffered) error {
	switch {
	case b.MLS == "commit" && b.Epoch < ch.Epoch:
		return e.oldCommit(ctx, ch, b)
	case b.Epoch > ch.Epoch, b.MLS == "commit" && b.Epoch == ch.Epoch && ch.Pending:
		return e.buffer(ctx, ch, b)
	case ch.Pending:
		// Nothing is applied while a commit waits for a verdict; keep the
		// message for after it.
		return e.buffer(ctx, ch, b)
	}
	switch b.MLS {
	case "commit":
		return e.processCommit(ctx, ch, b)
	case "app":
		return e.processApp(ctx, ch, b)
	case "proposal":
		return e.processProposal(ctx, ch, b)
	}
	return nil
}

// buffer keeps a message for a later epoch, and fetches the commits this
// device missed from the hub when a message shows it's behind (§11.6).
func (e *Engine) buffer(ctx context.Context, ch *Chat, b buffered) error {
	if len(ch.Buffered) >= maxBuffered {
		ch.Buffered = ch.Buffered[1:]
	}
	ch.Buffered = append(ch.Buffered, b)
	if err := e.save(); err != nil {
		return err
	}
	if b.Epoch > ch.Epoch && !ch.Pending {
		e.fetchMissed(ctx, ch)
	}
	return nil
}

// drain applies buffered messages that the group's epoch has reached.
func (e *Engine) drain(ctx context.Context, ch *Chat) error {
	for range maxBuffered {
		if ch.Pending || ch.Stopped != "" {
			return nil
		}
		i := slices.IndexFunc(ch.Buffered, func(b buffered) bool { return b.Epoch <= ch.Epoch })
		if i < 0 {
			return nil
		}
		// Commits for the current epoch first, so messages sent after one
		// wait for it.
		if j := slices.IndexFunc(ch.Buffered, func(b buffered) bool { return b.Epoch == ch.Epoch && b.MLS == "commit" }); j >= 0 && ch.Buffered[i].MLS != "commit" && ch.Buffered[i].Epoch == ch.Epoch {
			i = j
		}
		b := ch.Buffered[i]
		ch.Buffered = slices.Delete(ch.Buffered, i, i+1)
		if err := e.groupItem(ctx, ch, b); err != nil {
			e.log.Info("chat: dropped a buffered message", "err", err)
		}
		if err := e.save(); err != nil {
			return err
		}
	}
	return nil
}

// fetchMissed fetches the commits a group's hub holds from this device's
// epoch on, and applies them in order (§11.6).
func (e *Engine) fetchMissed(ctx context.Context, ch *Chat) {
	hub := ""
	if ch.Hub != e.c.Server() {
		hub = ch.Hub
	}
	after := uint64(0)
	if ch.Epoch > 0 {
		after = ch.Epoch - 1
	}
	commits, err := e.c.FetchCommits(ctx, ch.ID, after, hub)
	if err == nil || unreachable(err) {
		e.noteHubAnswer(ch, err == nil)
	}
	if expired(err) {
		// The hub no longer has what this device missed (§11.9).
		e.rejoinDue = true
	}
	if err != nil {
		e.log.Info("chat: fetching missed commits", "err", err)
		return
	}
	if ch.Epoch == 0 {
		// after=0 starts at epoch 1, so ask for epoch 0 on its own.
		commits = slices.DeleteFunc(commits, func(c client.HubCommit) bool { return c.Epoch == 0 })
	}
	for _, c := range commits {
		if c.Epoch < ch.Epoch {
			continue
		}
		r := c.Receipt
		b := buffered{Epoch: c.Epoch, MLS: "commit", Data: c.Data, Receipt: &r}
		if !slices.ContainsFunc(ch.Buffered, func(x buffered) bool { return x.MLS == "commit" && bytes.Equal(x.Data, b.Data) }) {
			ch.Buffered = append(ch.Buffered, b)
		}
	}
}

// catchUpGroups fetches each group's missed commits after the stream
// connects, so a device that missed a fan-out isn't stuck (§11.6).
func (e *Engine) catchUpGroups(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.st.Chats {
		if ch.Stopped != "" || ch.Kind == KindChannel || ch.Left {
			continue
		}
		e.fetchMissed(ctx, ch)
		if err := e.drain(ctx, ch); err != nil {
			e.log.Info("chat: catching up", "err", err)
		}
	}
	e.rejoinIfDue(ctx)
}

// oldCommit handles a commit for an epoch this device already left: its
// own commit coming back, or a commit already fetched from the hub. A
// different commit for the same epoch with a valid receipt means the hub
// sent members different changes (§11.6).
func (e *Engine) oldCommit(ctx context.Context, ch *Chat, b buffered) error {
	h := sha256.Sum256(b.Data)
	prev, ok := ch.Applied[b.Epoch]
	if !ok || bytes.Equal(prev, h[:]) || b.Receipt == nil {
		return nil
	}
	if _, err := e.checkReceipt(ctx, ch, *b.Receipt, mustB64(ch.ID), b.Epoch, b.Data); err != nil {
		return nil // not from the hub; dropped
	}
	ch.Stopped = "the hub sent members different changes; re-create the chat"
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// processCommit processes a commit from another member in the current
// epoch: it checks the hub's receipt (§11.6), judges the commit (§7.1,
// §11.8), and applies it, or refuses it and stops following the group.
func (e *Engine) processCommit(ctx context.Context, ch *Chat, b buffered) error {
	if b.Receipt == nil {
		return errors.New("chat: a commit without a receipt")
	}
	gid := mustB64(ch.ID)
	receipt, err := e.checkReceipt(ctx, ch, *b.Receipt, gid, b.Epoch, b.Data)
	if err != nil {
		return err
	}
	g := e.dev.mls.Group(gid)
	before, err := g.State()
	if err != nil {
		return err
	}
	p, err := g.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		if mls.IsCode(err, mls.ErrWrongEpoch) || mls.IsCode(err, mls.ErrPastEpoch) {
			return nil
		}
		return fmt.Errorf("chat: processing a commit: %w", err)
	}
	if p.Kind == mls.KindOwn {
		e.dev.Revert()
		return nil
	}
	if p.Kind != mls.KindCommit || p.Commit == nil {
		e.dev.Revert()
		return errors.New("chat: a commit delivery that isn't a commit")
	}
	j, verdict, err := e.judgeCommit(ctx, ch, before, p, receipt.TS)
	switch verdict {
	case identity.Pending:
		e.dev.Revert()
		ch.Pending = true
		ch.Buffered = append([]buffered{b}, ch.Buffered...)
		e.log.Info("chat: a commit is pending", "err", err)
		return e.save()
	case identity.Invalid:
		e.dev.Revert()
		ch.Stopped = "a member's change didn't follow the rules: " + err.Error()
		e.log.Info("chat: refused a commit", "err", err)
		if err := e.save(); err != nil {
			return err
		}
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
		return nil
	}
	if err := g.MergeStagedCommit(); err != nil {
		e.dev.Revert()
		return err
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	after, err := g.State()
	if err != nil {
		return err
	}
	h := sha256.Sum256(b.Data)
	ch.applied(b.Epoch, h[:])
	oldMembers, oldAdmins := ch.Members, ch.Admins
	ch.Epoch, ch.Hub, ch.Members = after.Epoch, j.next.Hub, members(after)
	ch.Admins = slices.Clone(j.next.Admins)
	if ch.Hold && !e.removalWaiting(after) {
		ch.Hold = false
	}
	if ch.Kind == KindGroup {
		// Requests for people who are in the group now are done.
		ch.Requests = slices.DeleteFunc(ch.Requests, func(r AddPending) bool { return slices.Contains(ch.Members, r.Who) })
	}
	if err := e.save(); err != nil {
		return err
	}
	if ch.Kind == KindGroup {
		e.membershipLines(ch, j.committer, oldMembers, ch.Members, j.added, j.leaving)
		if after.Active {
			e.adminLines(ch, oldAdmins, ch.Admins)
		}
	}
	if !after.Active {
		if ch.Kind == KindGroup {
			e.leftGroup(ctx, ch, "You're no longer in this group")
			return nil
		}
		ch.Stopped = "this device was removed from the chat"
		e.save()
	} else {
		e.deviceLines(ch, before, after)
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// judged is what judging a commit found, for the system lines and the
// chat's record once it's applied.
type judged struct {
	next      *GroupExt
	committer string
	added     []string        // new people
	removed   []string        // people the commit takes out
	leaving   map[string]bool // of removed, those who left on their own
}

// judgeCommit judges a staged commit by the rules this client implements
// for direct chats and group chats: every new or updated leaf checks out
// (§7.1); the commit binds the history its scope needs (§7.1); a group
// chat's admin rules hold (§11.8); tieroute_group stays well formed and
// follows the floor rule; and the group stays within its size (§11.3,
// §11.10). receiptTS is the hub receipt's ts.
func (e *Engine) judgeCommit(ctx context.Context, ch *Chat, before *mls.State, p *mls.Processed, receiptTS int64) (*judged, identity.Verdict, error) {
	bad := func(format string, args ...any) (*judged, identity.Verdict, error) {
		return nil, identity.Invalid, fmt.Errorf(format, args...)
	}
	data, _ := before.Extension(ExtGroup)
	old, err := ParseGroupExt(data)
	if err != nil {
		return bad("the group's tieroute_group: %v", err)
	}
	var next *GroupExt
	if data, ok := p.Commit.Extension(ExtGroup); ok {
		if next, err = ParseGroupExt(data); err != nil {
			return bad("%v", err)
		}
	} else {
		return bad("the commit drops tieroute_group")
	}
	e.noteHomes(next)
	if next.Kind != old.Kind {
		return bad("the commit changes the group's kind")
	}
	if !bytes.Equal(next.Replaces, old.Replaces) {
		return bad("the commit changes replaces")
	}
	committer, ok := before.Member(p.Sender.Index)
	if p.Sender.Kind != "member" || !ok {
		return bad("a commit from outside the group")
	}
	committerID, committerDev, err := parseCredential(committer.Identity)
	if err != nil {
		return bad("%v", err)
	}
	group := old.Kind == KindGroup
	admin := group && slices.Contains(old.Admins, committerID)

	// Who's in the group before the commit.
	beforeIDs := map[string]bool{}
	leafID := map[uint32]string{}
	for _, m := range before.Members {
		if id, _, err := parseCredential(m.Identity); err == nil {
			beforeIDs[id] = true
			leafID[m.LeafIndex] = id
		}
	}

	// What the commit does.
	type keyCheck struct {
		id  string
		key []byte
	}
	var addKeys []keyCheck
	var leaves []*mls.LeafInfo
	adds := map[string]bool{}
	newPeople := map[string]bool{}
	for _, pr := range p.Commit.Proposals {
		switch pr.Kind {
		case "add":
			if pr.KeyPackage == nil {
				return bad("an Add without a key package")
			}
			id, dev, err := parseCredential(pr.KeyPackage.Leaf.Identity)
			if err != nil {
				return bad("%v", err)
			}
			addKeys = append(addKeys, keyCheck{id, dev})
			leaves = append(leaves, &pr.KeyPackage.Leaf)
			adds[id] = true
			if !beforeIDs[id] {
				newPeople[id] = true
			}
		case "update":
			if pr.Leaf != nil {
				leaves = append(leaves, pr.Leaf)
			}
		case "remove", "group_context_extensions":
		default:
			return bad("a %s proposal, which chats don't use (§11.1)", pr.Kind)
		}
	}
	if p.Commit.PathLeaf != nil {
		leaves = append(leaves, p.Commit.PathLeaf)
		if id, dev, err := parseCredential(p.Commit.PathLeaf.Identity); err != nil || id != committerID || !bytes.Equal(dev, committerDev) {
			return bad("the committer's new leaf names another device")
		}
	}
	// Removes. A leave proposal, committed by reference, is the device
	// leaving, not the committer removing it (§11.1, §11.7). Another
	// Remove needs the committer's authority: an admin removing someone
	// else, or a commit binding a history in which the key isn't a device
	// (§11.9).
	removedIdx := map[uint32]bool{}
	var bindRemoves []keyCheck
	adminRemoves := false
	leaveIDs := map[string]bool{}
	for _, pr := range p.Commit.Proposals {
		if pr.Kind != "remove" || pr.Removed == nil {
			continue
		}
		removedIdx[*pr.Removed] = true
		if *pr.Removed == p.Sender.Index {
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
		if pr.ByReference && pr.Sender.Kind == "member" && leafID[pr.Sender.Index] == id {
			leaveIDs[id] = true
			continue
		}
		if admin && id != committerID {
			adminRemoves = true
			continue
		}
		bindRemoves = append(bindRemoves, keyCheck{id, dev})
	}

	// Who's in the group after it.
	after := map[string]bool{}
	leafCount := 0
	for _, m := range before.Members {
		if !removedIdx[m.LeafIndex] {
			if id, _, err := parseCredential(m.Identity); err == nil {
				after[id] = true
				leafCount++
			}
		}
	}
	for _, k := range addKeys {
		after[k.id] = true
		leafCount++
	}
	if old.Kind == KindDirect {
		// A direct chat's two identities never change, not even by a
		// swap (§11.3).
		for _, k := range addKeys {
			if k.id != e.c.ID() && k.id != ch.Peer {
				return bad("a direct chat gaining someone else (§11.3)")
			}
		}
		if len(after) > 2 || !after[e.c.ID()] && !p.Commit.SelfRemoved {
			return bad("a direct chat with someone else in it")
		}
	}
	if len(after) > MaxGroupPeople || leafCount > MaxGroupLeaves {
		return bad("the group would have more than 25 people or 250 leaves (§11.10)")
	}
	var removedPeople []string
	for id := range beforeIDs {
		if !after[id] {
			removedPeople = append(removedPeople, id)
		}
	}
	slices.Sort(removedPeople)

	// An approval, for a non-admin adding a new person (§11.8).
	var appr *AddApproval
	if group && !admin && len(newPeople) > 0 {
		if len(newPeople) != 1 || next.Approval == nil {
			return bad("a member who isn't an admin added someone without an approval")
		}
		a, err := parseApproval(*next.Approval)
		if err != nil {
			return bad("%v", err)
		}
		appr = &a
	}
	adminsChanged := !sameSet(old.Admins, next.Admins)
	authority := admin && (len(newPeople) > 0 || adminRemoves || adminsChanged) || next.Hub != old.Hub

	// The commit's scope (§7.1).
	inScope := map[string]bool{}
	for id := range adds {
		inScope[id] = true
	}
	if appr != nil {
		inScope[appr.By] = true
	}
	for _, k := range bindRemoves {
		inScope[k.id] = true
	}
	mustBe := slices.Clone(addKeys)
	if len(inScope) > 0 || authority {
		inScope[committerID] = true
		mustBe = append(mustBe, keyCheck{committerID, committerDev})
	}

	// The chain history the commit binds (§7.1).
	bound := map[string]*identity.Chain{}
	// A basis left as it was, in a commit whose scope is empty, binds
	// nothing, and isn't judged again (§7.1).
	bindsNothing := len(inScope) == 0 && bytes.Equal(wire.MustJCS(next.Basis), wire.MustJCS(old.Basis))
	for id, br := range next.Basis {
		if bindsNothing {
			break
		}
		var floor *identity.Bound
		if f, ok := old.Heads[id]; ok {
			fb := boundOf(f)
			floor = &fb
		}
		v, c, err := identity.JudgeBasis(id, identity.Basis{Bound: boundOf(br.Ref()), Tail: br.Tail}, e.chainForBasis(ctx, id, br), floor, identity.NoCheck, nil)
		if v != identity.Valid {
			return nil, v, err
		}
		bound[id] = c
	}
	latched := len(old.Heads) > 0
	for id := range inScope {
		if _, ok := next.Basis[id]; !ok {
			if latched {
				return bad("the basis leaves out %s, who's in the commit's scope", id)
			}
			// A group that hasn't latched: Reroute's chats don't bind
			// history yet, so judge on this device's copy instead.
			c, err := e.fetchChain(ctx, id, true)
			if err != nil {
				return nil, identity.Pending, err
			}
			bound[id] = c
		}
	}
	for id := range next.Basis {
		if !bindsNothing && !inScope[id] && !after[id] {
			return bad("the basis names %s, who isn't a member", id)
		}
	}
	for _, k := range mustBe {
		c := bound[k.id]
		if c == nil || !c.State.IsDevice(k.key) {
			return bad("a key that isn't a device of %s in the history the commit binds", k.id)
		}
	}
	for _, k := range bindRemoves {
		c := bound[k.id]
		if c == nil || c.State.IsDevice(k.key) {
			return bad("a Remove of a device that's still %s's in the history the commit binds", k.id)
		}
		if _, ok := old.Heads[k.id]; !ok {
			if _, ever := devicePub(c, k.key, true); !ever {
				return bad("a Remove of a key that was never %s's device", k.id)
			}
		}
	}
	if appr != nil {
		if err := checkApproval(*next.Approval, *appr, mustB64(ch.ID), old, next, newPeople, before.Epoch, receiptTS, bound[appr.By]); err != nil {
			return bad("%v", err)
		}
	}
	// Leaves: every new or updated leaf's msg-key checks out.
	get := func(id string) (*identity.Chain, error) {
		if c := bound[id]; c != nil {
			return c, nil
		}
		return e.fetchChain(ctx, id, false)
	}
	for _, l := range leaves {
		if _, _, err := e.checkLeaf(l, get, leafCurrent); err != nil {
			var r retryable
			if errors.As(err, &r) {
				return nil, identity.Pending, err
			}
			return bad("%v", err)
		}
	}

	// since: exactly the members, unchanged for those who stay, and the
	// new epoch for new people (§11.8).
	for id := range after {
		if _, ok := next.Since[id]; !ok {
			return bad("since leaves out %s", id)
		}
	}
	for id := range next.Since {
		if !after[id] {
			return bad("since lists %s, who isn't a member", id)
		}
	}
	for id, s := range old.Since {
		if after[id] && next.Since[id] != s {
			return bad("the commit changes when %s joined", id)
		}
	}
	for id := range newPeople {
		if next.Since[id] != before.Epoch+1 && group {
			return bad("since for %s isn't the epoch the commit creates", id)
		}
	}

	if group {
		if err := checkGroupChange(old, next, admin, after, appr, receiptTS); err != nil {
			return bad("%v", err)
		}
	}

	// The floor follows mechanically (§7.1).
	if latched || len(next.Heads) > 0 {
		want := map[string]ChainRef{}
		for id, h := range old.Heads {
			if after[id] {
				want[id] = h
			}
		}
		for id, br := range next.Basis {
			if bound[id] != nil {
				want[id] = br.Ref()
			}
		}
		if !maps.EqualFunc(want, next.Heads, sameRef) {
			return bad("heads doesn't follow from the basis")
		}
		if !latched {
			for id := range after {
				if _, ok := next.Heads[id]; !ok {
					return bad("the commit that latches the group leaves out %s", id)
				}
			}
		}
	}
	j := &judged{next: next, committer: committerID, removed: removedPeople, leaving: map[string]bool{}}
	for id := range newPeople {
		j.added = append(j.added, id)
	}
	slices.Sort(j.added)
	for _, id := range removedPeople {
		if leaveIDs[id] {
			j.leaving[id] = true
		}
	}
	return j, identity.Valid, nil
}

// checkGroupChange checks a group chat's tieroute_group against the admin
// rules (§11.8): admins are members, and a group never has none; only an
// admin changes admins or the hub; and used and approval change only as
// approvals allow.
func checkGroupChange(old, next *GroupExt, admin bool, after map[string]bool, appr *AddApproval, receiptTS int64) error {
	for _, a := range next.Admins {
		if !after[a] {
			return fmt.Errorf("admins lists %s, who isn't a member", a)
		}
	}
	if len(next.Admins) != len(uniq(next.Admins)) {
		return errors.New("admins lists someone twice")
	}
	if len(after) > 0 && len(next.Admins) == 0 {
		return errors.New("the commit leaves the group without an admin")
	}
	if !admin {
		var want []string
		for _, a := range old.Admins {
			if after[a] {
				want = append(want, a)
			}
		}
		if len(want) == 0 && len(after) > 0 {
			want = []string{pickAdmin(next.Since, after)}
		}
		if !sameSet(want, next.Admins) {
			return errors.New("a member who isn't an admin changed admins")
		}
		if next.Hub != old.Hub {
			return errors.New("a member who isn't an admin moved the hub")
		}
	}
	// used: entries stay until their time has passed, and the only new
	// one is the approval this commit uses (§11.8).
	for id, ts := range old.Used {
		if _, ok := next.Used[id]; !ok && ts >= receiptTS {
			return errors.New("the commit drops a used approval that hasn't expired")
		}
	}
	for id, ts := range next.Used {
		if ots, ok := old.Used[id]; ok {
			if ots != ts {
				return errors.New("the commit changes a used approval")
			}
			continue
		}
		if appr == nil || id != appr.Request || ts != appr.Exp {
			return errors.New("the commit adds to used without using that approval")
		}
	}
	if appr == nil && next.Approval != nil && (old.Approval == nil || !bytes.Equal(next.Approval.Canonical(), old.Approval.Canonical())) {
		if !admin {
			return errors.New("a member who isn't an admin set an approval it didn't use")
		}
	}
	return nil
}

// pickAdmin returns the member who becomes admin when none is left: the
// smallest since, then the bytewise-smallest identifier (§11.8).
func pickAdmin(since map[string]uint64, members map[string]bool) string {
	best := ""
	for id := range members {
		if best == "" || since[id] < since[best] || since[id] == since[best] && id < best {
			best = id
		}
	}
	return best
}

func sameSet(a, b []string) bool {
	x, y := uniq(a), uniq(b)
	return slices.Equal(x, y)
}

func uniq(a []string) []string {
	out := slices.Clone(a)
	slices.Sort(out)
	return slices.Compact(out)
}

// processProposal handles a standalone proposal. Chats use only one: a
// device leaving, which proposes removing its own leaves, and which the
// next committer includes by reference (§11.1, §11.7). This device
// commits it at once, since OpenMLS sends nothing while it waits.
func (e *Engine) processProposal(ctx context.Context, ch *Chat, b buffered) error {
	g := e.dev.mls.Group(mustB64(ch.ID))
	st, err := g.State()
	if err != nil {
		return err
	}
	p, err := g.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		return err
	}
	if p.Kind == mls.KindOwn {
		e.dev.Revert()
		return nil
	}
	ok := p.Proposal != nil && p.Proposal.Kind == "remove" && p.Proposal.Removed != nil && p.Sender.Kind == "member"
	var leaver string
	if ok {
		from, fok := st.Member(p.Sender.Index)
		target, tok := st.Member(*p.Proposal.Removed)
		fid, _, ferr := parseCredential(from.Identity)
		tid, _, terr := parseCredential(target.Identity)
		ok = fok && tok && ferr == nil && terr == nil && fid == tid
		leaver = fid
	}
	if !ok {
		if p.Proposal != nil {
			g.RemovePendingProposal(p.Proposal.Reference)
		}
		e.dev.Save()
		return errors.New("chat: a standalone proposal other than a device leaving")
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if ch.Kind != KindGroup {
		// A direct chat ends when either side leaves it, which only
		// removing or blocking the contact does (§9.6).
		e.directLeft(ctx, ch, leaver)
		return nil
	}
	if err := e.commitPending(ctx, ch); err != nil {
		e.log.Info("chat: committing a leave", "err", err)
	}
	return nil
}
