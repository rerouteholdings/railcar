package chat

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// recordID turns a group ID into the part of a record name that names it.
// Record names are lowercase, so it's the ID's bytes in hex.
func recordID(chat string) string {
	b, err := wire.DecodeB64(chat)
	if err != nil {
		return hex.EncodeToString([]byte(chat))
	}
	return hex.EncodeToString(b)
}

func verifyDevice(s wire.Signed, pub []byte) error { return sig.VerifyDevice(s, pub) }

// parseCredential reads a BasicCredential's identity: JCS([identifier,
// device-key]) (§7.1).
func parseCredential(cred []byte) (id string, dev []byte, err error) {
	var pair []string
	if err := wire.Unmarshal(cred, &pair); err != nil || len(pair) != 2 {
		return "", nil, errors.New("chat: a credential that isn't [identifier, device-key]")
	}
	if !identity.ValidIdentifier(pair[0]) {
		return "", nil, errors.New("chat: a credential with a bad identifier")
	}
	dev, err = wire.DecodeB64Len(pair[1], 32)
	if err != nil {
		return "", nil, errors.New("chat: a credential with a bad device key")
	}
	if !bytes.Equal(identity.MLSIdentity(pair[0], dev), cred) {
		return "", nil, errors.New("chat: a credential that isn't in canonical form")
	}
	return pair[0], dev, nil
}

// devicePub finds a device's composite public key in a chain: the current
// device, or with ever set, any device the chain added at some point.
func devicePub(c *identity.Chain, dev []byte, ever bool) ([]byte, bool) {
	if d, ok := c.State.Device(dev); ok {
		return d.Pub, true
	}
	if !ever {
		return nil, false
	}
	for _, info := range c.Info {
		for _, op := range info.Body.Ops {
			if op.Op == identity.OpAddDevice && bytes.Equal(op.Key, dev) {
				return op.Pub, true
			}
		}
	}
	return nil, false
}

// leafKind says which devices a leaf check takes.
type leafKind int

const (
	leafCurrent leafKind = iota // a current device in the chain given
	leafEver                    // any device the chain added at some point (§7.1, Welcomes)
)

// checkLeaf makes §7.1's checks on a leaf against its identity's chain:
// the credential names a device of the identity, the leaf's tieroute_msg_key
// is that device's signed msg-key, and the leaf's signature key is the
// msg-key's key. It returns the identity and device.
func (e *Engine) checkLeaf(leaf *mls.LeafInfo, chain func(id string) (*identity.Chain, error), kind leafKind) (string, []byte, error) {
	if leaf.CredentialType != mls.CredentialBasic {
		return "", nil, errors.New("chat: a leaf without a basic credential")
	}
	id, dev, err := parseCredential(leaf.Identity)
	if err != nil {
		return "", nil, err
	}
	if !slices.Contains(leaf.Capabilities.Extensions, ExtGroup) || !slices.Contains(leaf.Capabilities.Extensions, ExtMsgKey) {
		return "", nil, errors.New("chat: a leaf that doesn't list 0xF0A1 and 0xF0A2")
	}
	ext, ok := leaf.Extension(ExtMsgKey)
	if !ok {
		return "", nil, errors.New("chat: a leaf without tieroute_msg_key")
	}
	c, err := chain(id)
	if err != nil {
		return "", nil, err
	}
	pub, ok := devicePub(c, dev, kind == leafEver)
	if !ok {
		return "", nil, fmt.Errorf("chat: a leaf for a device that isn't %s's", id)
	}
	cacheKey := string(leaf.SignatureKey) + "\x00" + string(ext) + "\x00" + string(pub)
	if e.leaves[cacheKey] {
		return id, dev, nil
	}
	cert, err := wire.ParseSigned(ext)
	if err != nil {
		return "", nil, fmt.Errorf("chat: tieroute_msg_key: %w", err)
	}
	mk, err := identity.VerifyMsgKey(cert, id, pub)
	if err != nil {
		return "", nil, err
	}
	if !bytes.Equal(mk.Key, leaf.SignatureKey) {
		return "", nil, errors.New("chat: the leaf's signature key isn't its msg-key's key")
	}
	e.leaves[cacheKey] = true
	return id, dev, nil
}

// chainGetter returns a function that gets chains for leaf checks: this
// device's copy, fetched once when it's missing, or fresh when asked.
// The caller holds e.mu.
func (e *Engine) chainGetter(ctx context.Context, fresh bool) func(string) (*identity.Chain, error) {
	got := map[string]*identity.Chain{}
	return func(id string) (*identity.Chain, error) {
		if c, ok := got[id]; ok {
			return c, nil
		}
		c, err := e.fetchChain(ctx, id, fresh)
		if err != nil {
			return nil, err
		}
		got[id] = c
		return c, nil
	}
}

// checkClaimed checks a claimed key package before adding its device: its
// kp-binding against the device's composite key (§3.2.2), and its leaf
// (§7.1). It returns the key package's bytes to add.
func (e *Engine) checkClaimed(kp client.ClaimedKP, id string, chain *identity.Chain) ([]byte, error) {
	d, ok := chain.State.Device(kp.Dev)
	if !ok {
		return nil, errors.New("chat: a key package for a device that isn't in the chain")
	}
	if kp.Binding == nil {
		return nil, errors.New("chat: a key package without a kp-binding")
	}
	if err := identity.VerifyKPBinding(*kp.Binding, id, d.Pub, kp.KP); err != nil {
		return nil, err
	}
	info, err := mls.InspectKeyPackage(kp.KP)
	if err != nil {
		return nil, err
	}
	if info.Ciphersuite != mls.Ciphersuite {
		return nil, errors.New("chat: a key package in another ciphersuite")
	}
	gotID, dev, err := e.checkLeaf(&info.Leaf, func(string) (*identity.Chain, error) { return chain, nil }, leafCurrent)
	if err != nil {
		return nil, err
	}
	if gotID != id || !bytes.Equal(dev, kp.Dev) {
		return nil, errors.New("chat: a key package whose credential names another device")
	}
	return kp.KP, nil
}
