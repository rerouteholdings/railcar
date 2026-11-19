package client

import (
	"bytes"
	"context"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/codes"
	"github.com/rerouteholdings/railcar/internal/tieroute/discovery"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TypeLinkProof is the type tag of link proofs (§7.2).
const TypeLinkProof = "link-proof"

// HPKE sizes for MLKEM768-X25519 (§3).
const (
	HPKEPublicSize = 1216
	HPKEEncSize    = 1120
)

// linkInfo is the HPKE info string of link bundles (§7.2 step 4).
const linkInfo = "tieroute link bundle"

// approvalLifetime is how long an approval code works (§7.2.1).
const approvalLifetime = 10 * time.Minute

// The HPKE suite outside MLS: base mode, MLKEM768-X25519, HKDF-SHA256, and
// AES-128-GCM (§3).
func hpkeSuite() (hpke.KEM, hpke.KDF, hpke.AEAD) {
	return hpke.MLKEM768X25519(), hpke.HKDFSHA256(), hpke.AES128GCM()
}

// LinkBundle is what an existing device sends a new one (§7.2 step 4).
type LinkBundle struct {
	ID          string           `json:"id"`
	Contacts    []string         `json:"contacts"`
	Groups      []string         `json:"groups"`
	AccountKey  wire.Bytes       `json:"account-key"`
	History     []jsontext.Value `json:"history,omitempty"`
	Communities []string         `json:"communities,omitempty"`
}

// SealLinkBundle encrypts a link bundle to a new device's HPKE public key:
// the encapsulation, then the ciphertext of JCS(link-bundle), with the
// identifier as associated data (§7.2 step 4).
func SealLinkBundle(pub []byte, b LinkBundle) ([]byte, error) {
	if b.Contacts == nil {
		b.Contacts = []string{}
	}
	if b.Groups == nil {
		b.Groups = []string{}
	}
	plain, err := wire.JCS(b)
	if err != nil {
		return nil, err
	}
	kem, kdf, aead := hpkeSuite()
	pk, err := kem.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("client: the new device's HPKE key: %w", err)
	}
	enc, s, err := hpke.NewSender(pk, kdf, aead, []byte(linkInfo))
	if err != nil {
		return nil, err
	}
	ct, err := s.Seal([]byte(b.ID), plain)
	if err != nil {
		return nil, err
	}
	return append(enc, ct...), nil
}

// OpenLinkBundle decrypts a link bundle with the new device's HPKE private
// key, for identity id. A bundle for another identity is refused.
func OpenLinkBundle(priv hpke.PrivateKey, id string, data []byte) (*LinkBundle, error) {
	if len(data) <= HPKEEncSize {
		return nil, errShort
	}
	_, kdf, aead := hpkeSuite()
	r, err := hpke.NewRecipient(data[:HPKEEncSize], priv, kdf, aead, []byte(linkInfo))
	if err != nil {
		return nil, err
	}
	plain, err := r.Open([]byte(id), data[HPKEEncSize:])
	if err != nil {
		return nil, err
	}
	var b LinkBundle
	if err := wire.Unmarshal(plain, &b); err != nil {
		return nil, err
	}
	if b.ID != id {
		return nil, errors.New("client: the link bundle is for another identity")
	}
	if len(b.AccountKey) != 16 {
		return nil, errors.New("client: the link bundle's account key isn't 16 bytes")
	}
	return &b, nil
}

// LinkProofBody is the body of a link proof (client API, "Leave a link
// proof").
type LinkProofBody struct {
	ID   string     `json:"id"`
	Code string     `json:"code,omitempty"`
	HPKE wire.Bytes `json:"hpke,omitempty"`
	TS   int64      `json:"ts"`
}

// LinkProof is a link proof whose signature checked out.
type LinkProof struct {
	Body LinkProofBody
	Pub  []byte // the new device's composite public key, from k
}

// Device returns the new device's key: the Ed25519 component of Pub.
func (p LinkProof) Device() []byte { return p.Pub[len(p.Pub)-32:] }

