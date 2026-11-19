package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Type tags of the contact objects (§9.2, §9.3).
const (
	TypeContactRequest = "contact-request"
	TypeContactDecline = "contact-decline"
)

// InviteOptions adjust CreateInvite (§9.1).
type InviteOptions struct {
	// Public makes a code to publish on the profile, held to the
	// per-sender rate limits (§9.5).
	Public bool
	// Uses is how many requests the code may start; 0 is unlimited.
	Uses int
	// Expires is when the code stops working, in Unix seconds; 0 is never.
	Expires int64
}

// Invite is a new invite code. The homeserver keeps only its keyed hash,
// so the code is shown once.
type Invite struct {
	Code string     `json:"code"`
	Hash wire.Bytes `json:"hash"`
}

// CreateInvite makes an invite code (POST invites/create, §9.1).
func (c *Client) CreateInvite(ctx context.Context, opts InviteOptions) (Invite, error) {
	req := map[string]any{}
	if opts.Public {
		req["public"] = true
	}
	if opts.Uses > 0 {
		req["uses"] = opts.Uses
	}
	if opts.Expires > 0 {
		req["expires"] = opts.Expires
	}
	var inv Invite
	err := c.authed(ctx, http.MethodPost, "invites/create", nil, req, &inv)
	return inv, err
}

// RevokeInvite stops new requests with a code (POST invites/revoke).
func (c *Client) RevokeInvite(ctx context.Context, code string) error {
	return c.authed(ctx, http.MethodPost, "invites/revoke", nil, map[string]string{"code": code}, nil)
}

// InviteCode is a parsed `@handle::code` (§9.4).
type InviteCode struct {
	Handle string // without the @
	Code   string // the invite code; empty for a bare @handle
}

// String formats the code as it's shared: @handle::code.
func (i InviteCode) String() string {
	if i.Code == "" {
		return "@" + i.Handle
	}
	return "@" + i.Handle + "::" + i.Code
}

// LinkDomain is the project's link domain (§9.4).
const LinkDomain = "rerouteholdings.llc"

// ErrUnknownCode reports a code whose form isn't one §9.4 defines.
// Clients treat it as an error, not a guess.
var ErrUnknownCode = errors.New("client: that isn't a contact code")

// ParseInviteCode reads a person's code as §9.4 defines it: @handle::code,
// or @handle alone, or either one as a link on the link domain. It splits
// at the first ::. A code without the leading @ names a community server
// (§16.6), which it refuses with ErrUnknownCode.
func ParseInviteCode(s string) (InviteCode, error) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://" + LinkDomain + "/", LinkDomain + "/"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			s = rest
			if u, err := url.PathUnescape(s); err == nil {
				s = u
			}
			break
		}
	}
	if !strings.HasPrefix(s, "@") {
		return InviteCode{}, ErrUnknownCode
	}
	handle, code, has := strings.Cut(s[1:], "::")
	handle = identity.NormalizeHandle(handle)
	if err := identity.CheckHandle(handle); err != nil {
		return InviteCode{}, fmt.Errorf("%w: %v", ErrUnknownCode, err)
	}
	if has && !validInvite(code) {
		return InviteCode{}, fmt.Errorf("%w: an invite code is 10 letters and digits", ErrUnknownCode)
	}
	return InviteCode{Handle: handle, Code: code}, nil
}

// validInvite checks an invite code's form: 10 characters from
// [A-Za-z0-9] (§9.1).
func validInvite(code string) bool {
	if len(code) != 10 {
		return false
	}
	for _, r := range code {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return false
		}
	}
	return true
}

// ContactRequestBody is the body of a contact request (§9.2).
type ContactRequestBody struct {
	From   string     `json:"from"`
	To     string     `json:"to"`
	TS     int64      `json:"ts"`
	Nonce  wire.Bytes `json:"nonce"`
	Invite string     `json:"invite"`
}

// ContactDeclineBody is the body of a decline (§9.3).
type ContactDeclineBody struct {
	From    string     `json:"from"`
	To      string     `json:"to"`
	Request wire.Bytes `json:"request"` // the hash of the request's signed object
	TS      int64      `json:"ts"`
}

