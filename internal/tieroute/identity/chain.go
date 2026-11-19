package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Chain op names (§6.3).
const (
	OpAddDevice    = "add-device"
	OpRemoveDevice = "remove-device"
	OpSetHome      = "set-home"
	OpSetHandle    = "set-handle"
	OpSetRecovery  = "set-recovery"
	OpMigratedFrom = "migrated-from"
)

// TypeChainEntry is the type tag of a signed chain entry.
const TypeChainEntry = "chain-entry"

// MaxDevices is the most devices an identity can have at once (§6.4 rule 4).
const MaxDevices = 10

// FutureSkew is how far in the future of a verifier's clock an entry's ts
// can be (§6.4 rule 5).
const FutureSkew = 10 * time.Minute

// Op is one chain op (§6.3). Which fields it uses depends on Op.
type Op struct {
	Op     string
	Key    []byte  // add-device, remove-device: the device key
	Pub    []byte  // add-device: the composite public key
	Name   string  // add-device: an optional device name
	Home   string  // set-home
	Handle *string // set-handle: nil is null, which clears the handle
	Next   []byte  // set-recovery: the commitment to the next recovery key
	ID     string  // migrated-from: the wire version 1 identity
}

// AddDevice returns an add-device op for a composite public key.
func AddDevice(pub []byte, name string) Op {
	return Op{Op: OpAddDevice, Key: append([]byte{}, pub[len(pub)-32:]...), Pub: pub, Name: name}
}

// RemoveDevice returns a remove-device op.
func RemoveDevice(key []byte) Op { return Op{Op: OpRemoveDevice, Key: key} }

// SetHome returns a set-home op.
func SetHome(home string) Op { return Op{Op: OpSetHome, Home: home} }

// SetHandle returns a set-handle op; an empty handle clears it (null).
func SetHandle(handle string) Op {
	if handle == "" {
		return Op{Op: OpSetHandle}
	}
	return Op{Op: OpSetHandle, Handle: &handle}
}

// SetRecovery returns a set-recovery op committing to the next recovery
// key.
func SetRecovery(next [32]byte) Op { return Op{Op: OpSetRecovery, Next: next[:]} }

// MarshalJSON writes the op with exactly the members its kind has.
func (o Op) MarshalJSON() ([]byte, error) {
	m := map[string]any{"op": o.Op}
	switch o.Op {
	case OpAddDevice:
		m["key"], m["pub"] = wire.Bytes(o.Key), wire.Bytes(o.Pub)
		if o.Name != "" {
			m["name"] = o.Name
		}
	case OpRemoveDevice:
		m["key"] = wire.Bytes(o.Key)
	case OpSetHome:
		m["home"] = o.Home
	case OpSetHandle:
		if o.Handle == nil {
			m["handle"] = nil
		} else {
			m["handle"] = *o.Handle
		}
	case OpSetRecovery:
		m["next"] = wire.Bytes(o.Next)
	case OpMigratedFrom:
		m["id"] = o.ID
	default:
		return nil, fmt.Errorf("identity: unknown chain op %q", o.Op)
	}
	return wire.Marshal(m)
}

// ErrUnknownOp reports a chain op this implementation doesn't know. Chain
// ops are a closed set, so the entry is invalid (unknown-kind).
var ErrUnknownOp = errors.New("identity: unknown chain op")

// UnmarshalJSON reads one op and checks its members' types.
func (o *Op) UnmarshalJSON(data []byte) error {
	var m map[string]jsontext.Value
	if err := wire.Unmarshal(data, &m); err != nil {
		return err
	}
	var name string
	if err := wire.Unmarshal(m["op"], &name); err != nil {
		return errors.New("identity: an op needs a string \"op\"")
	}
	out := Op{Op: name}
	str := func(field string, dst *string, required bool) error {
		v, ok := m[field]
		if !ok {
			if required {
				return fmt.Errorf("identity: %s needs %q", name, field)
			}
			return nil
		}
		return wire.Unmarshal(v, dst)
	}
	byt := func(field string, dst *[]byte, n int) error {
		var s string
		if err := str(field, &s, true); err != nil {
			return err
		}
		b, err := wire.DecodeB64Len(s, n)
		if err != nil {
			return fmt.Errorf("identity: %s %q: %w", name, field, err)
		}
		*dst = b
		return nil
	}
	var err error
	switch name {
	case OpAddDevice:
		if err = byt("key", &out.Key, 32); err == nil {
			if err = byt("pub", &out.Pub, sig.CompositePublicSize); err == nil {
				err = str("name", &out.Name, false)
			}
		}
	case OpRemoveDevice:
		err = byt("key", &out.Key, 32)
	case OpSetHome:
		err = str("home", &out.Home, true)
	case OpSetHandle:
		v, ok := m["handle"]
		if !ok {
			return errors.New("identity: set-handle needs \"handle\"")
		}
		if string(v) != "null" {
			var h string
			if err = wire.Unmarshal(v, &h); err == nil {
				out.Handle = &h
			}
		}
	case OpSetRecovery:
		err = byt("next", &out.Next, 32)
	case OpMigratedFrom:
		err = str("id", &out.ID, true)
	default:
		return fmt.Errorf("%w %q", ErrUnknownOp, name)
	}
	if err != nil {
		return err
	}
	*o = out
	return nil
}

