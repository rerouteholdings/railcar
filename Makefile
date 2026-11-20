# Builds tieroute-mls, the Rust MLS library, and libopus, the Opus codec
# for calls, and then Railcar.
#
#   make          builds the libraries and Railcar
#   make mls      builds tieroute-mls only (mls/target/release/libtieroute_mls.a)
#   make opus     builds libopus only (build/opus/lib/libopus.a)
#   make test     runs the Rust and Go tests
#
# Rust comes from rustup. Set CARGO to use another cargo.

CARGO ?= $(shell test -x $(HOME)/.cargo/bin/cargo && echo $(HOME)/.cargo/bin/cargo || echo cargo)
GO ?= go
MLS_LIB := mls/target/release/libtieroute_mls.a
OPUS_LIB := build/opus/lib/libopus.a

.PHONY: all mls opus railcar test clean

all: railcar

mls: $(MLS_LIB)

$(MLS_LIB): mls/Cargo.toml mls/Cargo.lock $(wildcard mls/src/*.rs)
	cd mls && $(CARGO) build --release --locked
	scripts/strip-bitcode.sh $(MLS_LIB)

opus: $(OPUS_LIB)

$(OPUS_LIB): scripts/build-opus.sh
	scripts/build-opus.sh

railcar: $(MLS_LIB) $(OPUS_LIB)
	$(GO) build ./...
	$(GO) build -o railcar ./cmd/railcar

test: $(MLS_LIB) $(OPUS_LIB)
	cd mls && $(CARGO) test --locked
	$(GO) vet ./...
	$(GO) test ./...

clean:
	cd mls && $(CARGO) clean
	rm -rf railcar build