// ParseLinkProof checks a link proof's composite signature against the
// composite key in its k (§3.2.2), and reads its body.
func ParseLinkProof(s wire.Signed) (LinkProof, error) {
	if s.T != TypeLinkProof {
		return LinkProof{}, errors.New("client: not a link proof")
	}
	pub, err := sig.VerifyRevealed(s)
	if err != nil {
		return LinkProof{}, err
	}
	var b LinkProofBody
	if err := s.Body(&b); err != nil {
		return LinkProof{}, err
	}
	if b.Code == "" && b.HPKE == nil {
		return LinkProof{}, errors.New("client: a link proof with neither a code nor an HPKE key")
	}
	if b.HPKE != nil && len(b.HPKE) != HPKEPublicSize {
		return LinkProof{}, errors.New("client: a link proof's HPKE key isn't 1,216 bytes")
	}
	return LinkProof{Body: b, Pub: pub}, nil
}

type linkProofReq struct {
	Proof wire.Signed `json:"proof"`
}

// ErrNotLinkedYet reports a new device that the existing device hasn't
// added to the chain yet.
var ErrNotLinkedYet = errors.New("client: this device hasn't been added yet")

// Link is a new device signing in to an existing identity with another
// device's help (§7.2). It holds the new device's keys in memory until
// Finish creates the profile.
type Link struct {
	opts   Options
	server string
	handle string
	id     string
	device *identity.Device
	hpke   hpke.PrivateKey
	pub    []byte // the HPKE public key
	api    *api
	clock  *Clock
	done   bool
}

// StartLink starts linking this device to the identity with handle on
// server (§7.2 step 1): it finds the identity, makes the device key and a
// one-time HPKE key pair, and parks the HPKE public key on the homeserver.
// Show Code as a QR code and as text.
func StartLink(ctx context.Context, server, handle string, opts Options) (*Link, error) {
	l, err := beginLink(ctx, server, handle, opts)
	if err != nil {
		return nil, err
	}
	if err := l.leaveProof(ctx, LinkProofBody{ID: l.id, HPKE: l.pub}); err != nil {
		return nil, err
	}
	return l, nil
}

// StartLinkWithApproval starts linking this device with an approval code
// another device shows (§7.2.1 step 2). It checks the home's server
// document, finds the identity, makes the keys, and leaves one link proof
// that carries the HPKE key and the approval tag. There's no confirmation
// code; call Finish until it's added.
func StartLinkWithApproval(ctx context.Context, code string, opts Options) (*Link, error) {
	ac, err := ParseApprovalCode(code)
	if err != nil {
		return nil, err
	}
	l, err := beginLink(ctx, ac.Home, ac.Handle, opts)
	if err != nil {
		return nil, err
	}
	tag := codes.ApprovalTag(ac.Secret, l.device.Key.Public(), l.pub)
	if err := l.leaveProof(ctx, LinkProofBody{ID: l.id, Code: tag, HPKE: l.pub}); err != nil {
		return nil, err
	}
	return l, nil
}

