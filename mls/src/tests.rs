//! End-to-end tests of the client operations, with two or three members.

use crate::client::{
    inspect_group_info, inspect_key_package, inspect_message, Client, Signer, SUITE,
};
use crate::error::Code;
use crate::types::*;

const GROUP: &[u8; 32] = b"railcar test group 0000000000000";

fn leaf(name: &str) -> LeafSpec {
    LeafSpec {
        identity: name.as_bytes().to_vec(),
        capabilities: Caps {
            extensions: vec![0xF0A1, 0xF0A2],
            ..Default::default()
        },
        extensions: vec![Ext {
            ty: 0xF0A2,
            data: format!("msg-key of {name}").into_bytes(),
        }],
    }
}

fn chat() -> GroupConfig {
    GroupConfig {
        ratchet_tree_in_welcome: true,
        max_past_epochs: 2,
        ..Default::default()
    }
}

/// `required_capabilities` listing the given extension types (RFC 9420
/// §7.2), with no proposal or credential types.
fn required(exts: &[u16]) -> Ext {
    let mut data = vec![(exts.len() * 2) as u8];
    for e in exts {
        data.extend_from_slice(&e.to_be_bytes());
    }
    data.extend_from_slice(&[0, 0]);
    Ext { ty: 0x0003, data }
}

struct Device {
    c: Client,
    s: Signer,
    name: &'static str,
}

impl Device {
    fn new(name: &'static str, seed: u8) -> Device {
        Device {
            c: Client::open(&[]).unwrap(),
            s: Signer::from_seed(&[seed; 32]).unwrap(),
            name,
        }
    }

    fn key_package(&mut self, last_resort: bool) -> KeyPackageRes {
        self.c
            .key_package(
                &self.s,
                KeyPackageReq {
                    leaf: leaf(self.name),
                    last_resort,
                    lifetime: 0,
                    extensions: vec![],
                },
            )
            .unwrap()
    }

    fn send(&mut self, text: &str) -> Vec<u8> {
        self.c
            .encrypt(
                &self.s,
                EncryptReq {
                    group_id: GROUP.to_vec(),
                    plaintext: text.into(),
                    aad: vec![],
                },
            )
            .unwrap()
    }

    fn process(&mut self, msg: &[u8]) -> crate::error::Result<ProcessRes> {
        self.c.process(ProcessReq {
            group_id: GROUP.to_vec(),
            message: msg.to_vec(),
        })
    }

    fn export(&mut self, pending: bool) -> Vec<u8> {
        self.c
            .export_secret(ExportReq {
                group_id: GROUP.to_vec(),
                label: "tieroute hub".into(),
                context: GROUP.to_vec(),
                length: 32,
                pending,
            })
            .unwrap()
    }

    fn epoch(&mut self) -> u64 {
        self.c.group_info(GROUP).unwrap().epoch
    }
}

fn commit(d: &mut Device, req: CommitReq) -> CommitRes {
    d.c.commit(&d.s, None, req).unwrap()
}

fn base(adds: Vec<Vec<u8>>, removes: Vec<u32>) -> CommitReq {
    CommitReq {
        group_id: GROUP.to_vec(),
        adds,
        removes,
        extensions: None,
        pending_proposals: true,
        update_path: false,
        leaf: None,
        group_info: false,
        aad: vec![],
    }
}

fn create(d: &mut Device) {
    d.c.create_group(
        &d.s,
        CreateReq {
            group_id: GROUP.to_vec(),
            leaf: leaf(d.name),
            extensions: vec![
                required(&[0xF0A1]),
                Ext {
                    ty: 0xF0A1,
                    data: b"{\"kind\":\"group\"}".to_vec(),
                },
            ],
            config: chat(),
        },
    )
    .unwrap();
}

#[test]
fn ciphersuite_is_pinned() {
    assert_eq!(u16::from(SUITE), 0x004F);
    assert_eq!(
        format!("{SUITE:?}"),
        "MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519"
    );
}

