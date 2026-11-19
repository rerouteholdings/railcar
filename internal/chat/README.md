# Railcar's chat layer

This page describes `internal/chat`, the part of Railcar that turns the protocol client into contacts, direct chats, group chats, and communities. It's for anyone working on Railcar who needs to know which Tieroute rules it follows for chats, and where they live. Section references (§) are to the public Tieroute specification.

The package sits between `internal/tieroute/client`, which talks to the homeserver, and the interface. `internal/app` connects it to the interface's `Backend`. The MLS library, `internal/mls`, does MLS only: every Tieroute rule about groups and messages is here.

## What it does

- Contacts (§9): invite codes, contact requests in and out, accepting, declining, and requests in both directions, where the identity whose identifier sorts first creates the direct chat.
- Direct chats (§9.3, §11): the accepter creates a 1:1 MLS group with its homeserver as hub, adds every current device of both identities, registers the group with an epoch key, and sends the Add commit with its Welcome. The Add commit binds both identities' chains in `basis`, sets `heads`, and records when the contact joined in `since` (§7.1, §11.8). A commit's `to` lists the members before it, so an identity the commit adds gets only its Welcome (§10.2).
- Joining from a Welcome: the Welcome has to come from a contact, and every leaf is checked on this device's own fresh chain copies (§7.1).
- Commits from the other side, such as a bare Update after a new message key, or the Add of a newly linked device: each is checked against the hub's receipt (§11.6), judged, and applied, or refused.
- Linking devices (§7.2): a linked device opens its link bundle and takes the contact list from it and from the homeserver, so the Welcomes that follow find their senders. The approving device sends the bundle, with the contacts and the chats, and adds the new device to every chat.
- The message history backup (§7.5): `EnableBackup` refuses a passphrase the zxcvbn estimator scores under 4, stores the passphrase copy of the account key (Argon2id, m 65,536 KiB, t 3, p 1, a 16-byte key), uploads a snapshot of the history this device keeps, and names this device the backup device in the account data, merging with what other devices wrote. The backup device then uploads a segment of what's new once a day, or once about 1 MiB has built up, and records its newest `seq` after each. A device that finds another named stops writing. `RestoreBackup` reads the newest snapshot and every segment after it, opening the passphrase copy first on a device without the account key, verifies every message as imported history, and says when the homeserver lists fewer segments than the account data recorded. `DisableBackup` deletes the passphrase copy and takes the backup out of the account data. A history bigger than 1 MiB takes more than one segment, so its snapshot is the first of them.
- Moving to another homeserver (§6.6): `Move` signs the `set-home` entry, with a handle under the new server's handle domain, and asks the new server first with `check` and a signed `move` that carries the contact and groups allowlists. It keeps the move, with the new server's reservation, before it appends the entry on the old home with the reservation. Then it registers on the new home with the whole chain and a newly signed `move`, copies the recovery and passphrase copies of the account key, the account data, and the backup's segments, switches to the new home, and restarts the stream there with its inbox. A move that stopped partway is finished before anything else: Railcar reads the old home's chain to learn whether the append happened, and drops the move only when the old home refused it or the entry can't be appended anymore. Contacts' moves need nothing from Railcar: homeservers route by the chain.
- Reports (§17.2, §17.3): `Report` sends messages from one person, each as its sender signed it with the msg-key that certifies the key, to the homeserver's admins, and with `forward` to the sender's homeserver too. A channel's report goes to the community server for its moderators, with where each message was. Only messages this device holds signed copies of can be reported, and never the identity's own.
- History for a linked device (§7.2, §7.3): every direct chat and group chat keeps the signed app-messages it applied, texts, files, edits, deletes, and reactions, with the msg-key that signed each. The approving device exports them as JSON Lines, uploads the export as files of at most 64 MiB, and lists them in the bundle's `history`. The linked device downloads them, verifies each message against its msg-key, the msg-key against a device in the sender's chain, and its group against the chat, and keeps the ones that pass until it joins their chats. Then it applies them, oldest first, without counting them as unread. It skips a version 1 part, and community channels' items, whose history comes from segments (§16.5).
- The sync rule (§11.9): when a member's chain gains or loses a device, Railcar commits the Removes and Adds in each chat it shares with them, its own identity included. The commit binds every member's current history. A device without key packages yet is tried again every few seconds for 10 minutes, and a `stale-epoch` answer means someone else committed first, so Railcar applies theirs and checks again. Until a Remove lands, Railcar sends nothing else to the chat.
- System lines: a line in the chat when someone's devices change, such as "@kat has a new device" (§11.9).
- Messages (§12): text, replies, edits, deletes, reactions, typing indicators, and read receipts. Unknown content shows as an "unsupported message" placeholder, and malformed known content is dropped.
- Mentions (§12): a text names, in `mentions`, the chat's members whose `@handle` its body has. A received text whose `mentions` holds this identity is marked, as is an `@everyone` that counts; the interface notifies about both.
- Read receipts (§13.3): `SetAskReceipts` makes the identity's texts ask for them, which is off by default. The first message that asks in a chat makes `ReceiptQuestion` name the asker, so the interface asks the user once; `SetReadReceipts` keeps the answer for that chat. With a yes, reading the chat sends one ephemeral `receipt`, up to the newest message that asked. No receipt goes out for a message that didn't ask, before the user agrees, or in a channel. A received receipt marks the identity's own messages that asked, up to `upto`.
- Typing indicators in community text channels go to the channel's group as ephemeral deliveries with the channel's ID, which the community server doesn't number (§13.3, §16.5). A member who can't write there sends none.
- A direct chat re-created between the same two people stays one conversation: its history moves to the new group (§11.11).
- Group chats (§11.5 to §11.8): creating a group with some contacts, with this homeserver as hub and this identity as first admin; adding people directly as an admin, or by an `add-request` an admin answers with a signed approval, after which the member who asked commits the Add; removing people; granting and revoking admin; leaving, where the last admin first hands admin to the longest-standing member; and `group-meta` for the name, topic, and picture, sent again after every Add so new members and devices learn them. A member's leave proposal is committed at once, since OpenMLS sends nothing while one waits (§11.1).
- Group pictures (§12): a PNG, JPEG, WebP, or GIF of at most 256 KiB, carried inline in `group-meta` and checked against its type. A group-meta leaves `avatar` out unless it sets or removes the picture, so a member who hasn't seen another's picture yet doesn't clear it.
- Hub handoff (§11.11): an admin moves the hub to its homeserver with a GroupContextExtensions commit the old hub sequences. Between making the commit and sending it, the homeserver registers the group with itself at the epoch the commit creates, with that epoch's key; if that fails, nothing is committed.
- Re-creating a group (§11.11): `RecreateGroup` makes a new group with the old one's people who are this identity's contacts, its name, topic, and picture, and `replaces` naming the old group, and moves the chat's history to it at once. A Welcome with `replaces` is an offer when its sender, every admin, and everyone in the new group were members of the old one; the chat says who's missing and whether the admins differ, and `SwitchGroup` moves the history and drops the old group. The identity's own re-creation, from another device, switches at once.
- Members who aren't contacts: their chains come through the homeserver with `home` set, from the newest `set-home` in a group's basis tail, or else the group's hub, which is its creator's home (§11.5). The chain that comes back is checked as any other.
- Files (§14): `SendFile` encrypts a file with its own key, uploads it to the homeserver, and sends a `file` message; received `file` messages and `text` with `files` keep their media-refs as attachments. `SaveFile` downloads one through the homeserver, checks its hash, decrypts it, and saves it under its name without directories, never over an existing file. An image's size in pixels is read from its bytes, when this device has them. `410 expired` and `410 removed` are kept on the attachment, and `413 over-quota` is "storage full".
- Presence and profiles (§13): presence frames from the stream, kept in memory; each contact's profile fetched when a frame's `pv` is newer than the one stored, and the profiles of chat members and people asking to be contacts fetched once per connect when this device has none. Setting the identity's own presence, status message, and profile, which `EditProfile` changes field by field. The presence the user chose is sent again on every connect, and `AutoAway` moves `available` and `online` to `away` while the user is idle, and back (§13.2).
- Communities (§16): see below.

