//! tieroute-mls: OpenMLS with Tieroute's post-quantum ciphersuite, behind a
//! C interface.
//!
//! It wraps OpenMLS and nothing else. Tieroute's rules, such as which
//! commits a group accepts or what an extension means, live in the caller.
//! The interface is in `include/tieroute_mls.h`, and `README.md` describes
//! it.

pub mod client;
pub mod error;
mod ffi;
pub mod store;
pub mod types;

#[cfg(test)]
mod tests;

pub use client::{Client, Signer, SUITE};
pub use error::{Code, Error};
