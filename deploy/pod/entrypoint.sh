#!/bin/bash
# The pod's residents (docs/RUNTIME_POD.md): the attendant, the kanban
# dispatch loop, the serve backend the Hermes One desktop app connects to,
# and — per card — the runner the dispatcher spawns. The chat-platform
# gateway daemon is deliberately not used: it brings an inbound surface
# this constitution does not want. The serve backend binds loopback only,
# so the sole way in is kubectl port-forward.
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

# Board UI backend (Hermes One connects through kubectl port-forward). A UI
# crash must not take down a card mid-run, so it restarts in place instead
# of joining the fatal wait below.
serve_loop() {
  while true; do
    hermes serve --host 127.0.0.1 --port "${HERMES_SERVE_PORT:-9119}" \
      || echo "serve exited rc=$?" >&2
    sleep 5
  done
}
serve_loop &
SERVE=$!

term() { kill "$ATTENDANT" "$DISPATCHER" "$SERVE" 2>/dev/null || true; }
trap term TERM INT

# Either resident dying takes the pod down (restart = clean recovery: the
# ledger and kanban.db carry everything).
wait -n "$ATTENDANT" "$DISPATCHER"
term
exit 1
