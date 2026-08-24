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

REVIEW_A_HOME="$HOME/.hermes/profiles/lassdas-review-a"
mkdir -p "$REVIEW_A_HOME"
cat > "$REVIEW_A_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - review-a
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:-https://gateway.metelix.ai/api/v1}
    api_key_env: LASSDAS_REVIEW_A_KEY
    model: ${LASSDAS_REVIEW_A_MODEL:-anthropic/claude-opus-5}
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
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:-https://gateway.metelix.ai/api/v1}
    api_key_env: LASSDAS_REVIEW_B_KEY
    model: ${LASSDAS_REVIEW_B_MODEL:-openai/gpt-5.6-sol-pro}
YAML

# The implementer profile runs the native Hermes agent through the gateway
# under its own virtual key. The provider block uses the fork's documented
# keys (providers.<name>: base_url / api_key_env / model); its live shape
# is verified on the pod before the first cards-mode run — a wrong key here
# fails the implement card, never the seal.
IMPLEMENTER_HOME="$HOME/.hermes/profiles/lassdas-implementer"
mkdir -p "$IMPLEMENTER_HOME"
if [ ! -f "$IMPLEMENTER_HOME/config.yaml" ]; then
  cat > "$IMPLEMENTER_HOME/config.yaml" <<YAML
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:-https://gateway.metelix.ai/api/v1}
    api_key_env: LASSDAS_IMPLEMENTER_KEY
    model: ${LASSDAS_IMPLEMENTER_MODEL:-anthropic/claude-opus-5}
YAML
fi

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
