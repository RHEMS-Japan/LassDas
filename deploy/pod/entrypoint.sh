#!/bin/bash
# The pod's three residents (docs/RUNTIME_POD.md): the attendant, the
# kanban dispatch loop, and — per card — the runner the dispatcher spawns.
# The gateway daemon is deliberately not used: it brings an HTTP surface
# this constitution does not want.
set -euo pipefail

STATE="${LASSDAS_STATE_DIR:-/data}"
export LASSDAS_RUNTIME_CONFIG="${LASSDAS_RUNTIME_CONFIG:-/etc/lassdas/runtime.json}"
export HERMES_KANBAN_BOARD="${HERMES_KANBAN_BOARD:-lassdas}"
export HERMES_KANBAN_DB="${HERMES_KANBAN_DB:-$STATE/kanban.db}"
export HERMES_TUI=

mkdir -p "$STATE/workspaces"

# Profile with the direct-command worker (idempotent write; host-side
# configuration is the only thing that decides what executes).
PROFILE_HOME="$HOME/.hermes/profiles/lassdas-runner"
mkdir -p "$PROFILE_HOME"
cat > "$PROFILE_HOME/config.yaml" <<'YAML'
worker:
  command:
    - /usr/local/bin/runner
YAML

hermes kanban init

liveness() { touch "$STATE/heartbeat"; }

attendant --config "$LASSDAS_RUNTIME_CONFIG" --interval 60s &
ATTENDANT=$!

dispatch_loop() {
  while true; do
    hermes kanban dispatch || echo "dispatch pass failed rc=$?" >&2
    liveness
    sleep 60
  done
}
dispatch_loop &
DISPATCHER=$!

term() { kill "$ATTENDANT" "$DISPATCHER" 2>/dev/null || true; }
trap term TERM INT

# Either resident dying takes the pod down (restart = clean recovery: the
# ledger and kanban.db carry everything).
wait -n "$ATTENDANT" "$DISPATCHER"
term
exit 1
