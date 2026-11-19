package mls

/*
#include "tieroute_mls.h"
*/
import "C"

import (
	"bytes"
	"runtime"
)

// Group is a handle to one group in a client.
type Group struct {
	c  *Client
	id []byte
}

// ID returns the group's ID.
func (g *Group) ID() []byte {
	return bytes.Clone(g.id)
}

func (g *Group) raw(f func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	return g.c.raw(g.id, f)
}

// State returns the group's epoch, members, extensions, and pending
// proposals.
func (g *Group) State() (*State, error) {
	var s State
	if err := g.jsonRaw(&s, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_group_info(p, r, n, out)
	}); err != nil {
		return nil, err
	}
	return &s, nil
}

func (g *Group) jsonRaw(res any, f func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) error {
	b, err := g.raw(f)
	if err != nil {
		return err
	}
	return unmarshal(b, res)
}

// Epoch returns the group's current epoch.
func (g *Group) Epoch() (uint64, error) {
	s, err := g.State()
	if err != nil {
		return 0, err
	}
	return s.Epoch, nil
}

// Commit makes a commit. It stays pending until MergePendingCommit, which
// the caller runs once the hub accepts it (§11.6), or ClearPendingCommit.
func (g *Group) Commit(s *Signer, opts CommitOptions) (*CommitResult, error) {
	req := struct {
		GroupID          []byte      `json:"group_id"`
		Adds             [][]byte    `json:"adds,omitempty"`
		Removes          []uint32    `json:"removes,omitempty"`
		Extensions       []Extension `json:"extensions,omitempty"`
		PendingProposals bool        `json:"pending_proposals"`
		UpdatePath       bool        `json:"update_path"`
		Leaf             *Leaf       `json:"leaf,omitempty"`
		GroupInfo        bool        `json:"group_info"`
		AAD              []byte      `json:"aad"`
	}{g.id, opts.Adds, opts.Removes, opts.Extensions, opts.PendingProposals, opts.UpdatePath, opts.Leaf, opts.GroupInfo, opts.AAD}
	sp, err := s.handle()
	if err != nil {
		return nil, err
	}
	var np *C.TrmlsSigner
	if opts.NewSigner != nil {
		if np, err = opts.NewSigner.handle(); err != nil {
			return nil, err
		}
	}
	defer runtime.KeepAlive(s)
	defer runtime.KeepAlive(opts.NewSigner)
	var res CommitResult
	if _, err := g.c.plain(req, &res, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_commit(p, sp, np, r, n, out)
	}); err != nil {
		return nil, err
	}
	return &res, nil
}

// MergePendingCommit applies this device's own pending commit.
func (g *Group) MergePendingCommit() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_merge_pending_commit(p, r, n, out)
	})
	return err
}

// ClearPendingCommit drops this device's own pending commit, as after
// `409 stale-epoch` from the hub (§11.6).
func (g *Group) ClearPendingCommit() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_clear_pending_commit(p, r, n, out)
	})
	return err
}

// MergeStagedCommit applies the commit from another member that Process
// staged.
func (g *Group) MergeStagedCommit() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_merge_staged_commit(p, r, n, out)
	})
	return err
}

// DiscardStagedCommit drops the staged commit. Its decryption key stays
// used up until Revert, so a caller that may want to process it again
// reverts instead (§11.9).
func (g *Group) DiscardStagedCommit() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_discard_staged_commit(p, r, n, out)
	})
	return err
}

// ProposalResult is a standalone proposal this device made.
type ProposalResult struct {
	Message   []byte `json:"message"`
	Reference []byte `json:"reference"`
}

// ProposeRemove makes a standalone Remove proposal and queues it. For
// this device's own leaf, it's how a device leaves (§11.1).
func (g *Group) ProposeRemove(s *Signer, leaf uint32, aad []byte) (*ProposalResult, error) {
	req := struct {
		GroupID   []byte `json:"group_id"`
		LeafIndex uint32 `json:"leaf_index"`
		AAD       []byte `json:"aad"`
	}{g.id, leaf, aad}
	var res ProposalResult
	if _, err := g.c.signed(s, req, &res, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_propose_remove(p, s, r, n, out)
	}); err != nil {
		return nil, err
	}
	return &res, nil
}

