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
# The worker's agent user pool lives under the state directory and refuses
# to live anywhere else: the variable is exported so the pool sees the
# same directory whether or not the manifest set it.
export LASSDAS_STATE_DIR="$STATE"
export LASSDAS_RUNTIME_CONFIG="${LASSDAS_RUNTIME_CONFIG:-/etc/lassdas/runtime.json}"
export HERMES_KANBAN_BOARD="${HERMES_KANBAN_BOARD:-lassdas}"
export HERMES_KANBAN_DB="${HERMES_KANBAN_DB:-$STATE/kanban.db}"
export HERMES_TUI=
# Hermes masks what looks like a credential in the text it sends to the
# model, and treats source code the same way: an Authorization header's
# literal `Bearer ${token}` reached the implementer as `*** ${token}`, which
# it faithfully wrote back into the file (measured in the model gateway's
# request archive, 2026-09-02). Secret inspection is the gateway's job here;
# the agent must see the repository as it is.
export HERMES_REDACT_SECRETS="${HERMES_REDACT_SECRETS:-false}"

mkdir -p "$STATE/workspaces"

# The observation browser's session jar: the operator's seed is a secret
# mount (LASSDAS_E2E_SESSION_FILE); the engine's renewed copy — rewritten
# by every login that lands — lives in the state volume, owner-only. The
# copy remembers the seed it grew from and is the jar in use for as long
# as that seed is the one mounted; a replaced seed wins over the copy.
export LASSDAS_E2E_SESSION_STATE_FILE="${LASSDAS_E2E_SESSION_STATE_FILE:-$STATE/e2e-session/session.json}"
# Owner-only when this process owns it; a directory somebody else made (a
# read-only mount, a root-created volume) is left as it is rather than
# failing the pod — the writer reports a jar it cannot keep at the time.
E2E_SESSION_DIR="$(dirname "$LASSDAS_E2E_SESSION_STATE_FILE")"
mkdir -p "$E2E_SESSION_DIR" 2>/dev/null || echo "note: $E2E_SESSION_DIR could not be created; the renewed jar will not be kept" >&2
chmod 700 "$E2E_SESSION_DIR" 2>/dev/null || true

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
for CHAIN_STAGE in validate publish investigate design-decide; do
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

# The role models are read in two places — the profiles below and the
# attendant's budget check before every reception — so their defaults are
# resolved once, here, and exported. A role the attendant cannot see is a
# role it cannot probe.
export LASSDAS_IMPLEMENTER_MODEL="${LASSDAS_IMPLEMENTER_MODEL:-anthropic/claude-opus-5}"
export LASSDAS_REVIEW_A_MODEL="${LASSDAS_REVIEW_A_MODEL:-anthropic/claude-opus-5}"
export LASSDAS_REVIEW_B_MODEL="${LASSDAS_REVIEW_B_MODEL:-openai/gpt-5.6-sol-pro}"
# The investigating designer (docs/INVESTIGATING_DESIGNER.md §11): the
# designer is the strongest model (the kernel calls it directly, under its
# own key); the applier that copies an approved design is a light model.
# Defaults follow the implementer and the second reviewer's vendor.
export LASSDAS_DESIGNER_MODEL="${LASSDAS_DESIGNER_MODEL:-${LASSDAS_IMPLEMENTER_MODEL}}"
export LASSDAS_APPLIER_MODEL="${LASSDAS_APPLIER_MODEL:-${LASSDAS_REVIEW_B_MODEL}}"
# The design judges (docs/INVESTIGATING_DESIGNER.md §11, decision 3): by
# default the candidate reviewers' models and keys; a heavier judge or
# another vendor for designs is set here without moving the candidate
# reviews. The key variables name the variable the judge's profile reads
# (default: the candidate reviewer's key).
export LASSDAS_DESIGN_REVIEW_A_MODEL="${LASSDAS_DESIGN_REVIEW_A_MODEL:-${LASSDAS_REVIEW_A_MODEL}}"
export LASSDAS_DESIGN_REVIEW_B_MODEL="${LASSDAS_DESIGN_REVIEW_B_MODEL:-${LASSDAS_REVIEW_B_MODEL}}"
export LASSDAS_DESIGN_REVIEW_A_KEY_VAR="${LASSDAS_DESIGN_REVIEW_A_KEY_VAR:-LASSDAS_REVIEW_A_KEY}"
export LASSDAS_DESIGN_REVIEW_B_KEY_VAR="${LASSDAS_DESIGN_REVIEW_B_KEY_VAR:-LASSDAS_REVIEW_B_KEY}"

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

