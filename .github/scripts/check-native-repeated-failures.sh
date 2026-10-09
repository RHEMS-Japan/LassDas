#!/usr/bin/env bash
# Runs the packaged Hermes bridge on the image's own pinned Hermes (the commit
# deploy/pod/Dockerfile names) against a scripted model on the container's
# loopback: no network, no real model, no credential. The same tool call failing
# the same way twelve times must end the role after the fifth failure, before a
# sixth model call, with the bridge's "Stopped:" line on stdout and stderr; four
# failures must not end it, and neither must six with the limit set to 0. A
# Hermes that stops handing the bridge its verdict on a failed call, or has no
# tool-call guardrail for the bridge to extend, runs all twelve and fails here,
# though the bridge's own tests, which stand in for Hermes, still pass. What
# passes is written in check-native-repeated-failures.py.
#
# Usage: bash .github/scripts/check-native-repeated-failures.sh IMAGE
# image-check.yml runs it on every pull request; image.yml runs it after the
# build and before the push, so an image whose Hermes fails it is not pushed.
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo 'usage: bash .github/scripts/check-native-repeated-failures.sh IMAGE' >&2
  exit 2
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
docker run --rm --network none --platform linux/arm64 \
  --read-only --tmpfs /tmp:rw,nosuid,size=256m \
  --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,source=$script_dir/check-native-repeated-failures.py,target=/check/native-stop-check.py,readonly" \
  --entrypoint /opt/hermes/bin/python "$1" -B /check/native-stop-check.py