// Leave proposes removing this device's own leaf.
func (g *Group) Leave(s *Signer) (*ProposalResult, error) {
	st, err := g.State()
	if err != nil {
		return nil, err
	}
	return g.ProposeRemove(s, st.OwnLeafIndex, nil)
}

// RemovePendingProposal drops a queued proposal by its reference.
func (g *Group) RemovePendingProposal(reference []byte) error {
	req := struct {
		GroupID   []byte `json:"group_id"`
		Reference []byte `json:"reference"`
	}{g.id, reference}
	_, err := g.c.plain(req, nil, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_remove_pending_proposal(p, r, n, out)
	})
	return err
}

// ClearPendingProposals drops every queued proposal.
func (g *Group) ClearPendingProposals() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_clear_pending_proposals(p, r, n, out)
	})
	return err
}

// Encrypt makes an application message. aad is its authenticated data,
// which servers can read (RFC 9420 §6.3.1).
func (g *Group) Encrypt(s *Signer, plaintext, aad []byte) ([]byte, error) {
	req := struct {
		GroupID   []byte `json:"group_id"`
		Plaintext []byte `json:"plaintext"`
		AAD       []byte `json:"aad"`
	}{g.id, plaintext, aad}
	return g.c.signed(s, req, nil, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_encrypt(p, s, r, n, out)
	})
}

// Process processes a PublicMessage or PrivateMessage. A proposal is
// queued at once; RemovePendingProposal drops it again. A commit from
// another member is staged: the caller judges it, then runs
// MergeStagedCommit, or reverts. Until then, Process refuses more
// messages for the group with ErrPendingCommit.
func (g *Group) Process(message []byte) (*Processed, error) {
	req := struct {
		GroupID []byte `json:"group_id"`
		Message []byte `json:"message"`
	}{g.id, message}
	var res Processed
	if _, err := g.c.plain(req, &res, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_process(p, r, n, out)
	}); err != nil {
		return nil, err
	}
	return &res, nil
}

func (g *Group) export(label string, context []byte, length int, pending bool) ([]byte, error) {
	req := struct {
		GroupID []byte `json:"group_id"`
		Label   string `json:"label"`
		Context []byte `json:"context"`
		Length  int    `json:"length"`
		Pending bool   `json:"pending"`
	}{g.id, label, context, length, pending}
	return g.c.plain(req, nil, func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_export_secret(p, r, n, out)
	})
}

// ExportSecret is the MLS exporter for the current epoch (RFC 9420 §8.5),
// as used for epoch keys (§11.6) and call keys (§15.2).
func (g *Group) ExportSecret(label string, context []byte, length int) ([]byte, error) {
	return g.export(label, context, length, false)
}

// ExportPendingSecret is the MLS exporter for the epoch this device's
// pending commit creates, as needed for `next` in a commit request
// (§11.6).
func (g *Group) ExportPendingSecret(label string, context []byte, length int) ([]byte, error) {
	return g.export(label, context, length, true)
}

// ExportGroupInfo returns a signed GroupInfo for the current epoch, with
// the external_pub extension, and the ratchet tree if asked.
func (g *Group) ExportGroupInfo(s *Signer, withRatchetTree bool) ([]byte, error) {
	req := struct {
		GroupID         []byte `json:"group_id"`
		WithRatchetTree bool   `json:"with_ratchet_tree"`
	}{g.id, withRatchetTree}
	return g.c.signed(s, req, nil, func(p *C.TrmlsClient, s *C.TrmlsSigner, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_export_group_info(p, s, r, n, out)
	})
}

// ExportRatchetTree returns the ratchet tree, encoded as the ratchet_tree
// extension's data (RFC 9420 §12.4.3.3).
func (g *Group) ExportRatchetTree() ([]byte, error) {
	return g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_export_ratchet_tree(p, r, n, out)
	})
}

// Delete deletes the group and its secrets from the client.
func (g *Group) Delete() error {
	_, err := g.raw(func(p *C.TrmlsClient, r *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t {
		return C.trmls_delete_group(p, r, n, out)
	})
	return err
}
