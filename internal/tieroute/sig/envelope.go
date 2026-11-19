package sig

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// ErrWrongSigner reports an envelope whose k or kid doesn't match the
// signer the verifier expects (§3.2.2).
var ErrWrongSigner = errors.New("sig: envelope names another signer")

// SignDevice signs body as type t with a device's composite key. The
// envelope's k is the key's 32-byte Ed25519 component, and the verifier
// finds the composite key in the signer's chain (§3.2.2).
func SignDevice(key *CompositeKey, t string, body any) (wire.Signed, error) {
	return signComposite(key, t, body, key.EdPublic(), false)
}

// SignRevealing signs body with a composite key that isn't in any chain,
// such as a recovery key or a new device's link proof key. The envelope's k
// is the whole composite public key (§3.2.2).
func SignRevealing(key *CompositeKey, t string, body any) (wire.Signed, error) {
	return signComposite(key, t, body, key.Public(), false)
}

// SignDeviceDeterministic is SignDevice with deterministic ML-DSA, for
// reproducing the test vectors.
func SignDeviceDeterministic(key *CompositeKey, t string, body any) (wire.Signed, error) {
	return signComposite(key, t, body, key.EdPublic(), true)
}

func signComposite(key *CompositeKey, t string, body any, k []byte, det bool) (wire.Signed, error) {
	b, err := wire.BodyText(body)
	if err != nil {
		return wire.Signed{}, err
	}
	input := wire.SigInput(t, b)
	var s []byte
	if det {
		s, err = key.SignDeterministic(input)
	} else {
		s, err = key.Sign(input)
	}
	if err != nil {
		return wire.Signed{}, err
	}
	return wire.Signed{T: t, B: b, K: k, S: s}, nil
}

// SignMessageKey signs body with a device's Ed25519 message key (§3.2.3).
func SignMessageKey(key ed25519.PrivateKey, t string, body any) (wire.Signed, error) {
	b, err := wire.BodyText(body)
	if err != nil {
		return wire.Signed{}, err
	}
	s := ed25519.Sign(key, wire.SigInput(t, b))
	return wire.Signed{T: t, B: b, K: append([]byte{}, key.Public().(ed25519.PublicKey)...), S: s}, nil
}

// VerifyDevice checks an envelope signed with a device's composite key,
// whose composite public key the verifier took from the signer's chain.
// k has to be that key's Ed25519 component, and s a composite signature.
func VerifyDevice(s wire.Signed, devicePub []byte) error {
	_, ed, err := SplitPublic(devicePub)
	if err != nil {
		return err
	}
	if s.Kid != "" || !bytes.Equal(s.K, ed) {
		return ErrWrongSigner
	}
	return VerifyComposite(devicePub, s.SigInput(), s.S)
}

// VerifyRevealed checks an envelope whose k carries the whole composite
// public key, such as a recovery-signed chain entry. It returns that key,
// which the caller checks against a commitment or what it already holds.
func VerifyRevealed(s wire.Signed) ([]byte, error) {
	if s.Kid != "" || len(s.K) != CompositePublicSize {
		return nil, ErrWrongSigner
	}
	if err := VerifyComposite(s.K, s.SigInput(), s.S); err != nil {
		return nil, err
	}
	return s.K, nil
}

// VerifyMessageKey checks an envelope signed with an Ed25519 message key.
// An Ed25519-only signature is valid only with a 32-byte k and no kid
// (§3.2.2).
func VerifyMessageKey(s wire.Signed, key []byte) error {
	if s.Kid != "" || len(s.K) != ed25519.PublicKeySize || !bytes.Equal(s.K, key) {
		return ErrWrongSigner
	}
	if len(s.S) != ed25519.SignatureSize {
		return ErrSignature
	}
	return VerifyEd25519(key, s.SigInput(), s.S)
}

// VerifyServer checks an envelope signed with a server key named by kid.
// pub is the composite key the signer's server document lists under that
// kid (§5.1, §8.3).
func VerifyServer(s wire.Signed, kid string, pub []byte) error {
	if len(s.K) != 0 || s.Kid == "" || s.Kid != kid {
		return ErrWrongSigner
	}
	if len(s.S) != CompositeSigSize {
		return fmt.Errorf("%w: a server signature is composite", ErrSignature)
	}
	return VerifyComposite(pub, s.SigInput(), s.S)
}
