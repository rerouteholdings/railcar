//! The JSON requests and responses of the C interface.
//!
//! Byte strings are standard base64 with padding (RFC 4648 §4), which is
//! how Go's encoding/json writes `[]byte`. A JSON `null` reads as empty.

use serde::{Deserialize, Serialize};

pub mod b64 {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(v: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&STANDARD.encode(v))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        match Option::<String>::deserialize(d)? {
            None => Ok(Vec::new()),
            Some(s) => STANDARD.decode(s).map_err(serde::de::Error::custom),
        }
    }
}

pub mod b64_opt {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(v: &Option<Vec<u8>>, s: S) -> Result<S::Ok, S::Error> {
        match v {
            Some(v) => s.serialize_str(&STANDARD.encode(v)),
            None => s.serialize_none(),
        }
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Option<Vec<u8>>, D::Error> {
        match Option::<String>::deserialize(d)? {
            None => Ok(None),
            Some(s) => STANDARD
                .decode(s)
                .map(Some)
                .map_err(serde::de::Error::custom),
        }
    }
}

pub mod b64_list {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde::{Deserialize, Deserializer, Serialize, Serializer};

    pub fn serialize<S: Serializer>(v: &[Vec<u8>], s: S) -> Result<S::Ok, S::Error> {
        let list: Vec<String> = v.iter().map(|b| STANDARD.encode(b)).collect();
        list.serialize(s)
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<Vec<u8>>, D::Error> {
        let list = Option::<Vec<String>>::deserialize(d)?.unwrap_or_default();
        list.iter()
            .map(|s| STANDARD.decode(s).map_err(serde::de::Error::custom))
            .collect()
    }
}

/// An MLS extension, passed through as its type and opaque data.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Ext {
    #[serde(rename = "type")]
    pub ty: u16,
    #[serde(default, with = "b64")]
    pub data: Vec<u8>,
}

/// The capabilities a leaf lists, beyond what MLS requires by default.
/// The version is always MLS 1.0, and the ciphersuite always the pinned one.
#[derive(Serialize, Deserialize, Clone, Debug, Default)]
pub struct Caps {
    #[serde(default)]
    pub extensions: Vec<u16>,
    #[serde(default)]
    pub proposals: Vec<u16>,
    /// Credential types. Empty means basic only.
    #[serde(default)]
    pub credentials: Vec<u16>,
}

/// What a group or joiner keeps as its OpenMLS join configuration.
#[derive(Serialize, Deserialize, Clone, Debug, Default)]
pub struct GroupConfig {
    /// Put the ratchet tree in the Welcome's GroupInfo. Off when the tree
    /// travels separately, as in communities (§16.5).
    #[serde(default)]
    pub ratchet_tree_in_welcome: bool,
    /// Accept PublicMessage handshake messages, which external commits
    /// need (§16.5). Everything this device sends is a PrivateMessage.
    #[serde(default)]
    pub accept_public_messages: bool,
    /// How many past epochs' secrets to keep, to read late messages.
    #[serde(default)]
    pub max_past_epochs: usize,
    /// How many generations back a sender's ratchet accepts. Zero means the
    /// OpenMLS default.
    #[serde(default)]
    pub out_of_order_tolerance: u32,
    /// How far ahead a sender's ratchet may jump. Zero means the OpenMLS
    /// default.
    #[serde(default)]
    pub maximum_forward_distance: u32,
    /// Pad PrivateMessage plaintexts to a multiple of this many bytes.
    #[serde(default)]
    pub padding: usize,
}

/// The leaf node a device presents: its credential identity, its
/// capabilities, and its leaf node extensions.
#[derive(Serialize, Deserialize, Clone, Debug, Default)]
pub struct LeafSpec {
    #[serde(default, with = "b64")]
    pub identity: Vec<u8>,
    #[serde(default)]
    pub capabilities: Caps,
    #[serde(default)]
    pub extensions: Vec<Ext>,
}

#[derive(Deserialize)]
pub struct KeyPackageReq {
    pub leaf: LeafSpec,
    #[serde(default)]
    pub last_resort: bool,
    /// Seconds from now until the key package expires. Zero means the
    /// OpenMLS default.
    #[serde(default)]
    pub lifetime: u64,
    /// Key package extensions, beyond the last-resort one.
    #[serde(default)]
    pub extensions: Vec<Ext>,
}

#[derive(Serialize, Debug)]
pub struct KeyPackageRes {
    /// The KeyPackage, TLS-encoded, without the MLSMessage wrapper.
    #[serde(with = "b64")]
    pub key_package: Vec<u8>,
    /// The same KeyPackage inside an MLSMessage.
    #[serde(with = "b64")]
    pub message: Vec<u8>,
    #[serde(with = "b64")]
    pub hash_ref: Vec<u8>,
}

/// What a leaf node holds.
#[derive(Serialize, Clone, Debug)]
pub struct LeafInfo {
    #[serde(with = "b64")]
    pub identity: Vec<u8>,
    pub credential_type: u16,
    #[serde(with = "b64")]
    pub signature_key: Vec<u8>,
    #[serde(with = "b64")]
    pub encryption_key: Vec<u8>,
    pub capabilities: CapsInfo,
    pub extensions: Vec<Ext>,
}

#[derive(Serialize, Clone, Debug)]
pub struct CapsInfo {
    pub versions: Vec<u16>,
    pub ciphersuites: Vec<u16>,
    pub extensions: Vec<u16>,
    pub proposals: Vec<u16>,
    pub credentials: Vec<u16>,
}

#[derive(Serialize, Clone, Debug)]
pub struct KeyPackageInfo {
    pub ciphersuite: u16,
    #[serde(with = "b64")]
    pub hash_ref: Vec<u8>,
    pub last_resort: bool,
    pub not_before: u64,
    pub not_after: u64,
    pub leaf: LeafInfo,
    pub extensions: Vec<Ext>,
}

#[derive(Serialize, Debug)]
pub struct MemberInfo {
    pub leaf_index: u32,
    #[serde(flatten)]
    pub leaf: LeafInfo,
}

#[derive(Serialize, Debug)]
pub struct GroupInfoRes {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub epoch: u64,
    pub own_leaf_index: u32,
    pub active: bool,
    pub pending_commit: bool,
    pub members: Vec<MemberInfo>,
    pub extensions: Vec<Ext>,
    #[serde(with = "b64")]
    pub epoch_authenticator: Vec<u8>,
    #[serde(with = "b64")]
    pub tree_hash: Vec<u8>,
    pub pending_proposals: Vec<ProposalInfo>,
}

#[derive(Deserialize)]
pub struct CreateReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub leaf: LeafSpec,
    #[serde(default)]
    pub extensions: Vec<Ext>,
    #[serde(default)]
    pub config: GroupConfig,
}

