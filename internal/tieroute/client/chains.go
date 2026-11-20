package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

type chainRes struct {
	Entries []wire.Signed `json:"entries"`
	Head    uint64        `json:"head"`
}

// ChainWarning is a refused chain update: the homeserver served a chain
// that neither extends this device's copy nor is a valid recovery fork
// (§6.5). Show it as a warning that someone may be trying to slip a device
// into that person's identity.
type ChainWarning struct {
	ID      string
	Warning string // identity.WarnUnverified or identity.WarnRecoveryCompromised, or ""
	Err     error  // why the served chain was invalid, if it was
}

func (w *ChainWarning) Error() string {
	switch {
	case w.Warning == identity.WarnRecoveryCompromised:
		return "client: " + w.ID + " has two recovery-signed branches; its recovery code may be compromised"
	case w.Err != nil:
		return fmt.Sprintf("client: refused an invalid chain for %s: %v", w.ID, w.Err)
	default:
		return "client: refused a chain for " + w.ID + " that conflicts with this device's copy"
	}
}

func chainRecord(id string) string { return recChain + strings.TrimPrefix(id, "tr:") }

// cachedChain returns this device's copy of a chain, or nil.
func (c *Client) cachedChain(id string) (*identity.Chain, error) {
	if id == c.ID() {
		return c.Chain(), nil
	}
	var entries []wire.Signed
	err := c.profile.Load(chainRecord(id), &entries)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return identity.Validate(entries, identity.Options{})
}

// FetchOptions adjust FetchChain.
type FetchOptions struct {
	// Home is the identity's homeserver, as resolve returned it, for an
	// identity the homeserver hasn't cached.
	Home string
	// Fresh asks the homeserver to fetch the chain from the identity's
	// home again instead of answering from its cache (fresh=1).
	Fresh bool
}

// FetchChain fetches an identity's chain through the homeserver
// (GET chain/{id}), validates it, and compares it with this device's copy
// (§6.5): a chain that extends the copy, or a valid recovery fork, replaces
// it; an out-of-date answer keeps it; anything else is refused with a
// *ChainWarning.
func (c *Client) FetchChain(ctx context.Context, id string, opts FetchOptions) (*identity.Chain, error) {
	if !identity.ValidIdentifier(id) {
		return nil, fmt.Errorf("client: %q isn't an identifier", id)
	}
	q := url.Values{}
	if opts.Home != "" {
		q.Set("home", opts.Home)
	}
	if opts.Fresh {
		q.Set("fresh", "1")
	}
	var res chainRes
	if err := c.authed(ctx, http.MethodGet, "chain/"+id, q, nil, &res); err != nil {
		return nil, err
	}
	return c.acceptChain(id, res.Entries)
}

// acceptChain compares a served chain with this device's copy and keeps
// the winner (§6.5, "Client chain copies" in the client API).
func (c *Client) acceptChain(id string, entries []wire.Signed) (*identity.Chain, error) {
	cached, err := c.cachedChain(id)
	if err != nil {
		return nil, err
	}
	cmp := identity.Compare(cached, entries, identity.Options{Now: c.clock.Now()})
	if cmp.Outcome == identity.Reject {
		return nil, &ChainWarning{ID: id, Warning: cmp.Warning, Err: cmp.Err}
	}
	if cmp.Chain.ID() != id {
		return nil, &ChainWarning{ID: id, Err: fmt.Errorf("the chain is for %s", cmp.Chain.ID())}
	}
	if cmp.Outcome == identity.Same {
		return cached, nil
	}
	if id == c.ID() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.chain = cmp.Chain
		if !c.chain.State.IsDevice(c.device.ID()) && !c.promoteRotation() {
			// Keep the chain: it's how the interface shows why.
			_ = c.saveLocked()
			return cmp.Chain, ErrDeviceRemoved
		}
		return cmp.Chain, c.saveLocked()
	}
	return cmp.Chain, c.profile.Save(chainRecord(id), cmp.Chain.Entries)
}

