package chat

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Extension types Tieroute defines in MLS's private-use range. Both are
// provisional (§7.1, §11.2).
const (
	ExtGroup  uint16 = 0xF0A1 // tieroute_group, a GroupContext extension
	ExtMsgKey uint16 = 0xF0A2 // tieroute_msg_key, a leaf node extension
)

// Group kinds (§11.3).
const (
	KindDirect = "direct"
	KindGroup  = "group"
)

// MaxGroupExt is the largest encoded tieroute_group, and MaxBasis the most
// identities a basis names (§11.2).
const (
	MaxGroupExt = 640 << 10
	MaxBasis    = 26
)

// ChainRef names a chain entry: a group's floor for an identity (§7.1,
// §11.2).
type ChainRef struct {
	Seq  uint64     `json:"seq"`
	Hash wire.Bytes `json:"hash"`
	Rec  *uint64    `json:"rec,omitempty"`
}

// BasisRef is one identity's entry in a basis: the bound entry, and the
// tail of signed chain entries ending at it, oldest first (§7.1).
type BasisRef struct {
	Seq  uint64        `json:"seq"`
	Hash wire.Bytes    `json:"hash"`
	Rec  *uint64       `json:"rec,omitempty"`
	Tail []wire.Signed `json:"tail"`
}

// Ref returns the chain entry the basis binds.
func (b BasisRef) Ref() ChainRef { return ChainRef{Seq: b.Seq, Hash: b.Hash, Rec: b.Rec} }

// GroupExt is the tieroute_group extension (§11.2).
type GroupExt struct {
	Kind     string              `json:"kind"`
	Hub      string              `json:"hub"`
	Admins   []string            `json:"admins"`
	Since    map[string]uint64   `json:"since"`
	Replaces wire.Bytes          `json:"replaces,omitempty"`
	Used     map[string]int64    `json:"used,omitempty"`
	Approval *wire.Signed        `json:"approval,omitempty"`
	Heads    map[string]ChainRef `json:"heads,omitempty"`
	Basis    map[string]BasisRef `json:"basis,omitempty"`
}

// groupExtKeys are the members tieroute_group may have. The extension is
// closed: a client rejects one with any other member (§11.2, §20.2).
var groupExtKeys = []string{"kind", "hub", "admins", "since", "replaces", "used", "approval", "heads", "basis"}

// Encode returns the extension's data: JCS(tieroute-group).
func (g *GroupExt) Encode() ([]byte, error) {
	if g.Admins == nil {
		g.Admins = []string{}
	}
	if g.Since == nil {
		g.Since = map[string]uint64{}
	}
	b, err := wire.JCS(g)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxGroupExt {
		return nil, errors.New("chat: tieroute_group is over 640 KiB")
	}
	return b, nil
}

// Extension returns the extension as an MLS GroupContext extension.
func (g *GroupExt) Extension() (mls.Extension, error) {
	b, err := g.Encode()
	return mls.Extension{Type: ExtGroup, Data: b}, err
}

// ParseGroupExt decodes and checks tieroute_group.
func ParseGroupExt(data []byte) (*GroupExt, error) {
	if len(data) > MaxGroupExt {
		return nil, errors.New("chat: tieroute_group is over 640 KiB")
	}
	var members map[string]any
	if err := wire.Unmarshal(data, &members); err != nil {
		return nil, fmt.Errorf("chat: tieroute_group: %w", err)
	}
	for k := range members {
		if !slices.Contains(groupExtKeys, k) {
			return nil, fmt.Errorf("chat: tieroute_group has an unknown member %q", k)
		}
	}
	var g GroupExt
	if err := wire.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("chat: tieroute_group: %w", err)
	}
	if g.Kind != KindDirect && g.Kind != KindGroup {
		return nil, fmt.Errorf("chat: tieroute_group has kind %q", g.Kind)
	}
	if err := identity.CheckServerName(g.Hub); err != nil {
		return nil, fmt.Errorf("chat: tieroute_group's hub: %w", err)
	}
	if g.Kind == KindDirect && len(g.Admins) > 0 {
		return nil, errors.New("chat: a direct chat has admins")
	}
	if len(g.Basis) > MaxBasis {
		return nil, errors.New("chat: the basis names more than 26 identities")
	}
	for id, b := range g.Basis {
		if !identity.ValidIdentifier(id) || len(b.Hash) != 32 || len(b.Tail) == 0 || len(b.Tail) > identity.MaxTail {
			return nil, fmt.Errorf("chat: a malformed basis for %s", id)
		}
	}
	for id, h := range g.Heads {
		if !identity.ValidIdentifier(id) || len(h.Hash) != 32 {
			return nil, fmt.Errorf("chat: a malformed head for %s", id)
		}
	}
	if g.Since == nil {
		return nil, errors.New("chat: tieroute_group has no since")
	}
	return &g, nil
}

// Clone returns a deep enough copy to change for a new commit.
func (g *GroupExt) Clone() *GroupExt {
	c := *g
	c.Admins = slices.Clone(g.Admins)
	c.Since = maps.Clone(g.Since)
	c.Used = maps.Clone(g.Used)
	c.Heads = maps.Clone(g.Heads)
	c.Basis = maps.Clone(g.Basis)
	return &c
}

// sameRef reports whether two chain references name the same entry.
func sameRef(a, b ChainRef) bool {
	if a.Seq != b.Seq || !bytes.Equal(a.Hash, b.Hash) || (a.Rec == nil) != (b.Rec == nil) {
		return false
	}
	return a.Rec == nil || *a.Rec == *b.Rec
}

// boundOf converts a reference to the identity package's form.
func boundOf(r ChainRef) identity.Bound {
	return identity.Bound{Seq: r.Seq, Hash: r.Hash, Rec: r.Rec}
}

// basisFor builds an identity's basis from a validated chain: its head,
// and a tail of the entries since the newest recovery-signed one, at most
// the newest four (§7.1).
func basisFor(c *identity.Chain) BasisRef {
	head := c.Len() - 1
	start := 0
	if c.State.LastRecoverySeq >= 0 {
		start = int(c.State.LastRecoverySeq) + 1
	}
	if start > head {
		// The head is itself recovery-signed: carry it alone.
		start = head
	}
	if head-start+1 > identity.MaxTail {
		start = head - identity.MaxTail + 1
	}
	ref := BasisRef{Seq: uint64(head), Hash: wire.Bytes(c.Info[head].Hash[:]), Tail: slices.Clone(c.Entries[start : head+1])}
	if c.State.LastRecoverySeq >= 0 {
		r := uint64(c.State.LastRecoverySeq)
		ref.Rec = &r
	}
	return ref
}
