#!/bin/sh
# The cleanup of the preview namespace, run every ten minutes by the CronJob of
# cleanup.yaml.example with the ServiceAccount preview-cleanup. It removes,
# with the Service of the same preview:
#
#   - a Deployment past its lifetime (the annotation preview-expires-at), or
#     without a lifetime it can read;
#   - a Deployment that has had no ready Pod for 30 minutes: a Pod moved to
#     another node starts without the checkout and never gets ready, and a
#     Pod the cluster refused to create (a policy, the quota) never comes;
#   - a Deployment whose dependency step was started again (the init
#     container install restarted): the preview command removes it at once,
#     but not when it was stopped first;
#
# and a Service whose Deployment is gone. It works whether or not the engine
# is running. Exits 0 when every removal it tried went through.
#
# Needs a POSIX sh, kubectl, date that reads "-d @SECONDS" (BusyBox or GNU
# coreutils), expr, mktemp, cut, grep and rm. Without one it ends 1 before
# reading anything: without grep every Service would look like one without a
# Deployment, without expr no lifetime would end.
set -eu

for program in kubectl date expr mktemp cut grep rm; do
  if ! command -v "$program" > /dev/null 2>&1; then
    echo "the cleanup needs $program, which this image does not have; nothing was removed" >&2
    exit 1
  fi
done

namespace="${PREVIEW_NAMESPACE:?PREVIEW_NAMESPACE is not set}"
now_seconds=$(date +%s)
now=$(date -u -d "@$now_seconds" +%Y-%m-%dT%H:%M:%SZ)
cutoff=$(date -u -d "@$((now_seconds - 1800))" +%Y-%m-%dT%H:%M:%SZ)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0
removed=" "

# Both are RFC 3339 times in UTC with a Z, as the cluster and the preview
# command write them, so the earlier one sorts first.
earlier() { expr "$1" \< "$2" > /dev/null; }

readable() {
  case "$1" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z) return 0 ;;
    *) return 1 ;;
  esac
}

remove() { # name reason
  case "$removed" in *" $1 "*) return 0 ;; esac
  removed="$removed$1 "
  echo "removing preview $1: $2"
  if ! kubectl --namespace "$namespace" delete deployment,service -l "preview=$1" --ignore-not-found --wait=false; then
    echo "removing preview $1 failed" >&2
    failed=1
  fi
}

# The Services first, the Deployments after: a preview applies its Deployment
# before its Service, so a Service that was there before the Deployments were
# read has its Deployment in that list unless the Deployment is gone.
kubectl --namespace "$namespace" get services -l preview \
  -o jsonpath='{range .items[*]}{.metadata.labels.preview}{"\n"}{end}' > "$work/services"
kubectl --namespace "$namespace" get deployments -l preview \
  -o jsonpath='{range .items[*]}{.metadata.labels.preview}{"|"}{.metadata.annotations.preview-expires-at}{"|"}{.status.conditions[?(@.type=="Available")].status}{"|"}{.status.conditions[?(@.type=="Available")].lastTransitionTime}{"|"}{.metadata.creationTimestamp}{"\n"}{end}' \
  > "$work/deployments"
kubectl --namespace "$namespace" get pods -l preview \
  -o jsonpath='{range .items[*]}{.metadata.labels.preview}{"|"}{.status.initContainerStatuses[?(@.name=="install")].restartCount}{"\n"}{end}' \
  > "$work/pods"

while IFS='|' read -r name expires available since created; do
  [ -n "$name" ] || continue
  if ! readable "$expires"; then
    remove "$name" "no readable lifetime"
  elif earlier "$expires" "$now"; then
    remove "$name" "its lifetime ended at $expires"
  elif [ "$available" != "True" ]; then
    [ -n "$since" ] || since="$created"
    if readable "$since" && earlier "$since" "$cutoff"; then
      remove "$name" "no ready Pod since $since"
    fi
  fi
done < "$work/deployments"

while IFS='|' read -r name restarts; do
  [ -n "$name" ] || continue
  case "$restarts" in
    '' | *[!0-9]*) continue ;;
  esac
  if [ "$restarts" -ge 1 ]; then
    remove "$name" "its dependency step was started again ($restarts)"
  fi
done < "$work/pods"

cut -d '|' -f 1 "$work/deployments" > "$work/with-deployment"
while read -r name; do
  [ -n "$name" ] || continue
  if ! grep -Fqx -- "$name" "$work/with-deployment"; then
    remove "$name" "its Service has no Deployment"
  fi
done < "$work/services"

if [ "$removed" = " " ]; then
  echo "cleanup at $now: nothing to remove"
else
  echo "cleanup at $now: removed${removed% }"
fi
exit "$failed"
