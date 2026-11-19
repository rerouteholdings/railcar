package identity

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// MaxTail is the most entries a basis tail holds (§7.1).
const MaxTail = 4

// Bound names a chain entry a commit binds, or a group's floor for an
// identity (§7.1).
type Bound struct {
	Seq  uint64
	Hash []byte
	Rec  *uint64 // the seq of the newest recovery-signed entry at or before it; nil if none
}

// Basis is one identity's entry in a commit's basis: the bound entry and the
// tail of signed entries ending at it (§7.1).
type Basis struct {
	Bound
	Tail []wire.Signed
}

// Verdict is how a member judges one identity's part of a commit (§7.1).
type Verdict int

const (
	// Valid: the bound history checks out.
	Valid Verdict = iota
	// Pending: the member can't get the entries before the tail. It doesn't
	// decide, and doesn't apply the commit or send to the group.
	Pending
	// Invalid: the evidence fails a rule. The verdict is final.
	Invalid
)

func (v Verdict) String() string { return [...]string{"valid", "pending", "invalid"}[v] }

// CheckKind is the one check a commit makes on an identity's bound state.
type CheckKind int

const (
	// MustBeDevice: the key is a device at the bound entry, such as an
	// added leaf, an approval's signer, or the committer.
	MustBeDevice CheckKind = iota
	// MustNotBeDevice: the key isn't a device, for a Remove by a member who
	// isn't an admin (§11.9).
	MustNotBeDevice
	// NoCheck: nothing beyond the history itself, for an identity a commit
	// names only to advance its head (§7.1). The caller can make its own
	// checks on the returned chain.
	NoCheck
)

// ErrBasis wraps every reason a basis is invalid.
var ErrBasis = errors.New("identity: invalid basis")

// JudgeBasis judges one identity's basis (§7.1). copy is the member's own
// copy of the chain, which may be nil, and floor is the group's floor for
// the identity, which may be nil. It returns the joined chain when the
// verdict is Valid.
func JudgeBasis(id string, b Basis, copy []wire.Signed, floor *Bound, kind CheckKind, key []byte) (Verdict, *Chain, error) {
	bad := func(format string, args ...any) (Verdict, *Chain, error) {
		return Invalid, nil, fmt.Errorf("%w: %s", ErrBasis, fmt.Sprintf(format, args...))
	}
	// 1. The tail: one to four linked entries ending at the bound entry.
	if len(b.Tail) == 0 || len(b.Tail) > MaxTail {
		return bad("a tail of %d entries", len(b.Tail))
	}
	bodies := make([]Entry, len(b.Tail))
	for i, s := range b.Tail {
		if err := s.Body(&bodies[i]); err != nil {
			return bad("tail entry %d: %v", i, err)
		}
		if i > 0 {
			prevHash := b.Tail[i-1].Hash()
			if bodies[i].Seq != bodies[i-1].Seq+1 || !bytes.Equal(bodies[i].Prev, prevHash[:]) {
				return bad("the tail isn't linked")
			}
		}
	}
	last := b.Tail[len(b.Tail)-1].Hash()
	if bodies[len(bodies)-1].Seq != b.Seq || !bytes.Equal(last[:], b.Hash) {
		return bad("the tail doesn't end at the bound entry")
	}
	// 2. Join the tail to the entries before it.
	start := bodies[0].Seq
	var joined []wire.Signed
	if start > 0 {
		if uint64(len(copy)) < start {
			return Pending, nil, nil
		}
		h := copy[start-1].Hash()
		if !bytes.Equal(h[:], bodies[0].Prev) {
			return Pending, nil, nil
		}
		joined = append(joined, copy[:start]...)
	}
	joined = append(joined, b.Tail...)
	// 3. Validate the history: §6.4 rules 1 to 4, ts non-decreasing, and no
	// other clock check or vouching.
	c, err := Validate(joined, Options{})
	if err != nil {
		return bad("%v", err)
	}
	if c.ID() != id {
		return bad("the history belongs to %s", c.ID())
	}
	rec := c.State.LastRecoverySeq
	switch {
	case rec < 0 && b.Rec != nil, rec >= 0 && (b.Rec == nil || *b.Rec != uint64(rec)):
		return bad("rec is wrong")
	}
	// The floor rule.
	if floor != nil {
		holds := floor.Seq < uint64(c.Len()) && bytes.Equal(c.Info[floor.Seq].Hash[:], floor.Hash)
		moved := rec >= 0 && (floor.Rec == nil || uint64(rec) > *floor.Rec)
		if !holds && !moved {
			return bad("the history is below the group's floor")
		}
	}
	// 4. The check on the bound state.
	switch kind {
	case MustBeDevice:
		if !c.State.IsDevice(key) {
			return bad("the key isn't a device at the bound entry")
		}
	case MustNotBeDevice:
		if c.State.IsDevice(key) {
			return bad("the key is still a device at the bound entry")
		}
		if floor == nil && !everDevice(c, key) {
			return bad("the key was never a device of the identity")
		}
	}
	return Valid, c, nil
}

// everDevice reports whether any entry in the chain added key.
func everDevice(c *Chain, key []byte) bool {
	for _, info := range c.Info {
		for _, op := range info.Body.Ops {
			if op.Op == OpAddDevice && bytes.Equal(op.Key, key) {
				return true
			}
		}
	}
	return false
}
