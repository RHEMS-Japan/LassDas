#!/bin/bash
# The pod's residents (docs/RUNTIME_POD.md): the attendant, the kanban
# dispatch loop, the serve backend the Hermes One desktop app connects to,
# the requester status board (statusboard_loop, opt-in by secret), and —
# per card — the runner the dispatcher spawns. The chat-platform gateway
# daemon is deliberately not used: it brings an inbound surface this
# constitution does not want. Inbound surfaces, exhaustively: kubectl
# port-forward to the loopback serve backend; the authenticated board UI
# when the operator opts in with LASSDAS_DASHBOARD=1 (see serve_loop);
# the status board on :9200 when its credentials secret is mounted —
# basic-auth-guarded in-process and CIDR/TLS-guarded at its ingress
# (deploy/pod/statusboard.yaml); and the tracker's bell — POST
# /webhook/<token>, existing only when LASSDAS_BOARD_BELL_TOKEN is set,
# public by design but token-gated in the path (constant-time check,
# wrong token = 404): its body is discarded unread, so even a valid ring
# can only trigger one rate-limited extra look at the tracker, never
# inject data (deploy/pod/statusboard-hook.yaml).
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

# The cards orchestration's stage profiles: one per chain stage, each a
# fixed host-side command — the task-creation surface can never choose what
# executes. Written unconditionally (idempotent; unused in runner mode).
# The review profiles double as the judges' own agent identity: the same
# profile the card dispatches under is what `hermes --profile <name> -z`
# runs the review with, so each judge carries its own provider block and
# its own credential variable (two judges, two gateway identities).
for CHAIN_STAGE in validate publish; do
  STAGE_HOME="$HOME/.hermes/profiles/lassdas-${CHAIN_STAGE}"
  mkdir -p "$STAGE_HOME"
  cat > "$STAGE_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - ${CHAIN_STAGE}
YAML
done

# The debug role's card: waits for the human merge and the staging deploy,
# then observes the deployed page. Idempotent like every profile above.
E2E_HOME="$HOME/.hermes/profiles/lassdas-e2e"
mkdir -p "$E2E_HOME"
cat > "$E2E_HOME/config.yaml" <<'YAML'
worker:
  command:
    - /usr/local/bin/runner
    - e2e-check
YAML

# The v2 delivery cards: CI wait, staging merge + sealed observation, and
# the Go-driven promotion. One state-driven verb, three milestones.
for DELIVER_STAGE in checks:checks integrate:staging-observed promote:production-observed; do
  DELIVER_NAME="lassdas-${DELIVER_STAGE%%:*}"
  DELIVER_UNTIL="${DELIVER_STAGE#*:}"
  DELIVER_HOME="$HOME/.hermes/profiles/$DELIVER_NAME"
  mkdir -p "$DELIVER_HOME"
  cat > "$DELIVER_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - deliver
    - --until
    - $DELIVER_UNTIL
YAML
done

REVIEW_A_HOME="$HOME/.hermes/profiles/lassdas-review-a"
mkdir -p "$REVIEW_A_HOME"
cat > "$REVIEW_A_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - review-a
model:
  provider: custom:lassdas-gateway
  name: ${LASSDAS_REVIEW_A_MODEL:-anthropic/claude-opus-5}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: LASSDAS_REVIEW_A_KEY
agent:
  max_turns: 40
YAML

REVIEW_B_HOME="$HOME/.hermes/profiles/lassdas-review-b"
mkdir -p "$REVIEW_B_HOME"
cat > "$REVIEW_B_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - review-b
model:
  provider: custom:lassdas-gateway
  name: ${LASSDAS_REVIEW_B_MODEL:-openai/gpt-5.6-sol-pro}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: LASSDAS_REVIEW_B_KEY
agent:
  max_turns: 40
YAML

# The implementer profile runs the native Hermes agent through the gateway
# under its own virtual key. The shape (a named provider addressed as
# custom:<name>, the model selected via model.provider/model.name) is the
# one measured working on the pod (2026-08-24: OK-implementer /
# OK-review-a / OK-review-b probes through all three identities); written
# every boot like the other profiles, so a restart heals drift.
IMPLEMENTER_HOME="$HOME/.hermes/profiles/lassdas-implementer"
mkdir -p "$IMPLEMENTER_HOME"
cat > "$IMPLEMENTER_HOME/config.yaml" <<YAML
model:
  provider: custom:lassdas-gateway
  name: ${LASSDAS_IMPLEMENTER_MODEL:-anthropic/claude-opus-5}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: LASSDAS_IMPLEMENTER_KEY
YAML

# Cards orchestration: the destination credential moves from the process
# environment into an operator-file before any resident starts, because the
# dispatcher spawns every stage — the untrusted implementer included — from
# this environment. Runner mode keeps the environment path unchanged.
if grep -q '"orchestration"[[:space:]]*:[[:space:]]*"cards"' "$LASSDAS_RUNTIME_CONFIG"; then
  mkdir -p "$STATE/runs" "$STATE/secrets"
  if [ -n "${TARGET_GITHUB_TOKEN:-}" ]; then
    umask 077
    printf '%s' "$TARGET_GITHUB_TOKEN" > "$STATE/secrets/target-token"
    umask 022
    unset TARGET_GITHUB_TOKEN
  fi
