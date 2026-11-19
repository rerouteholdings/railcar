# Railcar's chat layer

This page describes `internal/chat`, the part of Railcar that turns the protocol client into contacts and direct chats. It's for anyone working on Railcar who needs to know which Tieroute rules it follows for chats, and where they live. Section references (§) are to the public Tieroute specification.

The package sits between `internal/tieroute/client`, which talks to the homeserver, and the interface. `internal/app` connects it to the interface's `Backend`. The MLS library, `internal/mls`, does MLS only: every Tieroute rule about groups and messages is here.

## What it does

- Contacts (§9): invite codes, contact requests in and out, accepting, declining, and requests in both directions, where the identity whose identifier sorts first creates the direct chat.
- Direct chats (§9.3, §11): the accepter creates a 1:1 MLS group with its homeserver as hub, adds every current device of both identities, registers the group with an epoch key, and sends the Add commit with its Welcome. The Add commit binds both identities' chains in `basis`, sets `heads`, and records when the contact joined in `since` (§7.1, §11.8).
- Joining from a Welcome: the Welcome has to come from a contact, and every leaf is checked on this device's own fresh chain copies (§7.1).
- Commits from the other side, such as a bare Update after a new message key, or the Add of a newly linked device: each is checked against the hub's receipt (§11.6), judged, and applied, or refused.
- Linking devices (§7.2): a linked device opens its link bundle and takes the contact list from it and from the homeserver, so the Welcomes that follow find their senders. The approving device sends the bundle, with the contacts and the chats, and adds the new device to every chat.
- The sync rule (§11.9): when a member's chain gains or loses a device, Railcar commits the Removes and Adds in each chat it shares with them, its own identity included. The commit binds every member's current history. A device without key packages yet is tried again every few seconds for 10 minutes, and a `stale-epoch` answer means someone else committed first, so Railcar applies theirs and checks again. Until a Remove lands, Railcar sends nothing else to the chat.
- System lines: a line in the chat when someone's devices change, such as "@kat has a new device" (§11.9).
- Messages (§12): text, replies, edits, deletes, reactions, typing indicators, and incoming read receipts. Unknown content shows as an "unsupported message" placeholder, and malformed known content is dropped.
- A direct chat re-created between the same two people stays one conversation: its history moves to the new group (§11.11).

## Checks it makes

| What | Checks | Section |
|---|---|---|
| Claimed key packages | The `kp-binding` against the device's composite key, the ciphersuite, the credential, the capabilities, and the leaf's `msg-key` | §3.2.2, §7.1, §11.1 |
| Leaves | A basic credential naming a device of the identity, `0xF0A1` and `0xF0A2` in the capabilities, and a signature key equal to the `msg-key`'s key, which the device's composite key signed | §7.1 |
| Welcomes | From a contact; every leaf a device its identity's chain added at some point; the sender a current device; exactly two identities; each in `since` | §7.1, §10.3 |
| Commits | The hub's receipt; only Add, Update, Remove, and GroupContextExtensions; every new or updated leaf; a closed `tieroute_group` whose kind doesn't change; each `basis` entry judged with `identity.JudgeBasis`, with the group's floor; `heads` following from the basis; `since` covering exactly the members, unchanged for those who stay; at most two identities | §7.1, §11.1, §11.2, §11.6, §11.8 |
| Removes | Another device's leaf goes only when the commit binds a history of its identity in which the key isn't a device, and, without a floor for that identity, was a device at some point | §7.1, §11.9 |
| Link bundles | Opened only with this device's one-time HPKE key, for its own identifier, and only from the identity itself | §7.2 |
| App-messages | The signature by the sender's leaf key, that key's `msg-key`, the sender's identity and the group, no `channel`, and edits and deletes only by the author | §3.2.3, §12 |
| Hub forks | A second commit with a valid receipt for an epoch this device already applied stops the chat | §11.6 |

A refused commit stops the chat: Railcar sends nothing to it again (§7.1). A commit whose history Railcar can't get yet is pending: it waits, and nothing is sent to the chat until it's decided.

