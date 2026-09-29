#!/usr/bin/env bash
# The one build path for the shipping binary.
#
# `go build -o bin/ceraa .` with no flags ships a fully unstripped binary: every
# symbol, every DWARF line number, every absolute source path, the VCS commit hash,
# and the whole system prompt. Measured on the unhardened binary: 28,344 symbols,
# 30,743 DWARF subprogram entries, 107 source filenames, and prompt/system.txt
# byte-exact at a fixed offset.
#
# -trimpath        removes absolute build paths.
# -buildvcs=false  removes the commit hash. That hash is a permanent liability: it
#                  names the exact source state, so if the repo is ever leaked the
#                  binary and source diff trivially.
# -s -w            removes .symtab and DWARF.
#
# What this does NOT do, so nobody is misled:
#   -s -w does not hide function names. The Go runtime requires .gopclntab to build
# panic tracebacks and to walk stacks for the GC, so the names survive in every
# build and no linker flag can remove them. strings | grep ceraa still yields the
# package structure.
# This also does not hide prompt/system.txt. go:embed data is placed by the
# compiler, not emitted as AST literals, so obfuscators do not touch it. Moving the
# prompt out of the binary is the only change that would; see TODO.md.
#
# Panic tracebacks and pprof keep working under these flags. garble is deliberately
# not used: it breaks pprof symbolization, is a new supply-chain dependency in the
# release path, and buys little here for the reason above.
set -euo pipefail

# Go is not always on PATH here; it lives in /usr/local/go/bin.
if ! command -v go >/dev/null 2>&1; then
  if [ -x /usr/local/go/bin/go ]; then
    PATH="/usr/local/go/bin:$PATH"
    export PATH
  else
    echo "build: no go toolchain found (looked on PATH and at /usr/local/go/bin/go)" >&2
    exit 1
  fi
fi

cd "$(dirname "$0")/.."
exec go build \
  -trimpath \
  -buildvcs=false \
  -ldflags="-s -w" \
  -o bin/ceraa \
  .