#[derive(Deserialize)]
pub struct CommitReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    /// KeyPackages to add, TLS-encoded, with or without the MLSMessage
    /// wrapper.
    #[serde(default, with = "b64_list")]
    pub adds: Vec<Vec<u8>>,
    #[serde(default)]
    pub removes: Vec<u32>,
    /// When set, a GroupContextExtensions proposal with exactly these
    /// extensions.
    #[serde(default)]
    pub extensions: Option<Vec<Ext>>,
    /// Commit the pending proposals by reference.
    #[serde(default)]
    pub pending_proposals: bool,
    /// Include an update path even when no proposal needs one.
    #[serde(default)]
    pub update_path: bool,
    /// A new leaf for the update path. Its identity is used only with a new
    /// signer.
    #[serde(default)]
    pub leaf: Option<LeafSpec>,
    /// Return a GroupInfo for the new epoch.
    #[serde(default)]
    pub group_info: bool,
    /// Authenticated data for the commit message.
    #[serde(default, with = "b64")]
    pub aad: Vec<u8>,
}

#[derive(Serialize, Debug)]
pub struct CommitRes {
    #[serde(with = "b64")]
    pub commit: Vec<u8>,
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub welcome: Option<Vec<u8>>,
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub group_info: Option<Vec<u8>>,
    /// The epoch the commit was made in.
    pub epoch: u64,
}

#[derive(Deserialize)]
pub struct WelcomeReq {
    #[serde(with = "b64")]
    pub welcome: Vec<u8>,
    #[serde(default, with = "b64")]
    pub ratchet_tree: Vec<u8>,
    #[serde(default)]
    pub config: GroupConfig,
}

#[derive(Serialize, Debug)]
pub struct WelcomeRes {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub epoch: u64,
    pub sender_leaf_index: u32,
}

#[derive(Deserialize)]
pub struct ExternalJoinReq {
    #[serde(with = "b64")]
    pub group_info: Vec<u8>,
    #[serde(default, with = "b64")]
    pub ratchet_tree: Vec<u8>,
    pub leaf: LeafSpec,
    #[serde(default, with = "b64")]
    pub aad: Vec<u8>,
    #[serde(default)]
    pub config: GroupConfig,
    /// Return a GroupInfo for the new epoch.
    #[serde(default)]
    pub group_info_out: bool,
}

#[derive(Serialize, Debug)]
pub struct ExternalJoinRes {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    #[serde(flatten)]
    pub commit: CommitRes,
}

