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
		if ch.Stopped != "" {
			continue
		}
		e.fetchMissed(ctx, ch)
		if err := e.drain(ctx, ch); err != nil {
			e.log.Info("chat: catching up", "err", err)
		}
	}
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
	if err := e.checkReceipt(ctx, ch, *b.Receipt, mustB64(ch.ID), b.Epoch, b.Data); err != nil {
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
	if err := e.checkReceipt(ctx, ch, *b.Receipt, gid, b.Epoch, b.Data); err != nil {
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
	next, verdict, err := e.judgeCommit(ctx, ch, before, p)
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
	ch.Epoch, ch.Hub, ch.Members = after.Epoch, next.Hub, members(after)
	if !after.Active {
		ch.Stopped = "this device was removed from the chat"
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// judgeCommit judges a staged commit by the rules this client implements
// for direct chats: every new or updated leaf checks out (§7.1),
// tieroute_group stays well formed and follows §7.1's basis and floor
// rules, and the chat keeps exactly its two identities (§11.3). It
// returns the new tieroute_group.
func (e *Engine) judgeCommit(ctx context.Context, ch *Chat, before *mls.State, p *mls.Processed) (*GroupExt, identity.Verdict, error) {
	bad := func(format string, args ...any) (*GroupExt, identity.Verdict, error) {
		return nil, identity.Invalid, fmt.Errorf(format, args...)
	}
	data, _ := before.Extension(ExtGroup)
	old, err := ParseGroupExt(data)
	if err != nil {
		return bad("the group's tieroute_group: %v", err)
	}
	next := old
	if data, ok := p.Commit.Extension(ExtGroup); ok {
		if next, err = ParseGroupExt(data); err != nil {
			return bad("%v", err)
		}
	} else {
		return bad("the commit drops tieroute_group")
	}
	if next.Kind != old.Kind {
		return bad("the commit changes the group's kind")
	}
	committer, ok := before.Member(p.Sender.Index)
	if p.Sender.Kind != "member" || !ok {
		return bad("a commit from outside the group")
	}
	committerID, committerDev, err := parseCredential(committer.Identity)
	if err != nil {
		return bad("%v", err)
	}

	// What the commit does, for its scope (§7.1).
	type keyCheck struct {
		id  string
		key []byte
	}
	var mustBe []keyCheck
	var leaves []*mls.LeafInfo
	adds := map[string]bool{}
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
			mustBe = append(mustBe, keyCheck{id, dev})
			leaves = append(leaves, &pr.KeyPackage.Leaf)
			adds[id] = true
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
	inScope := map[string]bool{}
	for id := range adds {
		inScope[id] = true
	}
	if len(adds) > 0 || next.Hub != old.Hub {
		inScope[committerID] = true
		mustBe = append(mustBe, keyCheck{committerID, committerDev})
	}

	// The chain history the commit binds (§7.1).
	bound := map[string]*identity.Chain{}
	for id, br := range next.Basis {
		if bytes.Equal(wire.MustJCS(br), wire.MustJCS(old.Basis[id])) && len(inScope) == 0 {
			continue // left as it was: it binds nothing (§7.1)
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
			// A group that hasn't latched: Reroute's direct chats don't bind
			// history yet, so judge on this device's copy instead.
			c, err := e.fetchChain(ctx, id, true)
			if err != nil {
				return nil, identity.Pending, err
			}
			bound[id] = c
		}
	}
	for id := range next.Basis {
		if !inScope[id] && !slices.Contains(ch.Members, id) && !adds[id] {
			return bad("the basis names %s, who isn't a member", id)
		}
	}
	for _, k := range mustBe {
		c := bound[k.id]
		if c == nil || !c.State.IsDevice(k.key) {
			return bad("a key that isn't a device of %s in the history the commit binds", k.id)
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
	// Who's in the group after the commit.
	removed := map[uint32]bool{}
	for _, pr := range p.Commit.Proposals {
		if pr.Kind == "remove" && pr.Removed != nil {
			removed[*pr.Removed] = true
		}
	}
	after := map[string]bool{}
	for _, m := range before.Members {
		if !removed[m.LeafIndex] {
			if id, _, err := parseCredential(m.Identity); err == nil {
				after[id] = true
			}
		}
	}
	for id := range adds {
		after[id] = true
	}
	if old.Kind == KindDirect && (len(after) > 2 || !after[e.c.ID()] && !p.Commit.SelfRemoved) {
		return bad("a direct chat with someone else in it")
	}
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
	return next, identity.Valid, nil
}

// processProposal handles a standalone proposal. Chats use only one: a
// device leaving, which the next committer includes (§11.1). Direct chats
// in this version of Railcar don't commit them yet; the proposal stays
// queued for the next commit.
func (e *Engine) processProposal(ctx context.Context, ch *Chat, b buffered) error {
	g := e.dev.mls.Group(mustB64(ch.ID))
	p, err := g.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		return err
	}
	if p.Kind == mls.KindOwn {
		e.dev.Revert()
		return nil
	}
	if p.Proposal == nil || p.Proposal.Kind != "remove" || p.Proposal.Removed == nil || *p.Proposal.Removed != p.Sender.Index {
		if p.Proposal != nil {
			g.RemovePendingProposal(p.Proposal.Reference)
		}
		e.dev.Save()
		return errors.New("chat: a standalone proposal other than a device leaving")
	}
	return e.dev.Save()
}
