package wire

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

// Signed is the envelope every signed object uses (§4.3). B holds the body
// as JSON text, exactly as the signer wrote it; nobody re-encodes it.
type Signed struct {
	T   string // type tag, such as "chain-entry"
	B   string // the body, as JSON text
	K   []byte // the signer's key, when Kid is empty (§3.2.2)
	Kid string // a server key ID, when K is empty (§5.1)
	S   []byte // the signature
}

type signedJSON struct {
	T   string `json:"t"`
	B   string `json:"b"`
	K   Bytes  `json:"k,omitempty"`
	Kid string `json:"kid,omitempty"`
	S   Bytes  `json:"s"`
}

// ErrEnvelope reports a signed envelope that breaks §4.3's shape.
var ErrEnvelope = errors.New("wire: malformed signed envelope")

// MarshalJSON encodes the envelope.
func (s Signed) MarshalJSON() ([]byte, error) {
	return Marshal(signedJSON{T: s.T, B: s.B, K: s.K, Kid: s.Kid, S: s.S})
}

// UnmarshalJSON decodes the envelope. The envelope is closed: any member
// other than t, b, k, kid, and s is an error, and exactly one of k and kid
// is present (§4.3).
func (s *Signed) UnmarshalJSON(data []byte) error {
	var m map[string]jsontext.Value
	if err := Unmarshal(data, &m); err != nil {
		return err
	}
	for name := range m {
		switch name {
		case "t", "b", "k", "kid", "s":
		default:
			return fmt.Errorf("%w: unexpected member %q", ErrEnvelope, name)
		}
	}
	_, hasK := m["k"]
	_, hasKid := m["kid"]
	if hasK == hasKid {
		return fmt.Errorf("%w: needs exactly one of k and kid", ErrEnvelope)
	}
	for _, need := range []string{"t", "b", "s"} {
		if _, ok := m[need]; !ok {
			return fmt.Errorf("%w: missing %q", ErrEnvelope, need)
		}
	}
	var j signedJSON
	if err := Unmarshal(data, &j); err != nil {
		return fmt.Errorf("%w: %v", ErrEnvelope, err)
	}
	if hasK && len(j.K) == 0 {
		return fmt.Errorf("%w: empty k", ErrEnvelope)
	}
	if hasKid && j.Kid == "" {
		return fmt.Errorf("%w: empty kid", ErrEnvelope)
	}
	*s = Signed{T: j.T, B: j.B, K: j.K, Kid: j.Kid, S: j.S}
	return nil
}

// ParseSigned decodes a signed envelope from JSON text.
func ParseSigned(data []byte) (Signed, error) {
	var s Signed
	err := s.UnmarshalJSON(data)
	return s, err
}

// SigInput returns the bytes a signature covers: JCS(["tieroute/v2", t, b])
// (§4.3).
func SigInput(t, b string) []byte {
	return MustJCS([]string{SignatureContext, t, b})
}

// SigInput returns the bytes s's signature covers.
func (s Signed) SigInput() []byte { return SigInput(s.T, s.B) }

// Canonical returns JCS(s), the bytes a signed object's hash covers.
func (s Signed) Canonical() []byte {
	b, err := s.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return MustJCS(b)
}

// Hash returns SHA-256(JCS(s)), the hash of a signed object (§4.3).
func (s Signed) Hash() [32]byte { return sha256.Sum256(s.Canonical()) }

// Body decodes the body text into v. It checks that b is valid I-JSON
// (§4.3) but doesn't re-check its form.
func (s Signed) Body(v any) error {
	return Unmarshal([]byte(s.B), v)
}

// BodyText returns JCS(body), the text a signer writes into b (§4.3).
func BodyText(body any) (string, error) {
	b, err := JCS(body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
