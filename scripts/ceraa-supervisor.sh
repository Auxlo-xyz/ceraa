#!/usr/bin/env bash
# Keeps the Ceraa Telegram bot running.
#
# There is no init system on this host: PID 1 is bun, systemd is offline and cron
# is absent, so nothing restarts the bot and a crash stays a crash. It has already
# happened more than once, once from the host wiping state and once from a process
# group kill, both with nothing in the log but an abrupt end.
#
# Usage:
#   ceraa-supervisor.sh start    # start the bot once, or report it already running
#   ceraa-supervisor.sh stop     # stop the bot through `ceraa stop`
#   ceraa-supervisor.sh watch    # stay in the foreground and restart on death
#
# Every path is overridable by environment variable so this file is not welded to
# one host. See IMPLEMENTATION.md section 2.1.
#
# Liveness is read from the pid file, never from a pgrep pattern: `pgrep -f ceraa`
# also matches the shell that is running the grep, so it reports a healthy bot as a
# duplicate and invites killing the wrong process.

set -uo pipefail

# Repo root is the parent of scripts/, so the defaults follow a checkout.
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

export CERAA_HOME="${CERAA_HOME:-$REPO_ROOT/../.config/ceraa}"
PIDFILE="${PIDFILE:-$CERAA_HOME/ceraa.pid}"
LOG="${LOG:-/tmp/ceraa.log}"
SUPERVISOR_LOG="${SUPERVISOR_LOG:-/tmp/ceraa.log.supervisor}"
BIN="${BIN:-$REPO_ROOT/bin/ceraa}"
# The launcher exports TELEGRAM_BOT_TOKEN, so it is deliberately NOT tracked by
# git and must not be. It lives outside the repo for that reason.
LAUNCHER="${LAUNCHER:-$REPO_ROOT/../run_ceraa.sh}"
LOCKFILE="${LOCKFILE:-/tmp/ceraa-supervisor.lock}"

# Long enough that a slow start is not mistaken for a crash, and short enough that
# a dead bot is noticed promptly.
SETTLE="${SETTLE:-20}"
BACKOFF="${BACKOFF:-30}"

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$SUPERVISOR_LOG"; }

die() {
	log "supervisor: $1"
	echo "ceraa-supervisor: $1" >&2
	exit 1
}

[ -x "$BIN" ] || die "no ceraa binary at $BIN (run: go build -o bin/ceraa .)"
[ -r "$LAUNCHER" ] || die "no launcher at $LAUNCHER (it supplies TELEGRAM_BOT_TOKEN)"

# running prints the live pid from the pid file, or returns 1.
#
# The recorded start_ticks is checked as well as the pid, because pids are reused:
# a recycled pid belonging to some unrelated process would otherwise look like a
# perfectly healthy bot.
running() {
	local pid want ticks state
	[ -f "$PIDFILE" ] || return 1
	pid=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1]))["pid"])
except Exception: pass' "$PIDFILE" 2>/dev/null)
	[ -n "${pid:-}" ] || return 1

	# Reject a zombie explicitly. kill -0 succeeds on one and PID 1 never reaps, so
	# a killed bot stays in the process table forever. Treating that as healthy is
	# how a <defunct> bot was reported as recovered instead of being restarted.
	state=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
	[ -n "$state" ] || return 1
	case "$state" in
	Z*) return 1 ;;
	esac

	[ -r "/proc/$pid/stat" ] || return 1
	ticks=$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null)
	want=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("start_ticks",""))
except Exception: pass' "$PIDFILE" 2>/dev/null)
	[ -n "${want:-}" ] && [ "$ticks" != "$want" ] && return 1
	printf '%s' "$pid"
}

# launch starts the bot in its own session so it survives this shell and any
# process-group kill aimed at the caller. Plain nohup is not enough: it blocks
# SIGHUP but not a SIGKILL to the group, which is exactly how the bot was lost once.
launch() {
	setsid nohup bash "$LAUNCHER" >>"$LOG" 2>&1 </dev/null &
	disown 2>/dev/null || true
}

start_once() {
	if pid=$(running); then
		log "supervisor: already running pid=$pid"
		echo "already running pid=$pid"
		return 0
	fi
	log "supervisor: starting"
	launch
	sleep "$SETTLE"
	if pid=$(running); then
		log "supervisor: started pid=$pid"
		echo "started pid=$pid"
	else
		die "FAILED to come up after ${SETTLE}s (see $LOG)"
	fi
}

case "${1:-watch}" in
start)
	start_once
	;;
stop)
	if pid=$(running); then
		log "supervisor: stopping pid=$pid"
		"$BIN" stop >>"$LOG" 2>&1 || die "ceraa stop failed (see $LOG)"
		echo "stopped pid=$pid"
	else
		echo "not running"
	fi
	;;
watch)
	# One watcher only. Two were running at once and double-logged every restart.
	# flock is released automatically when the process dies, including on SIGKILL,
	# so a crashed watcher cannot wedge the lock.
	exec 9>"$LOCKFILE"
	if ! flock -n 9; then
		die "another watcher is already running (lock: $LOCKFILE)"
	fi
	log "supervisor: watch loop start"
	while true; do
		if pid=$(running); then
			sleep "$BACKOFF"
			continue
		fi
		log "supervisor: bot not running, restarting"
		# A stale pid file would otherwise be read as liveness on the next pass.
		rm -f "$PIDFILE"
		launch
		sleep "$SETTLE"
		if pid=$(running); then
			log "supervisor: restarted pid=$pid"
		else
			log "supervisor: restart FAILED"
		fi
	done
	;;
*)
	echo "usage: $0 {start|stop|watch}" >&2
	exit 2
	;;
esac
