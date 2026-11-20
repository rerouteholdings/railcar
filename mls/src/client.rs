//! The MLS operations behind the C interface.
//!
//! A `Client` is one device's MLS state: its key packages and every group
//! it's in. It knows nothing about Tieroute's rules. The caller passes in
//! credential identities and extensions as bytes, and judges commits itself
//! before merging them.

use std::collections::HashMap;

use openmls::prelude::*;
use openmls::treesync::RatchetTreeIn;
use openmls_basic_credential::SignatureKeyPair;
use tls_codec::{Deserialize as _, Serialize as _, VLBytes};

use crate::error::{Code, Error, Result};
use crate::store::{self, Change, Provider, Values};
use crate::types::*;

/// The one ciphersuite Tieroute uses (§3, §11.1):
/// `MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519`, OpenMLS's provisional
/// code point `0x004F`.
pub const SUITE: Ciphersuite = Ciphersuite::MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519;

/// An Ed25519 signer. In Tieroute this is a device's message key, which
/// signs its MLS leaves and messages (§3.2.3, §7.1).
pub struct Signer {
    kp: SignatureKeyPair,
}

impl Signer {
    /// Makes a signer from a 32-byte Ed25519 private key (the RFC 8032 seed).
    pub fn from_seed(seed: &[u8]) -> Result<Signer> {
        let seed: [u8; 32] = seed
            .try_into()
            .map_err(|_| Error::bad("an Ed25519 private key is 32 bytes"))?;
        let public = ed25519_dalek::SigningKey::from_bytes(&seed)
            .verifying_key()
            .to_bytes();
        Ok(Signer {
            kp: SignatureKeyPair::from_raw(
                SignatureScheme::ED25519,
                seed.to_vec(),
                public.to_vec(),
            ),
        })
    }

    pub fn public_key(&self) -> &[u8] {
        self.kp.public()
    }

    fn credential(&self, identity: &[u8]) -> CredentialWithKey {
        CredentialWithKey {
            credential: BasicCredential::new(identity.to_vec()).into(),
            signature_key: self.kp.public().into(),
        }
    }
}

/// One device's MLS state.
pub struct Client {
    provider: Provider,
    /// The store as the caller last persisted it.
    shadow: Values,
    /// Groups loaded from the store.
    groups: HashMap<Vec<u8>, MlsGroup>,
    /// Commits that `process` staged and the caller hasn't merged yet.
    staged: HashMap<Vec<u8>, StagedCommit>,
}

impl Client {
    /// Opens a client from a snapshot. An empty snapshot is a new client.
    pub fn open(snapshot: &[u8]) -> Result<Client> {
        let values = store::decode(snapshot)?;
        Ok(Client {
            provider: Provider::with_values(values.clone()),
            shadow: values,
            groups: HashMap::new(),
            staged: HashMap::new(),
        })
    }

    /// The whole store as one snapshot. The store as it is now also becomes
    /// the point that `revert` returns to.
    pub fn snapshot(&mut self) -> Vec<u8> {
        let values = self.provider.values().clone();
        let out = store::encode(&values);
        self.shadow = values;
        out
    }

    /// The entries that changed since the last snapshot or change set. The
    /// caller persists them, and they become the point that `revert`
    /// returns to.
    pub fn take_changes(&mut self) -> Vec<Change> {
        let changes = store::diff(&self.provider.values(), &self.shadow);
        store::apply(&mut self.shadow, &changes);
        changes
    }

    /// Undoes every change since the last snapshot or change set, including
    /// staged commits and keys used up by decrypting.
    pub fn revert(&mut self) {
        let changes = store::diff(&self.shadow, &self.provider.values());
        store::apply(&mut self.provider.values_mut(), &changes);
        self.groups.clear();
        self.staged.clear();
    }

    // === Key packages ===

    pub fn key_package(&mut self, signer: &Signer, req: KeyPackageReq) -> Result<KeyPackageRes> {
        // The leaf lists every key package extension it carries, as OpenMLS
        // requires, the last-resort one included.
        let mut c = req.leaf.capabilities.clone();
        let mut kp_types: Vec<u16> = req.extensions.iter().map(|e| e.ty).collect();
        if req.last_resort {
            kp_types.push(u16::from(ExtensionType::LastResort));
        }
        for t in kp_types {
            if !c.extensions.contains(&t) {
                c.extensions.push(t);
            }
        }
        let mut b = KeyPackage::builder()
            .leaf_node_capabilities(caps(&c))
            .leaf_node_extensions(extensions::<LeafNode>(&req.leaf.extensions)?);
        if req.lifetime > 0 {
            b = b.key_package_lifetime(Lifetime::new(req.lifetime));
        }
        if !req.extensions.is_empty() {
            b = b.key_package_extensions(extensions::<KeyPackage>(&req.extensions)?);
        }
        if req.last_resort {
            b = b.mark_as_last_resort();
        }
        let bundle = b
            .build(
                SUITE,
                &self.provider,
                &signer.kp,
                signer.credential(&req.leaf.identity),
            )
            .map_err(|e| Error::mls("making a key package", e))?;
        let kp = bundle.key_package().clone();
        Ok(KeyPackageRes {
            key_package: tls(&kp)?,
            hash_ref: kp
                .hash_ref(self.provider.crypto())
                .map_err(|e| Error::mls("hashing a key package", e))?
                .as_slice()
                .to_vec(),
            message: tls(&MlsMessageOut::from(kp))?,
        })
    }

