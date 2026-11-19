package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

type recoverReq struct {
	Entry wire.Signed `json:"entry"`
}

type recoverRes struct {
	Seq  uint64     `json:"seq"`
	Head wire.Bytes `json:"head"`
}

type accountKeysRes struct {
	Items []WrappedKey `json:"items"`
}

// ErrNoRecoveryWrap reports a homeserver that has no recovery wrap of the
// account key that these words open (§7.4).
var ErrNoRecoveryWrap = errors.New("client: the homeserver has no copy of the account key these words open")

// Recover signs in to the identity with handle on server with its 12
// recovery words, as a new device, for a user who has lost every device
// (client API, Recover an identity; §6.3, §6.4.1). It finds the current
// recovery key, signs one entry with it that adds this device, removes
// every other device, and commits to the next recovery key, and sends it
// to POST recover. It then signs in, gets the account key back from the
// "recovery" wrap (§7.4), and creates the profile in profileDir.
//
// When a device-signed entry moved the identity since the newest
// recovery-signed entry, the entry attaches right before that move, which
// undoes it under the fork rule (§6.4.1).
//
// The words, prk, and the keys derived from them aren't kept (§6.1).
func Recover(ctx context.Context, server, handle string, words []string, profileDir string, opts Options) (*Client, error) {
	rec, err := identity.RecoveryFromWords(words)
	if err != nil {
		return nil, err
	}
	f, err := findIdentity(ctx, server, handle, opts, true)
	if err != nil {
		return nil, err
	}
	chain := f.chain

	// Where the entry attaches: on the head, or right before the first
	// device-signed move after the newest recovery-signed entry.
	attach := chain.Len()
	for p := max(1, int(chain.State.LastRecoverySeq+1)); p < chain.Len(); p++ {
		info := chain.Info[p]
		if info.Recovery {
			continue
		}
		moved := false
		for _, op := range info.Body.Ops {
			if op.Op == identity.OpSetHome && op.Home != info.HomeBefore {
				moved = true
			}
		}
		if moved {
			attach = p
			break
		}
	}
	base := chain.Prefix(attach)
	i, err := rec.Find(&base.State)
	if err != nil {
		return nil, err
	}
	key, err := rec.Key(i)
	if err != nil {
		return nil, err
	}
	next, err := rec.Commitment(i + 1)
	if err != nil {
		return nil, err
	}
	dev, err := identity.GenerateDevice(opts.DeviceName)
	if err != nil {
		return nil, err
	}

	// Removing first, then adding, keeps the count at 10 or under (§6.4
	// rule 4).
	var ops []identity.Op
	for _, d := range base.State.Devices {
		ops = append(ops, identity.RemoveDevice(d.Key))
	}
	ops = append(ops, identity.AddDevice(dev.Key.Public(), dev.Name), identity.SetRecovery(next))
	var res recoverRes
	var entry wire.Signed
	for attempt := 0; ; attempt++ {
		ts := max(f.clock.Now().Unix(), base.State.TS)
		entry, err = identity.SignWithRecovery(key, base.Next(ts, ops...))
		if err != nil {
			return nil, err
		}
		check := chain.Prefix(attach)
		if err := check.Append(entry, identity.Options{Now: f.clock.Now()}); err != nil {
			return nil, err
		}
		err = f.api.call(ctx, http.MethodPost, "recover", nil, recoverReq{Entry: entry}, &res)
		if wire.IsCode(err, wire.CodeClockSkew) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}

	// Sign in as the new device, and take the chain the homeserver now
	// holds: it has to end with the entry just sent.
	sres, err := signInDevice(ctx, f.api, f.clock, chain.ID(), f.server, dev)
	if err != nil {
		return nil, fmt.Errorf("client: recovered, but signing in failed: %w", err)
	}
	a := *f.api
	a.token = func() string { return sres.Token }
	var cr chainRes
	if err := a.call(ctx, http.MethodGet, "chain/"+chain.ID(), nil, nil, &cr); err != nil {
		return nil, err
	}
	now, err := identity.Validate(cr.Entries, identity.Options{Now: f.clock.Now()})
	if err != nil {
		return nil, err
	}
	if now.ID() != chain.ID() || !now.State.IsDevice(dev.ID()) || now.Len() <= attach || now.Info[attach].Hash != entry.Hash() {
		return nil, errors.New("client: the homeserver's chain doesn't hold the recovery entry")
	}

	// The account key, from the "recovery" wrap, or from a staged wrap of
	// this code if a rotation to it stopped before promoting it (§6.3).
	var keys accountKeysRes
	if err := a.call(ctx, http.MethodGet, "account/keys", nil, nil, &keys); err != nil {
		return nil, err
	}
	commit1, err := rec.Commitment(1)
	if err != nil {
		return nil, err
	}
	wrapKey := rec.WrapKey()
	defer clear(wrapKey)
	var accountKey []byte
	for _, via := range []string{viaRecovery, viaRecoveryNext} {
		for _, w := range keys.Items {
			if w.Via != via || (len(w.Commit) > 0 && !bytes.Equal(w.Commit, commit1[:])) {
				continue
			}
			if k, err := unwrapAccountKey(w, wrapKey, chain.ID()); err == nil && len(k) == 16 {
				accountKey = k
				break
			}
		}
		if accountKey != nil {
			break
		}
	}
	if accountKey == nil {
		return nil, ErrNoRecoveryWrap
	}

	p, err := store.Create(profileDir, opts.Store)
	if err != nil {
		return nil, err
	}
	st := savedState{
		Server: f.server, ID: chain.ID(), AccountKey: accountKey,
		Token: sres.Token, TokenExpires: sres.Expires, Chain: now.Entries,
	}
	if o, ok := opts.Origins[f.server]; ok {
		st.Origin = o
	}
	c := newClient(p, opts)
	c.clock = f.clock
	c.resolver.Self = f.server
	c.state, c.device, c.chain = st, dev, now
	c.mu.Lock()
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		p.Destroy()
		return nil, err
	}
	return c, nil
}
