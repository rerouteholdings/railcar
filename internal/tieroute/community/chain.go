// Package community is the protocol side of Tieroute's community servers
// (§16): the community chain and its validation (§16.2), the state it
// gives, sealed names (§16.2.4), join codes and proofs (§16.6), and history
// segments (§16.5). It doesn't touch MLS; the chat layer does.
package community

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TypeEntry is the type tag of a community chain entry (§16.2.1).
const TypeEntry = "community-entry"

// Permissions (§16.3).
const (
	PermTalk         = "talk"
	PermInvite       = "invite"
	PermModerate     = "moderate"
	PermAdministrate = "administrate"
)

var allPerms = []string{PermTalk, PermInvite, PermModerate, PermAdministrate}

// Ops (§16.2.3).
const (
	OpCreate            = "create"
	OpSetName           = "set-name"
	OpSetSettings       = "set-settings"
	OpAddRole           = "add-role"
	OpEditRole          = "edit-role"
	OpRemoveRole        = "remove-role"
	OpAssignRoles       = "assign-roles"
	OpAddChannel        = "add-channel"
	OpEditChannel       = "edit-channel"
	OpRemoveChannel     = "remove-channel"
	OpOrderChannels     = "order-channels"
	OpAddCode           = "add-code"
	OpRevokeCode        = "revoke-code"
	OpBan               = "ban"
	OpUnban             = "unban"
	OpTimeout           = "timeout"
	OpTransferOwnership = "transfer-ownership"
	OpSetHome           = "set-home"
)

// opMembers lists the members each op may have, besides "op". Ops are
// closed: any other member makes the entry invalid (§16.2.2, §20.2).
var opMembers = map[string][]string{
	OpCreate:            {"server", "name", "desc", "icon", "public", "approval", "everyone"},
	OpSetName:           {"name", "desc", "icon"},
	OpSetSettings:       {"public", "approval"},
	OpAddRole:           {"id", "rank", "perms", "name", "color"},
	OpEditRole:          {"id", "rank", "perms", "name", "color"},
	OpRemoveRole:        {"id"},
	OpAssignRoles:       {"member", "roles"},
	OpAddChannel:        {"id", "name", "topic", "see", "write", "kind", "history", "slow", "max", "cast"},
	OpEditChannel:       {"id", "name", "topic", "see", "write", "history", "slow", "max", "cast"},
	OpRemoveChannel:     {"id"},
	OpOrderChannels:     {"ids"},
	OpAddCode:           {"key", "exp", "uses", "public"},
	OpRevokeCode:        {"code"},
	OpBan:               {"member", "domain"},
	OpUnban:             {"member", "domain"},
	OpTimeout:           {"member", "until"},
	OpTransferOwnership: {"to"},
	OpSetHome:           {"server"},
}

// Limits (§16.1, §16.2).
const (
	MaxOps       = 256
	MaxRoles     = 20
	MaxChannels  = 100
	MaxRank      = 1000
	MaxCodeUses  = 1000
	MaxSlow      = 21600
	MaxTimeout   = 604800
	MaxSealedCT  = 508
	MaxDesc      = 300
	MaxNameRunes = 64
	MaxTopic     = 120
	MaxIcon      = 262144
	// MaxClockAhead is how far ahead of the checker's clock an entry's
	// ts may be (§16.2.2).
	MaxClockAhead = 600
)

// Sealed is a name, topic, or color encrypted under a metadata key
// (§16.2.4).
type Sealed struct {
	Key wire.Bytes `json:"key"` // SHA-256 of the metadata key
	CT  wire.Bytes `json:"ct"`
}

// Icon names a community's icon by hash (§16.2.3).
type Icon struct {
	MIME string     `json:"mime"`
	Hash wire.Bytes `json:"hash"`
	Size uint64     `json:"size"`
}

// Op is one community chain op. Which fields mean anything depends on
// Op; Raw holds the members the op was sent with.
type Op struct {
	Op string `json:"op"`

	Server   string   `json:"server,omitempty"`
	Desc     *string  `json:"desc,omitempty"`
	Icon     *Icon    `json:"icon,omitempty"`
	Public   *bool    `json:"public,omitempty"`
	Approval *bool    `json:"approval,omitempty"`
	Everyone []string `json:"everyone,omitempty"`

	// Name is a tstr for create and set-name, and sealed for roles and
	// channels.
	Name  jsontext.Value `json:"name,omitempty"`
	ID    *uint64        `json:"id,omitempty"`
	Rank  *uint64        `json:"rank,omitempty"`
	Perms *[]string      `json:"perms,omitempty"`
	Color jsontext.Value `json:"color,omitempty"` // sealed, or null in edit-role

	Member string    `json:"member,omitempty"`
	Roles  *[]uint64 `json:"roles,omitempty"`

	Topic   jsontext.Value `json:"topic,omitempty"` // sealed, or null in edit-channel
	See     *[]uint64      `json:"see,omitempty"`
	Write   *[]uint64      `json:"write,omitempty"`
	Kind    string         `json:"kind,omitempty"`
	History *bool          `json:"history,omitempty"`
	Slow    *uint64        `json:"slow,omitempty"`
	Max     *uint64        `json:"max,omitempty"`
	Cast    *uint64        `json:"cast,omitempty"`
	IDs     []uint64       `json:"ids,omitempty"`

	Key  wire.Bytes `json:"key,omitempty"`
	Exp  *int64     `json:"exp,omitempty"`
	Uses *uint64    `json:"uses,omitempty"`
	Code wire.Bytes `json:"code,omitempty"`

	Domain string `json:"domain,omitempty"`
	Until  *int64 `json:"until,omitempty"`
	To     string `json:"to,omitempty"`

	raw map[string]jsontext.Value
}