#[test]
fn key_packages() {
    let mut kat = Device::new("kat", 1);
    let kp = kat.key_package(false);
    let info = inspect_key_package(&kp.key_package).unwrap();
    assert_eq!(info.ciphersuite, 0x004F);
    assert_eq!(info.leaf.identity, b"kat");
    assert_eq!(info.leaf.signature_key, kat.s.public_key());
    assert_eq!(info.leaf.credential_type, 1);
    assert!(info.leaf.capabilities.extensions.contains(&0xF0A2));
    assert_eq!(info.leaf.capabilities.ciphersuites, vec![0x004F]);
    assert!(info
        .leaf
        .extensions
        .iter()
        .any(|e| e.ty == 0xF0A2 && e.data == b"msg-key of kat"));
    assert_eq!(info.hash_ref, kp.hash_ref);
    assert!(!info.last_resort);
    // The MLSMessage form describes the same key package.
    assert_eq!(
        inspect_key_package(&kp.message).unwrap().hash_ref,
        kp.hash_ref
    );
    assert_eq!(
        inspect_message(&kp.message).unwrap().wire_format,
        "key_package"
    );

    let lr = kat.key_package(true);
    assert!(inspect_key_package(&lr.key_package).unwrap().last_resort);

    let mut bad = kp.key_package.clone();
    let n = bad.len();
    bad[n - 1] ^= 1;
    assert!(inspect_key_package(&bad).is_err());
}

#[test]
fn three_members_end_to_end() {
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    let mut amara = Device::new("amara", 3);
    create(&mut kat);
    assert_eq!(kat.epoch(), 0);

    // Kat adds Ravi and Amara.
    let kps = vec![
        ravi.key_package(false).key_package,
        amara.key_package(true).key_package,
    ];
    let res = commit(&mut kat, base(kps, vec![]));
    assert_eq!(res.epoch, 0);
    let pending_secret = kat.export(true);
    kat.c.merge_pending_commit(GROUP).unwrap();
    assert_eq!(
        kat.export(false),
        pending_secret,
        "the pending export is the next epoch's"
    );
    let welcome = res.welcome.unwrap();
    assert_eq!(inspect_message(&welcome).unwrap().wire_format, "welcome");
    for d in [&mut ravi, &mut amara] {
        let w =
            d.c.join_welcome(WelcomeReq {
                welcome: welcome.clone(),
                ratchet_tree: vec![],
                config: chat(),
            })
            .unwrap();
        assert_eq!(w.group_id, GROUP);
        assert_eq!(w.epoch, 1);
        assert_eq!(w.sender_leaf_index, 0);
    }

    // Everyone agrees on the epoch, the members, and the exporter.
    let info = ravi.c.group_info(GROUP).unwrap();
    assert_eq!(info.members.len(), 3);
    assert_eq!(info.own_leaf_index, 1);
    assert!(info.extensions.iter().any(|e| e.ty == 0xF0A1));
    let kat_info = kat.c.group_info(GROUP).unwrap();
    assert_eq!(info.epoch_authenticator, kat_info.epoch_authenticator);
    assert_eq!(info.members[2].leaf.identity, b"amara");
    assert_eq!(info.members[2].leaf.signature_key, amara.s.public_key());
    let secret = kat.export(false);
    assert_eq!(ravi.export(false), secret);
    assert_eq!(amara.export(false), secret);

    // Messages both ways, with authenticated data.
    let m = kat
        .c
        .encrypt(
            &kat.s,
            EncryptReq {
                group_id: GROUP.to_vec(),
                plaintext: b"hi".to_vec(),
                aad: b"ad".to_vec(),
            },
        )
        .unwrap();
    let mi = inspect_message(&m).unwrap();
    assert_eq!(
        (mi.wire_format, mi.epoch, mi.content_type),
        ("private_message", Some(1), Some("application"))
    );
    for d in [&mut ravi, &mut amara] {
        let r = d.process(&m).unwrap();
        assert_eq!(r.kind, "application");
        assert_eq!(r.plaintext.as_deref(), Some(&b"hi"[..]));
        assert_eq!(r.identity, b"kat");
        assert_eq!(r.sender.index, 0);
        assert_eq!(r.authenticated_data, b"ad");
        assert_eq!(r.epoch, 1);
    }
    let m = ravi.send("hello back");
    assert_eq!(
        kat.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"hello back"[..])
    );
    assert_eq!(amara.process(&m).unwrap().identity, b"ravi");

    // State survives a snapshot and a restore, mid-conversation.
    let snap = ravi.c.snapshot();
    ravi.c = Client::open(&snap).unwrap();
    let m = amara.send("after restore");
    assert_eq!(
        ravi.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"after restore"[..])
    );

    // Kat removes Amara. Ravi stages the commit, inspects it, and merges.
    let res = commit(
        &mut kat,
        CommitReq {
            update_path: true,
            ..base(vec![], vec![2])
        },
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    let r = ravi.process(&res.commit).unwrap();
    assert_eq!(r.kind, "commit");
    let staged = r.commit.as_ref().unwrap();
    assert_eq!(staged.new_epoch, 2);
    assert!(staged.path_leaf.is_some());
    assert!(staged
        .proposals
        .iter()
        .any(|p| p.kind == "remove" && p.removed == Some(2)));
    ravi.c.merge_staged_commit(GROUP).unwrap();
    let r = amara.process(&res.commit).unwrap();
    assert!(r.commit.unwrap().self_removed);
    amara.c.merge_staged_commit(GROUP).unwrap();
    assert!(!amara.c.group_info(GROUP).unwrap().active);

    // Amara can't read what's sent after her removal.
    let m = kat.send("without amara");
    assert_eq!(
        ravi.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"without amara"[..])
    );
    assert!(amara.process(&m).is_err());
    assert_eq!(kat.export(false), ravi.export(false));
    assert_eq!(ravi.c.group_info(GROUP).unwrap().members.len(), 2);
}

