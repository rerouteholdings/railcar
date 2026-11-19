// Package codes holds Tieroute's short codes: contact verification codes
// (§6.7) and the keys of community join codes (§16.6).
package codes

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// crockford is the Crockford base32 alphabet, without I, L, O, or U (§6.7).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Commit returns a verification round's commitment to a random 32-byte
// value r: SHA-256("tieroute code commit" || r) (§6.7).
func Commit(r []byte) [32]byte {
	return sha256.Sum256(append([]byte("tieroute code commit"), r...))
}

// Side is one side of a direct chat's verification round.
type Side struct {
	ID     string // the identity's identifier
	Head   []byte // its current chain-head hash
	Reveal []byte // its revealed 32-byte r
}

// ContactCode returns the verification code two contacts compare, and the
// digest it comes from (§6.7). epochAuth is the direct chat's MLS
// epoch_authenticator.
func ContactCode(a, b Side, epochAuth []byte) (string, [32]byte) {
	sides := []Side{a, b}
	slices.SortFunc(sides, func(x, y Side) int { return strings.Compare(x.ID, y.ID) })
	reveals := [][]byte{a.Reveal, b.Reveal}
	slices.SortFunc(reveals, bytes.Compare)
	transcript := []any{
		[]string{sides[0].ID, sides[1].ID},
		wire.EncodeB64(epochAuth),
		[]string{wire.EncodeB64(sides[0].Head), wire.EncodeB64(sides[1].Head)},
	}
	input := wire.MustJCS([]any{wire.SignatureContext, "code", "contact", transcript,
		[]string{wire.EncodeB64(reveals[0]), wire.EncodeB64(reveals[1])}})
	digest := sha256.Sum256(input)
	return Format(digest[:5]), digest
}

// Format writes 5 bytes as 8 Crockford base32 characters, XXXX-XXXX.
func Format(b []byte) string {
	var v uint64
	for _, c := range b[:5] {
		v = v<<8 | uint64(c)
	}
	out := make([]byte, 0, 9)
	for i := 7; i >= 0; i-- {
		out = append(out, crockford[v>>(5*uint(i))&31])
		if i == 4 {
			out = append(out, '-')
		}
	}
	return string(out)
}

// Argon2id parameters for passphrases and join codes (§3): m = 64 MiB,
// t = 3, p = 1.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 1
)

// ErrJoinCode reports a join code that isn't SERVER::SECRET with a
// 16-character secret.
var ErrJoinCode = errors.New("codes: not a community join code")

// ParseJoinCode splits a community join code into its community server's
// name and the 16 characters after "::" (§16.6).
func ParseJoinCode(code string) (server, secret string, err error) {
	server, secret, ok := strings.Cut(strings.TrimSpace(code), "::")
	if !ok || server == "" || len(secret) != 16 {
		return "", "", ErrJoinCode
	}
	return server, secret, nil
}

// CodeKey derives a join code's composite key (§16.6): code_prk is
// Argon2id over the secret, salted with the community's 32-byte
// identifier, and the two components come from HKDF-Expand(code_prk,
// "code-signing") and "code-signing-mldsa".
func CodeKey(secret string, communityID []byte) (*sig.CompositeKey, error) {
	prk := argon2.IDKey([]byte(secret), communityID, argonTime, argonMemory, argonThreads, 32)
	ed, err := hkdf.Expand(sha256.New, prk, "code-signing", 32)
	if err != nil {
		return nil, err
	}
	m, err := hkdf.Expand(sha256.New, prk, "code-signing-mldsa", 32)
	if err != nil {
		return nil, err
	}
	return sig.NewCompositeKey(m, ed)
}
