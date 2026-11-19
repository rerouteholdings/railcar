package identity

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

var identifierRE = regexp.MustCompile(`^tr:[a-z2-7]{24}$`)

var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// IdentifierOf returns an identity's identifier from its genesis entry:
// "tr:" and the lowercase base32 of the first 15 bytes of
// SHA-256(JCS(genesis)) (§6.2).
func IdentifierOf(genesis wire.Signed) string {
	h := genesis.Hash()
	return "tr:" + lowerBase32.EncodeToString(h[:15])
}

// ValidIdentifier reports whether s has the identifier's form (§4.2).
func ValidIdentifier(s string) bool { return identifierRE.MatchString(s) }

// ErrHandle reports a handle that breaks §5.4's syntax.
var ErrHandle = errors.New("identity: not a valid handle")

// validDNSName checks a lowercase DNS name: labels of 1 to 63 characters
// from [a-z0-9-] that don't start or end with "-", at most 253 characters
// in all, and at least minLabels labels (§5.4).
func validDNSName(s string, minLabels int) error {
	if len(s) == 0 || len(s) > 253 {
		return fmt.Errorf("%w: it has to be 1 to 253 characters", ErrHandle)
	}
	labels := strings.Split(s, ".")
	if len(labels) < minLabels {
		return fmt.Errorf("%w: it needs at least %d labels", ErrHandle, minLabels)
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 {
			return fmt.Errorf("%w: each label is 1 to 63 characters", ErrHandle)
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%w: a label can't start or end with a hyphen", ErrHandle)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%w: use lowercase letters, digits, and hyphens, with A-labels for international names", ErrHandle)
			}
		}
	}
	return nil
}

// CheckHandle checks a handle's syntax: a lowercase DNS name with at least
// two labels (§5.4). The leading "@" isn't part of a handle.
func CheckHandle(h string) error { return validDNSName(h, 2) }

// CheckServerName checks a server name: a lowercase DNS name, never an IP
// address literal (§4.2, §5.1).
func CheckServerName(s string) error {
	if err := validDNSName(s, 1); err != nil {
		return fmt.Errorf("identity: not a valid server name: %w", err)
	}
	if allDigitsAndDots(s) {
		return errors.New("identity: a server name can't be an IP address")
	}
	return nil
}

func allDigitsAndDots(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == '.') {
			return false
		}
	}
	return true
}

// NormalizeHandle turns what a user typed into a handle: it trims space,
// drops a leading "@", and lowercases ASCII letters. It doesn't convert
// Unicode names to A-labels; CheckHandle refuses those.
func NormalizeHandle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

// reservedLabels are the subdomain names a homeserver reserves, at least
// (§5.4).
var reservedLabels = map[string]bool{
	"admin": true, "administrator": true, "root": true, "system": true, "support": true,
	"help": true, "security": true, "abuse": true, "postmaster": true, "hostmaster": true,
	"webmaster": true, "www": true, "mail": true, "api": true, "staff": true, "mod": true,
	"moderator": true, "official": true, "verified": true, "tieroute": true, "reroute": true,
	"rerouted": true,
}

// ReservedLabel reports whether a homeserver has to refuse label as a
// subdomain handle (§5.4). A server may reserve more.
func ReservedLabel(label string) bool { return reservedLabels[label] }

// HashOf returns SHA-256 of b, the hash type of §4.2.
func HashOf(b []byte) [32]byte { return sha256.Sum256(b) }
