#!/usr/bin/env bash
# Reference launcher for the Ceraa supervisor.
#
# The supervisor invokes this with no arguments, no stdin and the log on
# stdout, so a bare call must start the daemon: a bare `ceraa` prints its
# help and exits zero, which the supervisor would read as a crash loop.
# Copy this beside the binary as RUNTIME_DIR/run_ceraa.sh and keep
# RUNTIME_DIR/env.sh (mode 600, never committed) next to it.
#
# RUNTIME_DIR and TELEGRAM_BOT_TOKEN may also come from the environment, in
# which case the file is skipped and the variables are used as-is.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] && [ -r "$HERE/env.sh" ]; then
	# shellcheck disable=SC1091
	. "$HERE/env.sh"
fi
export TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-}"

if [ $# -eq 0 ]; then
	set -- start
fi
exec "$HERE/bin/ceraa" "$@"
