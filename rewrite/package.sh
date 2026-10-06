#!/bin/sh
# Build an explicitly new local bundle; do not install or change a service.
set -eu
if [ "$#" -ne 1 ]; then
  echo 'usage: sh package.sh NEW_OUTPUT_DIRECTORY' >&2
  exit 2
fi
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
# Refuse an existing path, including a symlink. Leave a failed partial build
# for inspection rather than recursively deleting an operator-selected path.
mkdir -- "$1"
output_dir=$(CDPATH= cd -- "$1" && pwd -P)
mkdir "$output_dir/bin" "$output_dir/harnesses" "$output_dir/examples"
for command in engine tracker status; do
  GOMAXPROCS="${GOMAXPROCS:-2}" CGO_ENABLED="${CGO_ENABLED:-0}" \
    go -C "$source_dir" build -p 1 -trimpath \
      -o "$output_dir/bin/ticket-$command" "./cmd/$command"
done
for harness in git_workspace hermes raven linux_role adversarial_review delivery_support; do
  cp "$source_dir/harnesses/$harness.py" "$output_dir/harnesses/$harness.py"
done
for guide in START RUNTIME README OPERATING; do
  cp "$source_dir/$guide.md" "$output_dir/$guide.md"
done
cp "$source_dir/THIRD-PARTY-NOTICES.txt" "$output_dir/THIRD-PARTY-NOTICES.txt"
for example in operator operator-gateway operator-stages operator-github operator-github-stages; do
  cp "$source_dir/examples/$example.json" "$output_dir/examples/$example.json"
done
printf '%s\n' "$output_dir"