## Checks it makes

| What | Checks | Section |
|---|---|---|
| Claimed key packages | The `kp-binding` against the device's composite key, the ciphersuite, the credential, the capabilities, and the leaf's `msg-key` | §3.2.2, §7.1, §11.1 |
| Leaves | A basic credential naming a device of the identity, `0xF0A1` and `0xF0A2` in the capabilities, and a signature key equal to the `msg-key`'s key, which the device's composite key signed | §7.1 |
| Welcomes | From a contact; every leaf a device its identity's chain added at some point; the sender a current device; exactly two identities; each in `since` | §7.1, §10.3 |
| Commits | The hub's receipt; only Add, Update, Remove, and GroupContextExtensions; every new or updated leaf; a closed `tieroute_group` whose kind doesn't change; each `basis` entry judged with `identity.JudgeBasis`, with the group's floor; `heads` following from the basis; `since` covering exactly the members, unchanged for those who stay; in a direct chat, no leaf of any identity but its two, even as a swap | §7.1, §11.1, §11.2, §11.3, §11.6, §11.8 |
| Removes | Another device's leaf goes only when the commit binds a history of its identity in which the key isn't a device, and, without a floor for that identity, was a device at some point | §7.1, §11.9 |
| Link bundles | Opened only with this device's one-time HPKE key, for its own identifier, and only from the identity itself | §7.2 |
| Imported history | Only a version 2 part for this identity; each message's signature under the msg-key its `mk` names, signed by a device that appears in the sender's chain; a group message's `group` the chat's; a direct chat's messages from one of its two people | §7.3 |
| App-messages | The signature by the sender's leaf key, that key's `msg-key`, the sender's identity and the group, no `channel`, and edits and deletes only by the author | §3.2.3, §12 |
| Files | A media-ref's form; the downloaded bytes against its `sha256`, its key, and its `size` | §14 |
| Hub forks | A second commit with a valid receipt for an epoch this device already applied stops the chat | §11.6 |
| Group chat commits | Only admins change `admins` or `hub`, and remove other people; a non-admin's Add of a new person carries an approval that §11.8's checks pass, and puts its request in `used`; `since` gives new people the epoch the commit creates; admins are members, and a group always keeps one; at most 25 people and 250 leaves; a leave proposal committed by reference is the device leaving, not the committer removing it | §7.1, §11.1, §11.8, §11.10 |