// UnmarshalJSON decodes an op and keeps which members it had.
func (o *Op) UnmarshalJSON(data []byte) error {
	type plain Op
	var p plain
	if err := wire.Unmarshal(data, &p); err != nil {
		return err
	}
	var raw map[string]jsontext.Value
	if err := wire.Unmarshal(data, &raw); err != nil {
		return err
	}
	*o = Op(p)
	o.raw = raw
	return nil
}

// Has reports whether the op was sent with a member.
func (o *Op) Has(member string) bool {
	if o.raw == nil {
		return false
	}
	_, ok := o.raw[member]
	return ok
}

// NameText returns create's and set-name's readable name.
func (o *Op) NameText() string {
	var s string
	wire.Unmarshal(o.Name, &s)
	return s
}

// SealedName returns a role's or channel's sealed name.
func (o *Op) SealedName() *Sealed { return sealedOf(o.Name) }

// SealedTopic returns a channel's sealed topic, or nil.
func (o *Op) SealedTopic() *Sealed { return sealedOf(o.Topic) }

// SealedColor returns a role's sealed color, or nil.
func (o *Op) SealedColor() *Sealed { return sealedOf(o.Color) }

func sealedOf(v jsontext.Value) *Sealed {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	var s Sealed
	if wire.Unmarshal(v, &s) != nil {
		return nil
	}
	return &s
}

// Entry is the body of a community chain entry (§16.2.1).
type Entry struct {
	Community wire.Bytes `json:"community,omitempty"`
	Seq       uint64     `json:"seq"`
	Prev      wire.Bytes `json:"prev,omitempty"`
	TS        int64      `json:"ts"`
	By        string     `json:"by"`
	Ops       []Op       `json:"ops"`
}

var entryMembers = []string{"community", "seq", "prev", "ts", "by", "ops"}

// ParseEntry decodes a signed entry's body and checks its form.
func ParseEntry(s wire.Signed) (Entry, error) {
	var e Entry
	if s.T != TypeEntry {
		return e, fmt.Errorf("community: a %q, not a community entry", s.T)
	}
	var raw map[string]jsontext.Value
	if err := wire.Unmarshal([]byte(s.B), &raw); err != nil {
		return e, fmt.Errorf("community: the entry: %w", err)
	}
	for k := range raw {
		if !slices.Contains(entryMembers, k) {
			return e, fmt.Errorf("community: the entry has an unknown member %q", k)
		}
	}
	if err := s.Body(&e); err != nil {
		return e, fmt.Errorf("community: the entry: %w", err)
	}
	return e, nil
}

// Role is one role in the state (§16.3).
type Role struct {
	ID    uint64   `json:"id"`
	Rank  uint64   `json:"rank"`
	Perms []string `json:"perms"`
	Name  *Sealed  `json:"name,omitempty"`
	Color *Sealed  `json:"color,omitempty"`
	// NameSeq and ColorSeq are the seqs of the entries that sealed them,
	// part of their associated data (§16.2.4).
	NameSeq  uint64 `json:"name_seq"`
	ColorSeq uint64 `json:"color_seq"`
}

// Channel is one channel in the state (§16.4).
type Channel struct {
	ID       uint64   `json:"id"`
	Kind     string   `json:"kind"`
	Name     *Sealed  `json:"name,omitempty"`
	Topic    *Sealed  `json:"topic,omitempty"`
	NameSeq  uint64   `json:"name_seq"`
	TopicSeq uint64   `json:"topic_seq"`
	See      []uint64 `json:"see"`
	Write    []uint64 `json:"write"`
	History  bool     `json:"history,omitempty"`
	Slow     uint64   `json:"slow,omitempty"`
	Max      uint64   `json:"max,omitempty"`
	Cast     uint64   `json:"cast,omitempty"`
}

// Restricted reports whether the channel's see list isn't [0] (§16.2.3).
func (c *Channel) Restricted() bool { return !slices.Equal(c.See, []uint64{0}) }

// Code is a join code in the state (§16.2.3).
type Code struct {
	Key     wire.Bytes `json:"key"` // the composite public key
	Exp     int64      `json:"exp,omitempty"`
	Uses    uint64     `json:"uses,omitempty"`
	Public  bool       `json:"public,omitempty"`
	Creator string     `json:"creator"`
}

// State is a community's state after some prefix of its chain
// (§16.2.1).
type State struct {
	ID       wire.Bytes `json:"id"`
	Seq      uint64     `json:"seq"`
	Head     wire.Bytes `json:"head"`
	TS       int64      `json:"ts"`
	Name     string     `json:"name"`
	Desc     string     `json:"desc,omitempty"`
	Icon     *Icon      `json:"icon,omitempty"`
	Public   bool       `json:"public"`
	Approval bool       `json:"approval"`
	Owner    string     `json:"owner"`
	Server   string     `json:"server"`
	Everyone []string   `json:"everyone"`

	Roles       map[uint64]*Role    `json:"roles"`
	LastRole    uint64              `json:"last_role"`
	Holds       map[string][]uint64 `json:"holds"`
	Channels    map[uint64]*Channel `json:"channels"`
	Order       []uint64            `json:"order"`
	LastChannel uint64              `json:"last_channel"`
	Codes       map[string]*Code    `json:"codes"` // by the hex of their hash
	KeysUsed    map[string]bool     `json:"keys_used"`
	Bans        map[string]bool     `json:"bans"`
	DomainBans  map[string]bool     `json:"domain_bans"`
	Timeouts    map[string]int64    `json:"timeouts"`
	// History records when each text channel's history setting changed,
	// by entry ts, for answering history requests (§16.5).
	HistoryOff map[uint64][][2]int64 `json:"history_off,omitempty"`
}

func newState() *State {
	return &State{
		Roles: map[uint64]*Role{}, Holds: map[string][]uint64{}, Channels: map[uint64]*Channel{},
		Codes: map[string]*Code{}, KeysUsed: map[string]bool{}, Bans: map[string]bool{},
		DomainBans: map[string]bool{}, Timeouts: map[string]int64{},
	}
}