    /// Deletes a key package's private keys, by its hash reference. Deleting
    /// one that isn't there does nothing.
    pub fn delete_key_package(&mut self, hash_ref: &[u8]) -> Result<()> {
        use openmls_traits::storage::StorageProvider as _;
        let r = KeyPackageRef::tls_deserialize_exact(
            VLBytes::new(hash_ref.to_vec())
                .tls_serialize_detached()
                .map_err(|e| Error::bad(e.to_string()))?,
        )
        .map_err(|e| Error::bad(format!("bad hash reference: {e}")))?;
        self.provider
            .storage()
            .delete_key_package(&r)
            .map_err(|e| Error::new(Code::Internal, e.to_string()))
    }

    // === Groups ===

    pub fn create_group(&mut self, signer: &Signer, req: CreateReq) -> Result<()> {
        if req.group_id.is_empty() {
            return Err(Error::bad("the group ID is empty"));
        }
        let cfg = create_config(&req)?;
        let group = MlsGroup::new_with_group_id(
            &self.provider,
            &signer.kp,
            &cfg,
            GroupId::from_slice(&req.group_id),
            signer.credential(&req.leaf.identity),
        )
        .map_err(|e| Error::mls("creating the group", e))?;
        self.groups.insert(req.group_id, group);
        Ok(())
    }

    pub fn join_welcome(&mut self, req: WelcomeReq) -> Result<WelcomeRes> {
        let welcome = match MlsMessageIn::tls_deserialize_exact(&req.welcome)
            .map_err(|e| Error::bad(format!("the Welcome doesn't parse: {e}")))?
            .extract()
        {
            MlsMessageBodyIn::Welcome(w) => w,
            _ => return Err(Error::bad("the message isn't a Welcome")),
        };
        let tree = ratchet_tree(&req.ratchet_tree)?;
        let staged = StagedWelcome::new_from_welcome(
            &self.provider,
            &join_config(&req.config),
            welcome,
            tree,
        )
        .map_err(|e| Error::mls("processing the Welcome", e))?;
        let sender = staged.welcome_sender_index().u32();
        let group = staged
            .into_group(&self.provider)
            .map_err(|e| Error::mls("joining from the Welcome", e))?;
        let id = group.group_id().as_slice().to_vec();
        let res = WelcomeRes {
            group_id: id.clone(),
            epoch: group.epoch().as_u64(),
            sender_leaf_index: sender,
        };
        self.staged.remove(&id);
        self.groups.insert(id, group);
        Ok(res)
    }

    /// Joins by an external commit (RFC 9420 §12.4.3.2). OpenMLS applies the
    /// commit at once, so the group is in the new epoch already. If the hub
    /// refuses the commit, the caller reverts or deletes the group.
    pub fn join_external(
        &mut self,
        signer: &Signer,
        req: ExternalJoinReq,
    ) -> Result<ExternalJoinRes> {
        let info = match MlsMessageIn::tls_deserialize_exact(&req.group_info)
            .map_err(|e| Error::bad(format!("the GroupInfo doesn't parse: {e}")))?
            .extract()
        {
            MlsMessageBodyIn::GroupInfo(g) => g,
            _ => return Err(Error::bad("the message isn't a GroupInfo")),
        };
        let epoch = info.epoch().as_u64();
        let mut b = MlsGroup::external_commit_builder()
            .with_config(join_config(&req.config))
            .with_aad(req.aad.clone());
        if let Some(tree) = ratchet_tree(&req.ratchet_tree)? {
            b = b.with_ratchet_tree(tree);
        }
        let leaf = LeafNodeParameters::builder()
            .with_capabilities(caps(&req.leaf.capabilities))
            .with_extensions(extensions::<LeafNode>(&req.leaf.extensions)?)
            .build();
        let cb = b
            .build_group(&self.provider, info, signer.credential(&req.leaf.identity))
            .map_err(|e| Error::mls("building the external commit", e))?
            .leaf_node_parameters(leaf);
        let (group, bundle) = cb
            .load_psks(self.provider.storage())
            .map_err(|e| Error::mls("building the external commit", e))?
            .create_group_info(req.group_info_out)
            .build(
                self.provider.rand(),
                self.provider.crypto(),
                &signer.kp,
                |_| true,
            )
            .map_err(|e| Error::mls("building the external commit", e))?
            .finalize(&self.provider)
            .map_err(|e| Error::mls("finishing the external commit", e))?;
        let (commit, _, group_info) = bundle.into_messages();
        let id = group.group_id().as_slice().to_vec();
        self.staged.remove(&id);
        self.groups.insert(id.clone(), group);
        Ok(ExternalJoinRes {
            group_id: id,
            commit: CommitRes {
                commit: tls(&commit)?,
                welcome: None,
                group_info: match group_info {
                    Some(g) if req.group_info_out => Some(tls(&g)?),
                    _ => None,
                },
                epoch,
            },
        })
    }

