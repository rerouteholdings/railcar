package codes

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"strings"
)

// Crockford writes b as Crockford base32 (§6.7), most significant bit
// first. A last group shorter than 5 bits is padded with zero bits.
func Crockford(b []byte) string {
	var out strings.Builder
	var acc uint32
	bits := 0
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(crockford[acc>>uint(bits)&31])
		}
	}
	if bits > 0 {
		out.WriteByte(crockford[acc<<uint(5-bits)&31])
	}
	return out.String()
}

// NewConfirmation makes a random confirmation code for linking a device
// by a typed link code: 40 bits as XXXX-XXXX (§7.2 step 2).
func NewConfirmation() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Format(b), nil
}

// NormalizeConfirmation puts a typed confirmation code in the form a link
// proof carries: uppercase, with everything but A to Z and 0 to 9 dropped,
// O read as 0, and I and L read as 1 (client API, "Link code").
func NormalizeConfirmation(s string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == 'O':
			out.WriteByte('0')
		case r == 'I' || r == 'L':
			out.WriteByte('1')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
		}
	}
	return out.String()
}

// approvalLabel is the HMAC label of approval tags (§7.2.1).
const approvalLabel = "tieroute link approval"

// ApprovalTag is the code a new device puts in its link proof after
// reading an approval code (§7.2.1): HMAC-SHA-256(secret, "tieroute link
// approval" || k || hpke), its first 20 bytes as 32 characters of
// uppercase Crockford base32. k is the new device's composite public key,
// and hpke its 1,216-byte HPKE public key.
func ApprovalTag(secret, k, hpke []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(approvalLabel))
	m.Write(k)
	m.Write(hpke)
	return Crockford(m.Sum(nil)[:20])
}
