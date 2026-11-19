package app

import (
	"context"
	"errors"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// clearLeftover deletes a profile that holds no identity, such as one a
// signup left before it registered, so a linked device can take the
// directory.
func (b *Backend) clearLeftover() {
	if !store.Exists(b.opts.ProfileDir) {
		return
	}
	if _, err := client.Open(b.opts.ProfileDir, b.opts.Client); errors.Is(err, store.ErrNotFound) {
		if p, err := store.Open(b.opts.ProfileDir, b.opts.Client.Store); err == nil {
			p.Destroy()
		}
	}
}

// StartLink implements ui.Backend (§7.2 step 1).
func (b *Backend) StartLink(ctx context.Context, req ui.LinkRequest) (string, error) {
	l, err := client.StartLink(ctx, req.Homeserver, req.Handle, b.opts.Client)
	if err != nil {
		return "", linkError(err)
	}
	b.mu.Lock()
	b.link = l
	b.mu.Unlock()
	return l.Code().String(), nil
}

// StartLinkWithCode implements ui.Backend (§7.2.1 step 2).
func (b *Backend) StartLinkWithCode(ctx context.Context, code string) error {
	l, err := client.StartLinkWithApproval(ctx, code, b.opts.Client)
	if err != nil {
		return linkError(err)
	}
	b.mu.Lock()
	b.link = l
	b.mu.Unlock()
	return nil
}

// ConfirmLink implements ui.Backend (§7.2 step 2).
func (b *Backend) ConfirmLink(ctx context.Context, code string) error {
	b.mu.Lock()
	l := b.link
	b.mu.Unlock()
	if l == nil {
		return errors.New("Start again: this device isn't signing in")
	}
	return linkError(l.Confirm(ctx, code))
}

// FinishLink implements ui.Backend (§7.2 step 5): once the other device has
// added this one, it creates the profile, takes the contact list, and
// starts the chat layer. The link bundle and the Welcomes arrive over the
// live stream.
func (b *Backend) FinishLink(ctx context.Context) (ui.Identity, error) {
	b.mu.Lock()
	l := b.link
	b.mu.Unlock()
	if l == nil {
		return ui.Identity{}, errors.New("Start again: this device isn't signing in")
	}
	b.clearLeftover()
	c, err := l.Finish(ctx, b.opts.ProfileDir)
	if errors.Is(err, client.ErrNotLinkedYet) {
		return ui.Identity{}, ui.ErrNotLinkedYet
	}
	if err != nil {
		return ui.Identity{}, friendly(err)
	}
	b.mu.Lock()
	b.link = nil
	b.mu.Unlock()
	id, err := b.start(c)
	if err != nil {
		return ui.Identity{}, err
	}
	if eng, err := b.engine(); err == nil {
		if err := eng.ImportContacts(ctx); err != nil {
			b.log.Info("app: the contact list", "err", err)
		}
	}
	return id, nil
}

// CancelLink implements ui.Backend.
func (b *Backend) CancelLink() {
	b.mu.Lock()
	b.link = nil
	b.mu.Unlock()
}

// ApproveLinkCode implements ui.Backend (§7.2 step 2).
func (b *Backend) ApproveLinkCode(ctx context.Context, code, name string) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", err
	}
	a, err := eng.Client().ApproveLinkCode(ctx, code)
	if err != nil {
		return "", linkError(err)
	}
	b.mu.Lock()
	b.forgetApproval()
	b.approval, b.approvalName = a, name
	b.mu.Unlock()
	return a.Confirmation(), nil
}

// ShowApprovalCode implements ui.Backend (§7.2.1 step 1).
func (b *Backend) ShowApprovalCode(_ context.Context, name string) (ui.ApprovalCode, error) {
	eng, err := b.engine()
	if err != nil {
		return ui.ApprovalCode{}, err
	}
	s, err := eng.Client().NewApprovalCode()
	if err != nil {
		return ui.ApprovalCode{}, err
	}
	b.mu.Lock()
	b.forgetApproval()
	b.approvalSecret, b.approvalName = s, name
	b.mu.Unlock()
	return ui.ApprovalCode{Text: s.Code().String(), Expires: s.Expires()}, nil
}