    /// Deletes a group and its secrets from the store.
    pub fn delete_group(&mut self, id: &[u8]) -> Result<()> {
        let mut group = self.take_group(id)?;
        self.staged.remove(id);
        group
            .delete(self.provider.storage())
            .map_err(|e| Error::new(Code::Internal, format!("deleting the group: {e}")))
    }

    pub fn group_info(&mut self, id: &[u8]) -> Result<GroupInfoRes> {
        let crypto = self.provider.crypto();
        let group = load(&mut self.groups, &self.provider, id)?;
        let members = group
            .members()
            .map(|m| {
                let leaf = group
                    .public_group()
                    .leaf(m.index)
                    .ok_or_else(|| Error::new(Code::Internal, "a member has no leaf"))?;
                Ok(MemberInfo {
                    leaf_index: m.index.u32(),
                    leaf: leaf_info(leaf)?,
                })
            })
            .collect::<Result<Vec<_>>>()?;
        let pending_proposals = group
            .pending_proposals()
            .map(|p| proposal_info(crypto, p))
            .collect::<Result<Vec<_>>>()?;
        Ok(GroupInfoRes {
            group_id: id.to_vec(),
            epoch: group.epoch().as_u64(),
            own_leaf_index: group.own_leaf_index().u32(),
            active: group.is_active(),
            pending_commit: group.pending_commit().is_some(),
            members,
            extensions: exts_out(group.extensions().iter())?,
            epoch_authenticator: group.epoch_authenticator().as_slice().to_vec(),
            tree_hash: group.public_group().group_context().tree_hash().to_vec(),
            pending_proposals,
        })
    }

    pub fn commit(
        &mut self,
        signer: &Signer,
        new_signer: Option<&Signer>,
        req: CommitReq,
    ) -> Result<CommitRes> {
        let kps = req
            .adds
            .iter()
            .map(|b| validate_key_package(&self.provider, b))
            .collect::<Result<Vec<_>>>()?;
        let gce = match &req.extensions {
            Some(e) => Some(extensions::<GroupContext>(e)?),
            None => None,
        };
        let leaf = match &req.leaf {
            Some(l) => Some(
                LeafNodeParameters::builder()
                    .with_capabilities(caps(&l.capabilities))
                    .with_extensions(extensions::<LeafNode>(&l.extensions)?)
                    .build(),
            ),
            None => None,
        };
        let new_cred = new_signer.map(|s| {
            let identity = req
                .leaf
                .as_ref()
                .map(|l| l.identity.as_slice())
                .unwrap_or_default();
            (s, s.credential(identity))
        });
        if let Some((_, c)) = &new_cred {
            if c.credential.serialized_content().is_empty() {
                return Err(Error::bad("a new signer needs the leaf's identity"));
            }
        }
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        // OpenMLS would replace a pending commit silently. Refusing keeps a
        // commit the hub may still accept from being lost.
        if group.pending_commit().is_some() {
            return Err(Error::new(
                Code::PendingCommit,
                "the group has a pending commit to merge or clear first",
            ));
        }
        let epoch = group.epoch().as_u64();
        group.set_aad(req.aad.clone());
        let mut cb = group
            .commit_builder()
            .consume_proposal_store(req.pending_proposals)
            .force_self_update(req.update_path)
            .propose_adds(kps)
            .propose_removals(req.removes.iter().map(|i| LeafNodeIndex::new(*i)));
        if let Some(gce) = gce {
            cb = cb
                .propose_group_context_extensions(gce)
                .map_err(|e| Error::mls("committing", e))?;
        }
        if let Some(leaf) = leaf {
            cb = cb.leaf_node_parameters(leaf);
        }
        let mut cb = cb
            .load_psks(self.provider.storage())
            .map_err(|e| Error::mls("committing", e))?;
        if req.group_info {
            cb = cb.create_group_info(true);
        }
        let built = match new_cred {
            None => cb.build(
                self.provider.rand(),
                self.provider.crypto(),
                &signer.kp,
                |_| true,
            ),
            Some((s, credential_with_key)) => cb.build_with_new_signer(
                self.provider.rand(),
                self.provider.crypto(),
                &signer.kp,
                NewSignerBundle {
                    signer: &s.kp,
                    credential_with_key,
                },
                |_| true,
            ),
        };
        let bundle = built
            .map_err(|e| Error::mls("committing", e))?
            .stage_commit(&self.provider)
            .map_err(|e| Error::mls("staging the commit", e))?;
        let (commit, welcome, group_info) = bundle.into_messages();
        Ok(CommitRes {
            commit: tls(&commit)?,
            welcome: welcome.map(|w| tls(&w)).transpose()?,
            group_info: if req.group_info {
                group_info.map(|g| tls(&g)).transpose()?
            } else {
                None
            },
            epoch,
        })
    }