A refused commit stops the chat: Railcar sends nothing to it again (§7.1). A commit whose history Railcar can't get yet is pending: it waits, and nothing is sent to the chat until it's decided.

Groups that haven't latched, such as the direct chats Reroute creates today, have no `heads`. For those, a commit's added leaves and committer are judged on this device's fresh copies of the chains instead.

## Communities

A community lives on a community server, which is the hub for its groups and its delivery point (§16.1). Railcar connects to each community server directly, with a stream of its own, and keeps:

- the community chain, checked entry by entry in §16.2.2's order by `internal/tieroute/community`, with the state after each entry, so a commit is judged by the entry its `community_head` names and not by later ones (§16.2);
- the members group, and the group of each restricted channel's see list this identity's roles see (§16.5);
- the metadata keys and history keys that `community-keys` brought, which open channel and role names (§16.2.4) and history segments (§16.5);
- each text channel's messages with their sequence numbers, and the signed app-messages, to build history segments from.

What it does:

- Creating a community: the create entry, with `everyone` holding only `talk`, the members group at epoch 0 with `community_head`, and a `#general` text channel that keeps history (§16.2.3). After every chain entry it appends, Railcar commits the new `community_head` to the members group, as Reroute does, so code joins are judged with the codes the chain holds.
- Joining with a code (§16.6): it checks the chain and the code, and the GroupInfo and tree the community server serves, builds the external commit, and signs the join proof and the commit-request with the code's key. Until a member sends the keys, channels show without names.
- Joins that need approval (§16.6): with a code for such a community, Railcar leaves key packages with the community server, asks with `POST join/request`, and follows the community's stream until the Welcome to the members group arrives, or a `join-declined` notice. A moderator gets a line in the first channel for each `join-request` notice, lists requests, and approves one: it claims the joiner's key packages with `POST join/approve`, commits the Add with its signed `add-approval` in the members group's `tieroute_group`, the request in `used`, and both identities' histories in `basis`, then sends the keys right after the Welcome. `SetApproval` turns approval on or off.
- When someone joins, the member with the lowest leaf index uploads what has built up past the last segment, then sends the keys, after 0 to 2 s; anyone else does after 10 s if nobody has (§16.5, §16.6).
- Channel messages, replies, edits, reactions, and deletes, which a moderator who outranks the author can make too, with `POST messages/delete` after the signed delete (§16.7). A timed-out member's messages are dropped.
- History: reading segments with each history key until one opens them, checking every line, and verifying every message as imported history (§16.5, §7.3); uploading segments that start at the first number this device holds and never overlap a stored one; asking for missing numbers with `history-request`, and answering requests.
- Roles, codes, and restricted channels: adding roles ranked just below this identity, assigning them, then adding the member to the restricted groups their roles see now and removing them from the others, with new keys for those groups. A restricted group requires `0xF0A1` and `0xF0A3` from creation on, and is pinned at its first commit when every leaf lists `0xF0A4`, which then adds `0xF0A4` to what it requires (§16.5).
- Read positions: sent when a channel is read, and taken from the community server every 30 seconds when they're ahead (§16.4).
- Following a community that moves (§16.6): a `set-home` in the chain, or `409 wrong-home` from the old community server, starts it. Railcar keeps reading the old server until the members group's hub is the new one, then signs in there, registers the members group first and then its other groups, leaves key packages, moves the channels' chats and history to the new server's name, and streams from there.
- Moderation: timeouts, kicks, and bans, with Removes of the member's leaves in every group this device is in. After a kick or ban, an admin's Railcar rotates the community metadata key: one entry re-seals every role's name and color, and every name and topic of the channels everyone sees, under a new key, which then goes to the members group (§16.2.4). A moderator without `administrate` leaves that to an admin. Either way, the moderator's Railcar gives every text channel that keeps history a new history key, and each restricted group it's in a new metadata key, and sends each group its keys (§16.5).
- Voice channels are listed with who's in them, from `voice-state`. Railcar can't join them.

