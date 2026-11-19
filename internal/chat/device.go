package chat

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
)

// recMLS is the profile record that holds the MLS state, as one
// tieroute-mls snapshot. The profile encrypts it at rest.
const recMLS = "mls"

// chatConfig is the OpenMLS configuration of every direct chat and group
// chat. The ratchet tree goes in each Welcome, since a chat has no other
// place to get it from (§11.1).
var chatConfig = mls.Config{RatchetTreeInWelcome: true, MaxPastEpochs: 3, Padding: 32}

// keyPackageLifetime is how long a key package stays valid: 90 days.
const keyPackageLifetime = 90 * 24 * 60 * 60

// Device is this device's MLS state: its key packages and groups, with the
// message key as its signer (§3.2.3). It implements
// client.KeyPackageMaker. Its methods are safe for concurrent use.
type Device struct {
	profile *store.Profile
	info    client.MLSInfo
	signer  *mls.Signer

	mu  sync.Mutex
	mls *mls.Client
}

// OpenDevice opens this device's MLS state from the profile, or starts a
// new one.
func OpenDevice(p *store.Profile, info client.MLSInfo) (*Device, error) {
	var snap []byte
	if err := p.Load(recMLS, &snap); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("chat: the MLS state: %w", err)
	}
	c, err := mls.Open(snap)
	if err != nil {
		return nil, fmt.Errorf("chat: the MLS state: %w", err)
	}
	s, err := mls.NewSigner(info.MsgKeySeed)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(s.PublicKey(), info.MsgKeyPublic) {
		return nil, errors.New("chat: the message key doesn't match its certificate")
	}
	return &Device{profile: p, info: info, signer: s, mls: c}, nil
}

// Close frees the MLS state. Unsaved changes are lost.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mls.Close()
	d.signer.Close()
}

// Save writes the MLS state to the profile. Saving also makes the state
// the point Revert returns to.
func (d *Device) Save() error {
	snap, err := d.mls.Snapshot()
	if err != nil {
		return err
	}
	return d.profile.Save(recMLS, snap)
}

// Revert undoes every MLS change since the last Save, including keys used
// up by decrypting, so a refused commit can be processed again (§11.9).
func (d *Device) Revert() error { return d.mls.Revert() }

// Leaf is this device's leaf node: its credential, the capabilities every
// leaf lists (§11.1), and its msg-key (§7.1).
func (d *Device) Leaf() mls.Leaf {
	return mls.Leaf{
		Identity:     d.info.Credential,
		Capabilities: mls.Capabilities{Extensions: []uint16{ExtGroup, ExtMsgKey}},
		Extensions:   []mls.Extension{{Type: ExtMsgKey, Data: d.info.MsgKeyExtension}},
	}
}

// KeyPackages makes key packages for upload (§11.4). It returns each one
// as its bare KeyPackage bytes, which the kp-binding hashes (§3.2.2). The
// private keys are saved with the MLS state before the packages leave the
// device.
func (d *Device) KeyPackages(n int, lastResort bool) ([][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, 0, n)
	for range n {
		kp, err := d.mls.NewKeyPackage(d.signer, mls.KeyPackageOptions{Leaf: d.Leaf(), LastResort: lastResort, Lifetime: keyPackageLifetime})
		if err != nil {
			return nil, err
		}
		out = append(out, kp.Raw)
	}
	if err := d.Save(); err != nil {
		return nil, err
	}
	return out, nil
}

// epochKey derives epoch_key_N from the group's exporter secret
// epoch_seed_N: Ed25519 from the seed itself, and ML-DSA-65 from
// HKDF-Expand(epoch_seed_N, "tieroute hub mldsa", 32) (§3.2.2, §11.6).
func epochKey(seed []byte) (*sig.CompositeKey, error) {
	m, err := hkdf.Expand(sha256.New, seed, "tieroute hub mldsa", 32)
	if err != nil {
		return nil, err
	}
	return sig.NewCompositeKey(m, seed)
}

// epochLabel is the exporter label of epoch keys (§11.6).
const epochLabel = "tieroute hub"

// EpochKey returns the group's epoch key for its current epoch.
func (d *Device) EpochKey(g *mls.Group) (*sig.CompositeKey, error) {
	seed, err := g.ExportSecret(epochLabel, g.ID(), 32)
	if err != nil {
		return nil, err
	}
	return epochKey(seed)
}

// PendingEpochKey returns the epoch key of the epoch this device's
// pending commit creates, for `next` in its commit request (§11.6).
func (d *Device) PendingEpochKey(g *mls.Group) (*sig.CompositeKey, error) {
	seed, err := g.ExportPendingSecret(epochLabel, g.ID(), 32)
	if err != nil {
		return nil, err
	}
	return epochKey(seed)
}
