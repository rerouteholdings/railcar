// Package wire holds Tieroute's wire version 2 encoding: I-JSON bodies,
// base64url byte strings, JCS for signed and hashed bytes, the signed
// envelope, and the error format (§4).
package wire

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Version is the wire version this package speaks (§20.1).
const Version = 2

// SignatureContext is the first element of every signature input (§4.3).
const SignatureContext = "tieroute/v2"

// Subprotocol is the WebSocket subprotocol of the live stream (§20.1).
const Subprotocol = "tieroute.v2"

// MediaType is the content type of every JSON body (§4.1).
const MediaType = "application/tieroute+json"

var b64 = base64.RawURLEncoding.Strict()

// ErrBase64 reports a byte string that isn't canonical base64url.
var ErrBase64 = errors.New("wire: invalid base64url")

// EncodeB64 encodes bytes as base64url without padding (RFC 4648 §5).
func EncodeB64(b []byte) string { return b64.EncodeToString(b) }

// DecodeB64 decodes base64url without padding. It rejects padding,
// characters outside the alphabet, line breaks, and nonzero unused bits in
// the last character (§4.1).
func DecodeB64(s string) ([]byte, error) {
	// The standard decoder skips \r and \n, which §4.1 doesn't allow.
	if strings.ContainsAny(s, "\r\n=") {
		return nil, ErrBase64
	}
	b, err := b64.DecodeString(s)
	if err != nil {
		return nil, ErrBase64
	}
	return b, nil
}

// DecodeB64Len decodes base64url and checks the decoded length.
func DecodeB64Len(s string, n int) ([]byte, error) {
	b, err := DecodeB64(s)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("wire: want %d bytes, got %d", n, len(b))
	}
	return b, nil
}

// Bytes is a byte string carried in JSON as base64url without padding.
type Bytes []byte

// MarshalJSON encodes b as a base64url JSON string.
func (b Bytes) MarshalJSON() ([]byte, error) {
	return []byte(`"` + EncodeB64(b) + `"`), nil
}

// UnmarshalJSON decodes a base64url JSON string strictly.
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var s string
	if err := Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := DecodeB64(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// String returns the base64url form.
func (b Bytes) String() string { return EncodeB64(b) }
