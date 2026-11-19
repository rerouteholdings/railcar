package community

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TypeJoinProof is the type tag of a join proof (§16.6).
const TypeJoinProof = "join-proof"

// JoinProof is the body of a join proof: the external commit's hash, the
// joiner, and the members group's epoch the commit is built on (§16.6).
type JoinProof struct {
	Commit wire.Bytes `json:"commit"`
	ID     string     `json:"id"`
	Epoch  uint64     `json:"epoch"`
}

// SignJoinProof signs a join proof with a code's composite key. The
// envelope's k is the whole composite public key (§16.6).
func SignJoinProof(key *sig.CompositeKey, p JoinProof) (wire.Signed, error) {
	return sig.SignRevealing(key, TypeJoinProof, p)
}

// VerifyJoinProof checks a join proof's signature, and returns its body
// and the code key it was signed with.
func VerifyJoinProof(s wire.Signed) (JoinProof, []byte, error) {
	var p JoinProof
	if s.T != TypeJoinProof {
		return p, nil, fmt.Errorf("community: a %q, not a join proof", s.T)
	}
	pub, err := sig.VerifyRevealed(s)
	if err != nil {
		return p, nil, fmt.Errorf("community: the join proof's signature: %w", err)
	}
	if err := s.Body(&p); err != nil {
		return p, nil, err
	}
	if len(p.Commit) != 32 {
		return p, nil, errors.New("community: a malformed join proof")
	}
	return p, pub, nil
}

// CodeLive reports whether a code is live at a time: in the state,
// before its exp, under its uses (given how many joins used it), and its
// creator not banned. Whether the creator is still a member is the
// caller's to check, against the members group (§16.2.3).
func (s *State) CodeLive(key []byte, now int64, used uint64) bool {
	c, ok := s.FindCode(key)
	if !ok || !bytes.Equal(c.Key, key) {
		return false
	}
	if c.Exp != 0 && now >= c.Exp {
		return false
	}
	if c.Uses != 0 && used >= c.Uses {
		return false
	}
	return !s.Bans[c.Creator]
}
