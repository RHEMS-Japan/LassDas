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
# What python3 -m unittest does with these names, and then how many tests were
# skipped for which reason: a skip (a program the runner lacks, a platform)
# is not a pass, and the log has to say which tests did not run.
runner='
import collections, sys, unittest
suite = unittest.defaultTestLoader.loadTestsFromNames(sys.argv[1:])
result = unittest.TextTestRunner(warnings=None if sys.warnoptions else "default").run(suite)
reasons = collections.Counter(reason for _, reason in result.skipped)
for reason, count in sorted(reasons.items(), key=lambda item: (-item[1], item[0])):
    print("skipped %d: %s" % (count, reason), file=sys.stderr)
sys.exit(0 if result.wasSuccessful() else 1)
'
exec python3 -B -c "$runner" "${tests[@]}"