# The implementer profile: a direct command like the reviews, so the
# runner receives the card and the worker starts Hermes under the agent
# user through the launcher (docs/RUNTIME_POD.md, "Agents under their own
# user"); the kanban used to run this profile as a native worker under the
# engine's user. The worker copies this profile into the home it makes for
# each launch, so the model settings below are what the agent runs with.
# The shape (a named provider addressed as custom:<name>, the model
# selected via model.provider/model.name) is the one measured working on
# the pod (2026-08-24: OK-implementer / OK-review-a / OK-review-b probes
# through all three identities); written every boot like the other
# profiles, so a restart heals drift.
#
# agent.max_turns is Hermes' cap on tool-calling iterations (its default is
# 500). An implementer that stops making progress burns the whole budget:
# measured live 2026-09-02, one run spent 458 iterations — 425 of them the
# same file search — without changing a file. The cap is an operator
# setting so the money a run can waste is bounded per deployment.
IMPLEMENTER_HOME="$HOME/.hermes/profiles/lassdas-implementer"
mkdir -p "$IMPLEMENTER_HOME"
cat > "$IMPLEMENTER_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - implement
model:
  provider: custom:lassdas-gateway
  name: ${LASSDAS_IMPLEMENTER_MODEL:-anthropic/claude-opus-5}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: LASSDAS_IMPLEMENTER_KEY
agent:
  max_turns: ${LASSDAS_IMPLEMENTER_MAX_TURNS:-200}
YAML

# The design-review profiles: the same two judges, their own gateway
# identities, dispatched under `--stage design-review-a/b` so the runner
# judges the sealed design (or the investigation report) instead of a
# candidate. worker.command is fixed per profile, which is why these are
# profiles of their own and not the review profiles reused.
for DESIGN_REVIEW in a:LASSDAS_DESIGN_REVIEW_A_MODEL:LASSDAS_DESIGN_REVIEW_A_KEY_VAR:anthropic/claude-opus-5 b:LASSDAS_DESIGN_REVIEW_B_MODEL:LASSDAS_DESIGN_REVIEW_B_KEY_VAR:openai/gpt-5.6-sol-pro; do
  DR_LETTER="${DESIGN_REVIEW%%:*}"
  DR_REST="${DESIGN_REVIEW#*:}"
  DR_MODEL_VAR="${DR_REST%%:*}"
  DR_REST="${DR_REST#*:}"
  DR_KEY_VAR_VAR="${DR_REST%%:*}"
  DR_DEFAULT="${DR_REST#*:}"
  eval "DR_MODEL=\${${DR_MODEL_VAR}:-${DR_DEFAULT}}"
  eval "DR_KEY_VAR=\${${DR_KEY_VAR_VAR}}"
  DR_HOME="$HOME/.hermes/profiles/lassdas-design-review-${DR_LETTER}"
  mkdir -p "$DR_HOME"
  cat > "$DR_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - design-review-${DR_LETTER}
model:
  provider: custom:lassdas-gateway
  name: ${DR_MODEL}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: ${DR_KEY_VAR}
agent:
  max_turns: 40
YAML
done

# The applier profile: a direct command like the implementer's, and the
# agent copies an approved design and stops on doubt
# (docs/INVESTIGATING_DESIGNER.md §7). Forty turns is its whole budget: the
# design already decided everything. The consumer's agents.applier names
# the launch the worker runs (hermes --profile lassdas-applier -z).
APPLIER_HOME="$HOME/.hermes/profiles/lassdas-applier"
mkdir -p "$APPLIER_HOME"
cat > "$APPLIER_HOME/config.yaml" <<YAML
worker:
  command:
    - /usr/local/bin/runner
    - chain-stage
    - --stage
    - apply
model:
  provider: custom:lassdas-gateway
  name: ${LASSDAS_APPLIER_MODEL}
