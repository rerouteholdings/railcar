# Railcar

Railcar is a terminal client for Tieroute, the federated, end-to-end encrypted (E2EE) chat protocol. Reroute Holdings LLC writes it from the public Tieroute specification, separately from the Reroute app, so it doubles as a check that the specification is enough to build a working client from.

Railcar is in early development. It has direct chats with contacts, which work with Reroute, the Reroute Holdings LLC app. Group chats come next. The plan and its progress are below.

## What it is

- A Tieroute client for your own terminal, on macOS, Linux, and Windows. It isn't a Secure Shell (SSH) service.
- Written in Go, with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Bubbles](https://github.com/charmbracelet/bubbles) for the interface.
- Group encryption with Messaging Layer Security (MLS, RFC 9420), through [OpenMLS](https://github.com/openmls/openmls) and a small Rust library in [`mls/`](mls/), `tieroute-mls`. OpenMLS is the one MLS library that supports Tieroute's post-quantum ciphersuite today.
- Wire version 2 of Tieroute only.

## Plan

1. The base: `tieroute-mls`, identities, signing up, sessions, and the live stream. Done.
2. Direct chats with contacts, working with Reroute. Done: invite codes, contact requests, and direct chats with replies, edits, deletes, reactions, and typing indicators, tested against Reroute's command-line client on one homeserver and across two.
3. Group chats and communities. Next.
4. Files, presence, linking devices, backups, and moving to another homeserver.
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

The first time, Railcar asks for a homeserver, a handle, and the homeserver's registration code, and shows your 12 recovery words. Then press Ctrl+N to add a contact with their code, or type `:invite` to make a code to give someone. `./railcar --help` lists the options, such as `--profile` to keep more than one identity.

Railcar keeps each profile's key in the system keychain. Without one, it stops, unless you run it with `--key-file`, which keeps the key in a file next to the data instead.

To try the interface without a homeserver, run `./railcar --demo`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It explains how Railcar is written from the specification, and the documentation style it follows.

## License

Railcar is licensed under the [Apache License 2.0](LICENSE). Reroute Holdings LLC's patent pledge for Tieroute is in [PATENTS.md](PATENTS.md).
