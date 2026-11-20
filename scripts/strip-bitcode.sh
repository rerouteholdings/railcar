#!/usr/bin/env bash
# Removes the LLVM bitcode sections (.llvmbc and .llvmcmd) from tieroute-mls's
# static library on Linux. Rust leaves them in some of the library's objects, and Go
# refuses an ELF archive with them in #cgo LDFLAGS. Elsewhere it does nothing.
#
#   scripts/strip-bitcode.sh LIBRARY
#
# env: OBJCOPY, the objcopy for the library's architecture (default objcopy).
set -euo pipefail
lib=${1:?usage: scripts/strip-bitcode.sh LIBRARY}
[ "$(uname -s)" = Linux ] || exit 0
"${OBJCOPY:-objcopy}" --remove-section=.llvmbc --remove-section=.llvmcmd "$lib"
