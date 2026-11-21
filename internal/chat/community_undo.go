package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Undoing a refused commit in a community group (§16.5): the owner, or a
// member with administrate, signs a commit-undo, and the community server
// sets the group back to the epoch the commit was made in. Members that
// refused the commit forget it, and carry on with the next commit for that
// epoch.

// TypeCommitUndo is a commit-undo's type tag.
const TypeCommitUndo = "commit-undo"

// CommitUndo is the body of a commit-undo (§16.5).
type CommitUndo struct {
	Community wire.Bytes `json:"community"`
	Group     wire.Bytes `json:"group"`
	Epoch     uint64     `json:"epoch"`
	Hash      wire.Bytes `json:"hash"`
	By        string     `json:"by"`
	TS        int64      `json:"ts"`
}

// RefusedCommit is the commit that stopped a community: its group, the
// epoch it was made in, its hash, and who made it.
type RefusedCommit struct {
	Group string     `json:"group"`
	Epoch uint64     `json:"epoch"`
	Hash  wire.Bytes `json:"hash"`
	By    string     `json:"by,omitempty"`
}

// ErrNoUndo reports a community server written before undoing.
var ErrNoUndo = errors.New("chat: this community server can't undo a change, so the community's groups have to be re-created instead")

// refused records the commit that stopped a community, and tells the owner
// and administrators who made it and that they can undo it. The caller
// holds e.mu.
func (e *Engine) refused(cm *Community, g *CGroup, b buffered, before *mls.State, p *mls.Processed) {
	sum := sha256.Sum256(b.Data)
	r := &RefusedCommit{Group: g.ID, Epoch: b.Epoch, Hash: sum[:]}
	switch {
	case p.Sender.Kind == "new_member_commit" && p.Commit != nil && p.Commit.PathLeaf != nil:
		r.By, _, _ = parseCredential(p.Commit.PathLeaf.Identity)
	case p.Sender.Kind == "member":
		if m, ok := before.Member(p.Sender.Index); ok {
			r.By, _, _ = parseCredential(m.Identity)
		}
	}
	cm.Refused = r
	e.save()
	if c, err := e.communityChain(cm); err == nil {
		me := e.c.ID()
		if st := c.State(); st.Owner == me || st.Can(me, community.PermAdministrate) {
			if ch := e.firstChannel(cm); ch != nil {
				who := "someone"
				if r.By != "" {
					who = e.nameOf(r.By)
				}
				e.systemLine(ch, "A change from "+who+" broke the community's rules, so the community stopped. Type :undo to undo it, and consider banning them")
			}
		}
	}
}

// UndoRefused asks the community server to undo the commit that stopped a
// community, as its owner or a member with administrate (§16.5).
func (e *Engine) UndoRefused(ctx context.Context, server string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm := e.st.Communities[server]
	if cm == nil || cm.Left {
		return errors.New("chat: you're not in that community")
	}
	r := cm.Refused
	if r == nil {
		return errors.New("chat: no change stopped this community")
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	me := e.c.ID()
	if st := c.State(); st.Owner != me && !st.Can(me, community.PermAdministrate) {
		return errors.New("chat: undoing a change needs the owner or administrate")
	}
	u := CommitUndo{Community: cm.ID, Group: mustB64(r.Group), Epoch: r.Epoch, Hash: r.Hash, By: me, TS: e.c.Clock().Now().Unix()}
	s, err := e.c.SignAsDevice(TypeCommitUndo, u)
	if err != nil {
		return err
	}
	err = e.c.Community(server).Undo(ctx, s)
	if wire.IsCode(err, wire.CodeNotFound) {
		return ErrNoUndo
	}
	if err != nil {
		return err
	}
	return e.takeUndo(ctx, cm, s)
}

// takeUndo applies a commit-undo for the commit that stopped a community:
// signed by a current device of the owner or a member with administrate as
// of the entry the members group last named, for that group, epoch, and
// hash (§16.5, step 3). An undo for anything else is ignored. The caller
// holds e.mu.
func (e *Engine) takeUndo(ctx context.Context, cm *Community, s wire.Signed) error {
	r := cm.Refused
	if r == nil || s.T != TypeCommitUndo {
		return nil
	}
	var u CommitUndo
	if err := s.Body(&u); err != nil {
		return nil
	}
	if !bytes.Equal(u.Community, cm.ID) || wire.EncodeB64(u.Group) != r.Group || u.Epoch != r.Epoch || !bytes.Equal(u.Hash, r.Hash) {
		return nil
	}
	ic, err := e.identityChain(ctx, cm, u.By, true)
	if err != nil {
		return retryable{err}
	}
	dev, ok := ic.State.Device(s.K)
	if !ok || verifyDevice(s, dev.Pub) != nil {
		return fmt.Errorf("chat: a commit-undo not signed by a current device of %s", u.By)
	}
	st, err := e.authorityState(ctx, cm)
	if err != nil {
		return err
	}
	if st.Owner != u.By && !st.Can(u.By, community.PermAdministrate) {
		return fmt.Errorf("chat: a commit-undo from %s, who can't undo changes", u.By)
	}
	cm.Refused, cm.Stopped = nil, ""
	if g := cm.Groups[r.Group]; g != nil {
		g.Buffered, g.Pending = nil, false
	}
	e.save()
	e.refreshChannels(cm)
	if ch := e.firstChannel(cm); ch != nil {
		e.systemLine(ch, e.nameOf(u.By)+" undid the change that stopped the community, and it carries on")
	}
	return nil
}

// authorityState is the community chain's state as of the entry the
// members group last named, or the newest when it names none.
func (e *Engine) authorityState(ctx context.Context, cm *Community) (*community.State, error) {
	if g := cm.Groups[cm.MembersGroup]; g != nil {
		if mst, err := e.dev.mls.Group(mustB64(g.ID)).State(); err == nil {
			if h, ok := communityHead(mst.Extensions); ok {
				st, _, err := e.stateAtHead(ctx, cm, h)
				return st, err
			}
		}
	}
	c, err := e.communityChain(cm)
	if err != nil {
		return nil, err
	}
	return c.State(), nil
}

// checkUndos looks for an undo of the commit that stopped a community in
// the community server's commits for its group (§16.5). The caller holds
// e.mu.
func (e *Engine) checkUndos(ctx context.Context, cm *Community) {
	r := cm.Refused
	if r == nil {
		return
	}
	after := uint64(0)
	if r.Epoch > 0 {
		after = r.Epoch - 1
	}
	undos, err := e.c.Community(cm.Server).Undos(ctx, mustB64(r.Group), after)
	if err != nil {
		return
	}
	for _, s := range undos {
		if err := e.takeUndo(ctx, cm, s); err != nil {
			e.log.Info("chat: a commit-undo", "err", err)
		}
		if cm.Refused == nil {
			return
		}
	}
}
