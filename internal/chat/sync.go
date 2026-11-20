package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// syncWindow is how long the sync rule keeps retrying after a device
// change, for devices that haven't uploaded key packages yet (client API,
// "The sync rule").
const syncWindow = 10 * time.Minute

// syncEvery is how often the live stream retries the sync rule while it's
// unfinished.
const syncEvery = 3 * time.Second

// leafOf is one leaf in a group: its index and device.
type leafOf struct {
	index uint32
	dev   []byte
}

// leavesByIdentity lists a group's leaves by identity.
func leavesByIdentity(st *mls.State) map[string][]leafOf {
	out := map[string][]leafOf{}
	for _, m := range st.Members {
		if id, dev, err := parseCredential(m.Identity); err == nil {
			out[id] = append(out[id], leafOf{m.LeafIndex, dev})
		}
	}
	return out
}

func hasDevice(ls []leafOf, dev []byte) bool {
	return slices.ContainsFunc(ls, func(l leafOf) bool { return bytes.Equal(l.dev, dev) })
}

// wantSync asks for the sync rule to run, and to keep retrying for a
// while. The caller holds e.mu.
func (e *Engine) wantSync() {
	e.syncUntil = time.Now().Add(syncWindow)
}

// SyncDevices runs the sync rule once in every chat (§11.9): it commits
// Removes for leaves whose device left its identity's chain, and Adds for
// devices that have no leaf yet. It reports whether every chat is in step;
// devices without key packages yet are retried later.
func (e *Engine) SyncDevices(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.syncAll(ctx)
}

// retrySync runs the sync rule while it's unfinished and the window is
// open.
func (e *Engine) retrySync(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.syncUntil.IsZero() {
		return
	}
	if time.Now().After(e.syncUntil) {
		e.syncUntil = time.Time{}
		return
	}
	done, err := e.syncAll(ctx)
	if err != nil {
		e.log.Info("chat: the sync rule", "err", err)
	}
	if done {
		e.syncUntil = time.Time{}
	}
}

// syncAll is SyncDevices for a caller that holds e.mu.
func (e *Engine) syncAll(ctx context.Context) (bool, error) {
	// A device MUST NOT run the rule once its own key is no longer in the
	// chain (§11.9).
	if !e.c.Chain().State.IsDevice(e.c.DeviceKey()) {
		return true, nil
	}
	done := true
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(e.st.Chats)) {
		ch := e.st.Chats[id]
		if ch.Stopped != "" || ch.Kind == KindChannel || ch.Left {
			continue
		}
		// Apply what the hub holds first, so the commit is for the newest
		// epoch (§11.6).
		e.fetchMissed(ctx, ch)
		if err := e.drain(ctx, ch); err != nil {
			errs = append(errs, err)
		}
		ok, err := e.syncChat(ctx, ch)
		if err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", ch.ID, err))
		}
		done = done && ok && err == nil
	}
	done = e.syncCommunityDevices(ctx) && done
	e.rejoinIfDue(ctx)
	return done, errors.Join(errs...)
}