// beginLink finds the identity and makes the new device's keys.
func beginLink(ctx context.Context, server, handle string, opts Options) (*Link, error) {
	server = strings.ToLower(strings.TrimSpace(server))
	if err := identity.CheckServerName(server); err != nil {
		return nil, err
	}
	handle = identity.NormalizeHandle(handle)
	if err := identity.CheckHandle(handle); err != nil {
		return nil, err
	}
	r := &discovery.Resolver{HTTP: opts.httpClient(), Origins: opts.Origins}
	doc, err := r.ServerDoc(ctx, server)
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
	clock := &Clock{}
	if !doc.Date.IsZero() {
		clock.observeRough(doc.Date, time.Now(), time.Now())
	}
	a := &api{base: base, http: opts.httpClient(), clock: clock, log: opts.Logger}

	// Resolving a local handle needs no session. The answer is only a
	// hint: the chain has to name the handle (§5.3).
	var res resolveRes
	if err := a.call(ctx, http.MethodGet, "resolve/"+handle, nil, nil, &res); err != nil {
		return nil, err
	}
	if !identity.ValidIdentifier(res.ID) {
		return nil, fmt.Errorf("client: resolve answered %q", res.ID)
	}
	if res.Home != server {
		return nil, fmt.Errorf("client: @%s's homeserver is %s, not %s", handle, res.Home, server)
	}
	var cr chainRes
	if err := a.call(ctx, http.MethodGet, "chain/"+res.ID, nil, nil, &cr); err != nil {
		return nil, err
	}
	chain, err := identity.Validate(cr.Entries, identity.Options{Now: clock.Now()})
	if err != nil {
		return nil, err
	}
	if chain.ID() != res.ID || chain.State.Handle != handle {
		return nil, ErrHandleMismatch
	}
	if chain.State.Home != server {
		return nil, fmt.Errorf("client: @%s's homeserver is %s, not %s", handle, chain.State.Home, server)
	}

	dev, err := identity.GenerateDevice(opts.DeviceName)
	if err != nil {
		return nil, err
	}
	kem, _, _ := hpkeSuite()
	priv, err := kem.GenerateKey()
	if err != nil {
		return nil, err
	}
	return &Link{
		opts: opts, server: server, handle: handle, id: res.ID, device: dev,
		hpke: priv, pub: priv.PublicKey().Bytes(), api: a, clock: clock,
	}, nil
}

// leaveProof signs a link proof with the new device's composite key, its
// whole public key in k, and leaves it (POST link/proof). A clock-skew
// answer corrects the clock and retries once (§8.4).
func (l *Link) leaveProof(ctx context.Context, body LinkProofBody) error {
	for attempt := 0; ; attempt++ {
		body.TS = l.clock.Now().Unix()
		s, err := sig.SignRevealing(l.device.Key, TypeLinkProof, body)
		if err != nil {
			return err
		}
		err = l.api.call(ctx, http.MethodPost, "link/proof", nil, linkProofReq{Proof: s}, nil)
		if wire.IsCode(err, wire.CodeClockSkew) && attempt == 0 {
			continue
		}
		return err
	}
}

// Code returns the link code to show (§7.2 step 1).
func (l *Link) Code() LinkCode {
	h := sha256.Sum256(l.pub)
	return LinkCode{Device: l.device.ID(), HPKEHash: h[:], Home: l.server}
}

// Handle returns the identity's handle.
func (l *Link) Handle() string { return l.handle }

// Server returns the identity's homeserver.
func (l *Link) Server() string { return l.server }

// Confirm signs the confirmation code the existing device shows, and
// leaves it as a link proof (§7.2 step 2). The existing device then adds
// this device; call Finish until it has.
func (l *Link) Confirm(ctx context.Context, code string) error {
	n := codes.NormalizeConfirmation(code)
	if len(n) != 8 {
		return errors.New("client: a confirmation code has 8 letters and digits, as XXXX-XXXX")
	}
	return l.leaveProof(ctx, LinkProofBody{ID: l.id, Code: n})
}

