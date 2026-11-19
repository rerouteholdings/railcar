package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Moving to another homeserver (§6.6): ask the new server first and get
// its move reservation, append the device-signed move on the old home
// with it, then register there with the whole chain and a signed move
// that carries the allowlists. A move that stopped partway is kept, and
// finished before anything else.

// TypeMove and TypeMoveReservation are the signed types of a move and a
// move reservation (§6.6).
const (
	TypeMove            = "move"
	TypeMoveReservation = "move-reservation"
)

// moveBody is the body of a signed move (§6.6).
type moveBody struct {
	ID       string       `json:"id"`
	Server   string       `json:"server"`
	Head     wire.Bytes   `json:"head"`
	TS       int64        `json:"ts"`
	Contacts []string     `json:"contacts"`
	Groups   []wire.Bytes `json:"groups"`
}

// PendingMove is a move this device started and hasn't finished.
type PendingMove struct {
	Server      string       `json:"server"`
	Origin      string       `json:"origin,omitempty"`
	Invite      string       `json:"invite,omitempty"`
	Entry       wire.Signed  `json:"entry"`
	Reservation *wire.Signed `json:"reservation,omitempty"`
	Contacts    []string     `json:"contacts"`
	Groups      []string     `json:"groups"`
	// Appended is set once the old home's chain shows the entry.
	Appended bool `json:"appended,omitempty"`
}

type moveRegisterReq struct {
	Chain  []wire.Signed `json:"chain"`
	Move   wire.Signed   `json:"move"`
	Invite string        `json:"invite,omitempty"`
	Check  bool          `json:"check,omitempty"`
}

type moveRegisterRes struct {
	ID          string       `json:"id"`
	Reservation *wire.Signed `json:"reservation,omitempty"`
}

// MoveRequest is what a move needs: the new homeserver, the name to use
// under its handle domain, its registration code, and the allowlists
// only this identity's devices know (§10.3).
type MoveRequest struct {
	Server   string
	Name     string
	Invite   string
	Contacts []string
	Groups   []string // group IDs, base64url
}

// PendingMove returns the move this device hasn't finished, if any.
func (c *Client) PendingMove() *PendingMove {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Move == nil {
		return nil
	}
	m := *c.state.Move
	return &m
}

// otherAPI is the client API of another homeserver, without a session.
func (c *Client) otherAPI(ctx context.Context, server string) (*api, error) {
	doc, err := c.resolver.ServerDoc(ctx, server)
	if err != nil {
		return nil, err
	}
	if doc.Name != server {
		return nil, fmt.Errorf("client: %s's server document names %s", server, doc.Name)
	}
	base, err := clientBase(doc)
	if err != nil {
		return nil, err
	}
	return &api{base: base, http: c.opts.httpClient(), clock: c.clock, log: c.opts.Logger, token: func() string { return "" }}, nil
}

// signMove signs a move for the chain whose newest entry is head.
func (c *Client) signMove(m *PendingMove, head [32]byte) (wire.Signed, error) {
	groups := make([]wire.Bytes, 0, len(m.Groups))
	for _, g := range m.Groups {
		b, err := wire.DecodeB64(g)
		if err != nil {
			return wire.Signed{}, err
		}
		groups = append(groups, b)
	}
	return sig.SignDevice(c.device.Key, TypeMove, moveBody{
		ID: c.ID(), Server: m.Server, Head: head[:], TS: c.clock.Now().Unix(),
		Contacts: slices.Clone(m.Contacts), Groups: groups,
	})
}