| What | Checks | Section |
|---|---|---|
| Community chain | Form, place, signature by a device the signer's chain added at some point, time, authority on the state before the entry, and each op's rules | §16.2.2, §16.2.3 |
| Community commits | The community server's receipt; `community_head` in the members group, and never going back in a pinned group; judged by the chain as of that entry | §16.2, §16.5 |
| Code joins | The join proof for this joiner, epoch, and commit; a live code as of the named entry, counting the joins this device saw; its creator a member; the joiner not banned; at most 1,000 members | §16.6 |
| Approval joins | An Add of a new identity to the members group, other than a code join, carries an `add-approval` for that one identity, signed by the committer with a device in the history the commit binds for them, who holds `moderate` at the commit's `community_head`; its epoch, expiry, and request checked as §11.8 does, and the commit's `basis` binding the joiner and the approver | §16.6 |
| Adds and Removes | A new member added only by someone with `moderate`, or in a pinned restricted group only with a role that sees it; Removes by leave, kick by someone who outranks, a ban, lost sight of the channel, or a removed device | §16.5, §16.7 |
| Leaves | The device appears in its identity's chain at some point, and its `msg-key` checks out | §16.5 |
| Channel messages | The channel matches the delivery's, and the group carries it | §12 |
| History lines | §16.5's line checks, then each message's signature, `msg-key`, and certifying device | §16.5, §7.3 |

A refused commit stops the community: Railcar sends nothing to it again.

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

- Removing a contact, and blocking.
- Offering re-creation on its own after 24 hours without the hub, or an hour of failed commits (§11.11): `:recreate` does it when the user asks.
- Moving a community as its owner, and re-creating a community's groups (§16.5, §16.6). A restricted group whose hub the owner doesn't hand over stays on the old community server.
- The fork rule for community chains: Railcar follows the community server's chain, and stops at an entry it refuses.
- Latching a group that hasn't latched when nothing else changes (§7.1). A device change latches it.
- Communities in link bundles: Railcar sends none, and a linked device doesn't join the ones a bundle lists, so it joins each community again with a code (§7.2 step 5).
- Moves signed with the recovery code, for an old home that's gone, and the recovery-signed entry that vouches for a move contacts' homeservers missed (§6.6).
- Sending the moved chain inside group chats (§6.6, step 3): the spec doesn't say in what content. Members who aren't contacts learn of the move when they fetch the chain.
- Rejoining after an inbox gap or an expired commit (§11.9), and re-creating a stuck direct chat (§11.11).
- Calls, and voice channels (§15, §16.4): phase 5.
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

