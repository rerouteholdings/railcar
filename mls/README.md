# tieroute-mls

tieroute-mls is a small Rust library that gives Railcar Messaging Layer Security (MLS, RFC 9420) with Tieroute's post-quantum ciphersuite. It wraps [OpenMLS](https://github.com/openmls/openmls) 0.9.2, built with its `draft-ietf-mls-pq-ciphersuites` feature, behind a C interface. Railcar links it into its Go binary through cgo, in `internal/mls`.

It does MLS only. Every Tieroute rule, such as which commits a group accepts, what an extension holds, and which extensions a leaf lists, lives in Railcar's Go code. Credential identities and extensions go in and out as bytes, with their extension type numbers.

## What it fixes

- The ciphersuite is `MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519`, OpenMLS's provisional code point `0x004F`, and nothing else (§3, §11.1).
- Credentials are basic credentials, and signature keys are Ed25519 (§7.1).
- Everything a device sends is a PrivateMessage, except an external commit, which RFC 9420 requires to be a PublicMessage (§16.5).
- Leaves list `0x004F` as their one ciphersuite, and basic as their credential type unless the caller lists others.

## State

A client is one device's MLS state: its key packages' private keys and every group it's in. OpenMLS keeps that state in a key-value store, which tieroute-mls holds in memory. The caller saves it in one of two ways:

- `trmls_client_snapshot` returns the whole store as one blob.
- `trmls_client_take_changes` returns only the entries that changed since the last save, for a caller that keeps the entries in its own database.

Both are secret, and the caller encrypts them at rest. `trmls_client_revert` returns the store to the last save. That undoes commits, proposals, and keys used up by decrypting, so a client can process a commit it refused again later (§11.9).

A snapshot is the 8 bytes `trmls` `00 00 01`, a 64-bit big-endian entry count, then each entry as its key length, value length, key, and value, with 64-bit big-endian lengths, sorted by key.

## The C interface

The header is [`include/tieroute_mls.h`](include/tieroute_mls.h). Every function that returns `int32_t` returns 0 on success, or an error code. On success, `out` holds the result; on failure, it holds the error message. The caller frees `out` with `trmls_buf_free` either way.

Requests and responses are JSON. Byte strings in them are standard base64 with padding (RFC 4648 §4), as Go's `encoding/json` writes `[]byte`, and `null` reads as empty. MLS messages, Welcomes, and GroupInfos are MLSMessage-encoded. Key packages come back in both forms, bare and as an MLSMessage, and are accepted in either.

| Function | Input | Result |
|---|---|---|
| `trmls_signer_new` | A 32-byte Ed25519 private key | A signer handle |
| `trmls_signer_public_key` | A signer | Its 32-byte public key |
| `trmls_client_open` | A snapshot, or nothing for a new client | A client handle |
| `trmls_client_snapshot` | A client | The snapshot |
| `trmls_client_take_changes` | A client | JSON: the changed entries, with `null` for deleted ones |
| `trmls_client_revert` | A client | Nothing |
| `trmls_key_package` | A signer, and JSON: the leaf, last resort, lifetime, extensions | JSON: the key package, bare and as an MLSMessage, and its hash reference |
| `trmls_delete_key_package` | A hash reference | Nothing |
| `trmls_create_group` | A signer, and JSON: the group ID, leaf, GroupContext extensions, configuration | Nothing |
| `trmls_join_welcome` | JSON: the Welcome, an optional ratchet tree, configuration | JSON: the group ID, epoch, and the sender's leaf index |
| `trmls_join_external` | A signer, and JSON: the GroupInfo, an optional ratchet tree, leaf, configuration | JSON: the group ID, the external commit, and an optional GroupInfo |
| `trmls_delete_group` | A group ID | Nothing |
| `trmls_group_info` | A group ID | JSON: epoch, own leaf, members, extensions, epoch authenticator, tree hash, pending proposals |
| `trmls_commit` | A signer, an optional new signer, and JSON: adds, removes, new extensions, whether to commit pending proposals, an update path, a new leaf | JSON: the commit, an optional Welcome and GroupInfo, and the epoch it was made in |
| `trmls_merge_pending_commit` | A group ID | Nothing |
| `trmls_clear_pending_commit` | A group ID | Nothing |
| `trmls_merge_staged_commit` | A group ID | Nothing |
| `trmls_discard_staged_commit` | A group ID | Nothing |
| `trmls_propose_remove` | A signer, and JSON: the group ID and leaf index | JSON: the proposal and its reference |
| `trmls_remove_pending_proposal` | JSON: the group ID and reference | Nothing |
| `trmls_clear_pending_proposals` | A group ID | Nothing |
| `trmls_set_config` | JSON: the group ID and a configuration, as for joining | Nothing; the group uses the configuration from then on |
| `trmls_encrypt` | A signer, and JSON: the group ID, plaintext, authenticated data | The application message |
| `trmls_process` | JSON: the group ID and message | JSON: the kind, epoch, sender, credential identity, authenticated data, and the plaintext, proposal, or staged commit |
| `trmls_export_secret` | JSON: the group ID, label, context, length, and whether to use the pending commit's epoch | The secret |
| `trmls_export_group_info` | A signer, and JSON: the group ID, and whether to include the tree | The GroupInfo |
| `trmls_export_ratchet_tree` | A group ID | The tree, as the `ratchet_tree` extension's data |
| `trmls_inspect_key_package` | A key package | JSON: its contents, after checking its signature and lifetime |
| `trmls_inspect_group_info` | A GroupInfo | JSON: its contents, unverified |
| `trmls_inspect_message` | An MLSMessage | JSON: its wire format, and its group, epoch, and content type when they're in the clear |
| `trmls_ciphersuite` | Nothing | `0x004F` |

The error codes are in the header: bad input (1), not found (2), invalid (3), wrong epoch (4), pending commit (5), pending proposal (6), inactive (7), past epoch (8), storage (9), and internal (10).

A client handle isn't safe to use from two threads at once. The Go package locks around each call.

### Commits from other members

`trmls_process` stages a commit from another member and returns what it does: its proposals, the committer's new leaf, and the new epoch's GroupContext extensions. The caller judges it by Tieroute's rules, then merges it with `trmls_merge_staged_commit`. If it refuses the commit, it reverts, so it can process the commit again later. Until then, `trmls_process` refuses the group's other messages with the pending commit error.

A standalone proposal is queued as soon as it's processed. The caller drops one it refuses with `trmls_remove_pending_proposal`.

### The next epoch's exporter

`trmls_export_secret` with `pending` exports from the epoch this device's pending commit creates, before the hub accepts it. That gives the `next` key in a commit request (§11.6).

## Build

You need Rust 1.85 or later, from rustup. From the repository's root:

```sh
make mls
```

That runs `cargo build --release --locked` in `mls/`, and leaves the static library at `mls/target/release/libtieroute_mls.a`. `internal/mls` links it with the cgo flags in `internal/mls/cgo.go`. Then `go build ./...` and `go test ./internal/mls/...` work.

On macOS, Homebrew's Rust can come first on your `PATH`. The Makefile uses `~/.cargo/bin/cargo` when it's there.

Run the tests with:

```sh
cd mls && cargo test
```