// Move moves the identity to another homeserver (§6.6). It returns once
// the identity is registered there and this device is signed in to it.
// Afterwards the account keys and account data are copied over, and the
// backup's segments too. A move interrupted after the append is kept;
// FinishMove completes it.
func (c *Client) Move(ctx context.Context, r MoveRequest) error {
	if c.PendingMove() != nil {
		return c.FinishMove(ctx)
	}
	if r.Server == c.Server() {
		return errors.New("client: that's this identity's homeserver already")
	}
	newAPI, err := c.otherAPI(ctx, r.Server)
	if err != nil {
		return err
	}
	doc, err := c.resolver.ServerDoc(ctx, r.Server)
	if err != nil {
		return err
	}
	ops := []identity.Op{identity.SetHome(r.Server)}
	if doc.Handles != "" && r.Name != "" {
		ops = append(ops, identity.SetHandle(r.Name+"."+doc.Handles))
	}
	chain := c.Chain()
	s, err := c.device.SignEntry(chain.Next(c.clock.Now().Unix(), ops...))
	if err != nil {
		return err
	}
	next := chain.Prefix(chain.Len())
	if err := next.Append(s, identity.Options{Now: c.clock.Now()}); err != nil {
		return err
	}
	m := &PendingMove{Server: r.Server, Invite: r.Invite, Entry: s, Contacts: r.Contacts, Groups: r.Groups}
	if o, ok := c.opts.Origins[r.Server]; ok {
		m.Origin = o
	}
	// Ask first: a refusal here changes nothing.
	mv, err := c.signMove(m, next.State.Head)
	if err != nil {
		return err
	}
	var res moveRegisterRes
	if err := newAPI.call(ctx, http.MethodPost, "register", nil, moveRegisterReq{Chain: next.Entries, Move: mv, Invite: r.Invite, Check: true}, &res); err != nil {
		return fmt.Errorf("client: %s won't take the move: %w", r.Server, err)
	}
	if res.Reservation == nil {
		return fmt.Errorf("client: %s took the move without a reservation", r.Server)
	}
	if err := c.checkReservation(ctx, *res.Reservation, r.Server, s.Hash()); err != nil {
		return err
	}
	m.Reservation = res.Reservation
	c.mu.Lock()
	c.state.Move = m
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.FinishMove(ctx)
}

// checkReservation checks a move reservation the new server gave: its
// type, server key, identity, server, and entry (§6.6).
func (c *Client) checkReservation(ctx context.Context, rs wire.Signed, server string, entry [32]byte) error {
	if rs.T != TypeMoveReservation || rs.Kid == "" {
		return errors.New("client: the move reservation isn't signed by the new server")
	}
	var b struct {
		ID     string     `json:"id"`
		Server string     `json:"server"`
		Entry  wire.Bytes `json:"entry"`
		Exp    int64      `json:"exp"`
	}
	if err := rs.Body(&b); err != nil {
		return err
	}
	if b.ID != c.ID() || b.Server != server || len(b.Entry) != 32 || [32]byte(b.Entry) != entry {
		return errors.New("client: the move reservation is for another move")
	}
	return nil
}

// FinishMove completes a pending move: it appends the entry on the old
// home with its reservation, unless the old home's chain already has it,
// then registers on the new home and switches to it.
func (c *Client) FinishMove(ctx context.Context) error {
	m := c.PendingMove()
	if m == nil {
		return nil
	}
	if !m.Appended {
		// A lost reply looks like a failure, so read the old home's
		// chain first (§6.6, "Retrying a move").
		var have bool
		fresh, err := c.FetchChain(ctx, c.ID(), FetchOptions{Fresh: true})
		if err == nil {
			have = slices.ContainsFunc(fresh.Entries, func(e wire.Signed) bool { return e.Hash() == m.Entry.Hash() })
		}
		if !have {
			chain := c.Chain()
			next := chain.Prefix(chain.Len())
			if err := next.Append(m.Entry, identity.Options{Now: c.clock.Now()}); err != nil {
				// The chain moved on, or the entry is too old: the move
				// can't happen as it was. Nothing was appended.
				c.dropMove()
				return fmt.Errorf("client: the move can't be appended anymore; start it again: %w", err)
			}
			var res appendRes
			err := c.authed(ctx, http.MethodPost, "chain/append", nil, appendReq{Entry: m.Entry, Reservation: m.Reservation}, &res)
			if wire.IsCode(err, wire.CodeNotAllowed) {
				// A 403 means nothing changed (§6.6).
				c.dropMove()
				return fmt.Errorf("client: the homeserver refused the move: %w", err)
			}
			if err != nil {
				return fmt.Errorf("client: the move is waiting: %w", err)
			}
			if _, err := c.acceptChain(c.ID(), next.Entries); err != nil && !errors.Is(err, ErrDeviceRemoved) {
				return err
			}
		}
		c.mu.Lock()
		c.state.Move.Appended = true
		err = c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return c.registerMoved(ctx, m)
}

func (c *Client) dropMove() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Move = nil
	_ = c.saveLocked()
}

