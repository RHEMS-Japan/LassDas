#!/usr/bin/env bash
# Starts the engine packaged in a built runtime image once, on the packaged
# example. The unedited example names no intake project, so --check goes
# through the checks a start makes before the intake and then refuses on
# exactly that, with exit status 1: the engine started, as the image's user
# and with no network, and read the example. Anything else fails here - a
# success, another message, or docker unable to start it.
#
# Then it reads which Go built that engine, with the Go the image carries,
# and holds it to the version deploy/pod/Dockerfile names
# (image-go-version.sh): the version the engine's CI jobs test with. A
# Dockerfile naming a series (1.26) accepts any release of it (go1.26.8).
#
# Usage: bash .github/scripts/check-packaged-engine.sh IMAGE
# image-check.yml runs it on every pull request; image.yml runs it after the
# build and before the push, so an image whose engine fails it is not pushed.
set -uo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: bash .github/scripts/check-packaged-engine.sh IMAGE" >&2
  exit 2
fi
expected="watch requires an explicit intake.project_id"
status=0
output="$(docker run --rm --network none --platform linux/arm64 \
  --entrypoint /opt/ticket-automation/bundle/bin/ticket-engine "$1" \
  --config /opt/ticket-automation/bundle/examples/operator.json --check 2>&1)" || status=$?
printf 'exit status %s\n%s\n' "$status" "$output"
if [ "$status" -ne 1 ] || ! grep -qxF "$expected" <<<"$output"; then
  echo "::error::the packaged engine did not refuse the unedited example with \"$expected\" and exit status 1"
  exit 1
fi
echo "the packaged engine started and refused the unedited example as expected"
named="$(bash "$(dirname "$0")/image-go-version.sh")" || exit 1
built="$(docker run --rm --network none --platform linux/arm64 \
  --entrypoint /usr/local/go/bin/go "$1" \
  version /opt/ticket-automation/bundle/bin/ticket-engine 2>&1)" || {
  printf '%s\n' "$built"
  echo "::error::could not read which Go built the packaged engine"
  exit 1
}
printf '%s\n' "$built"
built="${built##* }"
case "$built" in
  "go$named" | "go$named".*)
    echo "the packaged engine was built with $built, the Go deploy/pod/Dockerfile names ($named)" ;;
  *)
    echo "::error::the packaged engine was built with $built, not the Go deploy/pod/Dockerfile names ($named)"
    exit 1 ;;
esac