- `TestE2EInteropOneServer` and `TestE2EInteropTwoServers`: Railcar and Reroute become contacts on one homeserver, and across two federated ones. Reroute creates the direct chat; messages, a reply, edits, reactions, and a delete go both ways, and Railcar reports one of Reroute's messages; Reroute replaces its message key and links a second device, and Railcar follows both commits; Reroute re-creates the chat, which stays one conversation. Then a second Reroute identity adds Railcar, and Railcar creates the direct chat, with itself as hub; Reroute commits there too, which works only when both derive the same epoch keys.
- `TestE2EInteropDecline`: each side declines the other's request.
- `TestE2ERailcar`: two Railcar identities on two servers, one on the live stream, with typing, a restart that reloads the history and MLS state, and messages after it.
- `TestE2ERailcarMutual`: two Railcar identities send each other requests at once, and end up with one direct chat.
- `TestE2EInteropLinkFromReroute`: a Reroute identity with a chat approves Railcar as a new device, by Railcar's link code and a typed confirmation code, then a second Railcar by an approval code Reroute shows. Each gets the chat with the history from before it, and messages go both ways with the contact on another homeserver.
- `TestE2EInteropLinkToReroute`: Railcar approves a new Reroute device by its link code, which shows the earlier messages from Railcar's history, and another by an approval code Railcar shows, adds each to the chat, then removes the first; the Reroute contact follows every change.
- `TestE2ERailcarLink`: the same between Railcar devices, with history that carries an edit and a reaction and isn't unread, a wrong confirmation code, an approval code two devices use, and the contact's system lines.
- `TestE2EInteropRecover`: an identity reroute-cli made, with a direct chat with Railcar, is recovered in Railcar with its 12 words. Railcar's contact adds the new device, messages go both ways, and the words recover it a second time with the next recovery key.
- `TestE2EInteropRecoverWithReroute` and `TestE2ERailcarRecover`: a Railcar identity is recovered in Railcar, and its contact, in Reroute or Railcar, adds the new device. Words in the wrong order are refused.
- `TestE2EInteropBackup`: Railcar and Reroute each turn the backup on. Railcar recovers the Reroute identity, opens Reroute's passphrase copy, and restores its backup into the chat; Reroute recovers the Railcar identity and restores Railcar's backup. A weak passphrase is refused.
- `TestE2ERailcarBackup`: a Railcar backup with a snapshot and a later segment, restored by Railcar after a recovery, with an edit made after the snapshot; then the recovered device takes the backup over.
- `TestE2EInteropMove`: across three homeservers. A move with a bad registration code changes nothing. Railcar moves, and its Reroute and Railcar contacts reach it at the new home and hear from it. Then Reroute moves, and Railcar follows.
- `TestE2EInteropPeople`: Reroute publishes a profile and sets its presence, and Railcar on the live stream sees both. Railcar publishes a profile with a picture and sets its presence; Reroute reads the profile, and another Railcar contact sees the presence, the picture, and auto-away.
- `TestE2EInteropFiles`: Railcar sends an image that Reroute downloads and decrypts, Reroute sends a file with a caption that Railcar saves, and two Railcar identities send a voice message as an audio file.
- `TestE2EInteropGroups`: Railcar and Reroute across two homeservers. Railcar creates a group, adds a member as admin, and makes Reroute an admin, who removes someone. Reroute creates a group; Railcar, not an admin, asks to add someone, Reroute approves, and Railcar's commit with the approval passes Reroute's checks. The other way, Reroute asks and Railcar approves. Renames both ways, then each side leaves and the other commits it.
- `TestE2EInteropGroupExtras`: Reroute's group picture reaches two Railcar members, and Railcar's reaches the other; a Railcar admin moves the hub to its homeserver, and Reroute and Railcar keep talking and committing through it; Railcar re-creates the group, with its history and picture, and the other Railcar switches; Reroute re-creates a group, and Railcar switches to it; and Reroute's mention of Railcar counts.
- `TestE2ERailcarGroups`: the same between Railcar identities, with the last admin leaving and admin passing to the longest-standing member.
- `TestE2EInteropCommunities`: Reroute creates a community, and Railcar joins with a code, gets the keys, reads earlier messages from Reroute's history segment, and messages go both ways with sequence numbers. Railcar creates a community, and Reroute joins with a Railcar code, gets Railcar's keys, and reads Railcar's segment. Railcar adds a role and a restricted channel only it sees, and Reroute, given the role, reads it.
- `TestE2EInteropApprovalJoins`: Reroute approves Railcar's request to join its community, and Railcar approves Reroute's into its own. A Railcar member checks the approval in each commit and keeps going. Then Railcar kicks a member, rotates the metadata key, and Reroute reads the names under the new one.
- `TestE2EInteropCommunityMove`: Reroute's owner moves a community to another community server, and Railcar follows it, with its history, and messages go both ways there.
- `TestE2ERailcarCommunities`: between Railcar identities, with a member who can't make codes, a late joiner reading history and reporting a message, a moderator's delete, read positions, a timeout, a kick that rotates the metadata key, and mentions and an `@everyone`, and typing in a channel.
- `TestE2ERailcarReceipts`: two Railcar identities on the live stream. Nothing asks until the sender turns asking on; the reader is asked once, sends nothing before agreeing, and nothing after declining; receipts mark only the messages that asked. Reroute shows a message that asks.

The community tests run rerouted with community server names, which `testnet.StartWith` sets up, and give reroute-cli their origins in `REROUTE_COMMUNITY_URLS`. Set `RAILCAR_E2E_LOGS` to see the servers' logs and Railcar's own.
