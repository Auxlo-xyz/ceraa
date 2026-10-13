#!/usr/bin/env bash
# Build the public release artifacts: four binaries, the Mini App page,
# license files and checksums.
#
# The system prompt is compiled into each binary from prompt/system.txt. It is
# not staged as a sidecar, so a released binary remains self-contained.
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

# Same flags as scripts/build.sh: -trimpath removes absolute build paths,
# -buildvcs=false removes the commit hash, and -s -w strip the symbol table and
# DWARF. Only this script adds -X for the version, which is what makes a release
# report the tag rather than the -dev default.
for target in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64"; do
  set -- $target
  echo "release: building ceraa-$1-$2"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X ceraa/internal/build.Version=$VERSION" \
      -o "dist/ceraa-$1-$2" .
done

# The system prompt is compiled into the binary. The Mini App page is embedded
# too and is published as a separate reviewable asset.
echo "release: staging miniapp-index.html"
cp -f internal/miniapp/assets/index.html dist/miniapp-index.html

cp -f LICENSE NOTICE dist/

( cd dist && sha256sum ceraa-* miniapp-index.html > SHA256SUMS )

echo "release: version $VERSION"
echo "release: artifacts"
( cd dist && ls -1 | sed 's/^/  /' )
echo "release: checksums"
sed 's/^/  /' dist/SHA256SUMS