// Entry is the body of a signed chain entry (§6.3).
type Entry struct {
	ID   string // absent only in genesis
	Seq  uint64
	Prev []byte // the hash of the previous signed entry; absent only in genesis
	TS   int64
	Ops  []Op
}

type entryJSON struct {
	ID   string     `json:"id,omitempty"`
	Seq  uint64     `json:"seq"`
	Prev wire.Bytes `json:"prev,omitempty"`
	TS   int64      `json:"ts"`
	Ops  []Op       `json:"ops"`
}

// MarshalJSON writes the entry body.
func (e Entry) MarshalJSON() ([]byte, error) {
	return wire.Marshal(entryJSON{ID: e.ID, Seq: e.Seq, Prev: e.Prev, TS: e.TS, Ops: e.Ops})
}

// UnmarshalJSON reads an entry body.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var j struct {
		ID   *string          `json:"id"`
		Seq  *uint64          `json:"seq"`
		Prev *wire.Bytes      `json:"prev"`
		TS   *int64           `json:"ts"`
		Ops  []jsontext.Value `json:"ops"`
	}
	if err := wire.Unmarshal(data, &j); err != nil {
		return err
	}
	if j.Seq == nil || j.TS == nil || j.Ops == nil {
		return errors.New("identity: a chain entry needs seq, ts, and ops")
	}
	if *j.TS < 0 {
		return errors.New("identity: a negative ts")
	}
	out := Entry{Seq: *j.Seq, TS: *j.TS}
	if j.ID != nil {
		out.ID = *j.ID
		if out.ID == "" {
			return errors.New("identity: an empty id")
		}
	}
	if j.Prev != nil {
		if len(*j.Prev) != 32 {
			return errors.New("identity: prev is a 32-byte hash")
		}
		out.Prev = *j.Prev
	}
	if len(j.Ops) == 0 {
		return errors.New("identity: a chain entry needs at least one op")
	}
	for _, raw := range j.Ops {
		var op Op
		if err := op.UnmarshalJSON(raw); err != nil {
			return err
		}
		out.Ops = append(out.Ops, op)
	}
	*e = out
	return nil
}

// DeviceInfo is one current device in a chain's state.
type DeviceInfo struct {
	Key      []byte // the device key: the Ed25519 component
	Pub      []byte // the composite public key
	Name     string
	AddedSeq uint64
}

// State is an identity's current state: the result of applying every op in
// order (§6.4).
type State struct {
	ID              string
	Seq             uint64 // the head's seq
	Head            [32]byte
	TS              int64 // the head's ts
	Devices         []DeviceInfo
	Home            string
	Handle          string // empty when the handle is null
	RecoveryCommit  [32]byte
	RecoveryEntries int // how many entries the recovery key signed
	LastRecoverySeq int64
	MigratedFrom    string
}

// Device returns the current device with the given key.
func (s *State) Device(key []byte) (DeviceInfo, bool) {
	for _, d := range s.Devices {
		if bytes.Equal(d.Key, key) {
			return d, true
		}
	}
	return DeviceInfo{}, false
}

// IsDevice reports whether key is a current device.
func (s *State) IsDevice(key []byte) bool {
	_, ok := s.Device(key)
	return ok
}

func (s *State) clone() State {
	c := *s
	c.Devices = append([]DeviceInfo{}, s.Devices...)
	return c
}

// EntryInfo is what validation learned about one entry.
type EntryInfo struct {
	Seq        uint64
	Hash       [32]byte
	TS         int64
	Recovery   bool   // signed by the recovery key
	Signer     []byte // the device key, or the revealed composite recovery key
	HomeBefore string // the home as of the previous entry; empty for genesis
	Body       Entry
}

// Chain is a validated identity chain.
type Chain struct {
	Entries []wire.Signed
	Info    []EntryInfo
	State   State
}

// ID returns the identity's identifier.
func (c *Chain) ID() string { return c.State.ID }

// Len returns the number of entries.
func (c *Chain) Len() int { return len(c.Entries) }

// Options adjust validation.
type Options struct {
	// Now is the verifier's clock, for the future check of §6.4 rule 5. The
	// zero time skips it, as judging a commit's bound history does (§7.1).
	Now time.Time
}

