# Test vectors (wire version 2)

This page answers one question: what do the wire version 2 test vectors cover, and how do you regenerate and check them? They're byte-exact examples of the parts of SPEC.md version 2.0 that changed from version 1.0: composite signatures, message keys, key package bindings, the signature context `tieroute/v2`, the fork rule without a time limit, and the migration from wire version 1.

rerouted and reroute-core still speak wire version 1, and test against the version 1 vectors in the parent directory. While they're being built for version 2, they also test against copies of these files in `rerouted/testdata/vectors/v2/` and `reroute-desktop/core/testdata/vectors/v2/`. When these files change, copy them there too.

**Warning: every key in these files is public. Never use them for anything real.**

## The composite signature

Every signature that version 2.0 makes hybrid (SPEC §3.2.2) is composite ML-DSA, algorithm `MLDSA65-Ed25519-SHA512`, as defined in RFC 10073. RFC 10073 was published in November 2026 from `draft-ietf-lamps-pq-composite-sigs-19` (21 April 2026), and its construction is identical to revision 19's. The vectors pin it as follows:

- Prefix: the 32 ASCII bytes `CompositeAlgorithmSignatures2025` (hex `436f6d706f73697465416c676f726974686d5369676e61747572657332303235`).
- Label: the 30 ASCII bytes `COMPSIG-MLDSA65-Ed25519-SHA512`. The algorithm's object identifier is `1.3.6.1.5.5.7.6.48`.
- Message representative: `M' = Prefix || Label || len(ctx) || ctx || SHA-512(M)`, where `len(ctx)` is one byte. Tieroute signs `M` = the signature input `JCS(["tieroute/v2", t, b])` with an empty application context, so `len(ctx)` is `0x00` and `ctx` is empty.
- Components: pure ML-DSA-65 (FIPS 204) signs `M'` with its own context string set to the label, and pure Ed25519 (RFC 8032) signs `M'` with no context. A verifier checks both, and the Ed25519 component follows every rule of SPEC §4.3.
- Encoding: the public key is the ML-DSA-65 public key followed by the Ed25519 public key (1,952 + 32 = 1,984 bytes), and the signature is the ML-DSA-65 signature followed by the Ed25519 signature (3,309 + 64 = 3,373 bytes). An ML-DSA-65 private key is its 32-byte key generation seed (`ML-DSA.KeyGen_internal`).
- Determinism: ML-DSA signs in its deterministic variant (FIPS 204, `rnd` = 32 zero bytes), so the files are reproducible. An implementation that signs hedged gets different ML-DSA signatures that verify the same. OpenSSL 4.1 reproduces these signatures byte for byte with `-pkeyopt deterministic:1`.

The generator and `check.mjs` both check their construction against RFC 10073's own known-answer test for this algorithm (Appendix E) before anything else.

## Files