// forgetApproval drops the approval in progress. The caller holds b.mu.
func (b *Backend) forgetApproval() {
	if b.approvalSecret != nil {
		b.approvalSecret.Forget()
	}
	b.approval, b.approvalSecret, b.approvalName = nil, nil, ""
}

// FinishApproval implements ui.Backend: once the new device has answered,
// it adds the device to the chain, sends the link bundle, and adds the
// device to every chat (§7.2 steps 3 to 5).
func (b *Backend) FinishApproval(ctx context.Context) (bool, error) {
	eng, err := b.engine()
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	a, s, name := b.approval, b.approvalSecret, b.approvalName
	b.mu.Unlock()
	var dev *client.NewDevice
	switch {
	case a != nil:
		ok, err := a.Check(ctx)
		switch {
		case errors.Is(err, client.ErrWrongConfirmation):
			return false, ui.ErrWrongConfirmation
		case err != nil:
			return false, linkError(err)
		case !ok:
			return false, nil
		}
		d := a.Device()
		dev = &d
	case s != nil:
		d, err := s.Check(ctx)
		switch {
		case errors.Is(err, client.ErrApprovalUsedTwice):
			b.mu.Lock()
			b.forgetApproval()
			b.mu.Unlock()
			return false, ui.ErrApprovalUsedTwice
		case errors.Is(err, client.ErrApprovalExpired):
			return false, ui.ErrApprovalExpired
		case err != nil:
			return false, friendly(err)
		case d == nil:
			return false, nil
		}
		dev = d
	default:
		return false, errors.New("There's no device to add")
	}
	if err := eng.AddDevice(ctx, *dev, name); err != nil {
		return false, friendly(err)
	}
	b.mu.Lock()
	b.approval, b.approvalSecret, b.approvalName = nil, nil, ""
	b.mu.Unlock()
	return true, nil
}

// CancelApproval implements ui.Backend.
func (b *Backend) CancelApproval() {
	b.mu.Lock()
	b.forgetApproval()
	b.mu.Unlock()
}

// Devices implements ui.Backend: the identity's devices, from a fresh copy
// of its chain when the homeserver answers.
func (b *Backend) Devices(ctx context.Context) ([]ui.Device, error) {
	eng, err := b.engine()
	if err != nil {
		return nil, err
	}
	c := eng.Client()
	chain := c.Chain()
	if fresh, err := c.FetchChain(ctx, c.ID(), client.FetchOptions{}); err == nil {
		chain = fresh
	}
	var out []ui.Device
	for _, d := range chain.State.Devices {
		added := time.Time{}
		if int(d.AddedSeq) < len(chain.Info) {
			added = time.Unix(chain.Info[d.AddedSeq].TS, 0)
		}
		out = append(out, ui.Device{
			Key: wire.EncodeB64(d.Key), Name: d.Name, Added: added,
			This: string(d.Key) == string(c.DeviceKey()),
		})
	}
	return out, nil
}

// RemoveDevice implements ui.Backend (§7.6).
func (b *Backend) RemoveDevice(ctx context.Context, key string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	k, err := wire.DecodeB64Len(key, 32)
	if err != nil {
		return errors.New("That isn't a device key")
	}
	return friendly(eng.RemoveDevice(ctx, k))
}

// linkError puts the errors of linking in words.
func linkError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, client.ErrLinkCode):
		return errors.New("That isn't a link code. It starts with link-")
	case errors.Is(err, client.ErrApprovalCode):
		return errors.New("That isn't an approval code. It starts with approve-")
	case errors.Is(err, client.ErrLinkMismatch):
		return errors.New("That code doesn't match the new device. Check it, and enter it again")
	case errors.Is(err, client.ErrNoLinkProof):
		return errors.New("The new device hasn't left its key with your homeserver. Check the code, or start again on the new device")
	case errors.Is(err, client.ErrHandleMismatch):
		return errors.New("That handle is no longer valid")
	case wire.IsCode(err, wire.CodeNotFound):
		return errors.New("Nobody has that handle on that homeserver")
	case wire.IsCode(err, wire.CodeAlreadyExists):
		return errors.New("This device is already one of that identity's devices")
	}
	return friendly(err)
}