// Clone returns a deep copy.
func (s *State) Clone() *State {
	c := *s
	c.Everyone = slices.Clone(s.Everyone)
	c.Roles = map[uint64]*Role{}
	for k, r := range s.Roles {
		rr := *r
		rr.Perms = slices.Clone(r.Perms)
		c.Roles[k] = &rr
	}
	c.Holds = map[string][]uint64{}
	for k, v := range s.Holds {
		c.Holds[k] = slices.Clone(v)
	}
	c.Channels = map[uint64]*Channel{}
	for k, ch := range s.Channels {
		cc := *ch
		cc.See, cc.Write = slices.Clone(ch.See), slices.Clone(ch.Write)
		c.Channels[k] = &cc
	}
	c.Order = slices.Clone(s.Order)
	c.Codes = map[string]*Code{}
	for k, v := range s.Codes {
		vv := *v
		c.Codes[k] = &vv
	}
	c.KeysUsed = maps.Clone(s.KeysUsed)
	c.Bans = maps.Clone(s.Bans)
	c.DomainBans = maps.Clone(s.DomainBans)
	c.Timeouts = maps.Clone(s.Timeouts)
	c.HistoryOff = maps.Clone(s.HistoryOff)
	return &c
}

// Perms returns a member's permissions: the union of their roles', with
// everyone's, or all four for the owner (§16.2.1).
func (s *State) Perms(id string) []string {
	if id == s.Owner {
		return slices.Clone(allPerms)
	}
	set := map[string]bool{}
	for _, p := range s.Everyone {
		set[p] = true
	}
	for _, r := range s.Holds[id] {
		if role := s.Roles[r]; role != nil {
			for _, p := range role.Perms {
				set[p] = true
			}
		}
	}
	var out []string
	for _, p := range allPerms {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

// Can reports whether a member has a permission.
func (s *State) Can(id, perm string) bool { return slices.Contains(s.Perms(id), perm) }

// ownerLevel is above every rank.
const ownerLevel = MaxRank + 1

// Level returns a member's level: the highest rank of their roles, 0 for
// everyone alone, or above every rank for the owner (§16.2.1).
func (s *State) Level(id string) uint64 {
	if id == s.Owner {
		return ownerLevel
	}
	var l uint64
	for _, r := range s.Holds[id] {
		if role := s.Roles[r]; role != nil && role.Rank > l {
			l = role.Rank
		}
	}
	return l
}

// Outranks reports whether a signer outranks a member: the member's level
// is less than the signer's. Nobody outranks themselves or the owner.
func (s *State) Outranks(signer, member string) bool {
	if signer == member || member == s.Owner {
		return false
	}
	return s.Level(member) < s.Level(signer)
}

// SeesChannel reports whether a member's roles see a channel.
func (s *State) SeesChannel(id string, ch *Channel) bool {
	if slices.Contains(ch.See, 0) {
		return true
	}
	for _, r := range s.Holds[id] {
		if slices.Contains(ch.See, r) {
			return true
		}
	}
	return false
}

// SeesList reports whether a member holds a role in a see list.
func (s *State) SeesList(id string, see []uint64) bool {
	if slices.Contains(see, 0) {
		return true
	}
	for _, r := range s.Holds[id] {
		if slices.Contains(see, r) {
			return true
		}
	}
	return false
}

// Writes reports whether a member may write in a channel: a role in its
// write list and talk, and no timeout at now (§16.3, §16.7).
func (s *State) Writes(id string, ch *Channel, now int64) bool {
	if s.TimedOut(id, now) || !s.Can(id, PermTalk) {
		return false
	}
	if slices.Contains(ch.Write, 0) {
		return true
	}
	for _, r := range s.Holds[id] {
		if slices.Contains(ch.Write, r) {
			return true
		}
	}
	return false
}

// TimedOut reports whether a member is timed out at now.
func (s *State) TimedOut(id string, now int64) bool { return s.Timeouts[id] > now }

// Banned reports whether an identity is banned by identifier, or its
// homeserver by a domain ban. home may be empty when it isn't known.
func (s *State) Banned(id, home string) bool {
	if s.Bans[id] {
		return true
	}
	if home == "" || id == s.Owner {
		return false
	}
	for d := range s.DomainBans {
		if home == d || strings.HasSuffix(home, "."+d) {
			return true
		}
	}
	return false
}

// CodeHash names a code by the SHA-256 of its composite public key
// (§16.2.3).
func CodeHash(key []byte) []byte {
	h := sha256.Sum256(key)
	return h[:]
}

// FindCode returns the code with a composite public key, if it's in the
// state.
func (s *State) FindCode(key []byte) (*Code, bool) {
	c, ok := s.Codes[fmt.Sprintf("%x", CodeHash(key))]
	return c, ok
}

// OrderedChannels returns the channels in their order.
func (s *State) OrderedChannels() []*Channel {
	out := make([]*Channel, 0, len(s.Order))
	for _, id := range s.Order {
		if c := s.Channels[id]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// SeeLists returns each distinct see list of a restricted channel, as
// keys of SeeKey.
func (s *State) SeeLists() map[string][]uint64 {
	out := map[string][]uint64{}
	for _, c := range s.Channels {
		if c.Restricted() {
			out[SeeKey(c.See)] = slices.Clone(c.See)
		}
	}
	return out
}

// SeeKey names a see list.
func SeeKey(see []uint64) string { return fmt.Sprint(see) }

// Chain is a community chain whose entries have been checked.
type Chain struct {
	Entries []wire.Signed
	Hashes  [][32]byte
	states  []*State // the state after each entry
}

// ID returns the community's identifier: the hash of the create entry
// (§16.2.1).
func (c *Chain) ID() []byte {
	if len(c.Hashes) == 0 {
		return nil
	}
	return c.Hashes[0][:]
}

// Len returns the number of entries.
func (c *Chain) Len() int { return len(c.Entries) }

// State returns the state after the newest entry.
func (c *Chain) State() *State { return c.states[len(c.states)-1] }

// StateAt returns the state after the entry at seq.
func (c *Chain) StateAt(seq uint64) (*State, bool) {
	if seq >= uint64(len(c.states)) {
		return nil, false
	}
	return c.states[seq], true
}

// Find returns the seq of the entry whose hash is h.
func (c *Chain) Find(h []byte) (uint64, bool) {
	for i, x := range c.Hashes {
		if bytes.Equal(x[:], h) {
			return uint64(i), true
		}
	}
	return 0, false
}

// Head returns the hash of the newest entry.
func (c *Chain) Head() []byte { return c.Hashes[len(c.Hashes)-1][:] }

// Checker supplies what checking a chain needs from outside it.
type Checker struct {
	// DeviceKey returns the composite public key of a device an
	// add-device in an identity's chain added at some point (§16.2.2,
	// "Which device keys count"). ErrNoChain means the chain can't be
	// fetched yet.
	DeviceKey func(id string, key []byte) ([]byte, error)
	// Now is the checker's clock, in seconds since 1970.
	Now int64
}

// ErrNoChain reports an identity chain the checker can't get, so the
// entry can't be judged yet.
var ErrNoChain = errors.New("community: a signer's chain isn't available")

// ErrUnknownKind reports an unknown op, permission, or channel kind
// (§16.2.2).
var ErrUnknownKind = errors.New("community: an unknown op, permission, or channel kind")

// Validate checks a whole community chain, from its create entry.
func Validate(entries []wire.Signed, ck Checker) (*Chain, error) {
	c := &Chain{}
	for _, s := range entries {
		if err := c.Append(s, ck); err != nil {
			return c, err
		}
	}
	if len(c.Entries) == 0 {
		return nil, errors.New("community: an empty chain")
	}
	return c, nil
}

// Append checks one entry against the chain and appends it (§16.2.2). On
// error the chain is unchanged, and the caller accepts no entry built on
// the one refused.
func (c *Chain) Append(s wire.Signed, ck Checker) error {
	e, err := ParseEntry(s)
	if err != nil {
		return err
	}
	var prev *State
	if len(c.states) > 0 {
		prev = c.State()
	}
	// 1. Form.
	if len(e.Ops) == 0 || len(e.Ops) > MaxOps {
		return errors.New("community: an entry with no ops, or more than 256")
	}
	for i := range e.Ops {
		if err := checkForm(&e.Ops[i]); err != nil {
			return err
		}
	}
	// 2. Place.
	h := s.Hash()
	if prev == nil {
		if e.Seq != 0 || len(e.Community) != 0 || len(e.Prev) != 0 || len(e.Ops) != 1 || e.Ops[0].Op != OpCreate {
			return errors.New("community: the first entry isn't a create entry")
		}
	} else {
		if e.Seq != prev.Seq+1 || !bytes.Equal(e.Prev, prev.Head) || !bytes.Equal(e.Community, c.ID()) {
			return errors.New("community: an entry out of place")
		}
		for _, op := range e.Ops {
			if op.Op == OpCreate {
				return errors.New("community: create after the create entry")
			}
		}
	}
	// 3. Signature: k is a device key of by, at some point (§16.2.2).
	if !identity.ValidIdentifier(e.By) {
		return errors.New("community: a malformed signer")
	}
	pub, err := ck.DeviceKey(e.By, s.K)
	if err != nil {
		return err
	}
	if err := sig.VerifyDevice(s, pub); err != nil {
		return fmt.Errorf("community: the entry's signature: %w", err)
	}
	// 4. Time.
	if prev != nil && e.TS < prev.TS {
		return errors.New("community: an entry older than the one before it")
	}
	if ck.Now > 0 && e.TS > ck.Now+MaxClockAhead {
		return errors.New("community: an entry more than 10 minutes ahead")
	}
	// 5 and 6. Authority, on the state before the entry, and each op's
	// rules on the state as it applies.
	var st *State
	if prev == nil {
		st = newState()
	} else {
		st = prev.Clone()
		if st.Banned(e.By, "") {
			return errors.New("community: an entry by a banned identity")
		}
	}
	for i := range e.Ops {
		if err := apply(st, prev, &e, &e.Ops[i]); err != nil {
			return fmt.Errorf("community: entry %d, %s: %w", e.Seq, e.Ops[i].Op, err)
		}
	}
	st.Seq, st.Head, st.TS = e.Seq, wire.Bytes(h[:]), e.TS
	if prev == nil {
		st.ID = wire.Bytes(h[:])
	}
	c.Entries = append(c.Entries, s)
	c.Hashes = append(c.Hashes, h)
	c.states = append(c.states, st)
	return nil
}

// checkForm checks an op's members and value ranges (§16.2.2 step 1).
func checkForm(o *Op) error {
	allowed, ok := opMembers[o.Op]
	if !ok {
		return ErrUnknownKind
	}
	for k := range o.raw {
		if k != "op" && !slices.Contains(allowed, k) {
			return fmt.Errorf("community: %s has an unknown member %q", o.Op, k)
		}
	}
	perms := func(p []string) error {
		for _, x := range p {
			if !slices.Contains(allPerms, x) {
				return ErrUnknownKind
			}
		}
		if len(uniq(p)) != len(p) {
			return errors.New("community: a permission listed twice")
		}
		return nil
	}
	sealedOK := func(v jsontext.Value, nullable bool) error {
		if len(v) == 0 {
			return nil
		}
		if string(v) == "null" {
			if nullable {
				return nil
			}
			return errors.New("community: a null sealed value")
		}
		var s Sealed
		if err := wire.Unmarshal(v, &s); err != nil || len(s.Key) != 32 || len(s.CT) > MaxSealedCT {
			return errors.New("community: a malformed sealed value")
		}
		return nil
	}
	switch o.Op {
	case OpCreate:
		if o.Server == "" || !o.Has("name") || o.Public == nil || o.Approval == nil || !o.Has("everyone") {
			return errors.New("community: create needs server, name, public, approval, and everyone")
		}
		if err := perms(o.Everyone); err != nil {
			return err
		}
	case OpSetName:
		if !o.Has("name") {
			return errors.New("community: set-name needs name")
		}
	case OpSetSettings:
		if o.Public == nil || o.Approval == nil {
			return errors.New("community: set-settings needs public and approval")
		}
	case OpAddRole:
		if o.ID == nil || o.Rank == nil || o.Perms == nil || !o.Has("name") {
			return errors.New("community: add-role needs id, rank, perms, and name")
		}
	case OpEditRole:
		if o.ID == nil || len(o.raw) < 3 {
			return errors.New("community: edit-role needs id and something to change")
		}
	case OpRemoveRole, OpRemoveChannel:
		if o.ID == nil {
			return fmt.Errorf("community: %s needs id", o.Op)
		}
	case OpAssignRoles:
		if !identity.ValidIdentifier(o.Member) || o.Roles == nil {
			return errors.New("community: assign-roles needs member and roles")
		}
	case OpAddChannel:
		if o.ID == nil || !o.Has("name") || o.See == nil || o.Write == nil {
			return errors.New("community: add-channel needs id, name, see, and write")
		}
		switch o.Kind {
		case "text":
			if o.History == nil || o.Slow == nil || o.Has("max") || o.Has("cast") {
				return errors.New("community: a text channel needs history and slow, and nothing for voice")
			}
		case "voice":
			if o.Max == nil || o.Cast == nil || o.Has("history") || o.Has("slow") {
				return errors.New("community: a voice channel needs max and cast, and nothing for text")
			}
		default:
			return ErrUnknownKind
		}
	case OpEditChannel:
		if o.ID == nil || len(o.raw) < 3 {
			return errors.New("community: edit-channel needs id and something to change")
		}
	case OpOrderChannels:
		if len(o.IDs) == 0 {
			return errors.New("community: order-channels needs ids")
		}
	case OpAddCode:
		if !sig.ValidCompositePublic(o.Key) {
			return errors.New("community: add-code needs a composite key")
		}
		if o.Has("public") && (o.Public == nil || !*o.Public) {
			return errors.New("community: add-code's public is only ever true")
		}
	case OpRevokeCode:
		if len(o.Code) != 32 {
			return errors.New("community: revoke-code needs code")
		}
	case OpBan, OpUnban:
		if (o.Member == "") == (o.Domain == "") {
			return fmt.Errorf("community: %s needs member or domain", o.Op)
		}
		if o.Member != "" && !identity.ValidIdentifier(o.Member) {
			return errors.New("community: a malformed member")
		}
	case OpTimeout:
		if !identity.ValidIdentifier(o.Member) || o.Until == nil {
			return errors.New("community: timeout needs member and until")
		}
	case OpTransferOwnership:
		if !identity.ValidIdentifier(o.To) {
			return errors.New("community: transfer-ownership needs to")
		}
	case OpSetHome:
		if o.Server == "" {
			return errors.New("community: set-home needs server")
		}
	}
	if o.Perms != nil {
		if err := perms(*o.Perms); err != nil {
			return err
		}
	}
	if o.Op != OpCreate && o.Op != OpSetName {
		if err := sealedOK(o.Name, false); err != nil {
			return err
		}
	}
	if err := sealedOK(o.Topic, o.Op == OpEditChannel); err != nil {
		return err
	}
	if err := sealedOK(o.Color, o.Op == OpEditRole); err != nil {
		return err
	}
	return nil
}

// apply checks one op's authority, on the state before its entry (prev),
// and its rules, on the state as it applies (st), then applies it.
func apply(st, prev *State, e *Entry, o *Op) error {
	by := e.By
	can := func(perm string) bool { return prev != nil && prev.Can(by, perm) }
	level := uint64(0)
	if prev != nil {
		level = prev.Level(by)
	}
	signerPerms := []string{}
	if prev != nil {
		signerPerms = prev.Perms(by)
	}
	noAuthority := errors.New("the signer isn't allowed to")
	permsWithin := func(p []string) bool {
		for _, x := range p {
			if !slices.Contains(signerPerms, x) {
				return false
			}
		}
		return true
	}
	switch o.Op {
	case OpCreate:
		if !subset(o.Everyone, []string{PermTalk, PermInvite}) {
			return errors.New("everyone holds more than talk and invite")
		}
		name := o.NameText()
		if err := CheckName(name); err != nil {
			return err
		}
		if o.Desc != nil {
			if err := checkDesc(*o.Desc); err != nil {
				return err
			}
		}
		if err := checkIcon(o.Icon); err != nil {
			return err
		}
		if err := identity.CheckServerName(o.Server); err != nil {
			return err
		}
		st.Name, st.Desc, st.Icon = name, deref(o.Desc), o.Icon
		st.Public, st.Approval, st.Owner, st.Server = *o.Public, *o.Approval, by, o.Server
		st.Everyone = sortedPerms(o.Everyone)
	case OpSetName:
		if !can(PermAdministrate) {
			return noAuthority
		}
		name := o.NameText()
		if err := CheckName(name); err != nil {
			return err
		}
		if o.Desc != nil {
			if err := checkDesc(*o.Desc); err != nil {
				return err
			}
		}
		if err := checkIcon(o.Icon); err != nil {
			return err
		}
		st.Name, st.Desc, st.Icon = name, deref(o.Desc), o.Icon
	case OpSetSettings:
		if !can(PermAdministrate) {
			return noAuthority
		}
		st.Public, st.Approval = *o.Public, *o.Approval
		if !st.Public {
			for k, c := range st.Codes {
				if c.Public {
					delete(st.Codes, k)
				}
			}
		}
	case OpAddRole:
		if !can(PermAdministrate) || *o.Rank >= level {
			return noAuthority
		}
		if *o.ID != st.LastRole+1 {
			return errors.New("a role ID that isn't the next one")
		}
		if err := checkRank(st, *o.Rank, 0); err != nil {
			return err
		}
		if !permsWithin(*o.Perms) {
			return errors.New("a role with a permission the signer lacks")
		}
		if len(st.Roles) >= MaxRoles {
			return errors.New("more than 20 roles")
		}
		st.LastRole = *o.ID
		st.Roles[*o.ID] = &Role{ID: *o.ID, Rank: *o.Rank, Perms: sortedPerms(*o.Perms), Name: o.SealedName(), Color: o.SealedColor(), NameSeq: e.Seq, ColorSeq: e.Seq}
	case OpEditRole:
		if !can(PermAdministrate) {
			return noAuthority
		}
		if *o.ID == 0 {
			if o.Has("rank") || o.Has("name") || o.Has("color") || o.Perms == nil {
				return errors.New("everyone's role can change only its permissions")
			}
			if !subset(*o.Perms, []string{PermTalk, PermInvite}) {
				return errors.New("everyone holds more than talk and invite")
			}
			st.Everyone = sortedPerms(*o.Perms)
			return nil
		}
		r := st.Roles[*o.ID]
		if r == nil {
			return errors.New("no such role")
		}
		onlyLooks := !o.Has("rank") && !o.Has("perms")
		if !onlyLooks {
			newRank := r.Rank
			if o.Rank != nil {
				newRank = *o.Rank
			}
			if r.Rank >= level || newRank >= level {
				return noAuthority
			}
			if o.Rank != nil {
				if err := checkRank(st, *o.Rank, r.ID); err != nil {
					return err
				}
				r.Rank = *o.Rank
			}
			if o.Perms != nil {
				if !permsWithin(*o.Perms) {
					return errors.New("a role with a permission the signer lacks")
				}
				r.Perms = sortedPerms(*o.Perms)
			}
		}
		if o.Has("name") {
			r.Name, r.NameSeq = o.SealedName(), e.Seq
		}
		if o.Has("color") {
			r.Color, r.ColorSeq = o.SealedColor(), e.Seq
		}
	case OpRemoveRole:
		r := st.Roles[*o.ID]
		if r == nil || *o.ID == 0 {
			return errors.New("no such role")
		}
		if !can(PermAdministrate) || r.Rank >= level {
			return noAuthority
		}
		for _, c := range st.Channels {
			if slices.Equal(c.See, []uint64{r.ID}) || slices.Equal(c.Write, []uint64{r.ID}) {
				return errors.New("removing the role would leave a channel's see or write empty")
			}
		}
		delete(st.Roles, r.ID)
		for id, hs := range st.Holds {
			st.Holds[id] = slices.DeleteFunc(hs, func(x uint64) bool { return x == r.ID })
		}
		for _, c := range st.Channels {
			c.See = slices.DeleteFunc(c.See, func(x uint64) bool { return x == r.ID })
			c.Write = slices.DeleteFunc(c.Write, func(x uint64) bool { return x == r.ID })
		}
	case OpAssignRoles:
		roles := *o.Roles
		if !ascending(roles) {
			return errors.New("roles that aren't ascending, or listed twice")
		}
		for _, r := range roles {
			if r == 0 || st.Roles[r] == nil {
				return errors.New("a role that isn't current")
			}
		}
		cur := st.Holds[o.Member]
		ok := false
		switch {
		case o.Member == by && subset64(roles, cur):
			ok = true // removing one's own roles
		case len(roles) == 0 && can(PermModerate) && prev.Outranks(by, o.Member):
			ok = true // a kick
		case can(PermAdministrate) && (o.Member == by || prev.Outranks(by, o.Member)):
			ok = true
			for _, r := range symDiff(cur, roles) {
				if role := st.Roles[r]; role != nil && role.Rank >= level {
					ok = false
				}
			}
			for _, r := range roles {
				if !slices.Contains(cur, r) && !permsWithin(st.Roles[r].Perms) {
					ok = false
				}
			}
		}
		if !ok {
			return noAuthority
		}
		if len(roles) == 0 {
			delete(st.Holds, o.Member)
		} else {
			st.Holds[o.Member] = slices.Clone(roles)
		}
	case OpAddChannel:
		if !can(PermAdministrate) {
			return noAuthority
		}
		if *o.ID != st.LastChannel+1 {
			return errors.New("a channel ID that isn't the next one")
		}
		if err := checkRoleList(st, *o.See); err != nil {
			return err
		}
		if err := checkRoleList(st, *o.Write); err != nil {
			return err
		}
		if len(st.Channels) >= MaxChannels {
			return errors.New("more than 100 channels")
		}
		c := &Channel{ID: *o.ID, Kind: o.Kind, Name: o.SealedName(), Topic: o.SealedTopic(), NameSeq: e.Seq, TopicSeq: e.Seq,
			See: slices.Clone(*o.See), Write: slices.Clone(*o.Write)}
		if o.Kind == "text" {
			if *o.Slow > MaxSlow {
				return errors.New("slow mode over 21,600 s")
			}
			c.History, c.Slow = *o.History, *o.Slow
		} else {
			if *o.Max < 1 || *o.Max > 64 || *o.Cast < 1 || *o.Cast > 4 {
				return errors.New("a voice channel's max or cast out of range")
			}
			c.Max, c.Cast = *o.Max, *o.Cast
		}
		st.LastChannel = *o.ID
		st.Channels[c.ID] = c
		st.Order = append(st.Order, c.ID)
		if c.Kind == "text" && !c.History {
			st.historyOff(c.ID, e.TS)
		}
	case OpEditChannel:
		if !can(PermAdministrate) {
			return noAuthority
		}
		c := st.Channels[*o.ID]
		if c == nil {
			return errors.New("no such channel")
		}
		text := c.Kind == "text"
		if (o.Has("history") || o.Has("slow")) && !text || (o.Has("max") || o.Has("cast")) && text {
			return errors.New("a setting for the other kind of channel")
		}
		if o.See != nil {
			if err := checkRoleList(st, *o.See); err != nil {
				return err
			}
			if !slices.Equal(*o.See, c.See) && (!o.Has("name") || c.Topic != nil && !o.Has("topic")) {
				return errors.New("a change of see without the name and topic")
			}
			c.See = slices.Clone(*o.See)
		}
		if o.Write != nil {
			if err := checkRoleList(st, *o.Write); err != nil {
				return err
			}
			c.Write = slices.Clone(*o.Write)
		}
		if o.Has("name") {
			c.Name, c.NameSeq = o.SealedName(), e.Seq
		}
		if o.Has("topic") {
			c.Topic, c.TopicSeq = o.SealedTopic(), e.Seq
		}
		if o.History != nil {
			if c.History && !*o.History {
				st.historyOff(c.ID, e.TS)
			} else if !c.History && *o.History {
				st.historyOn(c.ID, e.TS)
			}
			c.History = *o.History
		}
		if o.Slow != nil {
			if *o.Slow > MaxSlow {
				return errors.New("slow mode over 21,600 s")
			}
			c.Slow = *o.Slow
		}
		if o.Max != nil {
			if *o.Max < 1 || *o.Max > 64 {
				return errors.New("a voice channel's max out of range")
			}
			c.Max = *o.Max
		}
		if o.Cast != nil {
			if *o.Cast < 1 || *o.Cast > 4 {
				return errors.New("a voice channel's cast out of range")
			}
			c.Cast = *o.Cast
		}
	case OpRemoveChannel:
		if !can(PermAdministrate) {
			return noAuthority
		}
		if st.Channels[*o.ID] == nil {
			return errors.New("no such channel")
		}
		delete(st.Channels, *o.ID)
		st.Order = slices.DeleteFunc(st.Order, func(x uint64) bool { return x == *o.ID })
	case OpOrderChannels:
		if !can(PermAdministrate) {
			return noAuthority
		}
		ids := slices.Clone(o.IDs)
		slices.Sort(ids)
		cur := slices.Sorted(maps.Keys(st.Channels))
		if !slices.Equal(ids, cur) {
			return errors.New("an order that isn't every channel once")
		}
		st.Order = slices.Clone(o.IDs)
	case OpAddCode:
		if !can(PermInvite) {
			return noAuthority
		}
		k := fmt.Sprintf("%x", CodeHash(o.Key))
		if st.KeysUsed[k] {
			return errors.New("a code key used before")
		}
		public := o.Public != nil && *o.Public
		if public && !st.Public {
			return errors.New("a public code while public codes are off")
		}
		if !public && (o.Exp == nil || o.Uses == nil || *o.Exp <= e.TS) {
			return errors.New("a private code needs a future exp and uses")
		}
		if o.Uses != nil && (*o.Uses < 1 || *o.Uses > MaxCodeUses) {
			return errors.New("uses out of range")
		}
		c := &Code{Key: slices.Clone(o.Key), Public: public, Creator: by}
		if o.Exp != nil {
			c.Exp = *o.Exp
		}
		if o.Uses != nil {
			c.Uses = *o.Uses
		}
		st.KeysUsed[k] = true
		st.Codes[k] = c
	case OpRevokeCode:
		k := fmt.Sprintf("%x", []byte(o.Code))
		c := st.Codes[k]
		if c == nil {
			return errors.New("no such code")
		}
		if c.Creator != by && !can(PermAdministrate) {
			return noAuthority
		}
		delete(st.Codes, k)
	case OpBan:
		if !can(PermAdministrate) {
			return noAuthority
		}
		if o.Member != "" {
			if o.Member == st.Owner || st.Bans[o.Member] {
				return errors.New("a ban of the owner, or of someone already banned")
			}
			if !prev.Outranks(by, o.Member) {
				return noAuthority
			}
			st.Bans[o.Member] = true
			delete(st.Holds, o.Member)
			delete(st.Timeouts, o.Member)
		} else {
			if err := identity.CheckServerName(o.Domain); err != nil {
				return err
			}
			st.DomainBans[o.Domain] = true
		}
	case OpUnban:
		if !can(PermAdministrate) {
			return noAuthority
		}
		if o.Member != "" {
			if !st.Bans[o.Member] {
				return errors.New("an unban of someone who isn't banned")
			}
			delete(st.Bans, o.Member)
		} else {
			if !st.DomainBans[o.Domain] {
				return errors.New("an unban of a domain that isn't banned")
			}
			delete(st.DomainBans, o.Domain)
		}
	case OpTimeout:
		if !can(PermModerate) || !prev.Outranks(by, o.Member) {
			return noAuthority
		}
		if *o.Until < e.TS || *o.Until > e.TS+MaxTimeout {
			return errors.New("a timeout outside 0 to 7 days")
		}
		if *o.Until == e.TS {
			delete(st.Timeouts, o.Member)
		} else {
			st.Timeouts[o.Member] = *o.Until
		}
	case OpTransferOwnership:
		if prev == nil || by != prev.Owner {
			return noAuthority
		}
		if o.To == st.Owner || st.Bans[o.To] {
			return errors.New("a transfer to the owner, or to someone banned")
		}
		st.Owner = o.To
	case OpSetHome:
		if prev == nil || by != prev.Owner {
			return noAuthority
		}
		if err := identity.CheckServerName(o.Server); err != nil {
			return err
		}
		if o.Server == st.Server {
			return errors.New("a set-home naming the current community server")
		}
		st.Server = o.Server
		st.Codes = map[string]*Code{}
	}
	return nil
}

func (s *State) historyOff(ch uint64, ts int64) {
	if s.HistoryOff == nil {
		s.HistoryOff = map[uint64][][2]int64{}
	}
	s.HistoryOff[ch] = append(slices.Clone(s.HistoryOff[ch]), [2]int64{ts, 0})
}

func (s *State) historyOn(ch uint64, ts int64) {
	spans := slices.Clone(s.HistoryOff[ch])
	if n := len(spans); n > 0 && spans[n-1][1] == 0 {
		spans[n-1][1] = ts
	}
	if s.HistoryOff == nil {
		s.HistoryOff = map[uint64][][2]int64{}
	}
	s.HistoryOff[ch] = spans
}

// HistoryWasOff reports whether a text channel's history was off at ts,
// by the entries that turned it off and on (§16.5).
func (s *State) HistoryWasOff(ch uint64, ts int64) bool {
	for _, sp := range s.HistoryOff[ch] {
		if ts >= sp[0] && (sp[1] == 0 || ts < sp[1]) {
			return true
		}
	}
	return false
}

func checkRank(st *State, rank, self uint64) error {
	if rank < 1 || rank > MaxRank {
		return errors.New("a rank out of range")
	}
	for _, r := range st.Roles {
		if r.ID != self && r.Rank == rank {
			return errors.New("two roles with one rank")
		}
	}
	return nil
}

func checkRoleList(st *State, l []uint64) error {
	if len(l) == 0 || !ascending(l) {
		return errors.New("a role list that's empty, out of order, or repeats a role")
	}
	if slices.Contains(l, 0) {
		if len(l) != 1 {
			return errors.New("a role list with everyone and other roles")
		}
		return nil
	}
	for _, r := range l {
		if st.Roles[r] == nil {
			return errors.New("a role list naming a role that isn't current")
		}
	}
	return nil
}

func checkIcon(i *Icon) error {
	if i == nil {
		return nil
	}
	switch i.MIME {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return errors.New("an icon of another type")
	}
	if len(i.Hash) != 32 || i.Size < 1 || i.Size > MaxIcon {
		return errors.New("a malformed icon")
	}
	return nil
}

func checkDesc(d string) error {
	if !utf8.ValidString(d) || !norm.NFC.IsNormalString(d) || utf8.RuneCountInString(d) > MaxDesc {
		return errors.New("a description over 300 characters, or not in NFC")
	}
	for _, r := range d {
		if r != '\n' && unicode.IsControl(r) || isBidiOverride(r) {
			return errors.New("a description with a control character or bidirectional override")
		}
	}
	return nil
}

// CheckName checks a channel's or community's name by §16.4's rules: 1
// to 64 characters in NFC, without control characters, bidirectional
// overrides, zero-width characters, Extended_Pictographic characters, or
// leading or trailing whitespace.
func CheckName(n string) error {
	c := utf8.RuneCountInString(n)
	if !utf8.ValidString(n) || c < 1 || c > MaxNameRunes || !norm.NFC.IsNormalString(n) {
		return errors.New("a name that isn't 1 to 64 characters in NFC")
	}
	if strings.TrimSpace(n) != n {
		return errors.New("a name with leading or trailing whitespace")
	}
	for _, r := range n {
		if unicode.IsControl(r) || isBidiOverride(r) || isZeroWidth(r) || isPictographic(r) {
			return errors.New("a name with a character names can't have")
		}
	}
	return nil
}

// CheckTopic checks a channel topic: at most 120 characters, otherwise as
// a name, and it may be empty.
func CheckTopic(t string) error {
	if t == "" {
		return nil
	}
	if utf8.RuneCountInString(t) > MaxTopic {
		return errors.New("a topic over 120 characters")
	}
	for _, r := range t {
		if unicode.IsControl(r) || isBidiOverride(r) {
			return errors.New("a topic with a control character or bidirectional override")
		}
	}
	return nil
}

func isBidiOverride(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x200E || r == 0x200F || r == 0x061C
}

func isZeroWidth(r rune) bool {
	return r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0xFEFF || r == 0x180E
}

// isPictographic approximates Unicode's Extended_Pictographic property,
// which Go's unicode package doesn't have, by its blocks.
func isPictographic(r rune) bool {
	switch {
	case r == 0x00A9 || r == 0x00AE || r == 0x203C || r == 0x2049 || r == 0x2122 || r == 0x2139:
		return true
	case r >= 0x2194 && r <= 0x21AA, r >= 0x231A && r <= 0x23FF, r >= 0x24C2 && r <= 0x24C2:
		return true
	case r >= 0x25AA && r <= 0x27BF, r >= 0x2934 && r <= 0x2935, r >= 0x2B05 && r <= 0x2B55:
		return true
	case r == 0x3030 || r == 0x303D || r == 0x3297 || r == 0x3299:
		return true
	case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x1FC00 && r <= 0x1FFFD:
		return true
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sortedPerms(p []string) []string {
	var out []string
	for _, x := range allPerms {
		if slices.Contains(p, x) {
			out = append(out, x)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func subset64(a, b []uint64) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func symDiff(a, b []uint64) []uint64 {
	var out []uint64
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) {
			out = append(out, x)
		}
	}
	return out
}

func ascending(l []uint64) bool {
	for i := 1; i < len(l); i++ {
		if l[i] <= l[i-1] {
			return false
		}
	}
	return true
}

func uniq(a []string) []string {
	out := slices.Clone(a)
	slices.Sort(out)
	return slices.Compact(out)
}
