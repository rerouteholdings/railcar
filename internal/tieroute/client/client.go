// Package client is Railcar's Tieroute client: it registers an identity on
// a homeserver, signs in, keeps its chain and keys in an encrypted profile,
// and receives the inbox over the live stream. It speaks wire version 2
// only.
package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/discovery"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Options configure a client. The zero value works for the public network.
type Options struct {
	// HTTP is the HTTP client; nil means a client with a 30-second timeout.
	HTTP *http.Client
	// Origins maps server names to origins for local test networks, such
	// as "a.test" to "http://127.0.0.1:18471" (§5.1). See
	// discovery.Resolver.
	Origins map[string]string
	// Store adjusts how the profile's key is kept.
	Store store.Options
	// Logger receives debug lines: request paths and statuses, never
	// tokens, keys, or bodies. Nil means no logging.
	Logger *slog.Logger
	// DeviceName labels this device in its chain and session lists, such
	// as "Kat's laptop".
	DeviceName string
}

func (o Options) httpClient() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Records in the profile.
const (
	recState = "identity"
	recChain = "chain." // followed by the identifier without "tr:"
)

// savedDevice is this device's private keys, as stored at rest.
type savedDevice struct {
	MLDSASeed []byte `json:"mldsa_seed"`
	EdSeed    []byte `json:"ed_seed"`
	Name      string `json:"name"`
	MsgSeed   []byte `json:"msg_seed"`
	MsgN      uint64 `json:"msg_n"`
}

// savedState is the profile's main record.
type savedState struct {
	Server       string        `json:"server"`
	Origin       string        `json:"origin,omitempty"`
	ID           string        `json:"id"`
	Device       savedDevice   `json:"device"`
	AccountKey   []byte        `json:"account_key"`
	Token        string        `json:"token,omitempty"`
	TokenExpires int64         `json:"token_expires,omitempty"`
	Chain        []wire.Signed `json:"chain"`
	Cursor       string        `json:"cursor,omitempty"`       // the last cursor this device acked
	PendingWrap  *WrappedKey   `json:"pending_wrap,omitempty"` // a recovery wrap the homeserver hasn't taken yet
	LinkKey      wire.Bytes    `json:"link_key,omitempty"`     // a linked device's HPKE private key, until its link bundle opens
}

// Client is a signed-up identity on this device. Its methods are safe for
// concurrent use.
type Client struct {
	opts     Options
	profile  *store.Profile
	resolver *discovery.Resolver
	clock    *Clock
	api      *api

	mu     sync.Mutex
	state  savedState
	device *identity.Device
	chain  *identity.Chain
	signIn sync.Mutex // one sign-in at a time
}

// Errors a caller can branch on.
var (
	// ErrSignedOut reports a call that needs a session when there's none.
	ErrSignedOut = errors.New("client: signed out")
	// ErrDeviceRemoved reports that this device was removed from the
	// identity's chain (§7.6).
	ErrDeviceRemoved = errors.New("client: this device was removed from the identity")
)

func newClient(p *store.Profile, opts Options) *Client {
	c := &Client{
		opts:     opts,
		profile:  p,
		resolver: &discovery.Resolver{HTTP: opts.httpClient(), Origins: opts.Origins},
		clock:    &Clock{},
	}
	c.resolver.Via = c.fetchServerDoc
	return c
}

// Open opens an existing profile. It doesn't contact the homeserver; the
// first call that needs a session signs in.
func Open(profileDir string, opts Options) (*Client, error) {
	p, err := store.Open(profileDir, opts.Store)
	if err != nil {
		return nil, err
	}
	var st savedState
	if err := p.Load(recState, &st); err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	if st.Origin != "" {
		// A profile made on a local test network remembers its origin.
		origins := map[string]string{st.Server: st.Origin}
		for k, v := range opts.Origins {
			origins[k] = v
		}
		opts.Origins = origins
	}
	c := newClient(p, opts)
	if err := c.load(st); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) load(st savedState) error {
	key, err := sig.NewCompositeKey(st.Device.MLDSASeed, st.Device.EdSeed)
	if err != nil {
		return fmt.Errorf("client: stored device key: %w", err)
	}
	if len(st.Device.MsgSeed) != ed25519.SeedSize {
		return errors.New("client: stored message key is damaged")
	}
	dev := &identity.Device{Key: key, Name: st.Device.Name, MsgKey: ed25519.NewKeyFromSeed(st.Device.MsgSeed), MsgN: st.Device.MsgN}
	chain, err := identity.Validate(st.Chain, identity.Options{})
	if err != nil {
		return fmt.Errorf("client: stored chain: %w", err)
	}
	c.state, c.device, c.chain = st, dev, chain
	c.resolver.Self = st.Server
	return nil
}

