#!/usr/bin/env bash
# Build the public release artifacts: four binaries, the system prompt, checksums.
#
# The system prompt ships as a file, not embedded in the binary. Ceraa reads
# prompt/system.txt from disk on purpose: there is exactly one copy, so an edit
# cannot be silently shadowed by a stale build. That means a packaged install is
# only complete if the prompt is beside the binary, and this script is what makes
# that true. A release built without it installs cleanly and then runs the agent
# on a one-line fallback, which is the worst way to fail.
#
# The prompt is not documentation and not a secret: it is the agent's behaviour,
# 7KB, and without it the product does not work.
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

# Same flags as .github/workflows/release.yml and scripts/build.sh: -trimpath
# removes absolute build paths, -buildvcs=false removes the commit hash, and
# -s -w strip the symbol table and DWARF.
for target in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64"; do
  set -- $target
  echo "release: building ceraa-$1-$2"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X ceraa/internal/build.Version=$VERSION" \
      -o "dist/ceraa-$1-$2" .
done

# The one copy of the prompt, copied verbatim. The loader searches the executable
# directory and its parent, so an install that places this beside the binary works.
echo "release: staging prompt/system.txt"
cp -f prompt/system.txt dist/system.txt

cp -f LICENSE NOTICE dist/

( cd dist && sha256sum ceraa-* system.txt > SHA256SUMS )

echo "release: version $VERSION"
echo "release: artifacts"
( cd dist && ls -1 | sed 's/^/  /' )
echo "release: checksums"
sed 's/^/  /' dist/SHA256SUMS
