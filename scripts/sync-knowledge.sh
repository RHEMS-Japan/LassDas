#!/usr/bin/env bash
# Brings the knowledge an agent works under into this repository, from wherever
# it is kept. Nothing here reads it: the framework only places bytes where the
# configured agent looks for them.
#
# Run this whenever the source changes. What lands here is committed, so drift
# is visible as a diff rather than as an agent quietly working under old rules.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rules_source="${KNOWLEDGE_RULES_SOURCE:-$HOME/.claude/CLAUDE.md}"
library_source="${KNOWLEDGE_LIBRARY_SOURCE:-}"

if [[ ! -f "$rules_source" ]]; then
  printf 'rules were not found at %s\n' "$rules_source" >&2
  printf 'set KNOWLEDGE_RULES_SOURCE to where they are kept\n' >&2
  exit 1
fi

install -m 0644 "$rules_source" "$repo_root/knowledge/rules.md"
printf 'rules: %s -> knowledge/rules.md (%s bytes)\n' \
  "$rules_source" "$(wc -c < "$repo_root/knowledge/rules.md" | tr -d ' ')"

if [[ -n "$library_source" ]]; then
  if [[ ! -d "$library_source" ]]; then
    printf 'the library was not found at %s\n' "$library_source" >&2
    exit 1
  fi
  rm -rf -- "$repo_root/knowledge/library"
  mkdir -p "$repo_root/knowledge/library"
  # Only readable notes are taken, and only from the top level of the source:
  # anything else there was not written to be handed to an agent. Links are
  # followed so an index kept elsewhere still lands. The note about this
  # framework itself never ships: it is not knowledge about any destination.
  find -L "$library_source" -maxdepth 1 -type f -name '*.md' \
    ! -name 'lass[d]as-*.md' \
    -exec install -m 0644 {} "$repo_root/knowledge/library/" \;
  # The index may point at the withheld note; that entry indexes nothing here.
  sed -i '' '/lass[d]as-framework\.md/d' "$repo_root/knowledge/library/MEMORY.md" 2>/dev/null || true
  printf 'library: %s -> knowledge/library (%s files, %s)\n' \
    "$library_source" \
    "$(find "$repo_root/knowledge/library" -type f | wc -l | tr -d ' ')" \
    "$(du -sh "$repo_root/knowledge/library" | cut -f1)"
else
  printf 'library: skipped (set KNOWLEDGE_LIBRARY_SOURCE to include one)\n'
fi

# What lands travels into destination working copies, where an agent may quote
# it. The project codename must never be able to make that trip.
if grep -riq "[Ll]ass[Dd]as" "$repo_root/knowledge"; then
  printf 'refusing: the engine name landed in knowledge/ - consumer-facing surfaces keep neutral naming\n' >&2
  grep -ril "[Ll]ass[Dd]as" "$repo_root/knowledge" >&2
  printf 'remove it from the source notes, then run this again\n' >&2
  exit 1
fi

printf '\nreview what landed, then commit it:\n  git -C %s status --short knowledge/\n' "$repo_root"
