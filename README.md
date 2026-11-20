# Railcar

Railcar is a terminal client for Tieroute, the federated, end-to-end encrypted (E2EE) chat protocol. Reroute Holdings LLC writes it from the public Tieroute specification, separately from the Reroute app, so it doubles as a check that the specification is enough to build a working client from.

Railcar is in early development. It has direct chats, group chats, and communities, links devices with their history, and has files, profiles, presence, read receipts, backups, moves to another homeserver, reports, calls, and voice channels, all working with Reroute, the Reroute Holdings LLC app. The plan and its progress are below.

## What it is

- A Tieroute client for your own terminal, on macOS, Linux, and Windows. It isn't a Secure Shell (SSH) service.
- Written in Go, with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Bubbles](https://github.com/charmbracelet/bubbles) for the interface.
- Group encryption with Messaging Layer Security (MLS, RFC 9420), through [OpenMLS](https://github.com/openmls/openmls) and a small Rust library in [`mls/`](mls/), `tieroute-mls`. OpenMLS is the one MLS library that supports Tieroute's post-quantum ciphersuite today.
- Calls and voice channels with [Pion](https://github.com/pion/webrtc) for WebRTC, SFrame (RFC 9605) in Go, [libopus](https://opus-codec.org) for Opus audio, and [miniaudio](https://miniaud.io), through [malgo](https://github.com/gen2brain/malgo), for the microphone and speakers. libopus is built from its release as a static library, as `tieroute-mls` is, so the `railcar` binary needs nothing installed.
- Wire version 2 of Tieroute only.

## Plan

1. The base: `tieroute-mls`, identities, signing up, sessions, and the live stream. Done.
2. Direct chats with contacts, working with Reroute. Done: invite codes, contact requests, and direct chats with replies, edits, deletes, reactions, and typing indicators, tested against Reroute's command-line client on one homeserver and across two.
3. Linking devices, group chats, and communities. Done:
   - Linking devices. A new device signs in with a link code and a confirmation code, or with an approval code another device shows, and gets the contact list and every chat. A signed-in device approves new devices, lists them, and removes them.
   - Group chats of up to 25 people: creating them, adding people directly as an admin or by an add-request an admin approves, removing people, admins, leaving, renaming, and topics, with the admin rules every member checks.
   - Communities on community servers: creating one, joining with a code, text channels with sequence numbers, roles, restricted channels, join codes, history for newcomers, read positions, deletes, timeouts, kicks, and bans. Voice channels are listed, and can't be joined from a terminal.

   Each works with Reroute's command-line client both ways.
4. Everything else a chat needs. Done:
   - Signing in with the recovery code; profiles and presence; files; read receipts, which ask before sending; and typing in community channels.
   - History for a newly linked device, from Reroute or another Railcar.
   - The message history backup, moving to another homeserver, and reports.
   - Group pictures, moving a group's hub, and re-created groups that carry on the same chat.
   - For communities: joins that need approval, following a community that moves, and new metadata and history keys after a kick or ban.
   - Notifications for mentions and `@everyone`.
   - The rules decided for direct chats, commits, restricted channels, and new communities.

   Each works with Reroute's command-line client both ways, where it has the commands.
5. Calls, voice channels, and packages. Done:
   - Calls between contacts, in direct chats and group chats of up to 10 people (§15.2): ringing on every device, answering, declining, missed calls, mute and deafen, hanging up, call waiting, and two calls in one chat settling on one. Media is WebRTC, with each Opus frame encrypted with SFrame under keys from the chat's MLS group. Calls go through the homeserver's TURN relay unless you allow direct calls.
   - Voice channels in communities, through the community server's selective forwarding unit (§16.4): joining, leaving, voice keys sent again for newcomers, mute and deafen, and who's speaking.
   - Media casting (§15.3), to receive only: Railcar plays a cast's audio and says who's casting video, which a terminal can't show. It doesn't cast.
   - A call bar, with who's in the call, how long it has lasted, mute state, and who's speaking, and a bell and a desktop notification for a ring.
   - `scripts/release.sh`, which builds tarballs for macOS and Linux, and .deb, .rpm, and Arch packages, with a man page and shell completions.

   Calls and voice channels work with Reroute's command-line client both ways, relayed or direct.
6. Finishing. In progress:
   - Linux builds in Docker, checked in a container of each architecture.
   - For calls: new relay credentials before the old ones expire (§15.1), a choice of microphone and speakers, and keys to answer, hang up, and mute.
   - Removing and blocking contacts, and unblocking them (§9.6).
   - Coming back as a new device after missing too much (§11.9), re-creating a stuck direct chat, and offering to re-create a chat that's stuck (§11.11).

## Build

You need Go 1.27 or later, Rust 1.85 or later for `tieroute-mls`, and a C compiler, make, and curl for libopus.

```sh
make
```

That builds `tieroute-mls` and libopus first, then leaves `railcar` in the repository's root. `scripts/build-opus.sh` downloads libopus 1.5.2 and checks its SHA-256 before building it.

To build a release for macOS and Linux, run `scripts/release.sh 0.1.0`. Its first lines say what each system needs.

## Run

```sh
./railcar
```

The first time, Railcar asks for a homeserver, a handle, and the homeserver's registration code, and shows your 12 recovery words. Or choose **Sign in with another device** to use an identity you already have in Reroute or another copy of Railcar. Then press Ctrl+N to add a contact with their code, or type `:invite` to make a code to give someone. `:remove-contact` and `:block` remove or block the person in the open direct chat. Type `:new-group` to start a group chat with some of your contacts, `:join` with a code to join a community, and `:create-community` to create one on a community server. Type `:add-device` to add a new device, and `:devices` to see yours. `:backup-on` turns on the encrypted message backup, and `:move` moves you to another homeserver. In a chat, `:call` calls the people in it, and in a voice channel, `:join-voice` joins it. Ctrl+Y answers a call, Ctrl+R mutes or unmutes, and Ctrl+X hangs up, in either. `:audio-devices` chooses the microphone and speakers. `./railcar --help` lists the options, such as `--profile` to keep more than one identity.

Railcar keeps each profile's key in the system keychain. Without one, it stops, unless you run it with `--key-file`, which keeps the key in a file next to the data instead.

To try the interface without a homeserver, run `./railcar --demo`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It explains how Railcar is written from the specification, and the documentation style it follows.

## License

Railcar is licensed under the [Apache License 2.0](LICENSE). Reroute Holdings LLC's patent pledge for Tieroute is in [PATENTS.md](PATENTS.md).
