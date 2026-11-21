package mls

// Extension types MLS itself defines (RFC 9420 §17.3), which a caller
// may need to name in capabilities or read back.
const (
	ExtApplicationID        uint16 = 0x0001
	ExtRatchetTree          uint16 = 0x0002
	ExtRequiredCapabilities uint16 = 0x0003
	ExtExternalPub          uint16 = 0x0004
	ExtExternalSenders      uint16 = 0x0005
	ExtLastResort           uint16 = 0x000A
)

// CredentialBasic is the basic credential type (RFC 9420 §5.3), the only
// one Tieroute uses (§11.1).
const CredentialBasic uint16 = 0x0001

// Extension is an MLS extension, as its type and opaque data.
type Extension struct {
	Type uint16 `json:"type"`
	Data []byte `json:"data"`
}

// Capabilities are what a leaf lists beyond MLS's defaults. The protocol
// version is always MLS 1.0, and the ciphersuite always 0x004F.
type Capabilities struct {
	Extensions []uint16 `json:"extensions,omitempty"`
	Proposals  []uint16 `json:"proposals,omitempty"`
	// Credentials lists credential types. Empty means basic only.
	Credentials []uint16 `json:"credentials,omitempty"`
}

// Leaf is the leaf node a device presents in key packages, new groups,
// external joins, and update paths.
type Leaf struct {
	// Identity is the basic credential's identity bytes (§7.1).
	Identity     []byte       `json:"identity"`
	Capabilities Capabilities `json:"capabilities"`
	// Extensions are leaf node extensions.
	Extensions []Extension `json:"extensions,omitempty"`
}

// Config is a group's OpenMLS configuration, set when this device creates
// or joins the group and kept with it.
type Config struct {
	// RatchetTreeInWelcome puts the ratchet tree in the GroupInfo of each
	// Welcome and GroupInfo this device makes. Without it, joiners get the
	// tree some other way, as in communities (§16.5).
	RatchetTreeInWelcome bool `json:"ratchet_tree_in_welcome"`
	// AcceptPublicMessages accepts PublicMessage handshake messages, which
	// external commits use (§16.5). Without it, the group refuses them.
	// What this device sends is always a PrivateMessage.
	AcceptPublicMessages bool `json:"accept_public_messages"`
	// MaxPastEpochs is how many past epochs' secrets to keep, to read
	// messages that arrive after a commit.
	MaxPastEpochs int `json:"max_past_epochs"`
	// OutOfOrderTolerance and MaximumForwardDistance tune each sender's
	// ratchet. Zero means the OpenMLS default.
	OutOfOrderTolerance    uint32 `json:"out_of_order_tolerance"`
	MaximumForwardDistance uint32 `json:"maximum_forward_distance"`
	// Padding pads PrivateMessage plaintexts to a multiple of this many
	// bytes.
	Padding int `json:"padding"`
}

// CapabilitiesInfo is what a leaf lists, as read from it.
type CapabilitiesInfo struct {
	Versions     []uint16 `json:"versions"`
	Ciphersuites []uint16 `json:"ciphersuites"`
	Extensions   []uint16 `json:"extensions"`
	Proposals    []uint16 `json:"proposals"`
	Credentials  []uint16 `json:"credentials"`
}

// LeafInfo is what a leaf node holds.
type LeafInfo struct {
	Identity       []byte           `json:"identity"`
	CredentialType uint16           `json:"credential_type"`
	SignatureKey   []byte           `json:"signature_key"`
	EncryptionKey  []byte           `json:"encryption_key"`
	Capabilities   CapabilitiesInfo `json:"capabilities"`
	Extensions     []Extension      `json:"extensions"`
}

// Extension returns the data of the leaf's extension of type t.
func (l *LeafInfo) Extension(t uint16) ([]byte, bool) {
	return findExtension(l.Extensions, t)
}

