package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Invite makes a private invite code and returns it as it's shared:
// @handle::code (§9.1, §9.4).
func (e *Engine) Invite(ctx context.Context) (string, error) {
	inv, err := e.c.CreateInvite(ctx, client.InviteOptions{})
	if err != nil {
		return "", err
	}
	return client.InviteCode{Handle: e.c.Handle(), Code: inv.Code}.String(), nil
}

// ErrNeedsCode reports a bare @handle: a request needs an invite code
// (§9.1). Public profiles, which can publish one, come in a later phase.
var ErrNeedsCode = errors.New("chat: a contact request needs an invite code, as @handle::code")

// Add sends a contact request with a code as §9.4 defines it,
// @handle::code or its link. It resolves the handle, checks both
// directions (§5.3), and sends the request (§9.2).
func (e *Engine) Add(ctx context.Context, code string) (Contact, error) {
	ic, err := client.ParseInviteCode(code)
	if err != nil {
		return Contact{}, err
	}
	if ic.Code == "" {
		return Contact{}, ErrNeedsCode
	}
	if ic.Handle == e.c.Handle() {
		return Contact{}, errors.New("chat: that's your own code")
	}
	r, err := e.c.ResolveHandle(ctx, ic.Handle)
	if err != nil {
		return Contact{}, err
	}
	e.mu.Lock()
	known := e.st.Contacts[r.ID]
	e.mu.Unlock()
	if known != nil && known.State == StatePendingIn {
		// They asked first: adding them is accepting.
		if _, err := e.Accept(ctx, r.ID); err != nil {
			return Contact{}, err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return *e.st.Contacts[r.ID], nil
	}
	if _, err := e.c.RequestContact(ctx, r.ID, ic.Code); err != nil {
		return Contact{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.Handles[r.ID] = r.Handle
	c := e.st.Contacts[r.ID]
	if c == nil {
		c = &Contact{ID: r.ID}
		e.st.Contacts[r.ID] = c
	}
	c.Handle = r.Handle
	if c.State != StateContact {
		c.State, c.Since = StatePendingOut, e.c.Clock().Now().Unix()
	}
	if err := e.save(); err != nil {
		return Contact{}, err
	}
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	return *c, nil
}

// Accept accepts a contact request and creates the direct chat (§9.3).
func (e *Engine) Accept(ctx context.Context, id string) (Chat, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.st.Contacts[id]
	if c == nil || (c.State != StatePendingIn && c.State != StateContact) {
		return Chat{}, errors.New("chat: there's no request from them")
	}
	if err := e.c.AcceptContact(ctx, id); err != nil && !wire.IsCode(err, wire.CodeNotFound) {
		return Chat{}, err
	}
	c.State, c.Request = StateContact, nil
	if err := e.save(); err != nil {
		return Chat{}, err
	}
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	ch, err := e.createDirect(ctx, id)
	if err != nil {
		return Chat{}, fmt.Errorf("chat: accepted, but the chat wasn't created: %w", err)
	}
	return ch.public(), nil
}

// Decline declines a contact request. The requester is told (§9.3).
func (e *Engine) Decline(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.st.Contacts[id]
	if c == nil || c.State != StatePendingIn || c.Request == nil {
		return errors.New("chat: there's no request from them")
	}
	if err := e.c.DeclineContact(ctx, *c.Request); err != nil {
		return err
	}
	delete(e.st.Contacts, id)
	if err := e.save(); err != nil {
		return err
	}
	c.State = StateDeclined
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	return nil
}

// notice handles a notice from the homeserver. The caller holds e.mu.
func (e *Engine) notice(ctx context.Context, n *client.Notice) error {
	switch n.T {
	case client.NoticeContactRequest:
		return e.contactRequest(ctx, n)
	case client.NoticeContactDeclined:
		return e.contactDeclined(n)
	case client.NoticeChainUpdate:
		if n.ID != "" && n.ID != e.c.ID() {
			if _, err := e.fetchChain(ctx, n.ID, true); err != nil {
				return err
			}
			// Their devices may have changed: every shared chat follows
			// (§11.9).
			e.fresh[n.ID] = time.Now()
			e.wantSync()
		}
	case client.NoticeChainAlert:
		if _, err := e.c.FetchChain(ctx, e.c.ID(), client.FetchOptions{}); err != nil && !errors.Is(err, client.ErrDeviceRemoved) {
			return retryable{err}
		}
		e.wantSync()
	case client.NoticeDeliveryFailed:
		e.deliveryFailed(n)
	}
	return nil
}

// contactRequest handles a contact-request notice: it checks the
// requester's chain and the request, and lists it (§9.2). If this
// identity had asked them too, they're contacts now, and the identity
// whose identifier sorts first creates the direct chat.
func (e *Engine) contactRequest(ctx context.Context, n *client.Notice) error {
	if n.Request == nil {
		return errors.New("chat: a contact-request notice without a request")
	}
	var body client.ContactRequestBody
	if err := n.Request.Body(&body); err != nil {
		return err
	}
	if body.To != e.c.ID() {
		return errors.New("chat: a contact request for someone else")
	}
	chain, err := e.c.AcceptChain(body.From, n.Chain)
	if err != nil {
		return err
	}
	if _, err := e.c.VerifyContactRequest(*n.Request, chain); err != nil {
		return err
	}
	e.st.Handles[body.From] = chain.State.Handle
	c := e.st.Contacts[body.From]
	if c == nil {
		c = &Contact{ID: body.From}
		e.st.Contacts[body.From] = c
	}
	c.Handle = chain.State.Handle
	mutual := c.State == StatePendingOut
	switch c.State {
	case StateContact:
		// A request between contacts changes nothing (§9.3). Keep it, so
		// the person can accept it again, which re-creates the chat.
		c.Request = n.Request
	case StateBlocked:
		return nil
	default:
		c.State, c.Request, c.Since = StatePendingIn, n.Request, body.TS
	}
	if mutual {
		c.State, c.Request = StateContact, nil
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	if mutual && e.c.ID() < body.From && c.Chat == "" {
		if _, err := e.createDirect(ctx, body.From); err != nil {
			e.log.Info("chat: creating the direct chat after mutual requests", "err", err)
		}
	}
	return nil
}

// contactDeclined handles a decline of this identity's request (§9.3).
func (e *Engine) contactDeclined(n *client.Notice) error {
	if n.Decline == nil {
		return errors.New("chat: a contact-declined notice without a decline")
	}
	var body client.ContactDeclineBody
	if err := n.Decline.Body(&body); err != nil {
		return err
	}
	c := e.st.Contacts[body.From]
	if body.To != e.c.ID() || c == nil || c.State != StatePendingOut {
		return nil
	}
	// The homeserver checked it against the pending request, so it came
	// from them. Check the signature anyway when this device has the chain.
	if chain, _ := e.c.CachedChain(body.From); chain != nil {
		if dev, ok := chain.State.Device(n.Decline.K); !ok || verifyDevice(*n.Decline, dev.Pub) != nil {
			return errors.New("chat: a decline not signed by a device of the decliner")
		}
	}
	c.State = StateDeclined
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventContact, Contact: ptr(*c)})
	return nil
}

// fetchChain returns an identity's chain: this device's copy, or the
// homeserver's when there's none or fresh is set. The caller holds e.mu.
func (e *Engine) fetchChain(ctx context.Context, id string, fresh bool) (*identity.Chain, error) {
	if !fresh {
		if c, err := e.c.CachedChain(id); err == nil && c != nil {
			return c, nil
		}
	}
	c, err := e.c.FetchChain(ctx, id, client.FetchOptions{Fresh: fresh && id != e.c.ID()})
	var w *client.ChainWarning
	if errors.As(err, &w) {
		return nil, err
	}
	if err != nil && !errors.Is(err, client.ErrDeviceRemoved) {
		return nil, retryable{err}
	}
	if c != nil && id != e.c.ID() {
		e.st.Handles[id] = c.State.Handle
	}
	return c, err
}
