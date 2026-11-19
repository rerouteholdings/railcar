# Builds tieroute-mls, the Rust MLS library, and then Railcar.
#
#   make          builds the library and Railcar
#   make mls      builds the library only (mls/target/release/libtieroute_mls.a)
#   make test     runs the Rust and Go tests
#
# Rust comes from rustup. Set CARGO to use another cargo.

CARGO ?= $(shell test -x $(HOME)/.cargo/bin/cargo && echo $(HOME)/.cargo/bin/cargo || echo cargo)
GO ?= go
MLS_LIB := mls/target/release/libtieroute_mls.a

.PHONY: all mls railcar test clean

all: railcar

mls: $(MLS_LIB)

$(MLS_LIB): mls/Cargo.toml mls/Cargo.lock $(wildcard mls/src/*.rs)
	cd mls && $(CARGO) build --release --locked

railcar: $(MLS_LIB)
	$(GO) build ./...
	$(GO) build -o railcar ./cmd/railcar

test: $(MLS_LIB)
	cd mls && $(CARGO) test --locked
	$(GO) vet ./...
	$(GO) test ./...

clean:
	cd mls && $(CARGO) clean
	rm -f railcar
