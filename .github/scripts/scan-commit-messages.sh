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
list="${ENGINE_PURITY_TOKENS:-}"
if [ -z "${list//[[:space:]]/}" ]; then
  echo "ENGINE_PURITY_TOKENS is not set; the commit-message scan is skipped"
  exit 0
fi
# The list is read the way the Go scan (rewrite/internal/enginepurity) reads
# it: split at commas and line breaks (CR or LF), each entry trimmed of the
# blanks around it and lowered, empty entries dropped, and the rest numbered
# from 1, so that #N names the same entry in both scans.
tokens=()
while IFS= read -r token; do
  token="${token#"${token%%[![:space:]]*}"}"
  token="${token%"${token##*[![:space:]]}"}"
  if [ -n "$token" ]; then
    tokens+=("$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]')")
  fi
done < <(printf '%s\n' "$list" | tr ',\r' '\n')
if [ "${#tokens[@]}" -eq 0 ]; then
  echo "ENGINE_PURITY_TOKENS is set but holds no tokens" >&2
  exit 1
fi
if [ -n "$RANGE_BASE" ] && git cat-file -e "$RANGE_BASE" 2>/dev/null; then
  range=("$RANGE_BASE..$RANGE_HEAD")
  # Anywhere but on the default branch itself (a branch, a pull request, a
  # tag), what the default branch already holds is not read either: a branch
  # that merges main to catch up brings main's published history into its
  # range, and no change on the branch can reword that.
  if [ "${GITHUB_REF:-}" != "refs/heads/${DEFAULT_BRANCH:-}" ] &&
     git rev-parse --verify --quiet "refs/remotes/origin/${DEFAULT_BRANCH:-}^{commit}" >/dev/null; then
    range+=(--not "refs/remotes/origin/$DEFAULT_BRANCH")
    echo "reading the messages of $RANGE_BASE..$RANGE_HEAD that $DEFAULT_BRANCH does not hold"
  else
    echo "reading the messages of $RANGE_BASE..$RANGE_HEAD"
  fi
elif [ "${GITHUB_REF:-}" = "refs/heads/${DEFAULT_BRANCH:?DEFAULT_BRANCH is not set}" ]; then
  # A push to the default branch itself without a usable base (a force push
  # of it): it holds every commit it has, so every reachable message is read,
  # as before.
  range=("$RANGE_HEAD")
  echo "no usable base on $DEFAULT_BRANCH itself; reading every reachable message"
else
  # Without a usable base (the first push of a branch, a force push), the
  # messages read are those of the commits the default branch does not hold
  # yet: every commit of the branch, mid-branch ones included, and none of
  # the history already published, which no change on a branch can reword.
  default="refs/remotes/origin/$DEFAULT_BRANCH"
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
# What a pushed commit can still be reworded on is a branch; on the default
# branch itself the commit is already published.
if [ "${GITHUB_REF:-}" = "refs/heads/${DEFAULT_BRANCH:-}" ]; then
  remedy="the commit is on $DEFAULT_BRANCH already; only rewriting $DEFAULT_BRANCH's history removes it"
else
  remedy="reword the commit"
fi
failed=0
for index in "${!tokens[@]}"; do
  # Not `printf | grep -q`: grep stops at the first match, the printf still
  # writing a long text dies of SIGPIPE, and pipefail turns the match into a
  # miss. A here-string leaves nothing to cut short.
  if grep -qF -e "${tokens[index]}" <<< "$lowered"; then
    echo "a commit message in the pushed range contains forbidden identifier #$((index + 1)) - $remedy"
    failed=1
  fi
done
if [ "$failed" -eq 0 ]; then
  echo "the messages of $count commits hold none of the listed identifiers"
fi
exit "$failed"