| File | What it covers |
|---|---|
| [`composite.json`](composite.json) | The composite construction: prefix, label, an example `M'`, and RFC 10073's known-answer test for `id-MLDSA65-Ed25519-SHA512` (SPEC §3.2.1) |
| [`signed.json`](signed.json) | The `signed` envelope (SPEC §3.2.2, §4.3): an `auth` body signed by a device, with `k` the 32-byte Ed25519 component and `s` a composite signature, broken down into `M'` and its two components; an `app-message` signed by a message key, with a 64-byte Ed25519 signature; and six envelopes to reject: an extra member, an Ed25519-only signature, a flipped bit in each component, components signed over the bare input instead of `M'`, and a signature over the `tieroute/v1` input |
| [`recovery.json`](recovery.json) | A recovery code, the HKDF derivation of composite recovery keys 1 to 3 (an Ed25519 seed from `"signing"` and an ML-DSA-65 seed from `"signing-mldsa"`), their pre-rotation commitments over the 1,984-byte composite public key, and `recovery_wrap` (SPEC §6.1, §6.3) |
| [`chain.json`](chain.json) | A valid four-entry identity chain with composite keys: genesis, add device (`add-device` with `key` and `pub`), a recovery-signed removal whose `k` reveals the composite recovery key, and a self-rotation. Includes the identifier and the final state (SPEC §6.2 to §6.4). |
| [`fork.json`](fork.json) | The fork rule without a time limit (SPEC §6.4.1): 12 cached-versus-candidate cases, including a recovery fork a year after the theft in both orders of arrival, a second recovery fork overriding device entries after the first, a recovery key that can't fork behind the entry that committed to it, a fork that can't reach behind the newest recovery-signed entry, and two recovery-signed branches. Cases name entries from `entries`, and give the required outcome and the warning a client shows. |
| [`msgkey.json`](msgkey.json) | Message keys and key package bindings (SPEC §3.2.2, §3.2.3): a device's `msg-key` for message key 1 (`n` = 1) and its replacement (`n` = 2), a `kp-binding` over a key package's hash, and two to reject: a `msg-key` whose `dev` names another device, and one with an Ed25519-only signature |
| [`migrate.json`](migrate.json) | Migrating from wire version 1 (SPEC §6.8): the version 1 chain from [`../chain.json`](../chain.json) ending in a `migrate` entry signed under `tieroute/v1`, and a version 2 genesis with `migrated-from`, plus five pairs to refuse: an entry after `migrate`, `migrate` signed by a removed device, `migrate` with another op in its entry, a genesis that `migrate` doesn't name, and no `migrate` at all |
| [`link.json`](link.json) | Linking a device (SPEC §7.2): the link code from the Ed25519 component and the HPKE key's hash, the parked link proof carrying the HPKE key, and the confirmation proof, both with the new device's composite key in `k` |
| [`community-history.json`](community-history.json) | Community history segments (SPEC §16.5): the AES-128-GCM key from a history key, the associated data, and a single-message and a multi-message segment sealed with fixed nonces, the second under an older key that a reader finds by trying each. Generated by reroute-core's own segment code, not by `gen` |
| [`code.json`](code.json) | A community join code (SPEC §16.6): `code_prk` from Argon2id, the composite code key, and a join proof signed with it |
| [`codes.json`](codes.json) | A contact verification-code round with the `tieroute/v2` context (SPEC §6.7) |
| [`s2s.json`](s2s.json) | An S2S request signed with a composite server key named by `kid`, and the server document's `pub` (SPEC §5.1, §8.3) |
| [`vouch.json`](vouch.json) | Who vouches for chain entries (SPEC §6.5): [`../vouch.json`](../vouch.json)'s 11 cases over a chain with composite keys, whose recovery-signed entry reveals the composite recovery key |
| [`basis.json`](basis.json) | Snapshot authorization (SPEC §7.1): [`../basis.json`](../basis.json)'s cases over a chain with composite keys, with tails of one to four entries, plus a valid tail of four after a recovery entry and a tail of five to reject (21 cases). `key_hex` is a device key, the Ed25519 component of its composite key. |

Raw bytes are hex, in fields ending in `_hex`. Signed objects and other signed or hashed inputs are given as their exact JCS text (`body`, `sig_input`, `signed`, `input`): sign and hash the UTF-8 bytes of that text. Inside it, byte strings are unpadded base64url, as on the wire (SPEC §4).

Keys are given by their seeds: a composite key by `mldsa_seed_hex` and `ed25519_seed_hex` (`recovery_seed_hex` for recovery keys), with `public_hex` the 1,984-byte composite public key and `ed25519_public` its last 32 bytes.

## Unchanged from wire version 1

These files in the parent directory hold for wire version 2 as they are:

- [`../ed25519.json`](../ed25519.json): the Ed25519 rules apply to message keys and to every composite signature's Ed25519 component.
- [`../formatting.json`](../formatting.json) and [`../sframe.json`](../sframe.json): message formatting and call encryption didn't change.
- [`../account.json`](../account.json): the account key wraps and their associated data didn't change. Its identifier is an input like any other.
- The confirmation-code normalization in [`../link.json`](../link.json).

`vouch.json` and `basis.json` have version 2 forms here: their rules carry over, over chains in the version 2 encoding, and version 2.0 limits a `basis` tail to four entries rather than 16 (SPEC §7.1).

## Stand-in bytes

The community identifier, the key package, the external commit, and the HPKE public key are stand-in bytes, since real ones need an MLS library or a community chain. The join proof's schema, the Argon2id input (the 16 characters after `::`), and the salt (the community identifier's 32 raw bytes) follow SPEC §16.6.

## Generate the vectors

The same Go program as the version 1 vectors generates them, with fixed inputs. From the `spec/vectors/` directory:

```
cd gen
go run . -wire 2 -out ../v2
```

`go run . -out ..` still writes the version 1 vectors, unchanged.

## Check the vectors

[`../check/check.mjs`](../check/check.mjs) checks the version 1 vectors and then these, and reports each. The version 2 checks are in [`../check/v2.mjs`](../check/v2.mjs). They need a Node.js recent enough for ML-DSA-65 and Argon2id in `node:crypto` (tested with Node.js 26), and use only its built-in crypto (ML-DSA-65, Ed25519, SHA-512, HKDF, and Argon2id), and build the composite construction from RFC 10073 independently of the Go generator. They validate every chain by SPEC §6.4's rules and decide every fork case themselves. From the `spec/vectors/` directory:

```
node check/check.mjs .
```
