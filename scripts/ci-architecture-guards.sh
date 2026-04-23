#!/usr/bin/env bash
# Architectural invariants enforced by CI. These guards keep the
# event-sourcing migration (R0..R5) from regressing once a feature lands;
# every rule below corresponds to an "anti-pattern" called out in the
# event-sourcing plan §11.2 / §13.
#
# Each check echoes the offending matches and exits non-zero on the first
# violation so the failure shows up in the GitHub Actions log without
# scrolling.

set -euo pipefail

failed=0

check() {
  local label="$1"; shift
  local description="$1"; shift
  echo "== $label =="
  if "$@"; then
    echo "  ok"
  else
    echo "::error::$label — $description"
    failed=1
  fi
}

# --- 1. eventlog.SQLiteLog.Append must stay unexported ---------------
# Business code must publish via the typed PublishXxx generated helpers
# or via a UnitOfWork inside Atomic; nothing outside internal/eventlog
# may call SQLiteLog.appendOne directly either (it's package-private but
# the guard catches accidental copy-paste from older diffs).
check "no-exported-Append" \
      "SQLiteLog.Append must remain unexported; use PublishXxx or UnitOfWork.Append" \
      bash -c '
        # We catch SQLiteLog.Append usage anywhere outside internal/eventlog
        # and outside test-only fakes (MemoryLog has its own Append signature
        # that takes a testing.TB; that one is fine).
        offenders=$(rg -nP "\b[A-Za-z_][A-Za-z0-9_]*\.Append\(" --type go internal cmd \
          | rg -v "^internal/eventlog/" \
          | rg -v "^internal/eventlogtest/" \
          | rg -v "uow\.Append\(" \
          | rg -v "MemoryLog|memoryLog" \
          | rg -v "\\.Append\\((t|tb|r\\.tb|testing\\.TB)\\b," \
          || true)
        if [ -n "$offenders" ]; then
          echo "$offenders"
          exit 1
        fi
        exit 0
      '

# --- 2. The legacy /api/ws path must be gone (frontend + backend) ----
# Anything that mentions /api/ws (excluding /api/ws-ticket and the
# /api/events/ws hub) is leftover from the pre-§12 transport.
check "no-legacy-ws-path" \
      "/api/ws is removed; use /api/events/ws via EnvelopeClient" \
      bash -c '
        # The legacy "/api/ws" path is gone. Acceptable matches today are
        # the surviving ticket/hub paths (/api/ws-ticket, /api/events/ws),
        # the internal "wshub" / "ssehub" package names, and historical
        # "no more /api/ws" / "legacy /api/ws" prose explaining their
        # removal. The whole-docs whitelist has been removed: docs must
        # play by the same rules as code so a regression in plan.md
        # cannot mask one in api/.
        offenders=$(rg -n "/api/ws[^-]" web/src internal docs contracts \
          | rg -v "/api/ws-ticket|/api/events/ws|wshub|ssehub|no more /api/ws|legacy /api/ws" \
          || true)
        if [ -n "$offenders" ]; then
          echo "$offenders"
          exit 1
        fi
        exit 0
      '

# --- 3. useWebSocket hook is gone -----------------------------------
# Subscribers must go through EnvelopeClient (useEventStore.trackSubscribe).
check "no-useWebSocket" \
      "useWebSocket is removed; subscribe via useEventStore.trackSubscribe" \
      bash -c '
        if rg -n "useWebSocket" web/src; then
          exit 1
        fi
        exit 0
      '

# --- 4. Legacy callback frame helpers are gone ----------------------
# chat.callback.* envelopes are the only source; processCallbackMessage
# / handleCallbackMessage / handleKanbanMessage stayed alive as dead code
# for several releases — guard against revival.
check "no-legacy-callback-helpers" \
      "callback_*/handleCallbackMessage/handleKanbanMessage are removed" \
      bash -c '
        if rg -nw "handleCallbackMessage|handleKanbanMessage|processCallbackMessage" web/src; then
          exit 1
        fi
        exit 0
      '

# --- 5. wsSink / sseSink / bridgeSubAgentStreamBoard are gone -------
# The unified hubs (wshub + ssehub) replace these; reintroducing them
# means re-introducing the pre-eventlog stream sinks.
check "no-legacy-stream-sinks" \
      "wsSink/sseSink/bridgeSubAgentStreamBoard/CardIDEnricher are removed" \
      bash -c '
        if rg -nw "wsSink|sseSink|bridgeSubAgentStreamBoard|CardIDEnricher" internal cmd; then
          exit 1
        fi
        exit 0
      '

if [ "$failed" -ne 0 ]; then
  echo
  echo "::error::architecture-guards: one or more invariants violated"
  exit 1
fi

echo
echo "All architecture guards passed."