#[derive(Serialize, Debug)]
pub struct GroupInfoInspect {
    pub ciphersuite: u16,
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub epoch: u64,
    #[serde(with = "b64")]
    pub tree_hash: Vec<u8>,
    /// GroupContext extensions.
    pub extensions: Vec<Ext>,
    /// GroupInfo extensions, such as external_pub or ratchet_tree.
    pub group_info_extensions: Vec<Ext>,
}

#[derive(Deserialize)]
pub struct EncryptReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    #[serde(with = "b64")]
    pub plaintext: Vec<u8>,
    #[serde(default, with = "b64")]
    pub aad: Vec<u8>,
}

#[derive(Deserialize)]
pub struct ProcessReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    #[serde(with = "b64")]
    pub message: Vec<u8>,
}

#[derive(Serialize, Clone, Debug)]
pub struct SenderInfo {
    /// "member", "external", "new_member_proposal", or "new_member_commit".
    pub kind: &'static str,
    /// The leaf index for a member, or the external sender's index.
    pub index: u32,
}

#[derive(Serialize, Clone, Debug)]
pub struct ProposalInfo {
    /// "add", "update", "remove", "group_context_extensions", "psk",
    /// "reinit", "external_init", "self_remove", or "other".
    pub kind: &'static str,
    pub sender: SenderInfo,
    #[serde(with = "b64")]
    pub reference: Vec<u8>,
    /// Whether the commit carried it by reference.
    pub by_reference: bool,
    /// For an Add: the key package.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub key_package: Option<KeyPackageInfo>,
    /// For an Add: the raw KeyPackage bytes.
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub key_package_bytes: Option<Vec<u8>>,
    /// For a Remove: the removed leaf.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub removed: Option<u32>,
    /// For an Update: the new leaf.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub leaf: Option<LeafInfo>,
    /// For a GroupContextExtensions proposal: the new extensions.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub extensions: Option<Vec<Ext>>,
}

#[derive(Serialize, Debug)]
pub struct StagedInfo {
    pub proposals: Vec<ProposalInfo>,
    /// The committer's new leaf, when the commit has an update path.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path_leaf: Option<LeafInfo>,
    pub self_removed: bool,
    /// The epoch the commit creates.
    pub new_epoch: u64,
    /// The GroupContext extensions of that epoch.
    pub extensions: Vec<Ext>,
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub epoch_authenticator: Option<Vec<u8>>,
}

#[derive(Serialize, Debug)]
pub struct ProcessRes {
    /// "application", "proposal", "commit", or "own_commit".
    pub kind: &'static str,
    pub epoch: u64,
    pub sender: SenderInfo,
    /// The sender's credential identity.
    #[serde(with = "b64")]
    pub identity: Vec<u8>,
    #[serde(with = "b64")]
    pub authenticated_data: Vec<u8>,
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub plaintext: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub proposal: Option<ProposalInfo>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub commit: Option<StagedInfo>,
}

#[derive(Serialize, Debug)]
pub struct MessageInfo {
    /// "public_message", "private_message", "welcome", "group_info", or
    /// "key_package".
    pub wire_format: &'static str,
    #[serde(with = "b64_opt", skip_serializing_if = "Option::is_none")]
    pub group_id: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub epoch: Option<u64>,
    /// "application", "proposal", or "commit", for public and private
    /// messages.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content_type: Option<&'static str>,
}

#[derive(Deserialize)]
pub struct ExportReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub label: String,
    #[serde(default, with = "b64")]
    pub context: Vec<u8>,
    pub length: usize,
    /// Export from the epoch the pending commit creates (§11.6).
    #[serde(default)]
    pub pending: bool,
}

#[derive(Deserialize)]
pub struct ProposeRemoveReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    pub leaf_index: u32,
    #[serde(default, with = "b64")]
    pub aad: Vec<u8>,
}

#[derive(Serialize, Debug)]
pub struct ProposeRes {
    #[serde(with = "b64")]
    pub message: Vec<u8>,
    #[serde(with = "b64")]
    pub reference: Vec<u8>,
}

#[derive(Deserialize)]
pub struct GroupReq {
    #[serde(with = "b64")]
    pub group_id: Vec<u8>,
    #[serde(default, with = "b64")]
    pub reference: Vec<u8>,
    #[serde(default)]
    pub with_ratchet_tree: bool,
}

#[derive(Serialize, Debug)]
pub struct ChangeOut {
    #[serde(with = "b64")]
    pub key: Vec<u8>,
    #[serde(with = "b64_opt")]
    pub value: Option<Vec<u8>>,
}

#[derive(Serialize, Debug)]
pub struct ChangesRes {
    pub changes: Vec<ChangeOut>,
}