    pub fn merge_pending_commit(&mut self, id: &[u8]) -> Result<()> {
        let group = load(&mut self.groups, &self.provider, id)?;
        if group.pending_commit().is_none() {
            return Err(Error::new(
                Code::NotFound,
                "the group has no pending commit",
            ));
        }
        group
            .merge_pending_commit(&self.provider)
            .map_err(|e| Error::mls("merging the pending commit", e))
    }

    pub fn clear_pending_commit(&mut self, id: &[u8]) -> Result<()> {
        let group = load(&mut self.groups, &self.provider, id)?;
        group
            .clear_pending_commit(self.provider.storage())
            .map_err(|e| Error::new(Code::Internal, format!("clearing the pending commit: {e}")))
    }

    /// Merges the commit that `process` staged for this group.
    pub fn merge_staged_commit(&mut self, id: &[u8]) -> Result<()> {
        let staged = self
            .staged
            .remove(id)
            .ok_or_else(|| Error::new(Code::NotFound, "the group has no staged commit"))?;
        let group = load(&mut self.groups, &self.provider, id)?;
        group
            .merge_staged_commit(&self.provider, staged)
            .map_err(|e| Error::mls("merging the staged commit", e))
    }

    /// Drops the commit that `process` staged. The decryption key it used
    /// stays used up until the caller reverts (§11.9).
    pub fn discard_staged_commit(&mut self, id: &[u8]) {
        self.staged.remove(id);
    }

    pub fn propose_remove(&mut self, signer: &Signer, req: ProposeRemoveReq) -> Result<ProposeRes> {
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        group.set_aad(req.aad.clone());
        let (msg, r) = if req.leaf_index == group.own_leaf_index().u32() {
            // A device leaving sends a Remove proposal for its own leaf (§11.1).
            let msg = group
                .leave_group(&self.provider, &signer.kp)
                .map_err(|e| Error::mls("proposing to leave", e))?;
            let r = group
                .pending_proposals()
                .last()
                .map(|p| p.proposal_reference_ref().as_slice().to_vec())
                .ok_or_else(|| Error::new(Code::Internal, "the leave proposal wasn't queued"))?;
            (msg, r)
        } else {
            let (msg, r) = group
                .propose_remove_member(
                    &self.provider,
                    &signer.kp,
                    LeafNodeIndex::new(req.leaf_index),
                )
                .map_err(|e| Error::mls("proposing a removal", e))?;
            (msg, r.as_slice().to_vec())
        };
        Ok(ProposeRes {
            message: tls(&msg)?,
            reference: r,
        })
    }

    pub fn remove_pending_proposal(&mut self, id: &[u8], reference: &[u8]) -> Result<()> {
        let group = load(&mut self.groups, &self.provider, id)?;
        let r = group
            .pending_proposals()
            .find(|p| p.proposal_reference_ref().as_slice() == reference)
            .map(|p| p.proposal_reference_ref().clone())
            .ok_or_else(|| Error::new(Code::NotFound, "no pending proposal has that reference"))?;
        group
            .remove_pending_proposal(self.provider.storage(), &r)
            .map_err(|e| Error::mls("removing the proposal", e))
    }

    pub fn clear_pending_proposals(&mut self, id: &[u8]) -> Result<()> {
        let group = load(&mut self.groups, &self.provider, id)?;
        group
            .clear_pending_proposals(self.provider.storage())
            .map_err(|e| Error::new(Code::Internal, format!("clearing the proposals: {e}")))
    }

    // === Messages ===

    /// Sets a group's configuration, as for joining it: what it was made
    /// or joined with changes only this way, such as its padding.
    pub fn set_config(&mut self, req: SetConfigReq) -> Result<()> {
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        group
            .set_configuration(self.provider.storage(), &join_config(&req.config))
            .map_err(|e| Error::new(Code::Internal, format!("setting the group's configuration: {e}")))
    }

    pub fn encrypt(&mut self, signer: &Signer, req: EncryptReq) -> Result<Vec<u8>> {
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        group.set_aad(req.aad);
        let msg = group
            .create_message(&self.provider, &signer.kp, &req.plaintext)
            .map_err(|e| Error::mls("encrypting", e))?;
        tls(&msg)
    }