// saveLocked writes the main record. The caller holds c.mu.
func (c *Client) saveLocked() error {
	c.state.Chain = c.chain.Entries
	ms, es := c.device.Key.Seeds()
	c.state.Device = savedDevice{MLDSASeed: ms, EdSeed: es, Name: c.device.Name, MsgSeed: c.device.MsgKey.Seed(), MsgN: c.device.MsgN}
	return c.profile.Save(recState, c.state)
}

// connect finds the homeserver's version 2 client API.
func (c *Client) connect(ctx context.Context) error {
	c.mu.Lock()
	ready := c.api != nil
	server := c.state.Server
	c.mu.Unlock()
	if ready {
		return nil
	}
	doc, err := c.resolver.ServerDoc(ctx, server)
	if err != nil {
		return err
	}
	base, err := clientBase(doc)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.api == nil {
		c.api = &api{base: base, http: c.opts.httpClient(), clock: c.clock, log: c.opts.Logger, token: c.token}
	}
	return nil
}

func clientBase(doc *discovery.ServerDoc) (string, error) {
	if !doc.HasRole(discovery.RoleHome) {
		return "", fmt.Errorf("client: %s isn't a homeserver", doc.Name)
	}
	a, ok := doc.API(wire.Version)
	if !ok {
		return "", fmt.Errorf("client: %s: %w", doc.Name, discovery.ErrNoVersion2)
	}
	if a.Client == "" {
		return "", fmt.Errorf("client: %s lists no client API", doc.Name)
	}
	return a.Client, nil
}

func (c *Client) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Token
}

// ID returns the identity's identifier. It's for debug views and QR codes
// only; users see the handle (§6.2).
func (c *Client) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.ID
}

// Server returns the homeserver's name.
func (c *Client) Server() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Server
}

// Handle returns the identity's current handle, from its chain.
func (c *Client) Handle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chain.State.Handle
}

// DeviceKey returns this device's key: the Ed25519 component of its
// composite key.
func (c *Client) DeviceKey() []byte { return c.device.ID() }

// Chain returns the identity's own chain, as this device last validated it.
func (c *Client) Chain() *identity.Chain {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chain
}

// Clock returns the client's view of the homeserver's time.
func (c *Client) Clock() *Clock { return c.clock }

// Warning returns a warning to show about the profile, such as a key kept
// in a file because there's no keychain, or "".
func (c *Client) Warning() string { return c.profile.Warning }

// ProfileDir returns the profile's directory.
func (c *Client) ProfileDir() string { return c.profile.Dir() }

// Session describes this device's session.
type Session struct {
	SignedIn bool
	Expires  time.Time // when the token expires if it isn't used again
}

// Session returns this device's session. Tokens expire after 30 days
// without use, and each use extends them (API conventions), so Expires is
// only the time the server gave at sign-in.
func (c *Client) Session() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Session{SignedIn: c.state.Token != "", Expires: time.Unix(c.state.TokenExpires, 0)}
}

// authed makes a call that needs a session. Without a token it signs in
// first, and after 401 unauthenticated it signs in again once and retries.
func (c *Client) authed(ctx context.Context, method, path string, query map[string][]string, in, out any) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	if c.token() == "" {
		if err := c.SignIn(ctx); err != nil {
			return err
		}
	}
	err := c.api.call(ctx, method, path, query, in, out)
	if wire.IsCode(err, wire.CodeUnauthenticated) {
		if err := c.SignIn(ctx); err != nil {
			return err
		}
		err = c.api.call(ctx, method, path, query, in, out)
	}
	return err
}

// unauthed makes a call without a session.
func (c *Client) unauthed(ctx context.Context, method, path string, query map[string][]string, in, out any) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	a := *c.api
	a.token = nil
	return a.call(ctx, method, path, query, in, out)
}

type challengeReq struct {
	ID  string     `json:"id"`
	Key wire.Bytes `json:"key"`
}

