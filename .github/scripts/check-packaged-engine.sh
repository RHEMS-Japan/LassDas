#!/usr/bin/env bash
# Starts the engine packaged in a built runtime image once, on the packaged
# example. The unedited example names no intake project, so --check goes
# through the checks a start makes before the intake and then refuses on
# exactly that, with exit status 1: the engine started, as the image's user
# and with no network, and read the example. Anything else fails here - a
# success, another message, or docker unable to start it.
#
# Usage: check-packaged-engine.sh IMAGE
# image-check.yml runs it on every pull request; image.yml runs it after the
# build and before the push, so an image whose engine fails it is not pushed.
set -uo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: check-packaged-engine.sh IMAGE" >&2
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
