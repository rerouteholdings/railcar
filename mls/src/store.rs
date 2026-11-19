//! The provider OpenMLS runs on, and the snapshots and change sets the
//! caller persists.
//!
//! OpenMLS keeps all of its state, groups and private keys alike, in a
//! key-value store. tieroute-mls holds that store in memory, beside a copy
//! of what the caller has persisted (the shadow). The caller either saves
//! the whole store as one snapshot, or takes the entries that changed since
//! the last save. Reverting puts the store back to the shadow, which undoes
//! everything since then, including a decryption that used up a key.

use std::collections::HashMap;

use openmls_memory_storage::MemoryStorage;
use openmls_rust_crypto::RustCrypto;
use openmls_traits::OpenMlsProvider;

use crate::error::{Code, Error, Result};

/// The first bytes of every snapshot, with the format's version.
const MAGIC: &[u8; 8] = b"trmls\x00\x00\x01";

pub type Values = HashMap<Vec<u8>, Vec<u8>>;

/// The OpenMLS provider: RustCrypto for cryptography and randomness, and an
/// in-memory store.
#[derive(Default)]
pub struct Provider {
    crypto: RustCrypto,
    storage: MemoryStorage,
}

impl OpenMlsProvider for Provider {
    type CryptoProvider = RustCrypto;
    type RandProvider = RustCrypto;
    type StorageProvider = MemoryStorage;

    fn storage(&self) -> &MemoryStorage {
        &self.storage
    }

    fn crypto(&self) -> &RustCrypto {
        &self.crypto
    }

    fn rand(&self) -> &RustCrypto {
        &self.crypto
    }
}

impl Provider {
    pub fn with_values(values: Values) -> Self {
        let p = Provider::default();
        *p.storage.values.write().unwrap() = values;
        p
    }

    pub fn values(&self) -> std::sync::RwLockReadGuard<'_, Values> {
        self.storage.values.read().unwrap()
    }

    pub fn values_mut(&self) -> std::sync::RwLockWriteGuard<'_, Values> {
        self.storage.values.write().unwrap()
    }
}

/// One changed entry. `value` is `None` when the entry was deleted.
pub struct Change {
    pub key: Vec<u8>,
    pub value: Option<Vec<u8>>,
}

/// Lists the entries of `current` that differ from `shadow`.
pub fn diff(current: &Values, shadow: &Values) -> Vec<Change> {
    let mut out = Vec::new();
    for (k, v) in current {
        if shadow.get(k) != Some(v) {
            out.push(Change {
                key: k.clone(),
                value: Some(v.clone()),
            });
        }
    }
    for k in shadow.keys() {
        if !current.contains_key(k) {
            out.push(Change {
                key: k.clone(),
                value: None,
            });
        }
    }
    out.sort_by(|a, b| a.key.cmp(&b.key));
    out
}

/// Applies changes to a map.
pub fn apply(values: &mut Values, changes: &[Change]) {
    for c in changes {
        match &c.value {
            Some(v) => {
                values.insert(c.key.clone(), v.clone());
            }
            None => {
                values.remove(&c.key);
            }
        }
    }
}

/// Encodes a snapshot: the magic, a count, then each entry as its key
/// length, value length, key, and value. Lengths are 64-bit big-endian.
/// Entries are sorted by key, so equal stores give equal snapshots.
pub fn encode(values: &Values) -> Vec<u8> {
    let mut keys: Vec<&Vec<u8>> = values.keys().collect();
    keys.sort();
    let size: usize = values.iter().map(|(k, v)| 16 + k.len() + v.len()).sum();
    let mut out = Vec::with_capacity(16 + size);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&(values.len() as u64).to_be_bytes());
    for k in keys {
        let v = &values[k];
        out.extend_from_slice(&(k.len() as u64).to_be_bytes());
        out.extend_from_slice(&(v.len() as u64).to_be_bytes());
        out.extend_from_slice(k);
        out.extend_from_slice(v);
    }
    out
}

/// Decodes a snapshot made by `encode`. An empty input is an empty store.
pub fn decode(mut b: &[u8]) -> Result<Values> {
    let mut values = Values::new();
    if b.is_empty() {
        return Ok(values);
    }
    let bad = || Error::new(Code::Storage, "the snapshot is malformed");
    if b.len() < 16 || &b[..8] != MAGIC {
        return Err(Error::new(
            Code::Storage,
            "the snapshot has an unknown format",
        ));
    }
    let count = u64::from_be_bytes(b[8..16].try_into().unwrap());
    b = &b[16..];
    for _ in 0..count {
        if b.len() < 16 {
            return Err(bad());
        }
        let kl = u64::from_be_bytes(b[..8].try_into().unwrap()) as usize;
        let vl = u64::from_be_bytes(b[8..16].try_into().unwrap()) as usize;
        b = &b[16..];
        if kl.checked_add(vl).map_or(true, |n| n > b.len()) {
            return Err(bad());
        }
        let (k, rest) = b.split_at(kl);
        let (v, rest) = rest.split_at(vl);
        if values.insert(k.to_vec(), v.to_vec()).is_some() {
            return Err(bad());
        }
        b = rest;
    }
    if !b.is_empty() {
        return Err(bad());
    }
    Ok(values)
}