fi

# The board's credentials leave the process environment for the same
# reason as TARGET_GITHUB_TOKEN above: every card stage — the untrusted
# implementer included — spawns from this environment, and the requester's
# tracker key carries the requester's full authority.
mkdir -p "$STATE/secrets"
if [ -n "${LASSDAS_BOARD_TRACKER_KEY:-}" ]; then
  umask 077
  printf '%s' "$LASSDAS_BOARD_TRACKER_KEY" > "$STATE/secrets/board-tracker-key"
  umask 022
  unset LASSDAS_BOARD_TRACKER_KEY
  export LASSDAS_BOARD_TRACKER_KEY_FILE="$STATE/secrets/board-tracker-key"
fi
if [ -n "${LASSDAS_BOARD_PASS:-}" ]; then
  if [ "${#LASSDAS_BOARD_PASS}" -ge 16 ]; then
    umask 077
    printf '%s' "$LASSDAS_BOARD_PASS" > "$STATE/secrets/board-pass"
    umask 022
    export LASSDAS_BOARD_PASS_FILE="$STATE/secrets/board-pass"
  else
    # The binary would refuse it anyway; skipping the file keeps the
    # start guard honest instead of spawning a permanent crash loop.
    echo "statusboard: LASSDAS_BOARD_PASS is shorter than 16 chars; board disabled (fail-closed)" >&2
  fi
  unset LASSDAS_BOARD_PASS
fi
# The tracker trio configures the board's answer/Go/stop actions; a
# partial set would make the binary refuse to start (fail-closed) and the
# loop retry forever, so degrade to the watch-only board loudly instead.
if [ -n "${LASSDAS_BOARD_TRACKER_KEY_FILE:-}" ] || [ -n "${LASSDAS_BOARD_TRACKER_ORIGIN:-}" ] || [ -n "${LASSDAS_BOARD_TRACKER_SPACE:-}" ]; then
  if [ -z "${LASSDAS_BOARD_TRACKER_KEY_FILE:-}" ] || [ -z "${LASSDAS_BOARD_TRACKER_ORIGIN:-}" ] || [ -z "${LASSDAS_BOARD_TRACKER_SPACE:-}" ]; then
    echo "statusboard: tracker settings are partial; board actions disabled (watch-only)" >&2
    unset LASSDAS_BOARD_TRACKER_KEY_FILE LASSDAS_BOARD_TRACKER_ORIGIN LASSDAS_BOARD_TRACKER_SPACE
  fi
fi

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
#
# LASSDAS_DASHBOARD=1 swaps the loopback-only backend for `hermes dashboard`
# on an outward bind: the same server plus the browser UI. Hermes refuses a
# non-loopback bind without an auth provider (basic-auth env or OIDC), so an
# unauthenticated exposure cannot be misconfigured into existence; the image
# ships the pre-built SPA, hence --skip-build.
serve_loop() {
  while true; do
    if [ "${LASSDAS_DASHBOARD:-}" = "1" ]; then
      hermes dashboard --skip-build --no-open \
        --host "${HERMES_SERVE_HOST:-0.0.0.0}" --port "${HERMES_SERVE_PORT:-9119}" \
        || echo "serve exited rc=$?" >&2
    else
      hermes serve --host 127.0.0.1 --port "${HERMES_SERVE_PORT:-9119}" \
        || echo "serve exited rc=$?" >&2
    fi
    sleep 5
  done
}
serve_loop &
SERVE=$!

# The status board: the requester-facing live view (and, when the
# requester credential is mounted, the answer/Go/stop actions). Restarts
# in place like the board UI backend — losing the viewer must never take
# down a card mid-run. It refuses to start without adequate basic-auth
# credentials (fail-closed), so the loop logs and retries rather than
# exposing anything.
statusboard_loop() {
  while true; do
    statusboard || echo "statusboard exited rc=$?" >&2
    sleep 5
  done
}
# Same gate the binary enforces (user + a password source): a partial
# secret must not become a permanent 5-second crash loop.
if [ -n "${LASSDAS_BOARD_USER:-}" ] && [ -n "${LASSDAS_BOARD_PASS_FILE:-}" ]; then
  statusboard_loop &
  STATUSBOARD=$!
elif [ -n "${LASSDAS_BOARD_USER:-}" ]; then
  echo "statusboard NOT started: LASSDAS_BOARD_PASS is missing (fail-closed)" >&2
fi

term() { kill "$ATTENDANT" "$DISPATCHER" "$SERVE" ${STATUSBOARD:-} 2>/dev/null || true; }
trap term TERM INT

# Either resident dying takes the pod down (restart = clean recovery: the
# ledger and kanban.db carry everything).
wait -n "$ATTENDANT" "$DISPATCHER"
term
exit 1