Groups that haven't latched, such as the direct chats Reroute creates today, have no `heads`. For those, a commit's added leaves and committer are judged on this device's fresh copies of the chains instead.

## State

Everything is kept in the client's profile, encrypted at rest:

- `mls`: the MLS state, as one tieroute-mls snapshot. It's saved after every change, and before any message that used a new key leaves the device. A refused or pending commit reverts to the last save, so its keys aren't used up (§11.9).
- `chats`: contacts, chats, each chat's epoch, the hashes of recently applied commits, and messages buffered for a later epoch.
- `messages.<group ID in hex>`: each chat's history, up to 5,000 messages.

The engine processes the inbox in order, one item at a time, and acks each item after it's processed. An item that failed for a reason that may pass, such as an unreachable homeserver, is tried again a few times first.

## Using it

```go
eng, err := chat.New(c, chat.Options{})
go eng.Run(ctx)                        // the live stream; or eng.Sync(ctx) once
code, err := eng.Invite(ctx)           // "@kat.chat.example.com::Kx82mPq1Za"
err = eng.AddDevice(ctx, dev, "Kat's desk") // after client.LinkApproval.Check
contact, err := eng.Add(ctx, "@ravi.example.org::Kx82mPq1Za")
ch, err := eng.Accept(ctx, requesterID) // creates the direct chat
m, err := eng.SendText(ctx, ch.ID, "hello", "")
for ev := range eng.Events() { ... }
```

## Not yet

- Group chats and communities, which come next. Leaving a chat, removing a contact, and blocking come with them.
- Latching a group that hasn't latched when nothing else changes (§7.1). A device change latches it.
- History in link bundles (§7.3): Railcar sends none, and doesn't import Reroute's yet, so a new device starts with an empty history in each chat.
- Sending read receipts, which needs the reader's consent (§13.3). Railcar never asks for them, and shows the ones it receives.
- Rejoining after an inbox gap or an expired commit (§11.9), and re-creating a stuck chat (§11.11).
- Checking a message against the sender's leaf in the epoch it was sent in, when the leaf changed since. Such a message is dropped for now.

## Tests

`go test ./internal/chat` runs the unit tests. The end-to-end tests run against real rerouted servers, and the interop tests against reroute-cli, Reroute's command-line client, as a black box:

```sh
(cd ../rerouted && go build -o /tmp/rerouted ./cmd/rerouted)
(cd ../reroute-desktop/core && cargo build -p reroute-cli)
RAILCAR_REROUTED=/tmp/rerouted \
RAILCAR_REROUTE_CLI=../reroute-desktop/core/target/debug/reroute-cli \
go test ./internal/chat -run E2E -v
```

- `TestE2EInteropOneServer` and `TestE2EInteropTwoServers`: Railcar and Reroute become contacts on one homeserver, and across two federated ones. Reroute creates the direct chat; messages, a reply, edits, reactions, and a delete go both ways; Reroute replaces its message key and links a second device, and Railcar follows both commits; Reroute re-creates the chat, which stays one conversation. Then a second Reroute identity adds Railcar, and Railcar creates the direct chat, with itself as hub; Reroute commits there too, which works only when both derive the same epoch keys.
- `TestE2EInteropDecline`: each side declines the other's request.
- `TestE2ERailcar`: two Railcar identities on two servers, one on the live stream, with typing, a restart that reloads the history and MLS state, and messages after it.
- `TestE2ERailcarMutual`: two Railcar identities send each other requests at once, and end up with one direct chat.
- `TestE2EInteropLinkFromReroute`: a Reroute identity with a chat approves Railcar as a new device, by Railcar's link code and a typed confirmation code, then a second Railcar by an approval code Reroute shows. Each gets the chat and messages go both ways with the contact on another homeserver.
- `TestE2EInteropLinkToReroute`: Railcar approves a new Reroute device by its link code, and another by an approval code Railcar shows, adds each to the chat, then removes the first; the Reroute contact follows every change.
- `TestE2ERailcarLink`: the same between Railcar devices, with a wrong confirmation code, an approval code two devices use, and the contact's system lines.