    /// Processes a handshake or application message. A proposal is queued
    /// in the group at once; the caller can remove it again by reference. A
    /// commit from another member is staged, to merge or discard after the
    /// caller has judged it.
    pub fn process(&mut self, req: ProcessReq) -> Result<ProcessRes> {
        let msg = MlsMessageIn::tls_deserialize_exact(&req.message)
            .map_err(|e| Error::bad(format!("the message doesn't parse: {e}")))?;
        let public = msg.wire_format() == WireFormat::PublicMessage;
        let pm = msg
            .try_into_protocol_message()
            .map_err(|_| Error::bad("the message isn't a PublicMessage or PrivateMessage"))?;
        if pm.group_id().as_slice() != req.group_id.as_slice() {
            return Err(Error::new(
                Code::Invalid,
                "the message is for another group",
            ));
        }
        if self.staged.contains_key(&req.group_id) {
            return Err(Error::new(
                Code::PendingCommit,
                "the group has a staged commit to merge or discard first",
            ));
        }
        let crypto = self.provider.crypto();
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        // OpenMLS lets external commits through whatever the policy says.
        // A group that doesn't accept PublicMessages refuses them too.
        if public
            && group.configuration().wire_format_policy().incoming()
                == IncomingWireFormatPolicy::AlwaysCiphertext
        {
            return Err(Error::new(
                Code::Invalid,
                "the group doesn't accept PublicMessages",
            ));
        }
        let processed = group
            .process_message(&self.provider, pm)
            .map_err(|e| Error::mls("processing the message", e))?;
        let mut res = ProcessRes {
            kind: "",
            epoch: processed.epoch().as_u64(),
            sender: sender_info(processed.sender()),
            identity: processed.credential().serialized_content().to_vec(),
            authenticated_data: processed.aad().to_vec(),
            plaintext: None,
            proposal: None,
            commit: None,
        };
        match processed.into_content() {
            ProcessedMessageContent::ApplicationMessage(m) => {
                res.kind = "application";
                res.plaintext = Some(m.into_bytes());
            }
            ProcessedMessageContent::ProposalMessage(p) => {
                res.kind = "proposal";
                res.proposal = Some(proposal_info(crypto, &p)?);
                group
                    .store_pending_proposal(self.provider.storage(), *p)
                    .map_err(|e| {
                        Error::new(Code::Internal, format!("queuing the proposal: {e}"))
                    })?;
            }
            ProcessedMessageContent::ExternalJoinProposalMessage(p) => {
                res.kind = "external_join_proposal";
                res.proposal = Some(proposal_info(crypto, &p)?);
            }
            ProcessedMessageContent::StagedCommitMessage(c) => {
                res.kind = "commit";
                res.commit = Some(staged_info(crypto, &c)?);
                self.staged.insert(req.group_id.clone(), *c);
            }
            ProcessedMessageContent::OwnPendingCommit
            | ProcessedMessageContent::OwnPrivateMessage => {
                res.kind = "own";
            }
            #[allow(unreachable_patterns)]
            _ => return Err(Error::new(Code::Invalid, "unsupported message content")),
        }
        Ok(res)
    }

    // === Exports ===

    pub fn export_secret(&mut self, req: ExportReq) -> Result<Vec<u8>> {
        let crypto = self.provider.crypto();
        let group = load(&mut self.groups, &self.provider, &req.group_id)?;
        let out = if req.pending {
            group
                .pending_commit()
                .ok_or_else(|| Error::new(Code::NotFound, "the group has no pending commit"))?
                .export_secret(crypto, &req.label, &req.context, req.length)
        } else {
            group.export_secret(crypto, &req.label, &req.context, req.length)
        };
        out.map_err(|e| Error::mls("exporting a secret", e))
    }

    pub fn export_group_info(
        &mut self,
        signer: &Signer,
        id: &[u8],
        with_tree: bool,
    ) -> Result<Vec<u8>> {
        let crypto = self.provider.crypto();
        let group = load(&mut self.groups, &self.provider, id)?;
        let gi = group
            .export_group_info(crypto, &signer.kp, with_tree)
            .map_err(|e| Error::mls("exporting the GroupInfo", e))?;
        tls(&gi)
    }

    /// The ratchet tree, encoded as the `ratchet_tree` extension's data
    /// (RFC 9420 §12.4.3.3).
    pub fn export_ratchet_tree(&mut self, id: &[u8]) -> Result<Vec<u8>> {
        let group = load(&mut self.groups, &self.provider, id)?;
        tls(&group.export_ratchet_tree())
    }

    fn take_group(&mut self, id: &[u8]) -> Result<MlsGroup> {
        load(&mut self.groups, &self.provider, id)?;
        Ok(self.groups.remove(id).expect("loaded"))
    }
}

// === Stateless helpers ===

/// Validates a KeyPackage's signature and lifetime, and describes it.
pub fn inspect_key_package(b: &[u8]) -> Result<KeyPackageInfo> {
    let p = Provider::default();
    let kp = validate_key_package(&p, b)?;
    let raw = tls(&kp)?;
    key_package_info(p.crypto(), &kp, raw).map(|(info, _)| info)
}

