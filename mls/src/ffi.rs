//! The C interface. Every function returns 0 on success, or an error code
//! from `error::Code`. On success, `out` holds the result; on failure, it
//! holds the error message in UTF-8. The caller frees `out` with
//! `trmls_buf_free`.
//!
//! A client handle isn't safe to use from two threads at once. A signer
//! handle is read-only, so it is.

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::ptr;

use serde::de::DeserializeOwned;
use serde::Serialize;

use crate::client::{self, Client, Signer, SUITE};
use crate::error::{Code, Error, Result};
use crate::types::*;

/// A byte buffer the library allocated.
#[repr(C)]
pub struct TrmlsBuf {
    pub ptr: *mut u8,
    pub len: usize,
}

impl TrmlsBuf {
    fn from_vec(v: Vec<u8>) -> TrmlsBuf {
        let b = v.into_boxed_slice();
        let len = b.len();
        let ptr = Box::into_raw(b) as *mut u8;
        TrmlsBuf { ptr, len }
    }
}

/// Frees a buffer the library returned. A buffer with a null pointer is
/// ignored.
#[no_mangle]
pub unsafe extern "C" fn trmls_buf_free(buf: TrmlsBuf) {
    if !buf.ptr.is_null() {
        drop(Box::from_raw(ptr::slice_from_raw_parts_mut(
            buf.ptr, buf.len,
        )));
    }
}

/// The ciphersuite's code point, `0x004F` (§11.1).
#[no_mangle]
pub extern "C" fn trmls_ciphersuite() -> u16 {
    u16::from(SUITE)
}

unsafe fn input<'a>(p: *const u8, len: usize) -> &'a [u8] {
    if p.is_null() || len == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(p, len)
    }
}

fn json<T: DeserializeOwned>(b: &[u8]) -> Result<T> {
    serde_json::from_slice(b).map_err(|e| Error::bad(format!("bad request: {e}")))
}

fn to_json(v: &impl Serialize) -> Result<Vec<u8>> {
    serde_json::to_vec(v).map_err(|e| Error::new(Code::Internal, e.to_string()))
}

/// Runs `f`, catching panics, and writes its result or error to `out`.
unsafe fn run(out: *mut TrmlsBuf, f: impl FnOnce() -> Result<Vec<u8>>) -> i32 {
    let r = catch_unwind(AssertUnwindSafe(f)).unwrap_or_else(|p| {
        let msg = p
            .downcast_ref::<&str>()
            .map(|s| s.to_string())
            .or_else(|| p.downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "unknown".into());
        Err(Error::new(Code::Internal, format!("panic: {msg}")))
    });
    let (code, bytes) = match r {
        Ok(v) => (0, v),
        Err(e) => (e.code as i32, e.msg.into_bytes()),
    };
    if !out.is_null() {
        *out = TrmlsBuf::from_vec(bytes);
    }
    code
}

unsafe fn client<'a>(c: *mut Client) -> Result<&'a mut Client> {
    c.as_mut().ok_or_else(|| Error::bad("the client is null"))
}

unsafe fn signer<'a>(s: *const Signer) -> Result<&'a Signer> {
    s.as_ref().ok_or_else(|| Error::bad("the signer is null"))
}

// === Signers ===

/// Makes a signer from a 32-byte Ed25519 private key.
#[no_mangle]
pub unsafe extern "C" fn trmls_signer_new(
    seed: *const u8,
    len: usize,
    s: *mut *mut Signer,
    out: *mut TrmlsBuf,
) -> i32 {
    let seed = input(seed, len);
    run(out, || {
        if s.is_null() {
            return Err(Error::bad("the signer pointer is null"));
        }
        let signer = Signer::from_seed(seed)?;
        *s = Box::into_raw(Box::new(signer));
        Ok(Vec::new())
    })
}

/// Writes the signer's 32-byte Ed25519 public key to `out`.
#[no_mangle]
pub unsafe extern "C" fn trmls_signer_public_key(s: *const Signer, out: *mut TrmlsBuf) -> i32 {
    run(out, || Ok(signer(s)?.public_key().to_vec()))
}

#[no_mangle]
pub unsafe extern "C" fn trmls_signer_free(s: *mut Signer) {
    if !s.is_null() {
        drop(Box::from_raw(s));
    }
}

// === Clients ===

/// Opens a client from a snapshot. An empty snapshot makes a new client.
#[no_mangle]
pub unsafe extern "C" fn trmls_client_open(
    snap: *const u8,
    len: usize,
    c: *mut *mut Client,
    out: *mut TrmlsBuf,
) -> i32 {
    let snap = input(snap, len);
    run(out, || {
        if c.is_null() {
            return Err(Error::bad("the client pointer is null"));
        }
        *c = Box::into_raw(Box::new(Client::open(snap)?));
        Ok(Vec::new())
    })
}

#[no_mangle]
pub unsafe extern "C" fn trmls_client_free(c: *mut Client) {
    if !c.is_null() {
        drop(Box::from_raw(c));
    }
}

#[no_mangle]
pub unsafe extern "C" fn trmls_client_snapshot(c: *mut Client, out: *mut TrmlsBuf) -> i32 {
    run(out, || Ok(client(c)?.snapshot()))
}

#[no_mangle]
pub unsafe extern "C" fn trmls_client_take_changes(c: *mut Client, out: *mut TrmlsBuf) -> i32 {
    run(out, || {
        let changes = client(c)?
            .take_changes()
            .into_iter()
            .map(|ch| ChangeOut {
                key: ch.key,
                value: ch.value,
            })
            .collect();
        to_json(&ChangesRes { changes })
    })
}