#[test]
fn revert_restores_a_used_key() {
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    create(&mut kat);
    let res = commit(
        &mut kat,
        base(vec![ravi.key_package(false).key_package], vec![]),
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    ravi.c
        .join_welcome(WelcomeReq {
            welcome: res.welcome.unwrap(),
            ratchet_tree: vec![],
            config: chat(),
        })
        .unwrap();
    ravi.c.take_changes();

    // Ravi decrypts a commit, refuses it, and reverts: the commit can still
    // be processed later (§11.9).
    let res = commit(
        &mut kat,
        CommitReq {
            update_path: true,
            ..base(vec![], vec![])
        },
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    assert_eq!(ravi.process(&res.commit).unwrap().kind, "commit");
    assert_eq!(
        ravi.process(&res.commit).unwrap_err().code,
        Code::PendingCommit
    );
    ravi.c.discard_staged_commit(GROUP);
    assert!(
        ravi.process(&res.commit).is_err(),
        "the key is used up until a revert"
    );
    ravi.c.revert();
    assert_eq!(ravi.process(&res.commit).unwrap().kind, "commit");
    ravi.c.merge_staged_commit(GROUP).unwrap();
    assert_eq!(ravi.epoch(), 2);
    assert_eq!(kat.export(false), ravi.export(false));

    // Change sets rebuild the same store as a snapshot.
    let changes = ravi.c.take_changes();
    assert!(!changes.is_empty());
    assert!(ravi.c.take_changes().is_empty());
}

#[test]
fn leave_by_proposal_and_wrong_epoch() {
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    create(&mut kat);
    let res = commit(
        &mut kat,
        base(vec![ravi.key_package(false).key_package], vec![]),
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    ravi.c
        .join_welcome(WelcomeReq {
            welcome: res.welcome.unwrap(),
            ratchet_tree: vec![],
            config: chat(),
        })
        .unwrap();

    // Ravi leaves with a standalone Remove proposal for his own leaf (§11.1).
    let p = ravi
        .c
        .propose_remove(
            &ravi.s,
            ProposeRemoveReq {
                group_id: GROUP.to_vec(),
                leaf_index: 1,
                aad: vec![],
            },
        )
        .unwrap();
    let r = kat.process(&p.message).unwrap();
    assert_eq!(r.kind, "proposal");
    let prop = r.proposal.unwrap();
    assert_eq!((prop.kind, prop.removed), ("remove", Some(1)));
    assert_eq!(prop.reference, p.reference);
    assert_eq!(kat.c.group_info(GROUP).unwrap().pending_proposals.len(), 1);
    // OpenMLS refuses application messages while proposals are pending.
    let err = kat
        .c
        .encrypt(
            &kat.s,
            EncryptReq {
                group_id: GROUP.to_vec(),
                plaintext: b"x".to_vec(),
                aad: vec![],
            },
        )
        .unwrap_err();
    assert_eq!(err.code, Code::PendingProposal);

    // Kat commits it by reference.
    let res = commit(&mut kat, base(vec![], vec![]));
    let pending = kat.export(true);
    kat.c.merge_pending_commit(GROUP).unwrap();
    assert_eq!(kat.export(false), pending);
    let r = ravi.process(&res.commit).unwrap();
    let staged = r.commit.unwrap();
    assert!(staged.self_removed);
    assert!(staged
        .proposals
        .iter()
        .any(|p| p.by_reference && p.kind == "remove"));
    assert_eq!(kat.c.group_info(GROUP).unwrap().members.len(), 1);

    // A message from a later epoch is reported as such.
    let mut amara = Device::new("amara", 3);
    let res = commit(
        &mut kat,
        base(vec![amara.key_package(false).key_package], vec![]),
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    let res2 = commit(
        &mut kat,
        CommitReq {
            update_path: true,
            ..base(vec![], vec![])
        },
    );
    amara
        .c
        .join_welcome(WelcomeReq {
            welcome: res.welcome.unwrap(),
            ratchet_tree: vec![],
            config: chat(),
        })
        .unwrap();
    kat.c.merge_pending_commit(GROUP).unwrap();
    let m = kat.send("epoch 4");
    assert_eq!(amara.process(&m).unwrap_err().code, Code::WrongEpoch);
    amara.process(&res2.commit).unwrap();
    amara.c.merge_staged_commit(GROUP).unwrap();
    assert_eq!(
        amara.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"epoch 4"[..])
    );
}

#[test]
fn extensions_and_new_signer() {
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    create(&mut kat);
    let res = commit(
        &mut kat,
        base(vec![ravi.key_package(false).key_package], vec![]),
    );
    kat.c.merge_pending_commit(GROUP).unwrap();
    ravi.c
        .join_welcome(WelcomeReq {
            welcome: res.welcome.unwrap(),
            ratchet_tree: vec![],
            config: chat(),
        })
        .unwrap();

    // Ravi changes the group extension, and moves to a new message key.
    let new = Signer::from_seed(&[9; 32]).unwrap();
    let mut l = leaf("ravi");
    l.extensions = vec![Ext {
        ty: 0xF0A2,
        data: b"msg-key 2".to_vec(),
    }];
    let exts = vec![
        required(&[0xF0A1]),
        Ext {
            ty: 0xF0A1,
            data: b"{\"kind\":\"group\",\"v\":2}".to_vec(),
        },
    ];
    let res = ravi
        .c
        .commit(
            &ravi.s,
            Some(&new),
            CommitReq {
                extensions: Some(exts.clone()),
                leaf: Some(l),
                ..base(vec![], vec![])
            },
        )
        .unwrap();
    ravi.c.merge_pending_commit(GROUP).unwrap();
    ravi.s = new;
    let r = kat.process(&res.commit).unwrap();
    let staged = r.commit.unwrap();
    assert_eq!(
        staged
            .extensions
            .iter()
            .find(|e| e.ty == 0xF0A1)
            .unwrap()
            .data,
        exts[1].data
    );
    let path = staged.path_leaf.unwrap();
    assert_eq!(path.signature_key, ravi.s.public_key());
    assert!(path.extensions.iter().any(|e| e.data == b"msg-key 2"));
    kat.c.merge_staged_commit(GROUP).unwrap();
    let m = ravi.send("new key");
    assert_eq!(
        kat.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"new key"[..])
    );

    // An extension type the group doesn't require is refused in a
    // GroupContextExtensions proposal.
    let bad = vec![
        required(&[0xF0A1]),
        exts[1].clone(),
        Ext {
            ty: 0xF0A9,
            data: vec![1],
        },
    ];
    assert!(kat
        .c
        .commit(
            &kat.s,
            None,
            CommitReq {
                extensions: Some(bad),
                ..base(vec![], vec![])
            }
        )
        .is_err());
}

#[test]
fn external_join_with_separate_tree() {
    let community = GroupConfig {
        accept_public_messages: true,
        max_past_epochs: 2,
        ..Default::default()
    };
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    kat.c
        .create_group(
            &kat.s,
            CreateReq {
                group_id: GROUP.to_vec(),
                leaf: leaf("kat"),
                extensions: vec![
                    required(&[0xF0A1]),
                    Ext {
                        ty: 0xF0A1,
                        data: b"{}".to_vec(),
                    },
                ],
                config: community.clone(),
            },
        )
        .unwrap();

    let gi = kat.c.export_group_info(&kat.s, GROUP, false).unwrap();
    let tree = kat.c.export_ratchet_tree(GROUP).unwrap();
    let insp = inspect_group_info(&gi).unwrap();
    assert_eq!((insp.ciphersuite, insp.epoch), (0x004F, 0));
    assert!(
        insp.group_info_extensions.iter().any(|e| e.ty == 0x0004),
        "external_pub is there"
    );
    assert!(
        !insp.group_info_extensions.iter().any(|e| e.ty == 0x0002),
        "the tree isn't"
    );

    // Without the tree, the join fails; with it, it works.
    let req = |tree: Vec<u8>| ExternalJoinReq {
        group_info: gi.clone(),
        ratchet_tree: tree,
        leaf: leaf("ravi"),
        aad: vec![],
        config: community.clone(),
        group_info_out: true,
    };
    assert!(ravi.c.join_external(&ravi.s, req(vec![])).is_err());
    let j = ravi.c.join_external(&ravi.s, req(tree)).unwrap();
    assert_eq!(j.commit.epoch, 0);
    assert!(j.commit.group_info.is_some());
    assert_eq!(
        inspect_message(&j.commit.commit).unwrap().wire_format,
        "public_message"
    );
    let r = kat.process(&j.commit.commit).unwrap();
    assert_eq!(r.sender.kind, "new_member_commit");
    assert_eq!(r.identity, b"ravi");
    kat.c.merge_staged_commit(GROUP).unwrap();
    assert_eq!(kat.export(false), ravi.export(false));
    let m = ravi.send("joined");
    assert_eq!(
        kat.process(&m).unwrap().plaintext.as_deref(),
        Some(&b"joined"[..])
    );

    // A chat group refuses PublicMessages, so it refuses external joins.
    let mut amara = Device::new("amara", 3);
    let mut lin = Device::new("lin", 4);
    create(&mut amara);
    let gi = amara.c.export_group_info(&amara.s, GROUP, true).unwrap();
    let j = lin
        .c
        .join_external(
            &lin.s,
            ExternalJoinReq {
                group_info: gi,
                leaf: leaf("lin"),
                ..req(vec![])
            },
        )
        .unwrap();
    assert!(amara.process(&j.commit.commit).is_err());
}

#[test]
fn delete_key_package_and_group() {
    let mut kat = Device::new("kat", 1);
    let mut ravi = Device::new("ravi", 2);
    let kp = ravi.key_package(false);
    ravi.c.delete_key_package(&kp.hash_ref).unwrap();
    create(&mut kat);
    let res = commit(&mut kat, base(vec![kp.key_package], vec![]));
    let err = ravi
        .c
        .join_welcome(WelcomeReq {
            welcome: res.welcome.unwrap(),
            ratchet_tree: vec![],
            config: chat(),
        })
        .unwrap_err();
    assert_eq!(err.code, Code::Invalid);
    kat.c.delete_group(GROUP).unwrap();
    assert_eq!(kat.c.group_info(GROUP).unwrap_err().code, Code::NotFound);
}