/// Reads a GroupInfo without verifying it. Verification needs the ratchet
/// tree and happens when joining.
pub fn inspect_group_info(b: &[u8]) -> Result<GroupInfoInspect> {
    let info = match MlsMessageIn::tls_deserialize_exact(b)
        .map_err(|e| Error::bad(format!("the GroupInfo doesn't parse: {e}")))?
        .extract()
    {
        MlsMessageBodyIn::GroupInfo(g) => g,
        _ => return Err(Error::bad("the message isn't a GroupInfo")),
    };
    let ctx = info.group_context();
    Ok(GroupInfoInspect {
        ciphersuite: u16::from(ctx.ciphersuite()),
        group_id: ctx.group_id().as_slice().to_vec(),
        epoch: ctx.epoch().as_u64(),
        tree_hash: ctx.tree_hash().to_vec(),
        extensions: exts_out(ctx.extensions().iter())?,
        group_info_extensions: exts_out(info.extensions().iter())?,
    })
}

/// Reads the unencrypted header of an MLSMessage.
pub fn inspect_message(b: &[u8]) -> Result<MessageInfo> {
    let msg = MlsMessageIn::tls_deserialize_exact(b)
        .map_err(|e| Error::bad(format!("the message doesn't parse: {e}")))?;
    let wf = msg.wire_format();
    let wire_format = match wf {
        WireFormat::PublicMessage => "public_message",
        WireFormat::PrivateMessage => "private_message",
        WireFormat::Welcome => "welcome",
        WireFormat::GroupInfo => "group_info",
        WireFormat::KeyPackage => "key_package",
        #[allow(unreachable_patterns)]
        _ => "other",
    };
    let mut info = MessageInfo {
        wire_format,
        group_id: None,
        epoch: None,
        content_type: None,
    };
    if let Ok(pm) = msg.try_into_protocol_message() {
        info.group_id = Some(pm.group_id().as_slice().to_vec());
        info.epoch = Some(pm.epoch().as_u64());
        info.content_type = Some(match pm.content_type() {
            ContentType::Application => "application",
            ContentType::Proposal => "proposal",
            ContentType::Commit => "commit",
        });
    }
    Ok(info)
}

fn load<'a>(
    groups: &'a mut HashMap<Vec<u8>, MlsGroup>,
    p: &Provider,
    id: &[u8],
) -> Result<&'a mut MlsGroup> {
    if !groups.contains_key(id) {
        let g = MlsGroup::load(p.storage(), &GroupId::from_slice(id))
            .map_err(|e| Error::new(Code::Internal, format!("loading the group: {e}")))?
            .ok_or_else(|| Error::new(Code::NotFound, "no group has that ID"))?;
        groups.insert(id.to_vec(), g);
    }
    Ok(groups.get_mut(id).expect("inserted"))
}

fn tls(v: &impl tls_codec::Serialize) -> Result<Vec<u8>> {
    v.tls_serialize_detached()
        .map_err(|e| Error::new(Code::Internal, format!("encoding: {e}")))
}

fn caps(c: &Caps) -> Capabilities {
    let exts: Vec<ExtensionType> = c
        .extensions
        .iter()
        .map(|t| ExtensionType::from(*t))
        .collect();
    let props: Vec<ProposalType> = c.proposals.iter().map(|t| ProposalType::from(*t)).collect();
    let creds: Vec<CredentialType> = if c.credentials.is_empty() {
        vec![CredentialType::Basic]
    } else {
        c.credentials
            .iter()
            .map(|t| CredentialType::from(*t))
            .collect()
    };
    Capabilities::new(
        None,
        Some(&[SUITE]),
        Some(&exts),
        Some(&props),
        Some(&creds),
    )
}

/// Builds typed extensions from their type and data. Each one goes through
/// its TLS encoding, so a known type such as `required_capabilities`
/// becomes the extension OpenMLS enforces, and any other is kept as opaque
/// bytes.
fn extensions<T>(list: &[Ext]) -> Result<Extensions<T>>
where
    Extensions<T>: ExtensionsFromVec,
{
    let mut out = Vec::with_capacity(list.len());
    for e in list {
        let mut buf = e.ty.to_be_bytes().to_vec();
        VLBytes::new(e.data.clone())
            .tls_serialize(&mut buf)
            .map_err(|err| Error::bad(format!("extension {:#06x}: {err}", e.ty)))?;
        let ext = Extension::tls_deserialize_exact(&buf)
            .map_err(|err| Error::bad(format!("extension {:#06x} doesn't parse: {err}", e.ty)))?;
        out.push(ext);
    }
    <Extensions<T> as ExtensionsFromVec>::build(out)
}

/// `Extensions::from_vec` for each context it's used in.
pub trait ExtensionsFromVec: Sized {
    fn build(v: Vec<Extension>) -> Result<Self>;
}

