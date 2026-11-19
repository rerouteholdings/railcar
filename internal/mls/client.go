// Package mls is Railcar's MLS layer: Go bindings to tieroute-mls, which
// wraps OpenMLS with Tieroute's post-quantum ciphersuite (§3, §11.1).
//
// The package does MLS only. Tieroute's rules, such as which commits a
// group accepts, what an extension means, and which extensions a leaf
// lists, belong to its callers. Credential identities and extensions go
// in and out as bytes.
//
// A Client holds one device's MLS state: its key packages and its groups.
// The state lives in memory. The caller saves it, either whole with
// Snapshot or as the entries that changed with TakeChanges, and encrypts
// it at rest. Revert goes back to the last save, which also undoes
// decryptions, so a refused commit can be processed again later (§11.9).
package mls

/*
#include "tieroute_mls.h"
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"runtime"
	"sort"
	"sync"
)

// Ciphersuite is the one MLS ciphersuite Tieroute uses (§11.1), at
// OpenMLS's provisional code point.
const Ciphersuite uint16 = 0x004F

// CiphersuiteName is the ciphersuite's name in draft-ietf-mls-pq-ciphersuites.
const CiphersuiteName = "MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519"

// LibraryCiphersuite returns the code point the linked library uses. It's
// always Ciphersuite.
func LibraryCiphersuite() uint16 {
	return ciphersuite()
}

var errClosed = &Error{Code: ErrBadInput, Message: "the handle is closed"}

// Signer is an Ed25519 signing key. In Tieroute it's a device's message
// key, which signs its MLS leaves and messages (§3.2.3).
type Signer struct {
	p   *C.TrmlsSigner
	pub []byte
}

// NewSigner makes a signer from an Ed25519 private key: the 32-byte seed,
// or a 64-byte crypto/ed25519 private key.
func NewSigner(privateKey []byte) (*Signer, error) {
	if len(privateKey) == 64 {
		privateKey = privateKey[:32]
	}
	var sp *C.TrmlsSigner
	p, n := in(privateKey)
	if _, err := call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_signer_new(p, n, &sp, out) }); err != nil {
		return nil, err
	}
	s := &Signer{p: sp}
	pub, err := call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_signer_public_key(s.p, out) })
	if err != nil {
		C.trmls_signer_free(s.p)
		return nil, err
	}
	s.pub = pub
	runtime.SetFinalizer(s, (*Signer).Close)
	return s, nil
}

// PublicKey returns the 32-byte Ed25519 public key.
func (s *Signer) PublicKey() []byte {
	return bytes.Clone(s.pub)
}

// Close frees the signer. Using it afterwards is an error.
func (s *Signer) Close() {
	if s.p != nil {
		C.trmls_signer_free(s.p)
		s.p = nil
	}
	runtime.SetFinalizer(s, nil)
}

func (s *Signer) handle() (*C.TrmlsSigner, error) {
	if s == nil || s.p == nil {
		return nil, errClosed
	}
	return s.p, nil
}

// Client is one device's MLS state. Its methods, and its groups' methods,
// are safe to call from several goroutines.
type Client struct {
	mu sync.Mutex
	p  *C.TrmlsClient
}

// Open opens a client from a snapshot that Snapshot returned. A nil or
// empty snapshot makes a new client.
func Open(snapshot []byte) (*Client, error) {
	var cp *C.TrmlsClient
	p, n := in(snapshot)
	if _, err := call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_client_open(p, n, &cp, out) }); err != nil {
		return nil, err
	}
	c := &Client{p: cp}
	runtime.SetFinalizer(c, (*Client).Close)
	return c, nil
}

// OpenEntries opens a client from the entries a caller kept with
// TakeChanges.
func OpenEntries(entries map[string][]byte) (*Client, error) {
	return Open(EncodeSnapshot(entries))
}

// Close frees the client. Unsaved changes are lost.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p != nil {
		C.trmls_client_free(c.p)
		c.p = nil
	}
	runtime.SetFinalizer(c, nil)
}

// with runs f with the client locked.
func (c *Client) with(f func(p *C.TrmlsClient) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p == nil {
		return nil, errClosed
	}
	return f(c.p)
}

// Snapshot returns the client's whole state. It's secret: it holds every
// group's keys. The state now becomes the point Revert returns to.
func (c *Client) Snapshot() ([]byte, error) {
	return c.with(func(p *C.TrmlsClient) ([]byte, error) {
		return call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_client_snapshot(p, out) })
	})
}

// TakeChanges returns the entries that changed since the last Snapshot or
// TakeChanges. The state now becomes the point Revert returns to. Values
// are secret.
func (c *Client) TakeChanges() ([]Change, error) {
	b, err := c.with(func(p *C.TrmlsClient) ([]byte, error) {
		return call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_client_take_changes(p, out) })
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Changes []struct {
			Key   []byte          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	out := make([]Change, 0, len(res.Changes))
	for _, ch := range res.Changes {
		if string(ch.Value) == "null" {
			out = append(out, Change{Key: ch.Key, Deleted: true})
			continue
		}
		var v []byte
		if err := json.Unmarshal(ch.Value, &v); err != nil {
			return nil, err
		}
		if v == nil {
			v = []byte{}
		}
		out = append(out, Change{Key: ch.Key, Value: v})
	}
	return out, nil
}

// Revert undoes everything since the last Snapshot or TakeChanges:
// commits made or staged, proposals queued, and keys used up by
// decrypting.
func (c *Client) Revert() error {
	_, err := c.with(func(p *C.TrmlsClient) ([]byte, error) {
		return call(func(out *C.TrmlsBuf) C.int32_t { return C.trmls_client_revert(p, out) })
	})
	return err
}

// signed runs a library function that takes a JSON request and a signer.
func (c *Client) signed(s *Signer, req, res any, f func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	sp, err := s.handle()
	if err != nil {
		return nil, err
	}
	defer runtime.KeepAlive(s)
	return c.with(func(p *C.TrmlsClient) ([]byte, error) {
		return callJSON(req, res, func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t { return f(p, sp, r, n, out) })
	})
}

// plain runs a library function that takes a JSON request.
func (c *Client) plain(req, res any, f func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	return c.with(func(p *C.TrmlsClient) ([]byte, error) {
		return callJSON(req, res, func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t { return f(p, r, n, out) })
	})
}

// raw runs a library function that takes raw bytes.
func (c *Client) raw(b []byte, f func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	return c.with(func(p *C.TrmlsClient) ([]byte, error) {
		r, n := in(b)
		return call(func(out *C.TrmlsBuf) C.int32_t { return f(p, r, n, out) })
	})
}

// NewKeyPackage makes a key package and keeps its private keys. A
// last-resort key package stays after a Welcome uses it (§11.4).
func (c *Client) NewKeyPackage(s *Signer, opts KeyPackageOptions) (*KeyPackage, error) {
	req := struct {
		Leaf       Leaf        `json:"leaf"`
		LastResort bool        `json:"last_resort"`
		Lifetime   uint64      `json:"lifetime"`
		Extensions []Extension `json:"extensions,omitempty"`
	}{opts.Leaf, opts.LastResort, opts.Lifetime, opts.Extensions}
	var kp KeyPackage
	if _, err := c.signed(s, req, &kp, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_key_package(p, s, r, n, out)
	}); err != nil {
		return nil, err
	}
	return &kp, nil
}

// DeleteKeyPackage deletes a key package's private keys by its hash
// reference, such as a last-resort one being replaced.
func (c *Client) DeleteKeyPackage(hashRef []byte) error {
	_, err := c.raw(hashRef, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_delete_key_package(p, r, n, out)
	})
	return err
}

// CreateGroup creates a group with this device as its only member, in
// epoch 0.
func (c *Client) CreateGroup(s *Signer, opts CreateOptions) (*Group, error) {
	if _, err := c.signed(s, opts, nil, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_create_group(p, s, r, n, out)
	}); err != nil {
		return nil, err
	}
	return c.Group(opts.GroupID), nil
}

// JoinWelcome joins a group from a Welcome. ratchetTree is needed when the
// Welcome carries no ratchet tree. The caller checks the members (§7.1)
// and deletes the group if it refuses the Welcome.
func (c *Client) JoinWelcome(welcome, ratchetTree []byte, cfg Config) (*Group, *WelcomeResult, error) {
	req := struct {
		Welcome     []byte `json:"welcome"`
		RatchetTree []byte `json:"ratchet_tree"`
		Config      Config `json:"config"`
	}{welcome, ratchetTree, cfg}
	var res WelcomeResult
	if _, err := c.plain(req, &res, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_join_welcome(p, r, n, out)
	}); err != nil {
		return nil, nil, err
	}
	return c.Group(res.GroupID), &res, nil
}

// JoinExternal joins a group by an external commit (RFC 9420 §12.4.3.2),
// as community code joins do (§16.6). The group is in the new epoch at
// once. If the hub refuses the commit, the caller reverts or deletes the
// group.
func (c *Client) JoinExternal(s *Signer, opts ExternalJoinOptions) (*Group, *CommitResult, error) {
	var res struct {
		GroupID []byte `json:"group_id"`
		CommitResult
	}
	if _, err := c.signed(s, opts, &res, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_join_external(p, s, r, n, out)
	}); err != nil {
		return nil, nil, err
	}
	return c.Group(res.GroupID), &res.CommitResult, nil
}

// Group returns a handle to a group by its ID. It doesn't check that the
// group exists: its methods return ErrNotFound if it doesn't.
func (c *Client) Group(id []byte) *Group {
	return &Group{c: c, id: bytes.Clone(id)}
}

// InspectKeyPackage checks a key package's signature, lifetime, and
// ciphersuite, and describes it. It accepts the KeyPackage with or
// without the MLSMessage wrapper.
func InspectKeyPackage(b []byte) (*KeyPackageInfo, error) {
	var info KeyPackageInfo
	if err := inspect(b, &info, func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_inspect_key_package(r, n, out)
	}); err != nil {
		return nil, err
	}
	return &info, nil
}

// InspectGroupInfo reads a GroupInfo without verifying it.
func InspectGroupInfo(b []byte) (*GroupInfoSummary, error) {
	var info GroupInfoSummary
	if err := inspect(b, &info, func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_inspect_group_info(r, n, out)
	}); err != nil {
		return nil, err
	}
	return &info, nil
}

// InspectMessage reads an MLSMessage's unencrypted header: its wire
// format, and for public and private messages its group, epoch, and
// content type. Callers use it to buffer messages by epoch (§11.6).
func InspectMessage(b []byte) (*MessageInfo, error) {
	var info MessageInfo
	if err := inspect(b, &info, func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_inspect_message(r, n, out)
	}); err != nil {
		return nil, err
	}
	return &info, nil
}

func inspect(b []byte, res any, f func(r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) error {
	r, n := in(b)
	out, err := call(func(o *C.TrmlsBuf) C.int32_t { return f(r, n, o) })
	if err != nil {
		return err
	}
	return json.Unmarshal(out, res)
}

// snapshotMagic starts every snapshot, with the format's version.
var snapshotMagic = []byte("trmls\x00\x00\x01")

// EncodeSnapshot builds a snapshot from store entries: the magic, a count,
// then each entry's key length, value length, key, and value, with 64-bit
// big-endian lengths, sorted by key.
func EncodeSnapshot(entries map[string][]byte) []byte {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := append([]byte(nil), snapshotMagic...)
	b = binary.BigEndian.AppendUint64(b, uint64(len(keys)))
	for _, k := range keys {
		v := entries[k]
		b = binary.BigEndian.AppendUint64(b, uint64(len(k)))
		b = binary.BigEndian.AppendUint64(b, uint64(len(v)))
		b = append(b, k...)
		b = append(b, v...)
	}
	return b
}

// IsCode reports whether err is a library error with the given code.
func IsCode(err error, code ErrorCode) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
