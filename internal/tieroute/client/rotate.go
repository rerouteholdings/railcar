package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
)

// RotateDevice brings this device back as a new device, for a device that
// was offline too long to catch up (§11.9 step 2). It appends a
// self-rotation, a remove-device for its key and an add-device for a
// fresh one, signed by the current key (§6.4.1), then uses the new key
// and signs in with it. The new key is saved before the append, so a stop
// in between finishes when the profile next opens, or on the next call.
// A device whose key is no longer in the chain gets ErrDeviceRemoved: it
// comes back by linking, or with the recovery code.
func (c *Client) RotateDevice(ctx context.Context) error {
	if _, err := c.FetchChain(ctx, c.ID(), FetchOptions{}); err != nil {
		return err
	}
	c.mu.Lock()
	if c.promoteRotation() {
		c.mu.Unlock()
		return c.SignIn(ctx)
	}
	old := c.device
	if !c.chain.State.IsDevice(old.ID()) {
		c.mu.Unlock()
		return ErrDeviceRemoved
	}
	name := old.Name
	if info, ok := c.chain.State.Device(old.ID()); ok && info.Name != "" {
		name = info.Name
	}
	var dev *identity.Device
	if c.state.Rotating != nil {
		d, err := loadDevice(*c.state.Rotating)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		dev = d
	} else {
		d, err := identity.GenerateDevice(name)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		dev = d
		sd := saveDevice(dev)
		c.state.Rotating = &sd
		if err := c.saveLocked(); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	if _, err := c.Append(ctx, identity.RemoveDevice(old.ID()), identity.AddDevice(dev.Key.Public(), name)); err != nil {
		return fmt.Errorf("client: coming back as a new device: %w", err)
	}
	// Accepting the new chain switched to the new key.
	c.mu.Lock()
	ok := c.state.Rotating == nil && c.chain.State.IsDevice(c.device.ID())
	c.mu.Unlock()
	if !ok {
		return errors.New("client: the chain doesn't have the new device key")
	}
	return c.SignIn(ctx)
}

// promoteRotation switches to the key a self-rotation saved, once the
// chain has it and no longer has the old one. It reports whether it did.
// The caller holds c.mu.
func (c *Client) promoteRotation() bool {
	r := c.state.Rotating
	if r == nil {
		return false
	}
	dev, err := loadDevice(*r)
	if err != nil || !c.chain.State.IsDevice(dev.ID()) || c.chain.State.IsDevice(c.device.ID()) {
		return false
	}
	// The new device has its own inbox, from its start, so the stream
	// starts again there.
	c.device, c.state.Rotating = dev, nil
	c.state.Token, c.state.TokenExpires, c.state.Cursor = "", 0, ""
	c.saveLocked()
	close(c.restart)
	c.restart = make(chan struct{})
	return true
}

func saveDevice(d *identity.Device) savedDevice {
	ms, es := d.Key.Seeds()
	return savedDevice{MLDSASeed: ms, EdSeed: es, Name: d.Name, MsgSeed: d.MsgKey.Seed(), MsgN: d.MsgN}
}

func loadDevice(s savedDevice) (*identity.Device, error) {
	key, err := sig.NewCompositeKey(s.MLDSASeed, s.EdSeed)
	if err != nil {
		return nil, fmt.Errorf("client: stored device key: %w", err)
	}
	if len(s.MsgSeed) != ed25519.SeedSize {
		return nil, errors.New("client: stored message key is damaged")
	}
	return &identity.Device{Key: key, Name: s.Name, MsgKey: ed25519.NewKeyFromSeed(s.MsgSeed), MsgN: s.MsgN}, nil
}