// RequestContact sends a contact request to an identity this device has
// resolved, with its invite code (POST contacts/request, §9.2). It returns
// the signed request.
func (c *Client) RequestContact(ctx context.Context, to, invite string) (wire.Signed, error) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	req, err := sig.SignDevice(c.device.Key, TypeContactRequest, ContactRequestBody{
		From: c.ID(), To: to, TS: c.clock.Now().Unix(), Nonce: nonce, Invite: invite,
	})
	if err != nil {
		return wire.Signed{}, err
	}
	return req, c.authed(ctx, http.MethodPost, "contacts/request", nil, map[string]any{"request": req}, nil)
}

// AcceptContact marks a requester as a contact (POST contacts/accept,
// §9.3). The caller then creates the direct chat.
func (c *Client) AcceptContact(ctx context.Context, id string) error {
	return c.authed(ctx, http.MethodPost, "contacts/accept", nil, map[string]string{"id": id}, nil)
}

// DeclineContact declines a request, which the requester is told about
// (POST contacts/decline, §9.3).
func (c *Client) DeclineContact(ctx context.Context, request wire.Signed) error {
	var body ContactRequestBody
	if err := request.Body(&body); err != nil {
		return err
	}
	h := request.Hash()
	d, err := sig.SignDevice(c.device.Key, TypeContactDecline, ContactDeclineBody{
		From: c.ID(), To: body.From, Request: h[:], TS: c.clock.Now().Unix(),
	})
	if err != nil {
		return err
	}
	return c.authed(ctx, http.MethodPost, "contacts/decline", nil, map[string]any{"decline": d}, nil)
}

// CancelContact withdraws a pending outgoing request (POST contacts/cancel).
func (c *Client) CancelContact(ctx context.Context, id string) error {
	return c.authed(ctx, http.MethodPost, "contacts/cancel", nil, map[string]string{"id": id}, nil)
}

// RemoveContact takes an identity off the contact list (POST
// contacts/remove, §9.6). The caller leaves the direct chat.
func (c *Client) RemoveContact(ctx context.Context, id string) error {
	return c.authed(ctx, http.MethodPost, "contacts/remove", nil, map[string]string{"id": id}, nil)
}

// Contact states in the homeserver's contact list.
const (
	ContactFull       = "contact"
	ContactPendingIn  = "pending-in"
	ContactPendingOut = "pending-out"
	ContactBlocked    = "blocked"
)

// Contact is one entry in the homeserver's contact list.
type Contact struct {
	ID      string       `json:"id"`
	State   string       `json:"state"`
	Since   int64        `json:"since"`
	Request *wire.Signed `json:"request,omitempty"` // for pending-in: the original request
}

// Contacts lists the contact list, every page of it (GET contacts).
func (c *Client) Contacts(ctx context.Context) ([]Contact, error) {
	var all []Contact
	after := ""
	for range 100 {
		q := url.Values{}
		if after != "" {
			q.Set("after", after)
		}
		var res struct {
			Items []Contact `json:"items"`
			Next  string    `json:"next,omitempty"`
		}
		if err := c.authed(ctx, http.MethodGet, "contacts", q, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.Next == "" {
			return all, nil
		}
		after = res.Next
	}
	return all, errors.New("client: the contact list doesn't end")
}

// VerifyContactRequest checks a contact request against the requester's
// chain: its signature by a current device of from, and that it's for
// this identity. It returns the body.
func (c *Client) VerifyContactRequest(req wire.Signed, chain *identity.Chain) (ContactRequestBody, error) {
	var body ContactRequestBody
	if req.T != TypeContactRequest {
		return body, errors.New("client: not a contact request")
	}
	if err := req.Body(&body); err != nil {
		return body, err
	}
	if body.To != c.ID() || chain.ID() != body.From {
		return body, errors.New("client: the request is for someone else")
	}
	dev, ok := chain.State.Device(req.K)
	if !ok {
		return body, errors.New("client: the request isn't signed by a device of the requester")
	}
	if err := sig.VerifyDevice(req, dev.Pub); err != nil {
		return body, err
	}
	return body, nil
}

// AcceptChain checks a chain the homeserver handed over with a notice,
// such as a requester's chain on a contact-request (§9.2), and keeps it as
// this device's copy under §6.5's rules.
func (c *Client) AcceptChain(id string, entries []wire.Signed) (*identity.Chain, error) {
	return c.acceptChain(id, entries)
}

// CachedChain returns this device's copy of an identity's chain, or nil.
func (c *Client) CachedChain(id string) (*identity.Chain, error) {
	return c.cachedChain(id)
}
