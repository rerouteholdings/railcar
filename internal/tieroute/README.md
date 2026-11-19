# Tieroute protocol base

This page describes `internal/tieroute`, the part of Railcar that speaks the Tieroute protocol, wire version 2. It's for anyone working on Railcar's interface or MLS layer who calls into it. Section references (§) are to the public Tieroute specification.

## Packages

| Package | What it does |
|---|---|
| `wire` | I-JSON decoding, base64url byte strings, JCS (RFC 8785), the signed envelope, the error format, and the media type and signature context (§4) |
| `sig` | Strict Ed25519 (§4.3), and the composite ML-DSA-65 + Ed25519 signature `MLDSA65-Ed25519-SHA512` (§3.2), with Go's `crypto/mldsa` |
| `identity` | Device keys and message keys (§3.2.3), recovery codes and their 12 words (§6.1), identity chains and their validation (§6.2 to §6.4), the fork rule (§6.4.1), vouching (§6.5), snapshot authorization (§7.1), `msg-key` and `kp-binding`, and handle syntax (§5.4) |
| `codes` | Contact verification codes (§6.7), confirmation codes and approval tags for linking devices (§7.2, §7.2.1), and the keys of community join codes (§16.6) |
| `discovery` | Server documents and their API base URLs (§5.1, §8.1), and handle records over HTTPS and DNS (§5.3) |
| `store` | The profile directory: records encrypted at rest under a key in the system keychain |
| `community` | Community chains and their validation, with the state after each entry (§16.2); sealed names (§16.2.4); join proofs and code liveness (§16.6); and history segments: encrypting, building, and reading them (§16.5) |
| `client` | Signing up, signing in, linking devices, chains, handles, key packages, the inbox, and the live stream |

## The client API

Sign up in two steps, so the interface can show the recovery words and ask for some of them back before anything is registered (§6.1):

```go
s, err := client.NewSignup(ctx, "chat.example.com", client.Options{DeviceName: "Kat's laptop"})
words := s.Words()                 // show words.Words() once
pos := words.PickPositions(3)      // ask for these back
ok := words.CheckTyped(pos, typed) // trimmed, any case
c, err := s.Register(ctx, profileDir, "kat", registrationCode)
```

`Register` takes a full handle, or one label to put under the server's handle domain. After a `handle-taken` or `bad-invite` error, call it again with another handle or code: the words stay the same. `Signup.Abort` deletes the profile of a signup that never registered. `client.Register(ctx, server, handle, code, profileDir, opts)` does both steps at once, and returns the words with the client.

Registering also signs in, and stores the account key's recovery wrap on the homeserver (§7.4). The recovery code isn't kept after that.

Other calls:

- `client.Open(profileDir, opts)` opens a saved profile. It doesn't contact the homeserver; the first call that needs a session signs in.
- `c.SignIn(ctx)` signs in again. Calls that get `401 unauthenticated` sign in once and retry on their own, so a client rarely needs it.
- `c.SignOut(ctx)` revokes the session token, and keeps the keys. `c.DeleteProfile()` then deletes the keys too.
- `c.Session()` reports whether there's a token, and when it expires. The client API has no endpoint to list sessions.
- `c.Stream(ctx, opts)` returns a channel of `client.Event`: `EventConnected`, `EventItem`, `EventPresence`, `EventError`, `EventDisconnected` (it reconnects with backoff), and `EventStopped`, after which the channel closes. `EventStopped` carries `client.ErrDeviceRemoved` when the homeserver closes the stream with code 4403.
- `c.Inbox(ctx, after, limit)` and `c.Ack(ctx, cursor)` fetch and acknowledge inbox items. The stream doesn't ack; the caller acks what it has processed. A new stream starts after the last acked cursor.
- `c.FetchChain(ctx, id, opts)` fetches and validates a chain, and keeps a copy under §6.5's rules. A chain that neither extends the copy nor is a valid recovery fork comes back as a `*client.ChainWarning`.
- `c.ResolveHandle(ctx, handle)` resolves a handle through the homeserver and checks both directions (§5.3). `client.ErrHandleMismatch` means "handle no longer valid".
- `c.Append(ctx, ops...)` appends a device-signed entry to the identity's chain, such as `identity.AddDevice(pub, name)`.
- `c.Send(ctx, ...)` and `c.SendLink(ctx, data)` submit deliveries.
- `c.CreateInvite(ctx, opts)` makes an invite code (§9.1). `client.ParseInviteCode` reads `@handle::code`, a bare `@handle`, or either as a link on the link domain (§9.4), and refuses any other form with `client.ErrUnknownCode`.
- `c.RequestContact(ctx, to, invite)` signs and sends a contact request (§9.2). `c.AcceptContact`, `c.DeclineContact`, `c.CancelContact`, and `c.RemoveContact` answer and manage them, and `c.Contacts(ctx)` lists the homeserver's contact list, every page of it. `c.VerifyContactRequest` checks a request against the requester's chain, and `c.AcceptChain` keeps a chain that came with a notice.
- `c.ClaimKeyPackages(ctx, id, opts)` claims one key package per current device (§11.4), with each one's `kp-binding`. The caller checks them.
- `c.CreateGroup`, `c.RegisterGroup`, `c.UnregisterGroup`, `c.FetchCommits`, and `c.RegisterEpochKey` are the group calls (§11.5, §11.6). `Outgoing.Welcomes` sends Welcomes with a commit, delivered only if the hub accepts it.
- `c.VerifyServerSigned(ctx, server, s)` checks an object a server signed, such as a hub's commit receipt. Documents of servers other than the homeserver come through the homeserver (`GET servers/{name}`), so those servers don't learn the device's address.
- `c.Profile()` is the profile, where other parts of Railcar keep their own records.
- `c.SignAsDevice(t, body)` signs an object with this device's composite key, such as an add approval (§11.8) or a community chain entry (§16.2).
- `c.Community(server)` is this device's connection to a community server (§16.1): it fetches the server's document there directly, signs in with the device's key and the identity's homeserver (§16.6 step 3), and calls the community server API, from `Preview`, `Create`, `Chain`, `Append`, and `Join` to `Send`, `Inbox`, `Stream`, the history segment calls, read positions, and `Kick`. Its stream reports voice signaling as `EventVoice`.
- `c.Clock()` is the homeserver's time. Warn the user when `Skew()` is over a minute (§8.4).
- `c.Warning()` is a warning to show about the profile, such as a key stored in a file.