func findExtension(list []Extension, t uint16) ([]byte, bool) {
	for _, e := range list {
		if e.Type == t {
			return e.Data, true
		}
	}
	return nil, false
}

// KeyPackageOptions describe a key package to make.
type KeyPackageOptions struct {
	Leaf Leaf
	// LastResort marks the key package as reusable (§11.4).
	LastResort bool
	// Lifetime is how long the key package is valid, in whole seconds.
	// Zero means the OpenMLS default.
	Lifetime uint64
	// Extensions are key package extensions, beyond the last-resort one.
	Extensions []Extension
}

// KeyPackage is a key package this device made. Its private keys stay in
// the client.
type KeyPackage struct {
	// Raw is the KeyPackage, TLS-encoded, without the MLSMessage wrapper.
	Raw []byte `json:"key_package"`
	// Message is the same KeyPackage inside an MLSMessage.
	Message []byte `json:"message"`
	// HashRef is its KeyPackageRef (RFC 9420 §5.2).
	HashRef []byte `json:"hash_ref"`
}

// KeyPackageInfo describes a key package whose signature and lifetime
// checked out.
type KeyPackageInfo struct {
	Ciphersuite uint16 `json:"ciphersuite"`
	HashRef     []byte `json:"hash_ref"`
	LastResort  bool   `json:"last_resort"`
	// NotBefore and NotAfter are the lifetime, in seconds since 1970.
	NotBefore  uint64      `json:"not_before"`
	NotAfter   uint64      `json:"not_after"`
	Leaf       LeafInfo    `json:"leaf"`
	Extensions []Extension `json:"extensions"`
}

// Member is a member of a group: one leaf.
type Member struct {
	LeafIndex uint32 `json:"leaf_index"`
	LeafInfo
}

// State is what a group looks like now.
type State struct {
	GroupID      []byte `json:"group_id"`
	Epoch        uint64 `json:"epoch"`
	OwnLeafIndex uint32 `json:"own_leaf_index"`
	// Active is false once this device has been removed.
	Active        bool     `json:"active"`
	PendingCommit bool     `json:"pending_commit"`
	Members       []Member `json:"members"`
	// Extensions are the GroupContext extensions.
	Extensions         []Extension `json:"extensions"`
	EpochAuthenticator []byte      `json:"epoch_authenticator"`
	TreeHash           []byte      `json:"tree_hash"`
	PendingProposals   []Proposal  `json:"pending_proposals"`
}

// Extension returns the data of the GroupContext extension of type t.
func (s *State) Extension(t uint16) ([]byte, bool) {
	return findExtension(s.Extensions, t)
}

// Member returns the member at a leaf index.
func (s *State) Member(leaf uint32) (*Member, bool) {
	for i := range s.Members {
		if s.Members[i].LeafIndex == leaf {
			return &s.Members[i], true
		}
	}
	return nil, false
}

// CreateOptions describe a new group.
type CreateOptions struct {
	// GroupID is the group's ID: 32 random bytes in Tieroute (§11.5).
	GroupID []byte `json:"group_id"`
	Leaf    Leaf   `json:"leaf"`
	// Extensions are the GroupContext extensions.
	Extensions []Extension `json:"extensions,omitempty"`
	Config     Config      `json:"config"`
}

// CommitOptions describe a commit.
type CommitOptions struct {
	// Adds are KeyPackages to add, with or without the MLSMessage wrapper.
	// Each one's signature and lifetime are checked.
	Adds [][]byte
	// Removes are leaf indexes to remove.
	Removes []uint32
	// Extensions, when not empty, are the complete new GroupContext
	// extensions, sent as a GroupContextExtensions proposal.
	Extensions []Extension
	// PendingProposals commits the group's pending proposals by reference.
	PendingProposals bool
	// UpdatePath includes an update path even when no proposal needs one.
	UpdatePath bool
	// Leaf, when set, is this device's new leaf for the update path.
	Leaf *Leaf
	// NewSigner, when set, moves this device's leaf to a new signature key
	// (§3.2.3). Leaf must then be set, with the new credential identity.
	NewSigner *Signer
	// GroupInfo asks for a GroupInfo for the new epoch.
	GroupInfo bool
	// AAD is the commit's authenticated data.
	AAD []byte
}