providers:
  lassdas-gateway:
    base_url: ${LASSDAS_GATEWAY_BASE_URL:?set LASSDAS_GATEWAY_BASE_URL (the OpenAI-compatible model gateway, e.g. https://gateway.example.com/api/v1)}
    api_key_env: LASSDAS_APPLIER_KEY
agent:
  max_turns: ${LASSDAS_APPLIER_MAX_TURNS:-40}
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

# Agents run as the agent user (#23, docs/RUNTIME_POD.md "Agents under
# their own user"): the worker starts every agent through the launcher,
# which lends it the workspace and a home made for that launch (seeded
# from this user's profiles above). The kept jar, the seed mount, the
# identities the probes use, the secrets and the run records stay closed
# to the agent user by their modes — checked below, before anything else
# starts.
export LASSDAS_AGENT_LAUNCHER="${LASSDAS_AGENT_LAUNCHER:-/usr/local/bin/agentexec}"
# The launcher lends and returns trees under the runs directory alone —
# the one the runtime configuration names (chain.runs_root), so a runs
# directory placed elsewhere is not refused at every launch.
RUNS_ROOT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("chain", {}).get("runs_root", ""))' "$LASSDAS_RUNTIME_CONFIG" 2>/dev/null || true)"
export LASSDAS_AGENT_TREE_ROOT="${LASSDAS_AGENT_TREE_ROOT:-${RUNS_ROOT:-$STATE/runs}}"
# Boot check, fail-closed. First the launcher itself: without its file
# capabilities (or under allowPrivilegeEscalation: false) it cannot switch
# users, no agent could start, and a pod that is up but fails every run
# is worse than one that does not start.
if "$LASSDAS_AGENT_LAUNCHER" --check /etc/passwd; then :; else
  CAP_RC=$?
  if [ "$CAP_RC" -eq 2 ]; then
    echo "REFUSING TO START: $LASSDAS_AGENT_LAUNCHER cannot switch users (file capabilities missing, or allowPrivilegeEscalation: false on the container)" >&2
    exit 1
  fi
fi
# What a launch left to the agent user when the pod died mid-run (a lent
# workspace, a lent home) comes back to this user before any card runs,
# or the next dispatch of that run could neither clear nor read its tree.
if [ -d "$STATE/runs" ]; then
  # A directory the agent user closed (0700) makes find exit non-zero
  # after listing it; that must not end the boot (it is what the reclaim
  # below is for), hence the || true on each.
  { find "$STATE/runs" -mindepth 2 -maxdepth 2 ! -user "$(id -un)" 2>/dev/null || true
    find "$STATE/runs" -mindepth 3 -maxdepth 3 -path '*/agent-home/*' ! -user "$(id -un)" 2>/dev/null || true; } \
  | while IFS= read -r LEFT; do
      "$LASSDAS_AGENT_LAUNCHER" --reclaim "$LEFT" || echo "note: $LEFT not reclaimed" >&2
    done
fi
# The engine's own home is closed to the agent user too (what an agent
# reads from it, the worker copies into the home made for its launch).
chmod 0700 "$HOME" 2>/dev/null || true
# Runs made before this engine closed its records (0644 files, 0755
# directories) are closed the same way once per boot: 0711 run directories
# (the agent user enters its clone, cannot list), 0600 files and 0700
# directories inside — the lent trees, the agents' homes and the MCP
# description an agent reads (agent-mcp.json) aside.
if [ -d "$STATE/runs" ]; then
  # The patterns are find's, not the shell's: globbing is off while the
  # expression is split into words, or a runs directory with two runs
  # would expand the pattern into paths and break the expression.
  LENT_TREES="( -path $STATE/runs/*/target-repo -o -path $STATE/runs/*/target-base -o -path $STATE/runs/*/validation-target -o -path $STATE/runs/*/agent-home )"
  find "$STATE/runs" -mindepth 1 -maxdepth 1 -type d -user "$(id -un)" -exec chmod 0711 {} + 2>/dev/null || true
  set -f
  # The expression is word-split on purpose.
  # shellcheck disable=SC2086
  find "$STATE/runs" -mindepth 2 $LENT_TREES -prune -o -user "$(id -un)" -type f -not -name agent-mcp.json -perm /077 -exec chmod 0600 {} + 2>/dev/null || true
  # shellcheck disable=SC2086
  find "$STATE/runs" -mindepth 2 $LENT_TREES -prune -o -user "$(id -un)" -type d -perm /077 -exec chmod 0700 {} + 2>/dev/null || true
  set +f
fi
# Then what the agent user must not open: the kept jar, the seed mount,
# the kubeconfig and the token or key files it names, the AWS identity
# files, a mounted service-account token, and whatever the operator lists
# in LASSDAS_GUARDED_FILES (colon-separated). A file this user owns is
# tightened to 0600 first; a file the agent user can still read refuses
# the boot, and the message names the mode to set. Secret volumes need
# defaultMode: 0440 (the kubelet writes projected tokens 0640 by itself).
GUARDED_LIST="$(printf '%s\n' "$LASSDAS_E2E_SESSION_STATE_FILE" "${LASSDAS_E2E_SESSION_FILE:-}" "${KUBECONFIG:-}" "${AWS_WEB_IDENTITY_TOKEN_FILE:-}" "${AWS_SHARED_CREDENTIALS_FILE:-}" "${AWS_CONFIG_FILE:-}" /var/run/secrets/kubernetes.io/serviceaccount/token; printf '%s\n' "${LASSDAS_GUARDED_FILES:-}" | tr ':' '\n')"
if [ -n "${KUBECONFIG:-}" ] && [ -r "$KUBECONFIG" ]; then
  # The files the kubeconfig names, quoted or not, taken relative to its
  # own directory when relative (as the client takes them).
  KUBECONFIG_DIR="$(dirname "$KUBECONFIG")"
  while IFS= read -r NAMED_FILE; do
    [ -n "$NAMED_FILE" ] || continue
    case "$NAMED_FILE" in /*) ;; *) NAMED_FILE="$KUBECONFIG_DIR/$NAMED_FILE" ;; esac
    [ -e "$NAMED_FILE" ] || echo "note: $KUBECONFIG names $NAMED_FILE, which does not exist" >&2
    GUARDED_LIST="$GUARDED_LIST
$NAMED_FILE"
  done <<EOF
$(sed -nE 's/^[[:space:]]*(tokenFile|token-file|client-key):[[:space:]]*"?([^"]*[^"[:space:]])"?[[:space:]]*$/\2/p' "$KUBECONFIG")
EOF
fi
BOOT_REFUSED=""
while IFS= read -r GUARDED; do
  [ -n "$GUARDED" ] && [ -e "$GUARDED" ] || continue
  if [ -O "$GUARDED" ] && [ -n "$(find "$GUARDED" -maxdepth 0 -perm /077 2>/dev/null)" ]; then
    chmod go-rwx "$GUARDED" 2>/dev/null && echo "agent separation: tightened $GUARDED to this user alone (0600)"
  fi
  if "$LASSDAS_AGENT_LAUNCHER" --check "$GUARDED"; then
    echo "agent separation: $GUARDED is closed to the agent user"
  else
    CHECK_RC=$?
    case $CHECK_RC in
      3) echo "REFUSING TO START: the agent user can read $GUARDED (a secret volume needs defaultMode: 0440 with the pod's fsGroup; a file of the engine's user needs 0600)" >&2; BOOT_REFUSED=1 ;;
      2) echo "REFUSING TO START: $LASSDAS_AGENT_LAUNCHER cannot switch users (file capabilities missing, or allowPrivilegeEscalation: false on the container)" >&2; BOOT_REFUSED=1 ;;
      *) echo "note: agent separation check of $GUARDED returned $CHECK_RC" >&2 ;;
    esac
  fi
done <<EOF
$(printf '%s\n' "$GUARDED_LIST" | sort -u)
EOF
[ -z "$BOOT_REFUSED" ] || exit 1

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
# down a card mid-run. Basic authentication is required by default; the
# explicit local mode serves a read-only board published to host loopback.
statusboard_loop() {
  while true; do
    statusboard || echo "statusboard exited rc=$?" >&2
    sleep 5
  done
}
# Local viewing is explicit; Basic mode needs both credential sources so
# a partial secret does not become a permanent 5-second crash loop.
if [ "${LASSDAS_BOARD_AUTH:-}" = local ] || { [ -n "${LASSDAS_BOARD_USER:-}" ] && [ -n "${LASSDAS_BOARD_PASS_FILE:-}" ]; }; then
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
