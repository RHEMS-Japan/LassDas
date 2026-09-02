#!/usr/bin/env bash
# Build, push and roll out one engine release, with the tool pins read from
# the image instead of copied by hand.
#
# Every engine change must go through this script: the runner refuses to
# start when runtime.json's sha256 pins disagree with the stage binaries in
# the image, and copying the pins by hand is exactly the step that was
# skipped once (a live ticket died on "worker binary does not match its
# configured sha256 pin"). Here the pins come from /etc/lassdas/tool-pins.txt
# inside the freshly built image, so ConfigMap and image can never drift.
#
# Usage:
#   deploy/pod/release.sh <image-repo> [--apply]
#
#   <image-repo>  registry path without a tag (e.g. 123.dkr.ecr.../lassdas/runtime)
#   --apply       actually patch the ConfigMap and set the image; without it
#                 the script builds, pushes, prints the new pins and the
#                 runtime.json diff, and stops.
#
# Environment (all required, none defaulted to a private value):
#   KUBE_CONTEXT      kubectl context of the cluster
#   KUBE_NAMESPACE    namespace of the StatefulSet
#   RELEASE_BUILD_DIR scratch directory for the build context. It must not
#                     exist, be empty, or hold a previous build context of
#                     this script (it is synced with --delete). On macOS
#                     keep it outside ~/Documents, ~/Desktop and ~/Downloads
#                     (a bind of those into Docker revokes the caller's own
#                     file access until restart)
# Optional:
#   STATEFULSET       (default: lassdas)
#   CONFIGMAP         (default: lassdas-config)
#   CONTAINER         container to update (default: the StatefulSet's first)
#
# Run it from the engine checkout root with a clean, committed tree: the
# engine sha baked into the image is HEAD, and an uncommitted change would
# ship under a commit that does not contain it.
set -euo pipefail

usage() { sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

image_repo="${1:-}"
apply="${2:-}"
[[ -n "$image_repo" ]] || usage
[[ -z "$apply" || "$apply" == "--apply" ]] || usage
: "${KUBE_CONTEXT:?set KUBE_CONTEXT}"
: "${KUBE_NAMESPACE:?set KUBE_NAMESPACE}"
: "${RELEASE_BUILD_DIR:?set RELEASE_BUILD_DIR (outside macOS TCC-protected folders)}"
statefulset="${STATEFULSET:-lassdas}"
configmap="${CONFIGMAP:-lassdas-config}"

say() { printf '\n== %s\n' "$*"; }
kc() { kubectl --context "$KUBE_CONTEXT" -n "$KUBE_NAMESPACE" "$@"; }

# ---- 1. engine identity ---------------------------------------------------
[[ -f deploy/pod/Dockerfile && -f go.mod ]] || { echo "run from the engine checkout root" >&2; exit 2; }
if [[ -n "$(git status --porcelain)" ]]; then
  echo "the working tree has uncommitted changes; commit first so the engine sha names what ships" >&2
  exit 2
fi
engine_sha="$(git rev-parse HEAD)"
say "engine $engine_sha"

# ---- 1b. the public CI must have judged this exact commit ------------------
# The purity gate (internal/enginepurity) runs only where the operator's
# token list exists — in CI — so a green local `go test` says nothing about
# it. This branch shipped for two days with that gate red before anyone
# looked; a release now refuses any commit whose CI run is not green, which
# also means the commit must be pushed first.
say "CI verdict for $engine_sha"
repo_slug="$(git remote get-url origin | sed -E 's#.*[:/]([^/]+/[^/]+?)(\.git)?$#\1#')"
ci_verdict="$(gh run list --repo "$repo_slug" --commit "$engine_sha" --limit 1 --json status,conclusion --jq '.[0] | "\(.status)/\(.conclusion)"' 2>/dev/null || true)"
echo "${ci_verdict:-no run found}"
[[ "$ci_verdict" == "completed/success" ]] || {
  echo "CI for $engine_sha is not green (${ci_verdict:-no run found}); push the commit, wait for a green run, then release" >&2
  exit 2
}

# ---- 2. what is live, read before anything is built or changed ------------
# Every value the rollout will touch is read and checked up front, so a
# mismatch stops the script before it has built anything, and long before
# it has changed anything.
say "live configuration ($KUBE_CONTEXT / $KUBE_NAMESPACE)"
container="${CONTAINER:-$(kc get "statefulset/$statefulset" -o jsonpath='{.spec.template.spec.containers[0].name}')}"
[[ -n "$container" ]] || { echo "could not read the container name from statefulset/$statefulset" >&2; exit 1; }
kc get "statefulset/$statefulset" -o jsonpath='{.spec.template.spec.containers[*].name}' | tr ' ' '\n' | grep -qx "$container" \
  || { echo "statefulset/$statefulset has no container named $container" >&2; exit 1; }
current="$(kc get configmap "$configmap" -o jsonpath='{.data.runtime\.json}')"
[[ -n "$current" ]] || { echo "configmap/$configmap has no runtime.json" >&2; exit 1; }
echo "$current" | python3 -c 'import json,sys; c=json.load(sys.stdin); print("container:", sys.argv[1]); print("engine:", c["identity"]["engine_sha"]); [print(k+":", c.get(k) or "(unset — its pin will not be written)") for k in ("worker_bin","controller_bin","browsercheck_bin")]' "$container"

# ---- 3. tests are the regression set --------------------------------------
say "go test ./... (the adversarial regression set; a release never skips it)"
go test ./... -count=1 2>&1 | grep -vE '^ok|no test files' || true
go test ./... -count=1 >/dev/null 2>&1 || { echo "tests failed; nothing was built" >&2; exit 1; }

# ---- 4. build context outside the checkout --------------------------------
# --delete would empty a mistyped directory; only a fresh directory or a
# previous context of this script is accepted.
if [[ -e "$RELEASE_BUILD_DIR" ]]; then
  if [[ ! -d "$RELEASE_BUILD_DIR" ]] || { [[ -n "$(ls -A "$RELEASE_BUILD_DIR")" ]] && [[ ! -f "$RELEASE_BUILD_DIR/deploy/pod/Dockerfile" ]]; }; then
    echo "RELEASE_BUILD_DIR=$RELEASE_BUILD_DIR is not empty and is not a previous build context; refusing to sync into it" >&2
    exit 2
  fi
fi
say "build context → $RELEASE_BUILD_DIR"
mkdir -p "$RELEASE_BUILD_DIR"
rsync -a --delete --exclude .git --exclude node_modules ./ "$RELEASE_BUILD_DIR/"

# ---- 5. build + push (arm64, no cache: a stale layer ships old code) -------
tag="$image_repo:$engine_sha"
say "docker build $tag"
docker build --platform linux/arm64 --no-cache --pull \
  --build-arg "ENGINE_SHA=$engine_sha" \
  -t "$tag" -f "$RELEASE_BUILD_DIR/deploy/pod/Dockerfile" "$RELEASE_BUILD_DIR"
docker push "$tag" >/dev/null
digest="$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$tag" | grep "^$image_repo@sha256:" | head -n 1)"
[[ "$digest" == "$image_repo@sha256:"* ]] || { echo "could not read the pushed digest for $image_repo" >&2; exit 1; }
say "pushed $digest"

