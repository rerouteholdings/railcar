# Contributing to Railcar

This page explains the rules Railcar's code, comments, documentation, and commit messages follow. Read it before you open a pull request.

## Write from the specification

Railcar exists to show that the Tieroute specification is enough to build a client from. So it's written from these public documents:

- The specification, `spec-public.md`, published at [rerouteholdings.llc/specs/tieroute](https://rerouteholdings.llc/specs/tieroute).
- The client, server-to-server (S2S), and community server API references, and their conventions page.
- The test vectors.

`tieroute-mls` (`mls/`) wraps OpenMLS and nothing else. Every Tieroute rule, such as which commits are valid in a community or what an extension means, lives in the Go code.

## Tests

Run these before you open a pull request:

```sh
go vet ./...
go test ./...
cd mls && cargo test
```

The end-to-end tests run against real rerouted servers, and the interop tests against Reroute's command-line client, reroute-cli. They're skipped unless `RAILCAR_REROUTED` and `RAILCAR_REROUTE_CLI` name the binaries. [`internal/chat/README.md`](internal/chat/README.md) says how to build and run them.
