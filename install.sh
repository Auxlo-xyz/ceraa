#!/usr/bin/env sh
# Install Ceraa from a prebuilt binary.
#
# The release contains compiled binaries only. There is no source in it, and this
# script does not need any: a Go binary is a self-contained executable, so
# installing Ceraa is a file download and nothing more.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh -s -- --version v0.1.0
#
# Options, passed after `--`:
#   --version VERSION   release tag to install (default: the latest release)
#   --dir PATH          install directory (default: /usr/local/bin, else ~/.local/bin)
#   --verify-only       check the download and checksum, then stop
set -eu

REPO="Auxlo-xyz/ceraa"
VERSION=""
INSTALL_DIR=""
VERIFY_ONLY=0

die() { echo "install: $*" >&2; exit 1; }
info() { echo "install: $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
    --dir) INSTALL_DIR="${2:?--dir needs a value}"; shift 2 ;;
    --verify-only) VERIFY_ONLY=1; shift ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"; }
need uname
need tar

# Platform detection. The release names files ceraa-<os>-<arch>, so this only has
# to agree with the names built by the release workflow.
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux)  os="linux" ;;
  darwin) os="darwin" ;;
  *) die "unsupported operating system: $os (this release ships linux and darwin builds)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) die "unsupported architecture: $arch (this release ships amd64 and arm64 builds)" ;;
esac

asset="ceraa-$os-$arch"

# Resolve the release. The API is used rather than the /releases/latest redirect so
# a wrong tag produces a clear message instead of an HTML page.
if [ -z "$VERSION" ]; then
  api="https://api.github.com/repos/$REPO/releases/latest"
  info "resolving the latest release"
  body="$(curl -fsSL "$api")" || die "could not reach $api"
  VERSION="$(printf '%s' "$body" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$VERSION" ] || die "could not read a tag_name from the GitHub API response"
else
  api="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
fi
info "installing $VERSION ($asset)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

base="https://github.com/$REPO/releases/download/$VERSION"
info "downloading"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "download failed: $base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
# The system prompt is optional in the sense that an older release predates it, so
# a 404 here is not fatal; it is only fatal that the binary then runs degraded.
curl -fsSL -o "$tmp/system.txt" "$base/system.txt" 2>/dev/null || rm -f "$tmp/system.txt"
# The Mini App page is optional for the same reason: an older release has none,
# and what breaks without it is one route rather than the whole agent.
curl -fsSL -o "$tmp/miniapp-index.html" "$base/miniapp-index.html" 2>/dev/null || rm -f "$tmp/miniapp-index.html"

# Checksum. The line is "<sha256>  <filename>", so the filename is matched
# exactly and not by prefix: a substring match would also accept
# "ceraa-linux-amd64-notreally".
info "verifying checksum"
verify() {
  name="$1"
  ( cd "$tmp" && grep " $name\$" SHA256SUMS > "$tmp/expected" ) \
    || die "no checksum for $name in SHA256SUMS"
  want="$(awk '{print $1}' "$tmp/expected")"
  got="$(sha256sum "$tmp/$name" | awk '{print $1}')"
  [ "$want" = "$got" ] || die "checksum mismatch for $name
  expected $want
  got      $got
The download is corrupt or was tampered with. Do not run it."
}
verify "$asset"
[ -f "$tmp/system.txt" ] && verify system.txt
[ -f "$tmp/miniapp-index.html" ] && verify miniapp-index.html

if [ "$VERIFY_ONLY" -eq 1 ]; then
  info "checksum OK. Not installing because --verify-only was passed."
  exit 0
fi

# Install location. A system directory needs root, and asking for sudo in a pipe
# is a bad idea, so an unwritable /usr/local/bin falls back to ~/.local/bin.
if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null || [ "$(id -u)" = "0" ]; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR" || die "could not create $INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable"

chmod 0755 "$tmp/$asset"
mv "$tmp/$asset" "$INSTALL_DIR/ceraa" || die "could not write $INSTALL_DIR/ceraa"

info "installed $INSTALL_DIR/ceraa"

# The system prompt ships as a file beside the binary, not inside it.
#
# Ceraa reads prompt/system.txt from disk and never embeds it, on purpose: there
# is exactly one copy, so an edit cannot be silently shadowed by a stale build.
# The loader already searches the executable's directory, so installing the file
# next to the binary is all that is needed. Without this the agent runs on a
# one-line fallback and most of its behaviour is gone.
if [ -f "$tmp/system.txt" ]; then
  prompt_dir="$INSTALL_DIR/prompt"
  mkdir -p "$prompt_dir" || die "could not create $prompt_dir"
  if cp -f "$tmp/system.txt" "$prompt_dir/system.txt"; then
    info "installed $prompt_dir/system.txt"
  else
    info "warning: could not install the system prompt."
    info "Ceraa will start on a minimal fallback prompt until you do:"
    info "    mkdir -p $prompt_dir"
    info "    curl -fsSL -o $prompt_dir/system.txt $base/system.txt"
  fi
else
  info "warning: the release has no system.txt, so Ceraa will start degraded."
fi

# The Mini App page ships as a file beside the binary, for the same reason and
# with the same fallback as the prompt. resolveAsset looks in the data directory,
# then the working directory, then next to the executable, so this is the third
# of those and it is the one an install controls. The release asset is flat
# because a release asset cannot contain a slash; the directory is created here.
if [ -f "$tmp/miniapp-index.html" ]; then
  miniapp_dir="$INSTALL_DIR/miniapp"
  mkdir -p "$miniapp_dir" || die "could not create $miniapp_dir"
  if cp -f "$tmp/miniapp-index.html" "$miniapp_dir/index.html"; then
    info "installed $miniapp_dir/index.html"
  else
    info "warning: could not install the Mini App page."
    info "    mkdir -p $miniapp_dir"
    info "    curl -fsSL -o $miniapp_dir/index.html $base/miniapp-index.html"
  fi
fi

# Create the agent's workspace, so the first run does not fail on a missing
# directory, and say where it is.
#
# Ceraa prefers a workspace beside the binary when it can write there, so a
# checkout keeps the agent next to the project. Installed to /usr/local/bin that
# resolves to /usr/local/workspace, which a non-root user cannot create, and the
# symptom is an agent reporting files it can plainly see. Ceraa now falls back to
# this path on its own when the beside-the-binary one is unusable, so all this has
# to do is create it and confirm it, rather than write a setting that would
# override a later decision.
#
# Set CERAA_WORKSPACE_ROOT to choose a different one, which is what a container or
# a CI job needs when the workspace has to be a mounted volume.
workspace="$HOME/ceraa/workspace"
if mkdir -p "$workspace" 2>/dev/null && [ -w "$workspace" ]; then
  info "workspace $workspace"
else
  info "warning: could not create $workspace."
  info "Ceraa will fall back to it anyway; set CERAA_WORKSPACE_ROOT if this matters."
fi

# Make it reachable without a PATH edit, but only if the user has to do it.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    info ""
    info "$INSTALL_DIR is not on your PATH. Add this to your shell profile:"
    info "    export PATH=\"\$PATH:$INSTALL_DIR\""
    info ""
    ;;
esac

info "run 'ceraa chat' to start, or 'ceraa setup' for the Telegram bridge"
