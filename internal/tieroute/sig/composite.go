package sig

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
)

// The composite construction, pinned to RFC 10073 (§3.2.1).
const (
	Prefix = "CompositeAlgorithmSignatures2025"
	Label  = "COMPSIG-MLDSA65-Ed25519-SHA512"

	MLDSAPublicSize     = mldsa.MLDSA65PublicKeySize // 1,952 bytes
	MLDSASignatureSize  = mldsa.MLDSA65SignatureSize // 3,309 bytes
	CompositePublicSize = MLDSAPublicSize + ed25519.PublicKeySize
	CompositeSigSize    = MLDSASignatureSize + ed25519.SignatureSize
	SeedSize            = 32
)

// MessageRepresentative returns M' = Prefix || Label || len(ctx) || ctx ||
// SHA-512(M), with Tieroute's empty application context (§3.2.1).
func MessageRepresentative(m []byte) []byte {
	h := sha512.Sum512(m)
	out := make([]byte, 0, len(Prefix)+len(Label)+1+len(h))
	out = append(out, Prefix...)
	out = append(out, Label...)
	out = append(out, 0x00)
	return append(out, h[:]...)
}

// CompositeKey is a composite ML-DSA-65 + Ed25519 private key.
type CompositeKey struct {
	mldsa *mldsa.PrivateKey
	ed    ed25519.PrivateKey
}

// NewCompositeKey makes a composite key from its two 32-byte seeds: the
// ML-DSA-65 key generation seed and the Ed25519 seed.
func NewCompositeKey(mldsaSeed, edSeed []byte) (*CompositeKey, error) {
	if len(mldsaSeed) != SeedSize || len(edSeed) != SeedSize {
		return nil, errors.New("sig: composite seeds are 32 bytes each")
	}
	m, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), mldsaSeed)
	if err != nil {
		return nil, err
	}
	return &CompositeKey{mldsa: m, ed: ed25519.NewKeyFromSeed(edSeed)}, nil
}

// GenerateCompositeKey makes a new random composite key.
func GenerateCompositeKey() (*CompositeKey, error) {
	seeds := make([]byte, 2*SeedSize)
	if _, err := rand.Read(seeds); err != nil {
		return nil, err
	}
	return NewCompositeKey(seeds[:SeedSize], seeds[SeedSize:])
}

// Seeds returns the ML-DSA-65 and Ed25519 seeds, for storage at rest.
func (k *CompositeKey) Seeds() (mldsaSeed, edSeed []byte) {
	return k.mldsa.Bytes(), k.ed.Seed()
}

// Public returns the composite public key: the ML-DSA-65 public key, then
// the Ed25519 public key (1,984 bytes).
func (k *CompositeKey) Public() []byte {
	out := append([]byte{}, k.mldsa.PublicKey().Bytes()...)
	return append(out, k.ed.Public().(ed25519.PublicKey)...)
}

// EdPublic returns the Ed25519 component's public key. For a device key it's
// also the device's identifier (§4.2).
func (k *CompositeKey) EdPublic() []byte {
	return append([]byte{}, k.ed.Public().(ed25519.PublicKey)...)
}

// Sign makes a composite signature over m, with hedged ML-DSA signing.
func (k *CompositeKey) Sign(m []byte) ([]byte, error) {
	return k.sign(m, false)
}

// SignDeterministic signs with deterministic ML-DSA, as the test vectors do.
func (k *CompositeKey) SignDeterministic(m []byte) ([]byte, error) {
	return k.sign(m, true)
}

func (k *CompositeKey) sign(m []byte, deterministic bool) ([]byte, error) {
	mp := MessageRepresentative(m)
	opts := &mldsa.Options{Context: Label}
	var ms []byte
	var err error
	if deterministic {
		ms, err = k.mldsa.SignDeterministic(mp, opts)
	} else {
		ms, err = k.mldsa.Sign(rand.Reader, mp, opts)
	}
	if err != nil {
		return nil, err
	}
	return append(ms, ed25519.Sign(k.ed, mp)...), nil
}

// SplitPublic splits a composite public key into its two components.
func SplitPublic(pub []byte) (mldsaPub, edPub []byte, err error) {
	if len(pub) != CompositePublicSize {
		return nil, nil, fmt.Errorf("sig: composite public key is %d bytes, want %d", len(pub), CompositePublicSize)
	}
	return pub[:MLDSAPublicSize], pub[MLDSAPublicSize:], nil
}

// ValidCompositePublic reports whether pub is a well-formed composite
// public key: a decodable ML-DSA-65 key and a valid Ed25519 key (§4.3).
func ValidCompositePublic(pub []byte) bool {
	m, e, err := SplitPublic(pub)
	if err != nil {
		return false
	}
	if _, err := mldsa.NewPublicKey(mldsa.MLDSA65(), m); err != nil {
		return false
	}
	return ValidPublicKey(e)
}

// VerifyComposite checks a composite signature over m. Both components have
// to verify, and the Ed25519 one under every rule of §4.3 (§3.2.1).
func VerifyComposite(pub, m, signature []byte) error {
	mPub, ePub, err := SplitPublic(pub)
	if err != nil {
		return err
	}
	if len(signature) != CompositeSigSize {
		return ErrSignature
	}
	pk, err := mldsa.NewPublicKey(mldsa.MLDSA65(), mPub)
	if err != nil {
		return ErrSignature
	}
	mp := MessageRepresentative(m)
	if err := mldsa.Verify(pk, mp, signature[:MLDSASignatureSize], &mldsa.Options{Context: Label}); err != nil {
		return ErrSignature
	}
	return VerifyEd25519(ePub, mp, signature[MLDSASignatureSize:])
}