// syncChat runs the sync rule in one chat. It returns true when the
// chat's leaves match every member's current devices.
func (e *Engine) syncChat(ctx context.Context, ch *Chat) (bool, error) {
	if ch.Stopped != "" || ch.Pending {
		return ch.Stopped != "", nil
	}
	me, myDev := e.c.ID(), e.c.DeviceKey()
	g := e.dev.mls.Group(mustB64(ch.ID))
	st, err := g.State()
	if err != nil {
		return false, err
	}
	data, _ := st.Extension(ExtGroup)
	old, err := ParseGroupExt(data)
	if err != nil {
		return false, err
	}
	leaves := leavesByIdentity(st)
	chains := map[string]*identity.Chain{}
	for id := range leaves {
		c, err := e.syncChain(ctx, id)
		if err != nil {
			return false, err
		}
		chains[id] = c
	}

	// Removes first: leaves whose device is no longer in its chain.
	var removes []uint32
	removedFrom := map[string]bool{}
	for id, ls := range leaves {
		for _, l := range ls {
			if id == me && bytes.Equal(l.dev, myDev) {
				continue
			}
			if !chains[id].State.IsDevice(l.dev) {
				removes = append(removes, l.index)
				removedFrom[id] = true
			}
		}
	}
	// Then adds: devices with no leaf, with key packages claimed for
	// only those devices.
	var adds [][]byte
	var welcomeTo []string
	complete := true
	for _, id := range slices.Sorted(maps.Keys(chains)) {
		c := chains[id]
		var missing [][]byte
		for _, d := range c.State.Devices {
			if !hasDevice(leaves[id], d.Key) {
				missing = append(missing, d.Key)
			}
		}
		if len(missing) == 0 {
			continue
		}
		claimed, err := e.c.ClaimKeyPackages(ctx, id, client.ClaimOptions{Devices: missing})
		if err != nil {
			e.log.Info("chat: claiming key packages for new devices", "err", err)
			complete = false
			continue
		}
		got := 0
		for _, kp := range claimed {
			if !slices.ContainsFunc(missing, func(m []byte) bool { return bytes.Equal(m, kp.Dev) }) {
				continue
			}
			b, err := e.checkClaimed(kp, id, c)
			if err != nil {
				e.log.Info("chat: refused a claimed key package", "err", err)
				continue
			}
			adds = append(adds, b)
			got++
		}
		if got > 0 {
			welcomeTo = append(welcomeTo, id)
		}
		if got < len(missing) {
			complete = false
		}
	}
	if len(removes) == 0 && len(adds) == 0 {
		return complete, nil
	}
	if len(removes) > 0 {
		// Hold traffic until the Remove lands (§11.9).
		ch.Hold = true
	}

	// The commit binds every member's current history, which also latches
	// a group that hasn't latched (§7.1).
	after := map[string]bool{}
	for id, ls := range leaves {
		for _, l := range ls {
			if !slices.Contains(removes, l.index) {
				after[id] = true
			}
		}
	}
	for _, id := range welcomeTo {
		after[id] = true
	}
	next := old.Clone()
	next.Basis, next.Heads = map[string]BasisRef{}, map[string]ChainRef{}
	for id := range after {
		b := basisFor(chains[id])
		var floor *identity.Bound
		if f, ok := old.Heads[id]; ok {
			fb := boundOf(f)
			floor = &fb
		}
		// Judge it as members will (§7.1, "Committing").
		if v, _, err := identity.JudgeBasis(id, identity.Basis{Bound: boundOf(b.Ref()), Tail: b.Tail}, chains[id].Entries, floor, identity.NoCheck, nil); v != identity.Valid {
			return false, fmt.Errorf("chat: %s's history conflicts with what the chat already bound: %v", id, err)
		}
		next.Basis[id], next.Heads[id] = b, b.Ref()
	}
	settleExt(next, after)
	if old.Kind == KindDirect && (!after[me] || len(after) > 2 || slices.ContainsFunc(slices.Collect(maps.Keys(after)), func(id string) bool { return id != me && id != ch.Peer })) {
		return false, errors.New("chat: the change would leave the direct chat without its two people")
	}
	to := slices.Sorted(maps.Keys(leaves))
	if err := e.commit(ctx, ch, g, mls.CommitOptions{Adds: adds, Removes: removes}, next, to, welcomeTo); err != nil {
		if wire.IsCode(err, wire.CodeStaleEpoch) {
			// Someone else got there first: process theirs, and check
			// again on the next round.
			e.fetchMissed(ctx, ch)
			return false, e.drain(ctx, ch)
		}
		return false, err
	}
	afterSt, err := g.State()
	if err != nil {
		return false, err
	}
	ch.Hold = false
	ch.Members, ch.Admins = members(afterSt), slices.Clone(next.Admins)
	if err := e.save(); err != nil {
		return false, err
	}
	e.deviceLines(ch, st, afterSt)
	if ch.Kind == KindGroup && len(adds) > 0 && ch.Name != "" {
		// New devices learn the group's name and topic from it (§12).
		if err := e.sendMeta(ctx, ch); err != nil {
			e.log.Info("chat: the group's metadata for new devices", "err", err)
		}
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return complete, nil
}

// freshEvery is the least time between two fresh fetches of another
// identity's chain for the sync rule, so retrying doesn't ask its home
// every few seconds (§7.1, §11.9).
const freshEvery = time.Minute

// syncChain returns an identity's chain for the sync rule: fresh from its
// home at most once a minute, and this device's copy in between. The
// identity's own chain comes from its homeserver, which is its home.
func (e *Engine) syncChain(ctx context.Context, id string) (*identity.Chain, error) {
	if id == e.c.ID() {
		return e.fetchChain(ctx, id, true)
	}
	if time.Since(e.fresh[id]) < freshEvery {
		if c, err := e.c.CachedChain(id); err == nil && c != nil {
			return c, nil
		}
	}
	c, err := e.fetchChain(ctx, id, true)
	if err == nil {
		e.fresh[id] = time.Now()
	}
	return c, err
}

// removalWaiting reports whether a group still has a leaf whose device
// left its identity's chain, by this device's copies.
func (e *Engine) removalWaiting(st *mls.State) bool {
	for id, ls := range leavesByIdentity(st) {
		c, err := e.c.CachedChain(id)
		if err != nil || c == nil {
			continue
		}
		for _, l := range ls {
			if !c.State.IsDevice(l.dev) {
				return true
			}
		}
	}
	return false
}

// deviceLines adds a system line to a chat for each identity whose devices
// changed between two states of its group (§11.9, "System lines").
func (e *Engine) deviceLines(ch *Chat, before, after *mls.State) {
	b, a := leavesByIdentity(before), leavesByIdentity(after)
	for _, id := range slices.Sorted(maps.Keys(a)) {
		if _, ok := b[id]; !ok {
			continue // a new person, not a new device
		}
		added, removed := false, false
		for _, l := range a[id] {
			if !hasDevice(b[id], l.dev) {
				added = true
			}
		}
		for _, l := range b[id] {
			if !hasDevice(a[id], l.dev) {
				removed = true
			}
		}
		who := "@" + e.handleOf(id)
		switch {
		case added && id == e.c.ID():
			e.systemLine(ch, "A new device was added to your account")
		case added:
			e.systemLine(ch, who+" has a new device")
		}
		if removed {
			if id == e.c.ID() {
				e.systemLine(ch, "A device was removed from your account")
			} else {
				e.systemLine(ch, "A device of "+who+"'s was removed")
			}
		}
	}
}

// systemLine adds a line the client shows on its own to a chat's history.
func (e *Engine) systemLine(ch *Chat, text string) {
	now := e.c.Clock().Now().Unix()
	m := Message{ID: "system-" + newMsgID(), TS: now, Received: now, Body: text, System: true, Status: Sent}
	if err := e.addMessage(ch, m); err != nil {
		e.log.Info("chat: a system line", "err", err)
	}
}
