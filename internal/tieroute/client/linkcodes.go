package client

import (
	"encoding/base32"
	"errors"
	"strings"
	"unicode"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
)

// lowerBase32 is RFC 4648 base32 in lowercase, without padding, as link
// codes and approval codes write their bytes.
var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// groupsOfFour writes s in groups of four characters joined by "-".
func groupsOfFour(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+4, len(s))])
	}
	return b.String()
}

// alnum keeps only the letters and digits of s, in lowercase.
func alnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripPrefix removes "word-" or "word " from the start of s, once, in
// any case.
func stripPrefix(s, word string) (string, bool) {
	if len(s) > len(word) && strings.EqualFold(s[:len(word)], word) && (s[len(word)] == '-' || s[len(word)] == ' ') {
		return s[len(word)+1:], true
	}
	return s, false
}

// LinkCode is the code a new device shows for another device to approve
// (§7.2): its device key, the SHA-256 of its one-time HPKE public key,
// and its homeserver. A short-form code, from devices that predate link
// bundles, has only the device key.
type LinkCode struct {
	Device   []byte // the Ed25519 component of the new device's composite key
	HPKEHash []byte // SHA-256 of the HPKE public key; nil in the short form
	Home     string // "" in the short form
}

// Short reports whether the code is the short form, which gets no link
// bundle.
func (l LinkCode) Short() bool { return l.HPKEHash == nil }

// String writes the code as text: link-, the bytes in lowercase base32 in
// groups of four, then @ and the home (client API, "Link code").
func (l LinkCode) String() string {
	s := "link-" + groupsOfFour(lowerBase32.EncodeToString(append(append([]byte{}, l.Device...), l.HPKEHash...)))
	if l.Home != "" {
		s += "@" + l.Home
	}
	return s
}

// ErrLinkCode reports text that isn't a link code.
var ErrLinkCode = errors.New("client: that isn't a link code")

// ParseLinkCode reads a link code. Case doesn't matter, "link " with a
// space also works as the prefix, the home is after the last @, and only
// letters and digits count in the rest (client API, "Link code").
func ParseLinkCode(s string) (LinkCode, error) {
	s, ok := stripPrefix(strings.TrimSpace(s), "link")
	if !ok {
		return LinkCode{}, ErrLinkCode
	}
	rest, home := s, ""
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		rest, home = s[:i], strings.ToLower(strings.TrimSpace(s[i+1:]))
		if identity.CheckServerName(home) != nil {
			return LinkCode{}, ErrLinkCode
		}
	}
	b, err := lowerBase32.DecodeString(alnum(rest))
	if err != nil {
		return LinkCode{}, ErrLinkCode
	}
	switch {
	case len(b) == 64 && home != "":
		return LinkCode{Device: b[:32], HPKEHash: b[32:], Home: home}, nil
	case len(b) == 32:
		return LinkCode{Device: b, Home: home}, nil
	}
	return LinkCode{}, ErrLinkCode
}

// ApprovalCode is the code an existing device shows for a new device to
// read (§7.2.1): a 16-byte secret, the identity's handle, and its home.
type ApprovalCode struct {
	Secret []byte
	Handle string
	Home   string
}

// String writes the code as text: approve-, the secret in lowercase
// base32 in groups of four, then @ and the handle, and @ and the home
// (§7.2.1).
func (a ApprovalCode) String() string {
	return "approve-" + groupsOfFour(lowerBase32.EncodeToString(a.Secret)) + "@" + a.Handle + "@" + a.Home
}

// ErrApprovalCode reports text that isn't an approval code.
var ErrApprovalCode = errors.New("client: that isn't an approval code")

// ParseApprovalCode reads an approval code by the link code's rules: case
// doesn't matter, "approve " with a space also works as the prefix, the
// home is after the last @ and the handle after the @ before it, and only
// letters and digits count in the secret. A secret that isn't 16 bytes, or
// an empty handle or home, is refused (§7.2.1).
func ParseApprovalCode(s string) (ApprovalCode, error) {
	s, ok := stripPrefix(strings.TrimSpace(s), "approve")
	if !ok {
		return ApprovalCode{}, ErrApprovalCode
	}
	i := strings.LastIndexByte(s, '@')
	if i < 0 {
		return ApprovalCode{}, ErrApprovalCode
	}
	home := strings.ToLower(strings.TrimSpace(s[i+1:]))
	j := strings.LastIndexByte(s[:i], '@')
	if j < 0 {
		return ApprovalCode{}, ErrApprovalCode
	}
	handle := identity.NormalizeHandle(s[j+1 : i])
	if home == "" || handle == "" {
		return ApprovalCode{}, ErrApprovalCode
	}
	secret, err := lowerBase32.DecodeString(alnum(s[:j]))
	if err != nil || len(secret) != 16 {
		return ApprovalCode{}, ErrApprovalCode
	}
	return ApprovalCode{Secret: secret, Handle: handle, Home: home}, nil
}
