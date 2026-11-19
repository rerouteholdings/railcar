package ui

import (
	"context"
	"errors"
	"strings"
	"time"
)

// demoLink is the demo's link code. It has the form of a real one, and
// links nothing.
const demoLink = "link-di3g-7vil-4vqh-7xug-ywj6-4ann-q4fs-cz6q-pwer-pv7u-s7to-upyf-727v-ajq3-6g3f-wx22-bxmj-bjdx-w5ed-dlur-r5uc-66mm-tzsw-gwp7-ecau-k4q@chat.example.com"

// demoDevices are the demo identity's devices.
func (d *DemoBackend) demoDevices() []Device {
	now := d.opts.Now()
	if d.devices == nil {
		d.devices = []Device{
			{Key: "this", Name: "Railcar on this computer", Added: now.AddDate(0, -2, 0), This: true},
			{Key: "phone", Name: "Kat's phone", Added: now.AddDate(0, -1, -3)},
		}
	}
	return d.devices
}

// StartLink implements Backend. The other device "approves" a few polls
// later.
func (d *DemoBackend) StartLink(_ context.Context, req LinkRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := demoKat
	id.Handle, id.Homeserver = req.Handle, req.Homeserver
	d.linking, d.linkPolls = &id, 0
	return strings.Replace(demoLink, "chat.example.com", req.Homeserver, 1), nil
}

// StartLinkWithCode implements Backend.
func (d *DemoBackend) StartLinkWithCode(_ context.Context, code string) error {
	at := strings.LastIndexByte(code, '@')
	if !strings.HasPrefix(strings.ToLower(code), "approve") || at < 0 {
		return errors.New("That isn't an approval code")
	}
	rest := code[:at]
	handle := rest[strings.LastIndexByte(rest, '@')+1:]
	d.mu.Lock()
	defer d.mu.Unlock()
	id := demoKat
	id.Handle, id.Homeserver = handle, code[at+1:]
	d.linking, d.linkPolls = &id, 0
	return nil
}

// ConfirmLink implements Backend.
func (d *DemoBackend) ConfirmLink(_ context.Context, code string) error {
	if checkConfirmation(code) != nil {
		return errors.New("A confirmation code has 8 letters and digits")
	}
	return nil
}

// FinishLink implements Backend.
func (d *DemoBackend) FinishLink(context.Context) (Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.linking == nil {
		return Identity{}, errors.New("There's no sign-in to finish")
	}
	d.linkPolls++
	if d.opts.Live && d.linkPolls < 3 {
		return Identity{}, ErrNotLinkedYet
	}
	d.identity, d.linking = d.linking, nil
	return *d.identity, nil
}

// CancelLink implements Backend.
func (d *DemoBackend) CancelLink() {
	d.mu.Lock()
	d.linking = nil
	d.mu.Unlock()
}

// ApproveLinkCode implements Backend.
func (d *DemoBackend) ApproveLinkCode(_ context.Context, code, name string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(code)), "link") {
		return "", errors.New("That isn't a link code. It starts with link-")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approving, d.approvePolls = name, 0
	return "7K2M-9QXD", nil
}

// ShowApprovalCode implements Backend.
func (d *DemoBackend) ShowApprovalCode(_ context.Context, name string) (ApprovalCode, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approving, d.approvePolls = name, 0
	return ApprovalCode{
		Text:    "approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@" + demoKat.Handle + "@" + demoKat.Homeserver,
		Expires: d.opts.Now().Add(10 * time.Minute),
	}, nil
}

// FinishApproval implements Backend. The new device answers a few polls
// later.
func (d *DemoBackend) FinishApproval(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.approving == "" {
		return false, errors.New("There's no device to add")
	}
	d.approvePolls++
	if d.opts.Live && d.approvePolls < 3 {
		return false, nil
	}
	d.devices = append(d.demoDevices(), Device{Key: "new" + d.approving, Name: d.approving, Added: d.opts.Now()})
	d.approving = ""
	return true, nil
}

// CancelApproval implements Backend.
func (d *DemoBackend) CancelApproval() {
	d.mu.Lock()
	d.approving = ""
	d.mu.Unlock()
}

// Devices implements Backend.
func (d *DemoBackend) Devices(context.Context) ([]Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Device{}, d.demoDevices()...), nil
}

// RemoveDevice implements Backend.
func (d *DemoBackend) RemoveDevice(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ds := d.demoDevices()
	for i, dev := range ds {
		if dev.Key == key {
			if dev.This {
				return errors.New("Railcar can't remove the device it's running on. Sign out instead")
			}
			d.devices = append(ds[:i:i], ds[i+1:]...)
			return nil
		}
	}
	return errors.New("That device isn't one of yours")
}
