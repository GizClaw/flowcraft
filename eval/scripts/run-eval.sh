#!/usr/bin/env bash
# run-eval.sh — process supervisor for long-running eval binaries.
#
# Responsibilities:
#   1. Pre-flight: fail fast if the named run is already alive or the
#      target disk is too full.
#   2. Run the command, teeing output to $LOG (so the operator can
#      re-attach and tail at any time).
#   3. Watchdog: if $LOG hasn't been touched in $STUCK_AFTER seconds,
#      log a one-line WARN and keep watching.
#   4. On exit, write a summary line (rc, duration, log path) and clean
#      up the PID file.
#
# All Feishu notifications now happen INSIDE the eval binary itself
# (CardKit, one live card per run). This script intentionally does NOT
# touch any webhooks — operating-system-level supervision only.
#
# Usage:
#     ./run-eval.sh <run-name> -- /root/bin/eval-locomo --dataset ... --out ...
#
# Tunables (env vars):
#     EVAL_LOG_DIR     log root, default /var/log/flowcraft-eval
#     STUCK_AFTER      seconds with no log writes that triggers a WARN
#                      line (default 1800 = 30 min)
#     DISK_MAX_PCT     refuse to start if target partition is >= this
#                      percent full (default 90)

set -u

: "${EVAL_LOG_DIR:=/var/log/flowcraft-eval}"
: "${STUCK_AFTER:=1800}"
: "${DISK_MAX_PCT:=90}"

usage() {
    echo "usage: $0 <run-name> -- <command...>" >&2
    exit 2
}

[[ $# -ge 3 ]] || usage
NAME="$1"; shift
[[ "$1" == "--" ]] || usage
shift

# mtime: portable file-modification-time-in-seconds (Linux + macOS).
mtime() {
    local f="$1"
    if [[ ! -e "$f" ]]; then echo 0; return; fi
    stat -c %Y "$f" 2>/dev/null || stat -f %m "$f" 2>/dev/null || echo 0
}

LOG="$EVAL_LOG_DIR/$NAME.log"
PIDFILE="$EVAL_LOG_DIR/$NAME.pid"
mkdir -p "$EVAL_LOG_DIR"

# Refuse to run if an earlier copy is still alive — concurrent writers
# would corrupt the log and the watchdog would page on the wrong process.
if [[ -e "$PIDFILE" ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "[run-eval] $NAME already running as pid $(cat "$PIDFILE")" >&2
    exit 1
fi

# Disk pre-flight: a full disk silently truncates extractor logs and OOM-kills
# the eval binary; checking up front turns a 5h ghost-failure into a 2-line
# error message before any API tokens are spent.
DISK_PCT="$(df -P "$EVAL_LOG_DIR" | awk 'NR==2 {gsub("%","",$5); print $5}')"
if [[ -n "$DISK_PCT" && "$DISK_PCT" -ge "$DISK_MAX_PCT" ]]; then
    echo "[run-eval] ABORT $NAME: disk ${DISK_PCT}% full (>= ${DISK_MAX_PCT}%)" >&2
    exit 1
fi

START_TS=$(date +%s)
HOST=$(hostname)
echo "[run-eval] START $NAME @$HOST disk=${DISK_PCT}% log=$LOG"
echo "[run-eval] cmd: $*"

# Run the eval binary in the background, capturing stdout+stderr to $LOG.
# Using 'tee -a' rather than '>>' lets us also watch live output if the
# operator re-attaches a terminal.
( "$@" 2>&1 | tee -a "$LOG" ) &
EVAL_PID=$!
echo "$EVAL_PID" > "$PIDFILE"

# Watchdog: poll log mtime every 60s; once it's been quiet for STUCK_AFTER
# seconds, log a one-shot WARN, then re-arm after another full window so we
# don't spam the log if the run is genuinely stuck. The Go-side CardKit
# notifier handles user-facing alerts; this watchdog is just for postmortem
# log evidence.
(
    last_alert=0
    while kill -0 "$EVAL_PID" 2>/dev/null; do
        sleep 60
        now=$(date +%s)
        age=$(( now - $(mtime "$LOG") ))
        if (( age >= STUCK_AFTER && now - last_alert >= STUCK_AFTER )); then
            echo "[run-eval] WARN STUCK $NAME @$HOST: log idle for ${age}s (threshold ${STUCK_AFTER}s) pid=$EVAL_PID" >&2
            last_alert=$now
        fi
    done
) &
WATCH_PID=$!

# Forward common signals to the eval child so Ctrl-C / systemd stop /
# remote SSH kill cleanly tear it down instead of orphaning the process.
trap 'kill -TERM "$EVAL_PID" 2>/dev/null || true; kill "$WATCH_PID" 2>/dev/null || true' INT TERM

wait "$EVAL_PID"
RC=$?
kill "$WATCH_PID" 2>/dev/null || true
wait "$WATCH_PID" 2>/dev/null || true

ELAPSED=$(( $(date +%s) - START_TS ))
ELAPSED_HMS=$(printf '%dh%dm%ds' $((ELAPSED/3600)) $(((ELAPSED%3600)/60)) $((ELAPSED%60)))

if (( RC == 0 )); then
    echo "[run-eval] DONE $NAME @$HOST rc=0 elapsed=$ELAPSED_HMS log=$LOG"
else
    echo "[run-eval] FAIL $NAME @$HOST rc=$RC elapsed=$ELAPSED_HMS log=$LOG" >&2
fi

rm -f "$PIDFILE"
exit $RC
