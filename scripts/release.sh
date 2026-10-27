#!/usr/bin/env bash
# Build the public release artifacts: four binaries, license files and
# checksums.
#
# The system prompt is compiled into each binary from prompt/system.txt, and
# the Mini App page likewise from internal/miniapp/assets/. Neither is staged
# as a sidecar, so a released binary remains self-contained.
set -euo pipefail

cd "$(dirname "$0")/.."

# The version is the latest tag, so a release can never report a version the tag
# does not name. It was a hardcoded constant once and every release said 0.1.0.
VERSION="${1:-$(git describe --tags --abbrev=0 2>/dev/null || true)}"
[ -n "$VERSION" ] || VERSION="0.0.0-dev"
VERSION="${VERSION#v}"

if ! command -v go >/dev/null 2>&1; then
  if [ -x /usr/local/go/bin/go ]; then
    PATH="/usr/local/go/bin:$PATH"; export PATH
  elif [ -x /workspace/.local/go/bin/go ]; then
    PATH="/workspace/.local/go/bin:$PATH"; export PATH
  else
    echo "release: no go toolchain found" >&2
    exit 1
  fi
fi

rm -rf dist
mkdir -p dist

# Release binaries bake the MTProto application credential the same way
# scripts/build.sh does, read from the deploy box's env file so the source
# tree stays clean. Extractability from a shipped binary is accepted: that is
# how every Telegram client works, and the app credential identifies the
# application, not any one box.
CERAA_ENV_FILE="${CERAA_ENV_FILE:-/workspace/ceraa/runtime/env.sh}"
RELEASE_LDFLAGS="-s -w -X ceraa/internal/build.Version=$VERSION"
if [ -n "${CERAA_TG_API_ID:-}" ] && [ -n "${CERAA_TG_API_HASH:-}" ]; then
  RELEASE_LDFLAGS="$RELEASE_LDFLAGS -X ceraa/internal/telegram.bakedAPIID=$CERAA_TG_API_ID -X ceraa/internal/telegram.bakedAPIHash=$CERAA_TG_API_HASH"
elif [ -f "$CERAA_ENV_FILE" ]; then
  set -a; # shellcheck disable=SC1090
  . "$CERAA_ENV_FILE" 2>/dev/null || true
  set +a
  if [ -n "${CERAA_TG_API_ID:-}" ] && [ -n "${CERAA_TG_API_HASH:-}" ]; then
    RELEASE_LDFLAGS="$RELEASE_LDFLAGS -X ceraa/internal/telegram.bakedAPIID=$CERAA_TG_API_ID -X ceraa/internal/telegram.bakedAPIHash=$CERAA_TG_API_HASH"
  fi
fi

# Same flags as scripts/build.sh: -trimpath removes absolute build paths,
# -buildvcs=false removes the commit hash, and -s -w strip the symbol table and
# DWARF. Only this script adds -X for the version, which is what makes a release
# report the tag rather than the -dev default.
for target in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64"; do
  set -- $target
  echo "release: building ceraa-$1-$2"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -buildvcs=false \
      -ldflags="$RELEASE_LDFLAGS" \
      -o "dist/ceraa-$1-$2" .
done

# The system prompt is compiled into the binary from prompt/system.txt and the
# Mini App page likewise from internal/miniapp/assets/. No sidecars are
# staged: the runtime serves both from memory and ignores copies beside the
# executable, so shipping them only invites editing a file that changes
# nothing.
cp -f LICENSE NOTICE dist/

( cd dist && sha256sum ceraa-* LICENSE NOTICE > SHA256SUMS )

echo "release: version $VERSION"
echo "release: artifacts"
( cd dist && ls -1 | sed 's/^/  /' )
echo "release: checksums"
sed 's/^/  /' dist/SHA256SUMS