// registerMoved registers the moved identity on the new home with a newly
// signed move, copies the account keys and data and the backup, and
// switches this device to the new home.
func (c *Client) registerMoved(ctx context.Context, m *PendingMove) error {
	newAPI, err := c.otherAPI(ctx, m.Server)
	if err != nil {
		return err
	}
	chain := c.Chain()
	mv, err := c.signMove(m, chain.State.Head)
	if err != nil {
		return err
	}
	var res moveRegisterRes
	if err := newAPI.call(ctx, http.MethodPost, "register", nil, moveRegisterReq{Chain: chain.Entries, Move: mv, Invite: m.Invite}, &res); err != nil {
		return fmt.Errorf("client: registering on %s: %w", m.Server, err)
	}
	if res.ID != c.ID() {
		return fmt.Errorf("client: %s registered %s", m.Server, res.ID)
	}
	// What the old home holds, read while this device still has its
	// session there. The old home keeps signing devices in (§6.6).
	keys, kerr := c.AccountKeys(ctx)
	var data *AccountData
	if len(c.AccountKey()) == 16 {
		data, _ = c.GetAccountData(ctx)
	}
	segs := c.readBackupRaw(ctx)

	c.mu.Lock()
	c.state.Server, c.state.Origin = m.Server, m.Origin
	c.state.Token, c.state.TokenExpires, c.state.Cursor = "", 0, ""
	c.state.Move = nil
	c.api = nil
	c.resolver.Self = m.Server
	err = c.saveLocked()
	restart := c.restart
	c.restart = make(chan struct{})
	c.mu.Unlock()
	if err != nil {
		return err
	}
	close(restart)
	if err := c.SignIn(ctx); err != nil {
		return err
	}
	if kerr == nil {
		for _, w := range keys {
			if w.Via == viaRecovery || w.Via == viaPassphrase {
				if err := c.StoreWrappedKey(ctx, w); err != nil {
					c.log().Info("client: copying a wrapped key to the new home", "via", w.Via, "err", err)
				}
			}
		}
	}
	if data != nil && data.Version > 0 {
		data.Version = 0
		if err := c.PutAccountData(ctx, data); err != nil {
			c.log().Info("client: copying the account data to the new home", "err", err)
		}
	}
	for _, s := range segs {
		if err := c.authed(ctx, http.MethodPost, "backup/put", nil, s, nil); err != nil {
			c.log().Info("client: copying the backup to the new home", "seq", s.Seq, "err", err)
			break
		}
	}
	return nil
}

// rawSegment is a backup segment as stored: its ciphertext is bound to the
// identity and seq, not the homeserver, so it copies as it is.
type rawSegment struct {
	Seq      uint64     `json:"seq"`
	CT       wire.Bytes `json:"ct"`
	Snapshot bool       `json:"snapshot,omitempty"`
}

// readBackupRaw reads the backup's segments from the newest snapshot on,
// without opening them. It gives nothing when there's no backup or it
// can't be read.
func (c *Client) readBackupRaw(ctx context.Context) []rawSegment {
	list, err := c.ListBackup(ctx)
	if err != nil || len(list) == 0 {
		return nil
	}
	from := 0
	for i, s := range list {
		if s.Snapshot {
			from = i
		}
	}
	var out []rawSegment
	for _, s := range list[from:] {
		var r rawSegment
		q := map[string][]string{"seq": {fmt.Sprint(s.Seq)}}
		if err := c.authed(ctx, http.MethodGet, "backup/get", q, nil, &r); err != nil {
			return nil
		}
		r.Snapshot = s.Snapshot
		out = append(out, r)
	}
	return out
}

// Restarted returns a channel that closes when this device switches to
// another homeserver, so the stream reconnects there.
func (c *Client) Restarted() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restart
}

// log returns the client's logger, or one that drops everything.
func (c *Client) log() *slog.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	return slog.New(slog.DiscardHandler)
}