## Linking devices

A new device signs in to an existing identity with another device's help (§7.2). The new device's keys stay in memory until it's added; `Finish` then creates the profile.

```go
l, err := client.StartLink(ctx, "chat.example.com", "kat.chat.example.com", opts)
show(l.Code().String())            // as a QR code and as text
err = l.Confirm(ctx, "7K2M-9QXD")  // the code the other device shows
c, err := l.Finish(ctx, profileDir) // client.ErrNotLinkedYet until it's added
```

`client.StartLinkWithApproval(ctx, "approve-…", opts)` starts from an approval code another device shows instead (§7.2.1). It leaves the approval tag in its link proof, so there's no confirmation code: call `Finish` until it's added.

A linked device opens its link bundle from the inbox with `c.OpenLinkBundle(data)`, which keeps the account key and forgets the one-time HPKE key. `c.LinkPending()` says whether it's still waiting for one.

On the device that approves:

- `c.ApproveLinkCode(ctx, code)` fetches the key the new device parked (`GET link/proof/{device}`), checks it against the code, and makes the confirmation code. Show `client.LinkWarning` before it. `Check` then looks for the new device's answer; `client.ErrWrongConfirmation` means it sent back another code.
- `c.NewApprovalCode()` makes an approval code, which works for 10 minutes and for one device. Its `Check` lists the waiting proofs (`GET link/proofs`) and returns the one device whose tag matches; when two devices used the code, it returns `client.ErrApprovalUsedTwice` and adds neither.
- `c.AddLinkedDevice(ctx, dev, name, bundle)` appends `add-device` with the name the user gave, and sends the link bundle, sealed with HPKE to the new device's key (§7.2 steps 3 and 4). The chat layer fills in the contact and chat lists, and adds the device to every chat.
- `c.RemoveDevice(ctx, key)` appends `remove-device` (§7.6).

`ParseLinkCode` and `ParseApprovalCode` read codes as the client API says: any case, a space instead of the dash after the prefix, and only letters and digits counting in the bytes. A short-form link code, with only the device key, gets no link bundle.

## Key packages and MLS

The MLS layer makes key packages through `client.KeyPackageMaker`:

```go
type KeyPackageMaker interface {
	KeyPackages(n int, lastResort bool) ([][]byte, error)
}
```

`c.MLSInfo()` gives the MLS layer what each leaf needs (§7.1): the `BasicCredential` identity, the message key's 32-byte Ed25519 seed (the MLS signature key), and the `tieroute_msg_key` extension data. `c.UploadKeyPackages(ctx, maker, client.DefaultKeyPackages, client.UploadOptions{LastResort: true})` binds each key package with a `kp-binding` and uploads it.

## Profiles

Each profile is one directory. Records are encrypted with AES-256-GCM under a 32-byte key stored in the system keychain (macOS Keychain, Windows Credential Manager, or the Secret Service on Linux). Writes go through a temporary file and a rename, so a crash leaves the old record or the new one.

When there's no keychain, `client.Register` and `client.Open` fail with `store.ErrNoKeychain`. To go on anyway, set `Options.Store.AllowKeyFile`: the key is then kept in a file in the profile directory, and `c.Warning()` says so. Show that warning, since anyone who can read the directory can read the keys.

Recovery words, tokens, and keys stay out of logs: `identity.RecoveryWords` doesn't print its words, and `Options.Logger` gets only request paths and statuses.

## Local test networks

Servers on a local test network are reached through `Options.Origins`, which maps server names to origins, such as `"a.test": "http://127.0.0.1:18471"`. Those servers may use plain HTTP. Development modes like this are implementation features, not protocol (§5.1). A profile made this way remembers its origin.

## Tests

The shared test vectors are copied into `testdata/vectors`. `go test ./internal/tieroute/...` checks every wire version 2 vector this part of Railcar covers: `composite`, `signed`, `msgkey`, `recovery`, `chain`, `fork`, `vouch`, `basis`, `s2s`, `codes`, `code`, and `link`, plus `ed25519.json`, `account.json`, and `link.json` (the short link code and confirmation-code normalization) from version 1, which still hold. `go test ./internal/tieroute/community` checks `community-history.json` and `code.json`, and chain validation cases of its own. The vectors don't cover link bundles, approval tags, or community chains yet, so those are tested against reroute-cli. When the vectors change, copy them here again.

The end-to-end test runs against a real rerouted, and is skipped unless `RAILCAR_REROUTED` names a rerouted binary:

```sh
(cd ../rerouted && go build -o /tmp/rerouted ./cmd/rerouted)
RAILCAR_REROUTED=/tmp/rerouted go test ./internal/tieroute/client -run E2E -v
```

It starts rerouted in dev mode on a free port, registers, uploads key packages, signs in again from the saved profile, resolves the handle, receives a link delivery and a `chain-alert` over the stream, acks them, fetches the chain from a second identity, and signs out.
