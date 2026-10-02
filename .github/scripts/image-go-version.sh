#!/usr/bin/env bash
# Prints the Go version the shipped image builds with, as deploy/pod/Dockerfile
# names it in its golang stages, and fails if those stages name different
# versions. The Dockerfile is where that version is set: the engine's CI jobs
# set up the Go it names, and the packaged-engine check holds the engine built
# into the image to it.
#
# Every FROM line that uses a golang image must name its version in the tag
# (golang:1.26-bookworm, golang:1.26.8, registry/library/golang:1.26-alpine),
# or this fails: a stage whose version cannot be read is not left out. The
# instruction is read without regard to case, with its continuation lines
# joined, and past any --platform or other flag.
#
# Usage: bash .github/scripts/image-go-version.sh [DOCKERFILE]
set -euo pipefail
dockerfile="${1:-$(dirname "$0")/../../deploy/pod/Dockerfile}"
if [ ! -r "$dockerfile" ]; then
  echo "cannot read $dockerfile" >&2
  exit 1
fi
versions=()
unreadable=()
read_from() {  # one FROM instruction, continuation lines joined
  local words word image ref name repository tag
  read -ra words <<< "$1"
  image=""
  for word in "${words[@]:1}"; do
    case "$word" in
      --*) ;;
      *) image="$word"; break ;;
    esac
  done
  [ -n "$image" ] || return 0
  ref="${image%%@*}"
  name="${ref##*/}"
  repository="$(printf '%s' "${name%%:*}" | tr '[:upper:]' '[:lower:]')"
  [ "$repository" = golang ] || return 0
  tag=""
  case "$name" in *:*) tag="${name#*:}" ;; esac
  if [[ "$tag" =~ ^([0-9]+\.[0-9]+(\.[0-9]+)?)(-.+)?$ ]]; then
    versions+=("${BASH_REMATCH[1]}")
  else
    unreadable+=("$1")
  fi
}
instruction=""
while IFS= read -r line || [ -n "$line" ]; do
  line="${line%$'\r'}"
  if [ -z "$instruction" ]; then
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in '#'*) continue ;; esac
  fi
  body="${line%"${line##*[![:space:]]}"}"
  if [ "${body%\\}" != "$body" ]; then
    instruction+="${body%\\} "
    continue
  fi
  instruction+="$line"
  instruction="${instruction#"${instruction%%[![:space:]]*}"}"
  if [[ "$instruction" =~ ^[Ff][Rr][Oo][Mm][[:space:]] ]]; then
    read_from "$instruction"
  fi
  instruction=""
done < "$dockerfile"
if [ -n "$instruction" ] && [[ "$instruction" =~ ^[Ff][Rr][Oo][Mm][[:space:]] ]]; then
  read_from "$instruction"
fi
if [ "${#unreadable[@]}" -gt 0 ]; then
  for line in "${unreadable[@]}"; do
    echo "a golang stage in $dockerfile names no Go version that can be read: $line" >&2
  done
  exit 1
fi
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
