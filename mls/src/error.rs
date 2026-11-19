//! Errors returned across the C interface: a numeric code and a message.

use std::fmt;

/// The error codes of the C interface. The numbers are part of the
/// interface, so they never change meaning.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(i32)]
pub enum Code {
    /// The request was malformed: bad JSON, bad lengths, or bad encodings.
    BadInput = 1,
    /// No group with that ID, or no staged commit to merge.
    NotFound = 2,
    /// OpenMLS rejected a message, a key package, a Welcome, or a GroupInfo.
    Invalid = 3,
    /// The message is for an epoch other than the group's current one.
    WrongEpoch = 4,
    /// The group has a pending commit, so it can't do this now.
    PendingCommit = 5,
    /// The group has pending proposals, so it can't send an application
    /// message until a commit covers them.
    PendingProposal = 6,
    /// This device is no longer a member of the group.
    Inactive = 7,
    /// The message is from a past epoch whose secrets were deleted.
    PastEpoch = 8,
    /// The storage snapshot is malformed.
    Storage = 9,
    /// A bug in tieroute-mls or OpenMLS, including a caught panic.
    Internal = 10,
}

/// An error with its code and a human-readable message.
#[derive(Debug)]
pub struct Error {
    pub code: Code,
    pub msg: String,
}

impl Error {
    pub fn new(code: Code, msg: impl Into<String>) -> Self {
        Error {
            code,
            msg: msg.into(),
        }
    }

    pub fn bad(msg: impl Into<String>) -> Self {
        Error::new(Code::BadInput, msg)
    }

    /// Wraps an OpenMLS error. The code comes from the error's debug form,
    /// which names the state error variants that callers need to tell apart.
    pub fn mls(context: &str, e: impl fmt::Debug + fmt::Display) -> Self {
        let debug = format!("{e:?}");
        Error::new(classify(&debug), format!("{context}: {e}"))
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.msg)
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

fn classify(debug: &str) -> Code {
    if debug.contains("WrongEpoch") {
        Code::WrongEpoch
    } else if debug.contains("NoPastEpochData") {
        Code::PastEpoch
    } else if debug.contains("PendingCommit") {
        Code::PendingCommit
    } else if debug.contains("PendingProposal") && !debug.contains("PendingProposalNotFound") {
        Code::PendingProposal
    } else if debug.contains("UseAfterEviction") {
        Code::Inactive
    } else if debug.contains("LibraryError") {
        Code::Internal
    } else {
        Code::Invalid
    }
}
