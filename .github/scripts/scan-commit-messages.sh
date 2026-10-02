#!/usr/bin/env bash
# Commit messages travel with the tree, so they are held to the same standard
# as the tracked files. On a match the failure names the token only by its
# position in the list - printing the token or the message here would
# republish the identifier this gate exists to keep out.
#
# Both jobs in ci.yml run this. It reads ENGINE_PURITY_TOKENS (the list, a
# repository secret), RANGE_BASE, RANGE_HEAD and DEFAULT_BRANCH from the
# environment, and needs the history of the range and the default branch (a
# checkout with fetch-depth: 0).
set -euo pipefail
if [ -z "${ENGINE_PURITY_TOKENS:-}" ]; then
  echo "ENGINE_PURITY_TOKENS is not set; the commit-message scan is skipped"
  exit 0
fi
if [ -n "$RANGE_BASE" ] && git cat-file -e "$RANGE_BASE" 2>/dev/null; then
  range=("$RANGE_BASE..$RANGE_HEAD")
  echo "reading the messages of $RANGE_BASE..$RANGE_HEAD"
else
  # Without a usable base (the first push of a branch, a force push), the
  # messages read are those of the commits the default branch does not hold
  # yet: every commit of the branch, mid-branch ones included, and none of
  # the history already published, which no change on a branch can reword.
  default="refs/remotes/origin/${DEFAULT_BRANCH:?DEFAULT_BRANCH is not set}"
  if ! git rev-parse --verify --quiet "$default^{commit}" >/dev/null; then
    echo "there is no usable base and $default is not in the checkout, so the new commits cannot be told apart" >&2
    exit 1
  fi
  range=("$RANGE_HEAD" --not "$default")
  echo "no usable base; reading the messages of the commits $DEFAULT_BRANCH does not hold"
fi
messages=$(git log --format=%B "${range[@]}")
count=$(git rev-list --count "${range[@]}")
lowered=$(printf '%s' "$messages" | tr '[:upper:]' '[:lower:]')
index=0
failed=0
IFS=',' read -ra tokens <<< "$(printf '%s' "$ENGINE_PURITY_TOKENS" | tr '\n' ',')"
for token in "${tokens[@]}"; do
  index=$((index + 1))
  token=$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]' | xargs)
  [ -z "$token" ] && continue
  # Not `printf | grep -q`: grep stops at the first match, the printf still
  # writing a long text dies of SIGPIPE, and pipefail turns the match into a
  # miss. A here-string leaves nothing to cut short.
  if grep -qF -e "$token" <<< "$lowered"; then
    echo "a commit message in the pushed range contains forbidden identifier #$index - reword the commit"
    failed=1
  fi
done
if [ "$failed" -eq 0 ]; then
  echo "the messages of $count commits hold none of the listed identifiers"
fi
exit "$failed"