macro_rules! from_vec {
    ($($t:ty),*) => {$(
        impl ExtensionsFromVec for Extensions<$t> {
            fn build(v: Vec<Extension>) -> Result<Self> {
                Extensions::<$t>::from_vec(v).map_err(|e| Error::bad(format!("bad extensions: {e}")))
            }
        }
    )*};
}
from_vec!(LeafNode, KeyPackage, GroupContext);

fn ext_out(e: &Extension) -> Result<Ext> {
    let b = tls(e)?;
    let ty = u16::from_be_bytes([b[0], b[1]]);
    let data = VLBytes::tls_deserialize_exact(&b[2..])
        .map_err(|err| Error::new(Code::Internal, format!("decoding an extension: {err}")))?;
    Ok(Ext {
        ty,
        data: data.as_slice().to_vec(),
    })
}

fn exts_out<'a>(it: impl Iterator<Item = &'a Extension>) -> Result<Vec<Ext>> {
    it.map(ext_out).collect()
}

/// A two-byte TLS value, such as a protocol version, as a number.
fn tls_u16(v: &impl tls_codec::Serialize) -> u16 {
    match v.tls_serialize_detached() {
        Ok(b) if b.len() == 2 => u16::from_be_bytes([b[0], b[1]]),
        _ => 0,
    }
}

/// The contents of a value TLS-encoded as one variable-length vector.
fn vl_bytes(v: &impl tls_codec::Serialize) -> Result<Vec<u8>> {
    let b = tls(v)?;
    VLBytes::tls_deserialize_exact(&b)
        .map(|v| v.as_slice().to_vec())
        .map_err(|e| Error::new(Code::Internal, format!("decoding a key: {e}")))
}

fn caps_info(c: &Capabilities) -> CapsInfo {
    CapsInfo {
        versions: c.versions().iter().map(|v| tls_u16(v)).collect(),
        ciphersuites: c.ciphersuites().iter().map(|s| s.value()).collect(),
        extensions: c.extensions().iter().map(|t| u16::from(*t)).collect(),
        proposals: c.proposals().iter().map(|t| u16::from(*t)).collect(),
        credentials: c.credentials().iter().map(|t| u16::from(*t)).collect(),
    }
}

fn leaf_info(l: &LeafNode) -> Result<LeafInfo> {
    Ok(LeafInfo {
        identity: l.credential().serialized_content().to_vec(),
        credential_type: u16::from(l.credential().credential_type()),
        signature_key: l.signature_key().as_slice().to_vec(),
        encryption_key: vl_bytes(l.encryption_key())?,
        capabilities: caps_info(l.capabilities()),
        extensions: exts_out(l.extensions().iter())?,
    })
}

fn sender_info(s: &Sender) -> SenderInfo {
    match s {
        Sender::Member(i) => SenderInfo {
            kind: "member",
            index: i.u32(),
        },
        Sender::External(i) => SenderInfo {
            kind: "external",
            index: i
                .tls_serialize_detached()
                .ok()
                .and_then(|b| b.try_into().ok())
                .map(u32::from_be_bytes)
                .unwrap_or(0),
        },
        Sender::NewMemberProposal => SenderInfo {
            kind: "new_member_proposal",
            index: 0,
        },
        Sender::NewMemberCommit => SenderInfo {
            kind: "new_member_commit",
            index: 0,
        },
    }
}

fn key_package_info(
    crypto: &impl OpenMlsCrypto,
    kp: &KeyPackage,
    raw: Vec<u8>,
) -> Result<(KeyPackageInfo, Vec<u8>)> {
    let lt = kp.life_time();
    Ok((
        KeyPackageInfo {
            ciphersuite: u16::from(kp.ciphersuite()),
            hash_ref: kp
                .hash_ref(crypto)
                .map_err(|e| Error::mls("hashing a key package", e))?
                .as_slice()
                .to_vec(),
            last_resort: kp.last_resort(),
            not_before: lt.not_before(),
            not_after: lt.not_after(),
            leaf: leaf_info(kp.leaf_node())?,
            extensions: exts_out(kp.extensions().iter())?,
        },
        raw,
    ))
}