// Finish tries to sign in (§7.2 step 5). Until the existing device has
// added this device, it returns ErrNotLinkedYet; call it again every
// couple of seconds. Once signed in, it creates the profile in profileDir
// and returns the client. The link bundle then arrives in the inbox, where
// Client.OpenLinkBundle opens it.
func (l *Link) Finish(ctx context.Context, profileDir string) (*Client, error) {
	if l.done {
		return nil, errors.New("client: this link already finished")
	}
	res, err := signInDevice(ctx, l.api, l.clock, l.id, l.server, l.device)
	if wire.IsCode(err, wire.CodeNotAllowed) {
		return nil, ErrNotLinkedYet
	}
	if err != nil {
		return nil, err
	}
	a := *l.api
	a.token = func() string { return res.Token }
	var cr chainRes
	if err := a.call(ctx, http.MethodGet, "chain/"+l.id, nil, nil, &cr); err != nil {
		return nil, err
	}
	chain, err := identity.Validate(cr.Entries, identity.Options{Now: l.clock.Now()})
	if err != nil {
		return nil, err
	}
	if chain.ID() != l.id || !chain.State.IsDevice(l.device.ID()) {
		return nil, ErrNotLinkedYet
	}
	// The name in the chain is the one the approving device gave.
	if d, ok := chain.State.Device(l.device.ID()); ok && d.Name != "" {
		l.device.Name = d.Name
	}
	priv, err := l.hpke.Bytes()
	if err != nil {
		return nil, err
	}
	p, err := store.Create(profileDir, l.opts.Store)
	if err != nil {
		return nil, err
	}
	st := savedState{
		Server: l.server, ID: l.id, Token: res.Token, TokenExpires: res.Expires,
		Chain: chain.Entries, LinkKey: priv,
	}
	if o, ok := l.opts.Origins[l.server]; ok {
		st.Origin = o
	}
	c := newClient(p, l.opts)
	c.clock = l.clock
	c.resolver.Self = l.server
	c.state, c.device, c.chain = st, l.device, chain
	c.mu.Lock()
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		p.Destroy()
		return nil, err
	}
	l.done = true
	return c, nil
}

// LinkPending reports whether this device was linked and hasn't opened its
// link bundle yet.
func (c *Client) LinkPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.state.LinkKey) > 0
}

// OpenLinkBundle opens a link delivery from the inbox with this device's
// one-time HPKE key (§7.2 step 4). Other devices' bundles don't open, and
// come back as an error to ignore. Once one opens, the device keeps the
// account key and forgets the HPKE key.
func (c *Client) OpenLinkBundle(data []byte) (*LinkBundle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.state.LinkKey) == 0 {
		return nil, errors.New("client: this device isn't waiting for a link bundle")
	}
	kem, _, _ := hpkeSuite()
	priv, err := kem.NewPrivateKey(c.state.LinkKey)
	if err != nil {
		return nil, err
	}
	b, err := OpenLinkBundle(priv, c.state.ID, data)
	if err != nil {
		return nil, err
	}
	c.state.AccountKey = append([]byte{}, b.AccountKey...)
	c.state.LinkKey = nil
	return b, c.saveLocked()
}

// AccountKey returns the identity's account key (§7.4), or nil on a linked
// device that hasn't opened its link bundle.
func (c *Client) AccountKey() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte{}, c.state.AccountKey...)
}

// NewDevice is a new device another device is adding: its composite public
// key, and its HPKE key for the link bundle (nil for a short-form code).
type NewDevice struct {
	Pub  []byte
	HPKE []byte
}

// Key returns the new device's key: the Ed25519 component of Pub.
func (d NewDevice) Key() []byte { return d.Pub[len(d.Pub)-32:] }