# ---- 6. pins and toolchain, read from the image itself --------------------
say "tool pins from the image"
pins="$(docker run --rm --platform linux/arm64 --entrypoint cat "$tag" /etc/lassdas/tool-pins.txt)"
echo "$pins"
pin_of() { awk -v name="$1" '$2 == name { print $1 }' <<<"$pins"; }
worker_sha="$(pin_of worker)"; controller_sha="$(pin_of controller)"; browsercheck_sha="$(pin_of browsercheck)"
for value in "$worker_sha" "$controller_sha" "$browsercheck_sha"; do
  [[ "$value" =~ ^[0-9a-f]{64}$ ]] || { echo "tool-pins.txt is incomplete" >&2; exit 1; }
done
say "toolchain present in the image"
docker run --rm --platform linux/arm64 --entrypoint go "$tag" version
docker run --rm --platform linux/arm64 --entrypoint node "$tag" --version

# ---- 7. runtime.json with the new identity --------------------------------
# A pin is written only for a stage binary the live configuration names:
# the runner opens every pinned path, so a pin for an unset binary would
# stop every run.
say "runtime.json changes"
updated="$(python3 - "$engine_sha" "$worker_sha" "$controller_sha" "$browsercheck_sha" "$current" <<'PY'
import json, sys
engine, worker, controller, browsercheck, current = sys.argv[1:6]
config = json.loads(current)
config["identity"]["engine_sha"] = engine
for binary, pin, value in (("worker_bin", "worker_sha256", worker), ("controller_bin", "controller_sha256", controller), ("browsercheck_bin", "browsercheck_sha256", browsercheck)):
    if config.get(binary):
        config[pin] = value
    else:
        config.pop(pin, None)
print(json.dumps(config, ensure_ascii=False, indent=2))
PY
)"
diff <(echo "$current" | python3 -m json.tool --no-ensure-ascii) <(echo "$updated") || true

if [[ "$apply" != "--apply" ]]; then
  say "dry run — nothing applied. To roll out:"
  echo "  $0 $image_repo --apply"
  exit 0
fi

# ---- 8. apply: the ConfigMap key alone, then the image ---------------------
# A merge patch touches only runtime.json; the ConfigMap also carries the
# consumer config and knowledge, which a whole-object apply would drop. If
# the image update fails, the key is put back so pins and binaries never
# stay crossed.
say "patch configmap/$configmap (runtime.json only)"
patch_new="$(mktemp)"; patch_old="$(mktemp)"
python3 -c 'import json,sys; print(json.dumps({"data": {"runtime.json": sys.argv[1]}}))' "$updated" > "$patch_new"
python3 -c 'import json,sys; print(json.dumps({"data": {"runtime.json": sys.argv[1]}}))' "$current" > "$patch_old"
kc patch configmap "$configmap" --type merge --patch-file "$patch_new"
restore_configmap() {
  echo "image update failed; restoring the previous runtime.json" >&2
  kc patch configmap "$configmap" --type merge --patch-file "$patch_old" || echo "RESTORE FAILED — fix configmap/$configmap by hand" >&2
}
say "set image $container=$digest"
if ! kc set image "statefulset/$statefulset" "$container=$digest"; then
  restore_configmap
  exit 1
fi
rm -f "$patch_new" "$patch_old"
kc rollout status "statefulset/$statefulset" --timeout=300s

# ---- 9. the pod's own verdict is the only acceptance --------------------
say "pod identity check"
kc get pod -l "app=$statefulset" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.spec.containers[0].image}{"\n"}{end}'
echo "watch the attendant log for a pin failure before calling this done:"
echo "  kubectl --context $KUBE_CONTEXT -n $KUBE_NAMESPACE logs statefulset/$statefulset --since=2m | grep -i 'sha256 pin' || echo 'no pin failure logged'"
