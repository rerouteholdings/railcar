# Railcar

Railcar is a terminal client for Tieroute, the federated, end-to-end encrypted (E2EE) chat protocol. Reroute Holdings LLC writes it from the public Tieroute specification, separately from the Reroute app, so it doubles as a check that the specification is enough to build a working client from.

Railcar is in early development, and you can't use it to chat yet. The plan and its progress are below.

## What it is

- A Tieroute client for your own terminal, on macOS, Linux, and Windows. It isn't a Secure Shell (SSH) service.
- Written in Go, with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Bubbles](https://github.com/charmbracelet/bubbles) for the interface.
- Group encryption with Messaging Layer Security (MLS, RFC 9420), through [OpenMLS](https://github.com/openmls/openmls) and a small Rust library in [`mls/`](mls/), `tieroute-mls`. OpenMLS is the one MLS library that supports Tieroute's post-quantum ciphersuite today.
- Wire version 2 of Tieroute only.

## Plan

1. The base: `tieroute-mls`, identities, signing up, sessions, and the live stream.
2. Direct chats with contacts, working with Reroute.
3. Group chats and communities.
4. Files, presence, linking devices, backups, and moving to another homeserver.
5. Calls, and packages.

## Build

You need Go 1.27 or later, and Rust 1.85 or later for `tieroute-mls`.

```sh
go build ./cmd/railcar
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It explains how Railcar is written from the specification, and the documentation style it follows.

## License

Railcar is licensed under the [Apache License 2.0](LICENSE). Reroute Holdings LLC's patent pledge for Tieroute is in [PATENTS.md](PATENTS.md).
