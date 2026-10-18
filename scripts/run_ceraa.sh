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
# env.sh wins: an inherited TELEGRAM_BOT_TOKEN (e.g. from a shared shell that
# also set up another bot) must not silently override the owner-managed file.
if [ -r "$HERE/env.sh" ]; then
	# set -a, or sourcing only sets shell variables and none of them reach the
	# child: every key in env.sh except the one exported below would be dropped
	# silently, which is the whole reason the file exists.
	set -a
	# shellcheck disable=SC1091
	. "$HERE/env.sh"
	set +a
fi
export TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-}"

if [ $# -eq 0 ]; then
	set -- start
fi
exec "$HERE/bin/ceraa" "$@"