// ErrInvalidChain wraps every reason a chain fails validation.
var ErrInvalidChain = errors.New("identity: invalid chain")

func invalid(seq int, format string, args ...any) error {
	return fmt.Errorf("%w: entry %d: %s", ErrInvalidChain, seq, fmt.Sprintf(format, args...))
}

// Validate checks a whole chain, genesis first, by §6.4 rules 1 to 6. It
// doesn't check vouching (§6.5) or the past clock check that only an
// appending homeserver makes.
func Validate(entries []wire.Signed, opts Options) (*Chain, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: no entries", ErrInvalidChain)
	}
	c := &Chain{}
	for _, e := range entries {
		if err := c.Append(e, opts); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// ParseChain decodes a list of JSON signed entries and validates them.
func ParseChain(raw [][]byte, opts Options) (*Chain, error) {
	entries := make([]wire.Signed, len(raw))
	for i, r := range raw {
		s, err := wire.ParseSigned(r)
		if err != nil {
			return nil, invalid(i, "%v", err)
		}
		entries[i] = s
	}
	return Validate(entries, opts)
}

// Append validates one more entry against the chain and adds it. On error
// the chain is unchanged.
func (c *Chain) Append(s wire.Signed, opts Options) error {
	n := len(c.Entries)
	if s.T != TypeChainEntry {
		return invalid(n, "type %q isn't chain-entry", s.T)
	}
	if s.Kid != "" {
		return invalid(n, "a chain entry isn't signed by a server key")
	}
	var e Entry
	if err := s.Body(&e); err != nil {
		return invalid(n, "%v", err)
	}
	if e.Seq != uint64(n) {
		return invalid(n, "seq is %d", e.Seq)
	}
	hash := s.Hash()
	info := EntryInfo{Seq: e.Seq, Hash: hash, TS: e.TS, Body: e}
	var st State
	if n == 0 {
		if e.ID != "" || e.Prev != nil {
			return invalid(n, "genesis has no id or prev")
		}
		st = State{ID: IdentifierOf(s), LastRecoverySeq: -1}
		// Rule 1: genesis is signed by the device in its own add-device.
		var signer *Op
		for i := range e.Ops {
			if e.Ops[i].Op == OpAddDevice && bytes.Equal(e.Ops[i].Key, s.K) {
				signer = &e.Ops[i]
				break
			}
		}
		if signer == nil {
			return invalid(n, "genesis isn't signed by a device it adds")
		}
		if err := sig.VerifyDevice(s, signer.Pub); err != nil {
			return invalid(n, "signature: %v", err)
		}
		info.Signer = s.K
		have := map[string]bool{}
		for _, op := range e.Ops {
			have[op.Op] = true
		}
		for _, need := range []string{OpAddDevice, OpSetHome, OpSetHandle, OpSetRecovery} {
			if !have[need] {
				return invalid(n, "genesis has no %s", need)
			}
		}
	} else {
		prev := c.State
		st = prev.clone()
		// Rule 2: seq, prev, and id link the entry to the one before.
		if e.ID != prev.ID {
			return invalid(n, "id isn't the identifier")
		}
		if !bytes.Equal(e.Prev, prev.Head[:]) {
			return invalid(n, "prev isn't the hash of entry %d", n-1)
		}
		if e.TS < prev.TS {
			return invalid(n, "ts is before the previous entry's")
		}
		info.HomeBefore = prev.Home
		// Rule 3: a key authorized as of the previous entry signs it.
		switch len(s.K) {
		case 32:
			d, ok := prev.Device(s.K)
			if !ok {
				return invalid(n, "signed by a key that isn't a current device")
			}
			if err := sig.VerifyDevice(s, d.Pub); err != nil {
				return invalid(n, "signature: %v", err)
			}
			info.Signer = s.K
			for _, op := range e.Ops {
				switch op.Op {
				case OpAddDevice, OpRemoveDevice, OpSetHome, OpSetHandle:
				default:
					return invalid(n, "a device can't sign %s", op.Op)
				}
			}
		case sig.CompositePublicSize:
			if sha256.Sum256(s.K) != prev.RecoveryCommit {
				return invalid(n, "signed by a key that isn't the current recovery key")
			}
			if _, err := sig.VerifyRevealed(s); err != nil {
				return invalid(n, "signature: %v", err)
			}
			info.Recovery = true
			info.Signer = s.K
			found := false
			for _, op := range e.Ops {
				if op.Op == OpSetRecovery {
					found = true
					if [32]byte(op.Next) == prev.RecoveryCommit {
						return invalid(n, "set-recovery commits to the key that signs it")
					}
				}
			}
			if !found {
				return invalid(n, "a recovery-signed entry needs a new set-recovery")
			}
		default:
			return invalid(n, "k is neither a device key nor a composite key")
		}
	}
	// Rule 5: the future check, when the verifier has a clock.
	if !opts.Now.IsZero() && e.TS > opts.Now.Add(FutureSkew).Unix() {
		return invalid(n, "ts is more than 10 minutes in the future")
	}
	// Rules 4 and 6: apply the ops in order.
	migrated := 0
	for _, op := range e.Ops {
		switch op.Op {
		case OpAddDevice:
			if !sig.ValidPublicKey(op.Key) || !bytes.Equal(op.Pub[sig.MLDSAPublicSize:], op.Key) || !sig.ValidCompositePublic(op.Pub) {
				return invalid(n, "add-device has a bad key")
			}
			if st.IsDevice(op.Key) {
				return invalid(n, "add-device of a current device")
			}
			if len(st.Devices) >= MaxDevices {
				return invalid(n, "more than %d devices", MaxDevices)
			}
			st.Devices = append(st.Devices, DeviceInfo{Key: op.Key, Pub: op.Pub, Name: op.Name, AddedSeq: e.Seq})
		case OpRemoveDevice:
			i := -1
			for j, d := range st.Devices {
				if bytes.Equal(d.Key, op.Key) {
					i = j
				}
			}
			if i < 0 {
				return invalid(n, "remove-device of a key that isn't a current device")
			}
			st.Devices = append(st.Devices[:i], st.Devices[i+1:]...)
		case OpSetHome:
			if err := CheckServerName(op.Home); err != nil {
				return invalid(n, "set-home: %v", err)
			}
			st.Home = op.Home
		case OpSetHandle:
			if op.Handle == nil {
				st.Handle = ""
			} else {
				if err := CheckHandle(*op.Handle); err != nil {
					return invalid(n, "set-handle: %v", err)
				}
				st.Handle = *op.Handle
			}
		case OpSetRecovery:
			st.RecoveryCommit = [32]byte(op.Next)
		case OpMigratedFrom:
			if n != 0 {
				return invalid(n, "migrated-from appears only in genesis")
			}
			if migrated++; migrated > 1 {
				return invalid(n, "more than one migrated-from")
			}
			if !ValidIdentifier(op.ID) {
				return invalid(n, "migrated-from has a bad identifier")
			}
			st.MigratedFrom = op.ID
		}
	}
	if len(st.Devices) == 0 {
		return invalid(n, "no devices remain")
	}
	if info.Recovery {
		st.RecoveryEntries++
		st.LastRecoverySeq = int64(e.Seq)
	}
	st.Seq, st.Head, st.TS = e.Seq, hash, e.TS
	c.Entries = append(c.Entries, s)
	c.Info = append(c.Info, info)
	c.State = st
	return nil
}

// Prefix returns a validated copy of the chain's first n entries.
func (c *Chain) Prefix(n int) *Chain {
	out := &Chain{}
	for i := range n {
		// Entries already passed validation in this order, so this can't fail.
		if err := out.Append(c.Entries[i], Options{}); err != nil {
			panic(err)
		}
	}
	return out
}

// Next returns the body of the entry that would follow the head, with the
// given ops and ts.
func (c *Chain) Next(ts int64, ops ...Op) Entry {
	return Entry{ID: c.State.ID, Seq: c.State.Seq + 1, Prev: append([]byte{}, c.State.Head[:]...), TS: ts, Ops: ops}
}

// NewGenesis builds and signs a genesis entry for a new identity (§6.2): the
// first device, its home and handle, and the commitment to recovery key 1.
func NewGenesis(dev *Device, home, handle string, recovery1 [32]byte, ts int64) (wire.Signed, error) {
	e := Entry{Seq: 0, TS: ts, Ops: []Op{
		AddDevice(dev.Key.Public(), dev.Name),
		SetHome(home),
		SetHandle(handle),
		SetRecovery(recovery1),
	}}
	return dev.SignEntry(e)
}

// SignWithRecovery signs an entry with a recovery key, revealing its
// composite public key in k (§6.3).
func SignWithRecovery(key *sig.CompositeKey, e Entry) (wire.Signed, error) {
	return sig.SignRevealing(key, TypeChainEntry, e)
}

// SelfRotation reports whether an entry is a self-rotation: only a
// remove-device for K and an add-device for one new key, signed by K
// (§6.4.1).
func (i EntryInfo) SelfRotation() bool {
	if i.Recovery || len(i.Body.Ops) != 2 {
		return false
	}
	var removed, added bool
	for _, op := range i.Body.Ops {
		switch {
		case op.Op == OpRemoveDevice && bytes.Equal(op.Key, i.Signer):
			removed = true
		case op.Op == OpAddDevice:
			added = true
		}
	}
	return removed && added
}
