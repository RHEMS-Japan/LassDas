#!/usr/bin/env bash
# Every test file belongs to exactly one group; new files join the remainder.
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo 'usage: test-harness-group.sh adversarial|remaining' >&2
  exit 2
fi
group=$1
case "$group" in adversarial|remaining) ;; *) echo 'unknown harness group' >&2; exit 2 ;; esac
cd "$(dirname "$0")/../../rewrite/harnesses"
shopt -s nullglob
tests=()
for file in test_*.py; do
  if [[ "$group" == adversarial && "$file" != test_adversarial_review.py ]]; then continue; fi
  if [[ "$group" == remaining && "$file" == test_adversarial_review.py ]]; then continue; fi
  tests+=("${file%.py}")
done
if [ "${#tests[@]}" -eq 0 ]; then
  echo 'the selected harness group has no test files' >&2
  exit 1
fi
printf 'Python harness group: %s (%s files)\n' "$group" "${#tests[@]}"
exec python3 -B -m unittest "${tests[@]}"