// FetchLinkProof fetches the link proof a new device left
// (GET link/proof/{device}), and checks its signature. It returns nil
// when there's none.
func (c *Client) FetchLinkProof(ctx context.Context, device []byte) (*LinkProof, error) {
	var res struct {
		Proof wire.Signed `json:"proof"`
	}
	err := c.authed(ctx, http.MethodGet, "link/proof/"+wire.EncodeB64(device), nil, nil, &res)
	if wire.IsCode(err, wire.CodeNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := ParseLinkProof(res.Proof)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// LinkProofs lists the link proofs waiting for this identity
// (GET link/proofs). Proofs whose signature doesn't check out are left
// out.
func (c *Client) LinkProofs(ctx context.Context) ([]LinkProof, error) {
	var res struct {
		Proofs []wire.Signed `json:"proofs"`
	}
	if err := c.authed(ctx, http.MethodGet, "link/proofs", nil, nil, &res); err != nil {
		return nil, err
	}
	var out []LinkProof
	for _, s := range res.Proofs {
		if p, err := ParseLinkProof(s); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// Errors from approving a device.
var (
	// ErrLinkMismatch reports a link code that doesn't match the keys the
	// new device left (§7.2 step 2).
	ErrLinkMismatch = errors.New("client: the code doesn't match the new device")
	// ErrNoLinkProof reports a link code whose device hasn't left its key
	// on the homeserver.
	ErrNoLinkProof = errors.New("client: the new device hasn't left its key on the homeserver yet")
	// ErrWrongConfirmation reports a new device that sent back another
	// confirmation code. Keep waiting: the user can type it again.
	ErrWrongConfirmation = errors.New("client: the new device sent back a different confirmation code")
	// ErrApprovalUsedTwice reports an approval code that more than one
	// device used: someone else may have seen it (§7.2.1 step 3).
	ErrApprovalUsedTwice = errors.New("client: more than one device used the approval code")
	// ErrApprovalExpired reports an approval code past its 10 minutes.
	ErrApprovalExpired = errors.New("client: the approval code expired")
)

// LinkApproval is an existing device approving a new device's link code
// (§7.2 step 2).
type LinkApproval struct {
	c       *Client
	code    LinkCode
	dev     NewDevice
	confirm string
}

// ApproveLinkCode starts approving the device that shows a link code: it
// fetches the key the device parked, checks it against the code, and
// makes the confirmation code to show. Before showing it, show
// LinkWarning (§7.2 step 2).
func (c *Client) ApproveLinkCode(ctx context.Context, text string) (*LinkApproval, error) {
	code, err := ParseLinkCode(text)
	if err != nil {
		return nil, err
	}
	if code.Home != "" && code.Home != c.Server() {
		return nil, fmt.Errorf("client: that code is for a device signing in on %s, not %s", code.Home, c.Server())
	}
	if c.Chain().State.IsDevice(code.Device) {
		return nil, errors.New("client: that device is already one of this identity's")
	}
	p, err := c.FetchLinkProof(ctx, code.Device)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrNoLinkProof
	}
	if p.Body.ID != c.ID() || !bytes.Equal(p.Device(), code.Device) {
		return nil, ErrLinkMismatch
	}
	d := NewDevice{Pub: p.Pub}
	if !code.Short() {
		if p.Body.HPKE == nil {
			return nil, ErrNoLinkProof
		}
		h := sha256.Sum256(p.Body.HPKE)
		if !bytes.Equal(h[:], code.HPKEHash) {
			return nil, ErrLinkMismatch
		}
		d.HPKE = p.Body.HPKE
	}
	confirm, err := codes.NewConfirmation()
	if err != nil {
		return nil, err
	}
	return &LinkApproval{c: c, code: code, dev: d, confirm: confirm}, nil
}

// LinkWarning is the warning to show before a confirmation code or an
// approval code (§7.2 step 2).
const LinkWarning = "Only continue if you're setting up your own device, right now. Your app will never ask you for this code."

// Confirmation returns the confirmation code to show, as XXXX-XXXX.
func (a *LinkApproval) Confirmation() string { return a.confirm }

// Device returns the device being approved.
func (a *LinkApproval) Device() NewDevice { return a.dev }

// Check looks once for the new device's proof with the confirmation code
// (§7.2 step 2). It returns true once the proof is there and matches, and
// ErrWrongConfirmation when it carries another code.
func (a *LinkApproval) Check(ctx context.Context) (bool, error) {
	p, err := a.c.FetchLinkProof(ctx, a.code.Device)
	if err != nil || p == nil || p.Body.Code == "" {
		return false, err
	}
	if p.Body.ID != a.c.ID() || !bytes.Equal(p.Pub, a.dev.Pub) {
		return false, ErrLinkMismatch
	}
	if codes.NormalizeConfirmation(p.Body.Code) != codes.NormalizeConfirmation(a.confirm) {
		return false, ErrWrongConfirmation
	}
	return true, nil
}

// ApprovalSecret is an approval code an existing device shows (§7.2.1).
// It works for 10 minutes and for one device, and its secret never leaves
// this device except in the code.
type ApprovalSecret struct {
	c       *Client
	code    ApprovalCode
	expires time.Time
	used    bool
}

// NewApprovalCode makes an approval code for this identity. Show
// LinkWarning before it (§7.2.1 step 1).
func (c *Client) NewApprovalCode() (*ApprovalSecret, error) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &ApprovalSecret{
		c:       c,
		code:    ApprovalCode{Secret: secret, Handle: c.Handle(), Home: c.Server()},
		expires: time.Now().Add(approvalLifetime),
	}, nil
}

// Code returns the approval code to show, as a QR code and as text.
func (s *ApprovalSecret) Code() ApprovalCode { return s.code }

// Expires returns when the code stops working.
func (s *ApprovalSecret) Expires() time.Time { return s.expires }

// Forget discards the secret, so the code can't add a device.
func (s *ApprovalSecret) Forget() {
	clear(s.code.Secret)
	s.used = true
}

// Check looks once for a new device's proof made with the code (§7.2.1
// step 3). It returns the device once proofs from exactly one device
// pass, and nil while there are none. When more than one device's proofs
// pass, it adds none, forgets the secret, and returns ErrApprovalUsedTwice.
// Either way, the code can't be used again.
func (s *ApprovalSecret) Check(ctx context.Context) (*NewDevice, error) {
	if s.used {
		return nil, errors.New("client: this approval code was already used")
	}
	if time.Now().After(s.expires) {
		s.Forget()
		return nil, ErrApprovalExpired
	}
	proofs, err := s.c.LinkProofs(ctx)
	if err != nil {
		return nil, err
	}
	var found *NewDevice
	for _, p := range proofs {
		if p.Body.ID != s.c.ID() || p.Body.HPKE == nil || p.Body.Code == "" {
			continue
		}
		if p.Body.Code != codes.ApprovalTag(s.code.Secret, p.Pub, p.Body.HPKE) {
			continue
		}
		if found != nil && !bytes.Equal(found.Pub, p.Pub) {
			s.Forget()
			return nil, ErrApprovalUsedTwice
		}
		found = &NewDevice{Pub: p.Pub, HPKE: p.Body.HPKE}
	}
	if found != nil {
		s.Forget()
	}
	return found, nil
}

// AddLinkedDevice adds an approved device to the identity's chain with an
// add-device entry (§7.2 step 3), then sends it the link bundle when it
// has an HPKE key (step 4). bundle's ID and account key are filled in
// here. The caller then adds the device to every group (§11.9).
func (c *Client) AddLinkedDevice(ctx context.Context, d NewDevice, name string, bundle LinkBundle) error {
	if !sig.ValidCompositePublic(d.Pub) {
		return errors.New("client: the new device's key isn't a valid composite key")
	}
	if !c.Chain().State.IsDevice(d.Key()) {
		if _, err := c.Append(ctx, identity.AddDevice(d.Pub, name)); err != nil {
			return err
		}
	}
	if d.HPKE == nil {
		return nil
	}
	bundle.ID, bundle.AccountKey = c.ID(), c.AccountKey()
	data, err := SealLinkBundle(d.HPKE, bundle)
	if err != nil {
		return err
	}
	res, err := c.SendLink(ctx, data)
	if err != nil {
		return err
	}
	if res.Status != "sent" {
		return &wire.Error{Code: res.Error, Msg: "the homeserver refused the link bundle"}
	}
	return nil
}

// RemoveDevice removes another of the identity's devices from its chain
// (§7.6). The caller then removes its leaves from every group (§11.9).
func (c *Client) RemoveDevice(ctx context.Context, key []byte) error {
	if bytes.Equal(key, c.DeviceKey()) {
		return errors.New("client: a device can't remove itself; sign out instead")
	}
	if !c.Chain().State.IsDevice(key) {
		return errors.New("client: that isn't one of this identity's devices")
	}
	_, err := c.Append(ctx, identity.RemoveDevice(key))
	return err
}
