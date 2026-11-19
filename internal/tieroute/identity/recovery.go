package identity

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"math/big"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
)

// RecoveryWords are the 12 words of a recovery code. They're shown once at
// setup (§6.1), so the type keeps them out of logs: String and LogValue
// don't print them. Call Words to show them.
type RecoveryWords struct{ words []string }

// NewRecoveryWords wraps 12 words.
func NewRecoveryWords(words []string) RecoveryWords {
	return RecoveryWords{words: append([]string{}, words...)}
}

// Words returns the 12 words, in order.
func (w RecoveryWords) Words() []string { return append([]string{}, w.words...) }

// Empty reports whether there are no words.
func (w RecoveryWords) Empty() bool { return len(w.words) == 0 }

// String keeps the words out of formatted output.
func (w RecoveryWords) String() string { return "[recovery words]" }

// GoString keeps the words out of %#v output.
func (w RecoveryWords) GoString() string { return w.String() }

// LogValue keeps the words out of structured logs.
func (w RecoveryWords) LogValue() slog.Value { return slog.StringValue(w.String()) }

// PickPositions picks n distinct word positions (0 to 11) at random, for
// asking the user to type those words back (§6.1).
func (w RecoveryWords) PickPositions(n int) []int {
	if n > len(w.words) {
		n = len(w.words)
	}
	idx := make([]int, len(w.words))
	for i := range idx {
		idx[i] = i
	}
	for i := len(idx) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		k := int(j.Int64())
		idx[i], idx[k] = idx[k], idx[i]
	}
	out := idx[:n]
	for i := 1; i < len(out); i++ { // in reading order
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// CheckTyped reports whether typed[i] is the word at positions[i], trimmed
// and in any case.
func (w RecoveryWords) CheckTyped(positions []int, typed []string) bool {
	if len(positions) != len(typed) {
		return false
	}
	for i, p := range positions {
		if p < 0 || p >= len(w.words) || strings.ToLower(strings.TrimSpace(typed[i])) != w.words[p] {
			return false
		}
	}
	return true
}

// NewRecoveryCode makes a random 16-byte recovery code and its words.
func NewRecoveryCode() ([]byte, RecoveryWords, error) {
	code := make([]byte, CodeSize)
	if _, err := rand.Read(code); err != nil {
		return nil, RecoveryWords{}, err
	}
	words, err := EncodeWords(code)
	if err != nil {
		return nil, RecoveryWords{}, err
	}
	return code, NewRecoveryWords(words), nil
}

// Recovery derives the recovery keys from a recovery code (§6.1). Devices
// don't keep it after setup or recovery: neither prk nor anything it
// derives is ever stored.
type Recovery struct{ prk []byte }

// NewRecovery derives prk = HKDF-Extract("tieroute recovery", code).
func NewRecovery(code []byte) (*Recovery, error) {
	if len(code) != CodeSize {
		return nil, errors.New("identity: a recovery code is 16 bytes")
	}
	prk, err := hkdf.Extract(sha256.New, code, []byte("tieroute recovery"))
	if err != nil {
		return nil, err
	}
	return &Recovery{prk: prk}, nil
}

// RecoveryFromWords derives the recovery keys from the 12 words.
func RecoveryFromWords(words []string) (*Recovery, error) {
	code, err := DecodeWords(words)
	if err != nil {
		return nil, err
	}
	return NewRecovery(code)
}

func (r *Recovery) expand(info string, i uint32, n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], i)
	out, err := hkdf.Expand(sha256.New, r.prk, info+string(b[:]), n)
	if err != nil {
		panic(err)
	}
	return out
}

// Key returns recovery key i, for i = 1, 2, 3, …: Ed25519 from
// recovery_seed_i, and ML-DSA-65 from HKDF-Expand(prk, "signing-mldsa" ||
// uint32_be(i), 32) (§6.1).
func (r *Recovery) Key(i uint32) (*sig.CompositeKey, error) {
	if i == 0 {
		return nil, errors.New("identity: recovery keys start at 1")
	}
	return sig.NewCompositeKey(r.expand("signing-mldsa", i, 32), r.expand("signing", i, 32))
}

// Commitment returns the pre-rotation commitment to recovery key i: the
// SHA-256 of its 1,984-byte composite public key (§6.3).
func (r *Recovery) Commitment(i uint32) ([32]byte, error) {
	k, err := r.Key(i)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(k.Public()), nil
}

// WrapKey returns recovery_wrap = HKDF-Expand(prk, "account wrap", 16),
// which wraps the account key (§6.1, §7.4).
func (r *Recovery) WrapKey() []byte {
	out, err := hkdf.Expand(sha256.New, r.prk, "account wrap", 16)
	if err != nil {
		panic(err)
	}
	return out
}

// ErrNotCurrentCode reports recovery words that aren't the identity's
// current recovery code.
var ErrNotCurrentCode = errors.New("identity: these words aren't the current recovery code")

// Find returns the index of the recovery key the chain currently commits
// to. It tries i = 1, 2, … up to one more than the number of
// recovery-signed entries (§6.1).
func (r *Recovery) Find(st *State) (uint32, error) {
	limit := uint32(st.RecoveryEntries) + 1
	for i := uint32(1); i <= limit; i++ {
		c, err := r.Commitment(i)
		if err != nil {
			return 0, err
		}
		if c == st.RecoveryCommit {
			return i, nil
		}
	}
	return 0, ErrNotCurrentCode
}