// CommitResult is a commit this device made. It's pending until merged.
type CommitResult struct {
	Commit  []byte `json:"commit"`
	Welcome []byte `json:"welcome"`
	// GroupInfo is set when asked for.
	GroupInfo []byte `json:"group_info"`
	// Epoch is the epoch the commit was made in.
	Epoch uint64 `json:"epoch"`
}

// WelcomeResult says what a Welcome joined.
type WelcomeResult struct {
	GroupID []byte `json:"group_id"`
	Epoch   uint64 `json:"epoch"`
	// SenderLeafIndex is the leaf of the member who made the Welcome.
	SenderLeafIndex uint32 `json:"sender_leaf_index"`
}

// ExternalJoinOptions describe a join by external commit.
type ExternalJoinOptions struct {
	GroupInfo []byte `json:"group_info"`
	// RatchetTree is needed when the GroupInfo has no ratchet_tree
	// extension: the extension's data (RFC 9420 §12.4.3.3).
	RatchetTree []byte `json:"ratchet_tree"`
	Leaf        Leaf   `json:"leaf"`
	AAD         []byte `json:"aad"`
	Config      Config `json:"config"`
	// WantGroupInfo asks for a GroupInfo for the new epoch.
	WantGroupInfo bool `json:"group_info_out"`
}

// Sender is who sent a message.
type Sender struct {
	// Kind is "member", "external", "new_member_proposal", or
	// "new_member_commit".
	Kind string `json:"kind"`
	// Index is a member's leaf index.
	Index uint32 `json:"index"`
}

// Proposal describes a proposal, standalone or inside a commit.
type Proposal struct {
	// Kind is "add", "update", "remove", "group_context_extensions",
	// "psk", "reinit", "external_init", "self_remove", or "other".
	Kind        string `json:"kind"`
	Sender      Sender `json:"sender"`
	Reference   []byte `json:"reference"`
	ByReference bool   `json:"by_reference"`
	// KeyPackage and KeyPackageRaw are set for an Add.
	KeyPackage    *KeyPackageInfo `json:"key_package"`
	KeyPackageRaw []byte          `json:"key_package_bytes"`
	// Removed is set for a Remove.
	Removed *uint32 `json:"removed"`
	// Leaf is set for an Update.
	Leaf *LeafInfo `json:"leaf"`
	// Extensions is set for a GroupContextExtensions proposal.
	Extensions []Extension `json:"extensions"`
}

// StagedCommit describes a commit from another member, staged and waiting
// for MergeStagedCommit or DiscardStagedCommit.
type StagedCommit struct {
	Proposals []Proposal `json:"proposals"`
	// PathLeaf is the committer's new leaf, when the commit has an update
	// path.
	PathLeaf    *LeafInfo `json:"path_leaf"`
	SelfRemoved bool      `json:"self_removed"`
	// NewEpoch is the epoch the commit creates.
	NewEpoch uint64 `json:"new_epoch"`
	// Extensions are the GroupContext extensions of that epoch.
	Extensions         []Extension `json:"extensions"`
	EpochAuthenticator []byte      `json:"epoch_authenticator"`
}

// Extension returns the data of the new epoch's GroupContext extension of
// type t.
func (s *StagedCommit) Extension(t uint16) ([]byte, bool) {
	return findExtension(s.Extensions, t)
}

// Kinds of processed message.
const (
	KindApplication = "application"
	KindProposal    = "proposal"
	KindCommit      = "commit"
	// KindOwn is a message this device sent, coming back to it.
	KindOwn = "own"
	// KindExternalJoinProposal is an external join proposal. It isn't
	// queued.
	KindExternalJoinProposal = "external_join_proposal"
)