// Resolved is a handle that resolved both ways: its record points to the
// identity, and the identity's chain names the handle (§5.3).
type Resolved struct {
	Handle string
	ID     string
	Home   string
	Chain  *identity.Chain
}

type resolveRes struct {
	ID   string `json:"id"`
	Home string `json:"home"`
}

// ErrHandleMismatch reports a handle whose identity's chain doesn't name
// it. Show it as "handle no longer valid" (§5.3).
var ErrHandleMismatch = errors.New("client: the handle doesn't match the identity's chain")

// ResolveHandle resolves a handle through the homeserver
// (GET resolve/{handle}), then fetches the identity's chain, following
// moves to its current home, and checks that the chain's current handle
// is the handle asked for (§5.3).
func (c *Client) ResolveHandle(ctx context.Context, handle string) (*Resolved, error) {
	handle = identity.NormalizeHandle(handle)
	if err := identity.CheckHandle(handle); err != nil {
		return nil, err
	}
	var res resolveRes
	if err := c.authed(ctx, http.MethodGet, "resolve/"+handle, nil, nil, &res); err != nil {
		return nil, err
	}
	if !identity.ValidIdentifier(res.ID) {
		return nil, fmt.Errorf("client: resolve answered %q", res.ID)
	}
	home := res.Home
	var chain *identity.Chain
	var err error
	// The record's home is only a hint; the chain is authoritative. A chain
	// that names another home is fetched from there (§5.3 step 3).
	for hop := 0; hop < 4; hop++ {
		chain, err = c.FetchChain(ctx, res.ID, FetchOptions{Home: home})
		if err != nil {
			return nil, err
		}
		if chain.State.Home == home {
			break
		}
		home = chain.State.Home
	}
	if chain.State.Handle != handle {
		return nil, ErrHandleMismatch
	}
	return &Resolved{Handle: handle, ID: res.ID, Home: chain.State.Home, Chain: chain}, nil
}

type appendReq struct {
	Entry       wire.Signed  `json:"entry"`
	Reservation *wire.Signed `json:"reservation,omitempty"`
}

type appendRes struct {
	Seq  uint64     `json:"seq"`
	Head wire.Bytes `json:"head"`
}

// Append signs an entry with this device's key and appends it to the
// identity's chain (POST chain/append, §6.4). If another device appended
// first (stale-head), it fetches the chain and tries again. A clock-skew
// answer corrects the clock and retries once.
func (c *Client) Append(ctx context.Context, ops ...identity.Op) (*identity.Chain, error) {
	skewed := false
	for attempt := 0; attempt < 4; attempt++ {
		chain := c.Chain()
		e := chain.Next(c.clock.Now().Unix(), ops...)
		s, err := c.device.SignEntry(e)
		if err != nil {
			return nil, err
		}
		// Check the entry before sending it, so a bad op fails here.
		next := chain.Prefix(chain.Len())
		if err := next.Append(s, identity.Options{Now: c.clock.Now()}); err != nil {
			return nil, err
		}
		var res appendRes
		err = c.authed(ctx, http.MethodPost, "chain/append", nil, appendReq{Entry: s}, &res)
		switch {
		case wire.IsCode(err, wire.CodeStaleHead):
			if _, err := c.FetchChain(ctx, c.ID(), FetchOptions{}); err != nil {
				return nil, err
			}
			continue
		case wire.IsCode(err, wire.CodeClockSkew) && !skewed:
			skewed = true
			continue
		case err != nil:
			return nil, err
		}
		if res.Seq != e.Seq || len(res.Head) != 32 || [32]byte(res.Head) != next.State.Head {
			return nil, fmt.Errorf("client: the homeserver appended seq %s, not %d", strconv.FormatUint(res.Seq, 10), e.Seq)
		}
		return c.acceptChain(c.ID(), next.Entries)
	}
	return nil, errors.New("client: the chain kept changing; try again")
}
