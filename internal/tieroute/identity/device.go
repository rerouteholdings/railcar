package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Type tags of the objects a device signs.
const (
	TypeMsgKey    = "msg-key"
	TypeKPBinding = "kp-binding"
	TypeAuth      = "auth"
)

// Device is this device's keys: its composite device key, which never
// leaves the device (§6.1), and its Ed25519 message key, which signs its
// messages and MLS leaves (§3.2.3).
type Device struct {
	Key    *sig.CompositeKey
	Name   string
	MsgKey ed25519.PrivateKey
	MsgN   uint64 // 1 for the first message key, then one higher each rotation
}

// GenerateDevice makes a new device key and its first message key.
func GenerateDevice(name string) (*Device, error) {
	k, err := sig.GenerateCompositeKey()
	if err != nil {
		return nil, err
	}
	_, mk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Device{Key: k, Name: name, MsgKey: mk, MsgN: 1}, nil
}

// ID returns the device key: the Ed25519 component of its composite key,
// which also identifies the device (§4.2).
func (d *Device) ID() []byte { return d.Key.EdPublic() }

// RotateMsgKey replaces the message key, for the events §3.2.3 lists.
func (d *Device) RotateMsgKey() error {
	_, mk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	d.MsgKey, d.MsgN = mk, d.MsgN+1
	return nil
}

// SignEntry signs a chain entry with the device key.
func (d *Device) SignEntry(e Entry) (wire.Signed, error) {
	return sig.SignDevice(d.Key, TypeChainEntry, e)
}

// MsgKeyBody is the body of a msg-key certificate (§3.2.3).
type MsgKeyBody struct {
	ID  string     `json:"id"`
	Dev wire.Bytes `json:"dev"` // SHA-256 of the device's composite public key
	Key wire.Bytes `json:"key"` // the Ed25519 message key
	N   uint64     `json:"n"`
}

// MsgKeyCert signs the current message key's msg-key certificate with the
// composite device key (§3.2.3).
func (d *Device) MsgKeyCert(id string) (wire.Signed, error) {
	dev := sha256.Sum256(d.Key.Public())
	return sig.SignDevice(d.Key, TypeMsgKey, MsgKeyBody{
		ID: id, Dev: dev[:], Key: wire.Bytes(d.MsgKey.Public().(ed25519.PublicKey)), N: d.MsgN,
	})
}

// VerifyMsgKey checks a msg-key against the device's composite key from the
// sender's chain: the composite signature, dev, the identity, and that key
// is a valid Ed25519 public key (§3.2.3).
func VerifyMsgKey(s wire.Signed, id string, devicePub []byte) (MsgKeyBody, error) {
	var b MsgKeyBody
	if s.T != TypeMsgKey {
		return b, errors.New("identity: not a msg-key")
	}
	if err := sig.VerifyDevice(s, devicePub); err != nil {
		return b, err
	}
	if err := s.Body(&b); err != nil {
		return b, err
	}
	dev := sha256.Sum256(devicePub)
	if b.ID != id || !bytes.Equal(b.Dev, dev[:]) || b.N == 0 {
		return b, errors.New("identity: the msg-key names another identity or device")
	}
	if !sig.ValidPublicKey(b.Key) {
		return b, errors.New("identity: the msg-key's key isn't a valid Ed25519 key")
	}
	return b, nil
}

// KPBindingBody is the body of a key package binding (§3.2.2).
type KPBindingBody struct {
	ID string     `json:"id"`
	KP wire.Bytes `json:"kp"` // SHA-256 of the KeyPackage's raw bytes
}

// KPBinding signs a key package binding for one key package (§3.2.2).
func (d *Device) KPBinding(id string, kp []byte) (wire.Signed, error) {
	h := sha256.Sum256(kp)
	return sig.SignDevice(d.Key, TypeKPBinding, KPBindingBody{ID: id, KP: h[:]})
}

// VerifyKPBinding checks a key package's binding against the device's
// composite key from its chain. A client that claims key packages runs this
// before adding the device (§3.2.2).
func VerifyKPBinding(s wire.Signed, id string, devicePub, kp []byte) error {
	if s.T != TypeKPBinding {
		return errors.New("identity: not a kp-binding")
	}
	if err := sig.VerifyDevice(s, devicePub); err != nil {
		return err
	}
	var b KPBindingBody
	if err := s.Body(&b); err != nil {
		return err
	}
	h := sha256.Sum256(kp)
	if b.ID != id || !bytes.Equal(b.KP, h[:]) {
		return fmt.Errorf("identity: the kp-binding is for another identity or key package")
	}
	return nil
}

// MLSIdentity returns the identity field of a device's MLS BasicCredential:
// JCS([identifier, device-key]) (§7.1).
func MLSIdentity(id string, deviceKey []byte) []byte {
	return wire.MustJCS([]string{id, wire.EncodeB64(deviceKey)})
}