// Processed is a message after processing.
type Processed struct {
	Kind   string `json:"kind"`
	Epoch  uint64 `json:"epoch"`
	Sender Sender `json:"sender"`
	// Identity is the sender's credential identity.
	Identity          []byte `json:"identity"`
	AuthenticatedData []byte `json:"authenticated_data"`
	// Plaintext is set for an application message.
	Plaintext []byte `json:"plaintext"`
	// Proposal is set for a proposal, which the group has queued.
	Proposal *Proposal `json:"proposal"`
	// Commit is set for a commit, which the group has staged.
	Commit *StagedCommit `json:"commit"`
}

// MessageInfo is an MLSMessage's unencrypted header.
type MessageInfo struct {
	// WireFormat is "public_message", "private_message", "welcome",
	// "group_info", or "key_package".
	WireFormat string `json:"wire_format"`
	GroupID    []byte `json:"group_id"`
	// Epoch is set for public and private messages.
	Epoch *uint64 `json:"epoch"`
	// ContentType is "application", "proposal", or "commit", for public and
	// private messages.
	ContentType string `json:"content_type"`
}

// GroupInfoSummary is what a GroupInfo says, read without verifying it.
// Joining verifies it.
type GroupInfoSummary struct {
	Ciphersuite uint16 `json:"ciphersuite"`
	GroupID     []byte `json:"group_id"`
	Epoch       uint64 `json:"epoch"`
	TreeHash    []byte `json:"tree_hash"`
	// Extensions are the GroupContext extensions.
	Extensions []Extension `json:"extensions"`
	// GroupInfoExtensions are the GroupInfo's own, such as external_pub.
	GroupInfoExtensions []Extension `json:"group_info_extensions"`
}

// Change is one entry of the client's store that changed. Deleted entries
// have a nil Value.
type Change struct {
	Key     []byte
	Value   []byte
	Deleted bool
}

// RequiredCapabilities builds a required_capabilities extension (RFC 9420
// §7.2) listing the given extension, proposal, and credential types.
func RequiredCapabilities(extensions, proposals, credentials []uint16) Extension {
	var data []byte
	for _, list := range [][]uint16{extensions, proposals, credentials} {
		data = appendVarint(data, uint64(2*len(list)))
		for _, t := range list {
			data = append(data, byte(t>>8), byte(t))
		}
	}
	return Extension{Type: ExtRequiredCapabilities, Data: data}
}

// RequiredExtensions reads the extension types a required_capabilities
// extension's data lists (RFC 9420 §7.2), or nil if it doesn't parse.
func RequiredExtensions(data []byte) []uint16 {
	n, rest, ok := readVarint(data)
	if !ok || n%2 != 0 || uint64(len(rest)) < n {
		return nil
	}
	out := make([]uint16, 0, n/2)
	for i := uint64(0); i < n; i += 2 {
		out = append(out, uint16(rest[i])<<8|uint16(rest[i+1]))
	}
	return out
}

// readVarint reads an MLS variable-length integer (RFC 9420 §2.1.2).
func readVarint(b []byte) (uint64, []byte, bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	n := 1 << (b[0] >> 6)
	if n > 4 || len(b) < n {
		return 0, nil, false
	}
	v := uint64(b[0] & 0x3f)
	for _, x := range b[1:n] {
		v = v<<8 | uint64(x)
	}
	return v, b[n:], true
}

// appendVarint appends an MLS variable-length integer (RFC 9420 §2.1.2).
func appendVarint(b []byte, n uint64) []byte {
	switch {
	case n < 1<<6:
		return append(b, byte(n))
	case n < 1<<14:
		return append(b, byte(n>>8)|0x40, byte(n))
	default:
		return append(b, byte(n>>24)|0x80, byte(n>>16), byte(n>>8), byte(n))
	}
}
