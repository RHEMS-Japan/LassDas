#!/usr/bin/env bash
# Commit messages travel with the tree, so they are held to the same standard
# as the tracked files. On a match the failure names the token only by its
# position in the list - printing the token or the message here would
# republish the identifier this gate exists to keep out.
#
# Both jobs in ci.yml run this. It reads ENGINE_PURITY_TOKENS (the list, a
# repository secret), RANGE_BASE and RANGE_HEAD from the environment, and
# needs the history of the range (a checkout with fetch-depth: 0).
set -euo pipefail
if [ -z "${ENGINE_PURITY_TOKENS:-}" ]; then
  echo "ENGINE_PURITY_TOKENS is not set; the commit-message scan is skipped"
  exit 0
fi
# Without a usable base (first push of a branch, force push), every
# reachable message is scanned - histories here are short, and a
# single-commit fallback measurably let mid-branch commits through.
if [ -z "$RANGE_BASE" ] || ! git cat-file -e "$RANGE_BASE" 2>/dev/null; then
  messages=$(git log --format=%B "$RANGE_HEAD")
  count=$(git rev-list --count "$RANGE_HEAD")
else
  messages=$(git log --format=%B "$RANGE_BASE".."$RANGE_HEAD")
  count=$(git rev-list --count "$RANGE_BASE".."$RANGE_HEAD")
fi
lowered=$(printf '%s' "$messages" | tr '[:upper:]' '[:lower:]')
index=0
failed=0
IFS=',' read -ra tokens <<< "$(printf '%s' "$ENGINE_PURITY_TOKENS" | tr '\n' ',')"
for token in "${tokens[@]}"; do
  index=$((index + 1))
  token=$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]' | xargs)
  [ -z "$token" ] && continue
  if printf '%s' "$lowered" | grep -qF "$token"; then
    echo "a commit message in the pushed range contains forbidden identifier #$index - reword the commit"
    failed=1
  fi
done
if [ "$failed" -eq 0 ]; then
  echo "the messages of $count commits hold none of the listed identifiers"
fi
exit "$failed"
