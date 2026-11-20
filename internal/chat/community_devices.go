package chat

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// A newly linked device in its identity's communities (§7.2 step 5): the
// new device signs in to each community server the link bundle names and
// leaves key packages there, and the identity's other devices add it to
// the members group and each restricted channel's group the identity is
// in, then send it the keys those groups hold.

// claimEvery is the least time between two claims in a community for this
// identity's new devices. Tests shorten it.
var claimEvery = time.Minute

// joinLinkedCommunities is the new device's side, for the community
// servers a link bundle names. The caller holds e.mu.
func (e *Engine) joinLinkedCommunities(ctx context.Context, servers []string) {
	for _, server := range servers {
		if cm := e.st.Communities[server]; cm != nil && !cm.Left {
			continue
		}
		if err := e.joinLinked(ctx, server); err != nil {
			e.log.Info("chat: a community from the link bundle", "server", server, "err", err)
		}
	}
}

func (e *Engine) joinLinked(ctx context.Context, server string) error {
	conn := e.c.Community(server)
	preview, err := conn.Preview(ctx)
	if err != nil {
		return err
	}
	if len(preview.ID) != 32 || len(preview.Group) == 0 {
		return errors.New("chat: the community server's preview has no community or members group")
	}
	if err := conn.SignIn(ctx); err != nil {
		return err
	}
	cm := &Community{Server: server, ID: preview.ID, MembersGroup: wire.EncodeB64(preview.Group), Groups: map[string]*CGroup{}, Linking: true}
	delete(e.cchains, server)
	e.profile().Delete(chainRecord(cm.ID))
	if _, err := e.syncCommunityChain(ctx, cm, nil); err != nil {
		return err
	}
	chain, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	if !bytes.Equal(chain.ID(), preview.ID) {
		return errors.New("chat: the chain is another community's")
	}
	e.st.Communities[server] = cm
	if err := e.save(); err != nil {
		return err
	}
	if err := e.topUpCommunity(ctx, cm); err != nil {
		return err
	}
	e.refreshChannels(cm)
	go e.startCommunity(e.runCtx, server)
	return nil
}

// linkedIn notes that another device of this identity added this one to
// a community's members group. The caller holds e.mu.
func (e *Engine) linkedIn(cm *Community) {
	if cm.Linking {
		cm.Linking = false
		e.save()
		e.refreshChannels(cm)
	}
}

// missingDevices lists the devices of this identity that have no leaf
// among mine, other than this one.
func (e *Engine) missingDevices(mine []leafOf) [][]byte {
	var out [][]byte
	for _, d := range e.c.Chain().State.Devices {
		if !hasDevice(mine, d.Key) && !bytes.Equal(d.Key, e.c.DeviceKey()) {
			out = append(out, d.Key)
		}
	}
	return out
}

// syncCommunityDevices is the other devices' side: it adds this
// identity's devices that aren't in its communities' groups yet, to the
// members group first, then to each restricted channel's group this
// device is in, and sends them the keys. It reports whether every
// community is in step; a device without key packages there yet is
// tried again later. The caller holds e.mu.
func (e *Engine) syncCommunityDevices(ctx context.Context) bool {
	me := e.c.ID()
	done := true
	for _, server := range slices.Sorted(maps.Keys(e.st.Communities)) {
		cm := e.st.Communities[server]
		if cm.Left || cm.Stopped != "" || cm.Asking || cm.Linking || cm.MovingTo != "" {
			continue
		}
		g := cm.Groups[cm.MembersGroup]
		if g == nil {
			continue
		}
		st, err := e.dev.mls.Group(mustB64(g.ID)).State()
		if err != nil {
			continue
		}
		if missing := e.missingDevices(leavesByIdentity(st)[me]); len(missing) > 0 {
			// Claims are limited to 50 a day per claimant and target:
			// try a community at most once a minute.
			if time.Since(e.cclaimed[server]) < claimEvery {
				done = false
				continue
			}
			if e.cclaimed == nil {
				e.cclaimed = map[string]time.Time{}
			}
			e.cclaimed[server] = time.Now()
			ok, err := e.addOwnDevices(ctx, cm, g, missing)
			if err != nil {
				e.log.Info("chat: adding this identity's new device to a community", "server", server, "err", err)
			}
			if !ok {
				done = false
				continue
			}
		}
		for _, id := range slices.Sorted(maps.Keys(cm.Groups)) {
			rg := cm.Groups[id]
			if id == cm.MembersGroup {
				continue
			}
			rst, err := e.dev.mls.Group(mustB64(rg.ID)).State()
			if err != nil || len(e.missingDevices(leavesByIdentity(rst)[me])) == 0 {
				continue
			}
			if err := e.fillRestricted(ctx, cm, rg); err != nil {
				e.log.Info("chat: adding this identity's new device to a restricted channel", "err", err)
				done = false
			}
		}
	}
	return done
}

// addOwnDevices adds devices of this identity to a community's members
// group, with key packages they left with the community server, and sends
// them the keys. It reports whether it added them all.
func (e *Engine) addOwnDevices(ctx context.Context, cm *Community, g *CGroup, missing [][]byte) (bool, error) {
	me := e.c.ID()
	kps, err := e.c.Community(cm.Server).ClaimKeyPackages(ctx, me, missing)
	if err != nil {
		return false, err
	}
	chain := e.c.Chain()
	var adds [][]byte
	var devs []wire.Bytes
	for _, kp := range kps {
		b, err := e.checkClaimed(kp, me, chain)
		if err != nil {
			e.log.Info("chat: refused a community key package", "err", err)
			continue
		}
		adds = append(adds, b)
		devs = append(devs, kp.Dev)
	}
	if len(adds) == 0 {
		return false, nil
	}
	o := ccommitOpts{adds: adds, welcome: []string{me}, devs: devs, pending: e.hasPending(g)}
	if err := e.ccommit(ctx, cm, g, o); err != nil {
		if wire.IsCode(err, wire.CodeStaleEpoch) {
			// Another device was first, perhaps with the same Add.
			e.cfetchMissed(ctx, cm, g)
			return false, e.cdrain(ctx, cm, g)
		}
		return false, err
	}
	e.sendKeys(ctx, cm, g)
	return len(adds) == len(missing), nil
}