#[no_mangle]
pub unsafe extern "C" fn trmls_client_revert(c: *mut Client, out: *mut TrmlsBuf) -> i32 {
    run(out, || {
        client(c)?.revert();
        Ok(Vec::new())
    })
}

// === Operations ===

macro_rules! op {
    // Raw bytes, such as a group ID.
    ($name:ident, bytes, |$cl:ident, $b:ident| $body:expr) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(
            c: *mut Client,
            b: *const u8,
            len: usize,
            out: *mut TrmlsBuf,
        ) -> i32 {
            let $b = input(b, len);
            run(out, || {
                let $cl = client(c)?;
                $body
            })
        }
    };
    // A JSON request, with a signer.
    ($name:ident, signer, $req:ty, |$cl:ident, $s:ident, $r:ident| $body:expr) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(
            c: *mut Client,
            s: *const Signer,
            req: *const u8,
            len: usize,
            out: *mut TrmlsBuf,
        ) -> i32 {
            let req = input(req, len);
            run(out, || {
                let $cl = client(c)?;
                let $s = signer(s)?;
                let $r: $req = json(req)?;
                $body
            })
        }
    };
    // A JSON request, without a signer.
    ($name:ident, $req:ty, |$cl:ident, $r:ident| $body:expr) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(
            c: *mut Client,
            req: *const u8,
            len: usize,
            out: *mut TrmlsBuf,
        ) -> i32 {
            let req = input(req, len);
            run(out, || {
                let $cl = client(c)?;
                let $r: $req = json(req)?;
                $body
            })
        }
    };
}

op!(trmls_key_package, signer, KeyPackageReq, |c, s, r| to_json(
    &c.key_package(s, r)?
));
op!(trmls_delete_key_package, bytes, |c, b| c
    .delete_key_package(b)
    .map(|_| Vec::new()));
op!(trmls_create_group, signer, CreateReq, |c, s, r| c
    .create_group(s, r)
    .map(|_| Vec::new()));
op!(trmls_join_welcome, WelcomeReq, |c, r| to_json(
    &c.join_welcome(r)?
));
op!(trmls_join_external, signer, ExternalJoinReq, |c, s, r| {
    to_json(&c.join_external(s, r)?)
});
op!(trmls_delete_group, bytes, |c, b| c
    .delete_group(b)
    .map(|_| Vec::new()));
op!(trmls_group_info, bytes, |c, b| to_json(&c.group_info(b)?));
op!(trmls_merge_pending_commit, bytes, |c, b| c
    .merge_pending_commit(b)
    .map(|_| Vec::new()));
op!(trmls_clear_pending_commit, bytes, |c, b| c
    .clear_pending_commit(b)
    .map(|_| Vec::new()));
op!(trmls_merge_staged_commit, bytes, |c, b| c
    .merge_staged_commit(b)
    .map(|_| Vec::new()));
op!(trmls_discard_staged_commit, bytes, |c, b| {
    c.discard_staged_commit(b);
    Ok(Vec::new())
});
op!(trmls_propose_remove, signer, ProposeRemoveReq, |c, s, r| {
    to_json(&c.propose_remove(s, r)?)
});
op!(trmls_remove_pending_proposal, GroupReq, |c, r| c
    .remove_pending_proposal(&r.group_id, &r.reference)
    .map(|_| Vec::new()));
op!(trmls_clear_pending_proposals, bytes, |c, b| c
    .clear_pending_proposals(b)
    .map(|_| Vec::new()));
op!(trmls_set_config, SetConfigReq, |c, r| c
    .set_config(r)
    .map(|_| Vec::new()));
op!(trmls_encrypt, signer, EncryptReq, |c, s, r| c.encrypt(s, r));
op!(trmls_process, ProcessReq, |c, r| to_json(&c.process(r)?));
op!(trmls_export_secret, ExportReq, |c, r| c.export_secret(r));
op!(trmls_export_group_info, signer, GroupReq, |c, s, r| c
    .export_group_info(s, &r.group_id, r.with_ratchet_tree));
op!(trmls_export_ratchet_tree, bytes, |c, b| c
    .export_ratchet_tree(b));

/// Commits. `new_signer` may be null; when set, the commit's update path
/// moves this device's leaf to the new signature key, whose credential
/// identity is the request's `leaf.identity` (§3.2.3).
#[no_mangle]
pub unsafe extern "C" fn trmls_commit(
    c: *mut Client,
    s: *const Signer,
    new_signer: *const Signer,
    req: *const u8,
    len: usize,
    out: *mut TrmlsBuf,
) -> i32 {
    let req = input(req, len);
    run(out, || {
        let cl = client(c)?;
        let s = signer(s)?;
        let r: CommitReq = json(req)?;
        to_json(&cl.commit(s, new_signer.as_ref(), r)?)
    })
}

// === Inspecting bytes, without a client ===

macro_rules! inspect {
    ($name:ident, $f:path) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(b: *const u8, len: usize, out: *mut TrmlsBuf) -> i32 {
            let b = input(b, len);
            run(out, || to_json(&$f(b)?))
        }
    };
}

inspect!(trmls_inspect_key_package, client::inspect_key_package);
inspect!(trmls_inspect_group_info, client::inspect_group_info);
inspect!(trmls_inspect_message, client::inspect_message);
