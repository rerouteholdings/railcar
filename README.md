# Railcar

Railcar is a terminal client for Tieroute, the federated, end-to-end encrypted (E2EE) chat protocol. Reroute Holdings LLC writes it from the public Tieroute specification, separately from the Reroute app, so it doubles as a check that the specification is enough to build a working client from.

Railcar is in early development. It has direct chats, group chats, and communities, and links devices, all working with Reroute, the Reroute Holdings LLC app. Files, presence, and backups come next. The plan and its progress are below.

## What it is

- A Tieroute client for your own terminal, on macOS, Linux, and Windows. It isn't a Secure Shell (SSH) service.
- Written in Go, with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Bubbles](https://github.com/charmbracelet/bubbles) for the interface.
- Group encryption with Messaging Layer Security (MLS, RFC 9420), through [OpenMLS](https://github.com/openmls/openmls) and a small Rust library in [`mls/`](mls/), `tieroute-mls`. OpenMLS is the one MLS library that supports Tieroute's post-quantum ciphersuite today.
- Wire version 2 of Tieroute only.

## Plan

1. The base: `tieroute-mls`, identities, signing up, sessions, and the live stream. Done.
2. Direct chats with contacts, working with Reroute. Done: invite codes, contact requests, and direct chats with replies, edits, deletes, reactions, and typing indicators, tested against Reroute's command-line client on one homeserver and across two.
3. Linking devices, group chats, and communities. Done:
   - Linking devices. A new device signs in with a link code and a confirmation code, or with an approval code another device shows, and gets the contact list and every chat. A signed-in device approves new devices, lists them, and removes them.
   - Group chats of up to 25 people: creating them, adding people directly as an admin or by an add-request an admin approves, removing people, admins, leaving, renaming, and topics, with the admin rules every member checks.
   - Communities on community servers: creating one, joining with a code, text channels with sequence numbers, roles, restricted channels, join codes, history for newcomers, read positions, deletes, timeouts, kicks, and bans. Voice channels are listed, and can't be joined from a terminal.

   Each works with Reroute's command-line client both ways.
4. Files, presence, backups, and moving to another homeserver. For communities: joins that need approval, following a community that moves, re-creating stuck groups, and rotating the metadata key after a kick or ban.
5. Calls, and packages.

## Build

You need Go 1.27 or later, and Rust 1.85 or later for `tieroute-mls`.

```sh
make
```

That builds `tieroute-mls` first, then leaves `railcar` in the repository's root.

## Run

```sh
./railcar
```

The first time, Railcar asks for a homeserver, a handle, and the homeserver's registration code, and shows your 12 recovery words. Or choose **Sign in with another device** to use an identity you already have in Reroute or another copy of Railcar. Then press Ctrl+N to add a contact with their code, or type `:invite` to make a code to give someone. Type `:new-group` to start a group chat with some of your contacts, `:join` with a code to join a community, and `:create-community` to create one on a community server. Type `:add-device` to add a new device, and `:devices` to see yours. `./railcar --help` lists the options, such as `--profile` to keep more than one identity.

Railcar keeps each profile's key in the system keychain. Without one, it stops, unless you run it with `--key-file`, which keeps the key in a file next to the data instead.

To try the interface without a homeserver, run `./railcar --demo`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It explains how Railcar is written from the specification, and the documentation style it follows.

## License

Railcar is licensed under the [Apache License 2.0](LICENSE). Reroute Holdings LLC's patent pledge for Tieroute is in [PATENTS.md](PATENTS.md).