fn proposal_info(crypto: &impl OpenMlsCrypto, p: &QueuedProposal) -> Result<ProposalInfo> {
    let mut info = ProposalInfo {
        kind: "other",
        sender: sender_info(p.sender()),
        reference: p.proposal_reference_ref().as_slice().to_vec(),
        by_reference: matches!(p.proposal_or_ref_type(), ProposalOrRefType::Reference),
        key_package: None,
        key_package_bytes: None,
        removed: None,
        leaf: None,
        extensions: None,
    };
    match p.proposal() {
        Proposal::Add(a) => {
            info.kind = "add";
            let kp = a.key_package();
            let (ki, raw) = key_package_info(crypto, kp, tls(kp)?)?;
            info.key_package = Some(ki);
            info.key_package_bytes = Some(raw);
        }
        Proposal::Update(u) => {
            info.kind = "update";
            info.leaf = Some(leaf_info(u.leaf_node())?);
        }
        Proposal::Remove(r) => {
            info.kind = "remove";
            info.removed = Some(r.removed().u32());
        }
        Proposal::GroupContextExtensions(g) => {
            info.kind = "group_context_extensions";
            info.extensions = Some(exts_out(g.extensions().iter())?);
        }
        Proposal::PreSharedKey(_) => info.kind = "psk",
        Proposal::ReInit(_) => info.kind = "reinit",
        Proposal::ExternalInit(_) => info.kind = "external_init",
        Proposal::SelfRemove => info.kind = "self_remove",
        #[allow(unreachable_patterns)]
        _ => {}
    }
    Ok(info)
}

fn staged_info(crypto: &impl OpenMlsCrypto, c: &StagedCommit) -> Result<StagedInfo> {
    let ctx = c.group_context();
    Ok(StagedInfo {
        proposals: c
            .queued_proposals()
            .map(|p| proposal_info(crypto, p))
            .collect::<Result<Vec<_>>>()?,
        path_leaf: c.update_path_leaf_node().map(leaf_info).transpose()?,
        self_removed: c.self_removed(),
        new_epoch: ctx.epoch().as_u64(),
        extensions: exts_out(ctx.extensions().iter())?,
        epoch_authenticator: c.epoch_authenticator().map(|a| a.as_slice().to_vec()),
    })
}

fn validate_key_package(p: &Provider, b: &[u8]) -> Result<KeyPackage> {
    // Accept a bare KeyPackage, or one inside an MLSMessage.
    let kp_in = match KeyPackageIn::tls_deserialize_exact(b) {
        Ok(kp) => kp,
        Err(_) => match MlsMessageIn::tls_deserialize_exact(b)
            .map_err(|e| Error::bad(format!("the key package doesn't parse: {e}")))?
            .extract()
        {
            MlsMessageBodyIn::KeyPackage(kp) => kp,
            _ => return Err(Error::bad("the message isn't a key package")),
        },
    };
    let kp = kp_in
        .validate(p.crypto(), ProtocolVersion::Mls10)
        .map_err(|e| Error::mls("the key package is invalid", e))?;
    if kp.ciphersuite() != SUITE {
        return Err(Error::new(
            Code::Invalid,
            "the key package uses another ciphersuite",
        ));
    }
    Ok(kp)
}

fn ratchet_tree(b: &[u8]) -> Result<Option<RatchetTreeIn>> {
    if b.is_empty() {
        return Ok(None);
    }
    RatchetTreeIn::tls_deserialize_exact(b)
        .map(Some)
        .map_err(|e| Error::bad(format!("the ratchet tree doesn't parse: {e}")))
}

fn wire_format_policy(c: &GroupConfig) -> WireFormatPolicy {
    if c.accept_public_messages {
        MIXED_CIPHERTEXT_WIRE_FORMAT_POLICY
    } else {
        PURE_CIPHERTEXT_WIRE_FORMAT_POLICY
    }
}

fn ratchet_config(c: &GroupConfig) -> SenderRatchetConfiguration {
    let d = SenderRatchetConfiguration::default();
    SenderRatchetConfiguration::new(
        if c.out_of_order_tolerance > 0 {
            c.out_of_order_tolerance
        } else {
            d.out_of_order_tolerance()
        },
        if c.maximum_forward_distance > 0 {
            c.maximum_forward_distance
        } else {
            d.maximum_forward_distance()
        },
    )
}

fn join_config(c: &GroupConfig) -> MlsGroupJoinConfig {
    MlsGroupJoinConfig::builder()
        .use_ratchet_tree_extension(c.ratchet_tree_in_welcome)
        .wire_format_policy(wire_format_policy(c))
        .max_past_epochs(c.max_past_epochs)
        .padding_size(c.padding)
        .sender_ratchet_configuration(ratchet_config(c))
        .build()
}

fn create_config(req: &CreateReq) -> Result<MlsGroupCreateConfig> {
    let c = &req.config;
    Ok(MlsGroupCreateConfig::builder()
        .ciphersuite(SUITE)
        .use_ratchet_tree_extension(c.ratchet_tree_in_welcome)
        .wire_format_policy(wire_format_policy(c))
        .max_past_epochs(c.max_past_epochs)
        .padding_size(c.padding)
        .sender_ratchet_configuration(ratchet_config(c))
        .capabilities(caps(&req.leaf.capabilities))
        .with_group_context_extensions(extensions::<GroupContext>(&req.extensions)?)
        .with_leaf_node_extensions(extensions::<LeafNode>(&req.leaf.extensions)?)
        .map_err(|e| Error::bad(format!("bad leaf extensions: {e}")))?
        .build())
}
