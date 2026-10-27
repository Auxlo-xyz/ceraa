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
# build and no linker flag can remove them. Measured on this binary: 23,652
# functions remain recoverable, 1,456 of them named ceraa/*.
#
# On the system prompt: it IS in this binary through prompt/embed.go. The source
# file remains prompt/system.txt, and changing it requires a rebuild and release.
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
# The version is deliberately left at its -dev default here. scripts/build.sh
# makes a working binary from the tree, not a release; scripts/release.sh is the
# release path and it injects the tag. Injecting "0.1.0" here is what made every
# release claim to be 0.1.0 regardless of its tag.
# The MTProto application credential is baked from the deploy box's env file
# when it defines CERAA_TG_API_ID and CERAA_TG_API_HASH, so a shipped binary
# brings up large-file downloads with zero configuration while the source tree
# and the public mirror never carry the values. Without them the binary still
# builds and the download path falls back to the Bot API cap.
CERAA_ENV_FILE="${CERAA_ENV_FILE:-/workspace/ceraa/runtime/env.sh}"
LDFLAGS="-s -w"
if [ -n "${CERAA_TG_API_ID:-}" ] && [ -n "${CERAA_TG_API_HASH:-}" ]; then
  LDFLAGS="$LDFLAGS -X ceraa/internal/telegram.bakedAPIID=$CERAA_TG_API_ID -X ceraa/internal/telegram.bakedAPIHash=$CERAA_TG_API_HASH"
elif [ -f "$CERAA_ENV_FILE" ]; then
  set -a; # shellcheck disable=SC1090
  . "$CERAA_ENV_FILE" 2>/dev/null || true
  set +a
  if [ -n "${CERAA_TG_API_ID:-}" ] && [ -n "${CERAA_TG_API_HASH:-}" ]; then
    LDFLAGS="$LDFLAGS -X ceraa/internal/telegram.bakedAPIID=$CERAA_TG_API_ID -X ceraa/internal/telegram.bakedAPIHash=$CERAA_TG_API_HASH"
  fi
fi

# CERAA_GCFLAGS lets a memory-starved build box opt packages out of
# optimization. The gotd/td packages are the RAM whale here: with full
# optimization their compile alone peaks near 3 GiB, which kills every
# build on a ~3 GiB budget. Compiling them with -N -l cuts that peak to
# well under 2 GiB at the cost of a bigger, somewhat slower binary.
# The build cache keys on the flags, so once a -N -l pass of those
# packages is cached, later builds with the same CERAA_GCFLAGS reuse it
# and stay cheap. Drop the variable on a roomier box to ship fully
# optimized.
# The gcflags value contains spaces and go build parses only pre-split
# words, so the two cases are spelled out instead of passed through a
# variable that bash would word-split.
if [ -n "${CERAA_GCFLAGS:-}" ]; then
  # Each ;-separated pair is one pattern=flags spec; -gcflags holds a
  # single pattern per flag, so pairs become separate flags.
  gcflags_args=()
  IFS=';' read -ra gcflags_specs <<< "$CERAA_GCFLAGS"
  for spec in "${gcflags_specs[@]}"; do
    gcflags_args+=("-gcflags=$spec")
  done
  go build \
    -trimpath \
    -buildvcs=false \
    "${gcflags_args[@]}" \
    -ldflags="$LDFLAGS" \
    -o bin/ceraa \
    .
else
  go build \
    -trimpath \
    -buildvcs=false \
    -ldflags="$LDFLAGS" \
    -o bin/ceraa \
    .
fi

# The terms ship with the binary, and the notice is also compiled in. Both, because
# they fail differently: the license files are included in source and release
# assets, while the compiled-in notice travels with a copied binary.
for doc in LICENSE NOTICE; do
  cp -f "$doc" bin/"$doc"
done

# The Mini App page and mascot are embedded in the binary (see
# internal/miniapp/assets.go), so a single-file install serves the dashboard
# with nothing else on disk. They are copied out beside the binary so the tree
# has something to read and edit, but nothing serves them from there: the binary
# is the only source of the page, and there is no variable that changes it.
#
# That was not always true, in two directions. A copy in the data directory and
# then a directory named by an environment variable both outranked the binary,
# which meant a rebuilt binary kept serving a stale page, a fix looked like it
# had not worked, and no upgrade could replace what a user was seeing. Editing the
# page now means rebuilding, which is the honest cost of a page that matches the
# binary serving it.
mkdir -p bin/miniapp
cp -f internal/miniapp/assets/index.html bin/miniapp/index.html
cp -f internal/miniapp/assets/mascot.svg bin/miniapp/mascot.svg
