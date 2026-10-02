#!/usr/bin/env bash
# Prints the Go version the shipped image builds with, as deploy/pod/Dockerfile
# names it in its golang stages, and fails if those stages name different
# versions. The Dockerfile is where that version is set: the engine's CI jobs
# set up the Go it names, and the packaged-engine check holds the engine built
# into the image to it.
#
# Usage: bash .github/scripts/image-go-version.sh [DOCKERFILE]
set -euo pipefail
dockerfile="${1:-$(dirname "$0")/../../deploy/pod/Dockerfile}"
versions=()
while IFS= read -r version; do
  versions+=("$version")
done < <(sed -n -E 's/^FROM[[:space:]]+golang:([0-9]+(\.[0-9]+)+)-.*$/\1/p' "$dockerfile")
if [ "${#versions[@]}" -eq 0 ]; then
  echo "no golang stage in $dockerfile names a Go version" >&2
  exit 1
fi
for version in "${versions[@]}"; do
  if [ "$version" != "${versions[0]}" ]; then
    echo "the golang stages in $dockerfile name different Go versions (${versions[*]}); the engine's checks need one" >&2
    exit 1
  fi
done
printf '%s\n' "${versions[0]}"