type challengeRes struct {
	Nonce   string `json:"nonce"`
	Expires int64  `json:"expires"`
	Now     int64  `json:"now"`
}

// AuthBody is the body of a signed "auth" object (§8.2).
type AuthBody struct {
	ID     string `json:"id"`
	Nonce  string `json:"nonce"`
	Server string `json:"server"`
	TS     int64  `json:"ts"`
	Name   string `json:"name,omitempty"`
}

type sessionReq struct {
	Auth wire.Signed `json:"auth"`
}

type sessionRes struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
}

// SignIn signs this device in (§8.2): it gets a nonce from auth/challenge,
// which also gives the server's clock (§8.4), and trades the signed nonce
// for a session token.
func (c *Client) SignIn(ctx context.Context) error {
	c.signIn.Lock()
	defer c.signIn.Unlock()
	if err := c.connect(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	id, server := c.state.ID, c.state.Server
	c.mu.Unlock()
	a := *c.api
	a.token = nil
	res, err := signInDevice(ctx, &a, c.clock, id, server, c.device)
	if wire.IsCode(err, wire.CodeNotAllowed) {
		return ErrDeviceRemoved
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Token, c.state.TokenExpires = res.Token, res.Expires
	return c.saveLocked()
}

// signInDevice gets a nonce from auth/challenge, which also gives the
// server's clock (§8.4), and trades the signed nonce for a session token
// (§8.2). A device that isn't current gets not-allowed.
func signInDevice(ctx context.Context, a *api, clock *Clock, id, server string, dev *identity.Device) (sessionRes, error) {
	for attempt := 0; ; attempt++ {
		var ch challengeRes
		start := time.Now()
		err := a.call(ctx, http.MethodPost, "auth/challenge", nil, challengeReq{ID: id, Key: dev.ID()}, &ch)
		end := time.Now()
		if err != nil {
			return sessionRes{}, err
		}
		if ch.Now > 0 {
			clock.observe(time.Unix(ch.Now, 0), start, end)
		}
		auth, err := sig.SignDevice(dev.Key, identity.TypeAuth, AuthBody{
			ID: id, Nonce: ch.Nonce, Server: server, TS: clock.Now().Unix(), Name: dev.Name,
		})
		if err != nil {
			return sessionRes{}, err
		}
		var res sessionRes
		err = a.call(ctx, http.MethodPost, "auth/session", nil, sessionReq{Auth: auth}, &res)
		if wire.IsCode(err, wire.CodeClockSkew) && attempt == 0 {
			continue // the error corrected the clock; start over with a new nonce
		}
		return res, err
	}
}

// SignOut ends this device's session: the homeserver revokes the token
// (POST auth/logout), and the profile forgets it. The identity and its keys
// stay in the profile, so the device can sign in again.
func (c *Client) SignOut(ctx context.Context) error {
	if c.token() != "" {
		if err := c.connect(ctx); err != nil {
			return err
		}
		err := c.api.call(ctx, http.MethodPost, "auth/logout", nil, struct{}{}, nil)
		if err != nil && !wire.IsCode(err, wire.CodeUnauthenticated) {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Token, c.state.TokenExpires = "", 0
	return c.saveLocked()
}

// DeleteProfile deletes this device's profile: its keys, chain copies, and
// the profile key in the keychain. It doesn't sign out or remove the
// device from the identity's chain; call SignOut first. Without another
// device, only the recovery code gets the identity back.
func (c *Client) DeleteProfile() error {
	return c.profile.Destroy()
}

// Profile returns the profile the client keeps its records in. Other parts
// of Railcar, such as the MLS state and the local message history, keep
// their own records in it, under names that don't start with "identity"
// or "chain.".
func (c *Client) Profile() *store.Profile { return c.profile }

// SignAsDevice signs body as type t with this device's composite key. The
// envelope's k is the key's Ed25519 component, so a verifier finds the
// composite key in the identity's chain (§3.2.2). Add approvals (§11.8),
// community chain entries (§16.2), and member lists (§16.6) are signed this
// way.
func (c *Client) SignAsDevice(t string, body any) (wire.Signed, error) {
	return sig.SignDevice(c.device.Key, t, body)
}

// DevicePublic returns this device's composite public key.
func (c *Client) DevicePublic() []byte { return c.device.Key.Public() }
