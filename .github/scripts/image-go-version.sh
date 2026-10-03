#!/usr/bin/env bash
# Prints the Go version the shipped image builds with, as deploy/pod/Dockerfile
# names it in its golang stages, and fails if those stages name different
# versions. The Dockerfile is where that version is set: the engine's CI jobs
# set up the Go it names, and the packaged-engine check holds the engine built
# into the image to it.
#
# Every FROM line that uses a golang image must name its version in the tag
# (golang:1.26-bookworm, golang:1.26.8, registry/library/golang:1.26-alpine),
# or this fails: a stage whose version cannot be read is not left out, and
# neither is one whose image is named through a variable (FROM ${IMAGE}),
# since whether it is golang cannot be told. Instructions are read as Docker
# reads them: without regard to case, from any indentation, with continuation
# lines joined (comment and blank lines inside dropped), past --platform or
# any other flag, with a registry or a digest around the image, and with the
# bodies of heredocs (RUN <<EOF ... EOF) left unread. A Dockerfile that sets
# its own escape character (# escape=) is refused: this script reads only the
# default backslash.
# Heredoc words are read outside ordinary quoted strings, retaining their
# quotes and escapes. Delimiters are identifiers, bare or wholly single/double
# quoted, optionally with a numeric file descriptor and <<-. Other delimiter
# forms and unfinished quoting fail explicitly instead of hiding later FROMs.
#
# Usage: bash .github/scripts/image-go-version.sh [DOCKERFILE]
set -euo pipefail
dockerfile="${1:-$(dirname "$0")/../../deploy/pod/Dockerfile}"
if [ ! -r "$dockerfile" ]; then
  echo "cannot read $dockerfile" >&2
  exit 1
fi
versions=()
problems=()
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
  case "$repository" in
    *'$'*)
      problems+=("a stage names its image through a variable, so whether it is golang cannot be told: $1")
      return 0 ;;
    golang) ;;
    *) return 0 ;;
  esac
  tag=""
  case "$name" in *:*) tag="${name#*:}" ;; esac
  if [[ "$tag" =~ ^([0-9]+\.[0-9]+(\.[0-9]+)?)(-.+)?$ ]]; then
    versions+=("${BASH_REMATCH[1]}")
  else
    problems+=("a golang stage names no Go version that can be read: $1")
  fi
}
heredocs() {  # RUN/COPY/ADD, including their ONBUILD forms
  local rest="$1"
  if [[ "$rest" =~ ^[[:space:]]*[Oo][Nn][Bb][Uu][Ii][Ll][Dd][[:space:]]+ ]]; then
    rest="${rest:${#BASH_REMATCH[0]}}"
  fi
  [[ "$rest" =~ ^[[:space:]]*([Rr][Uu][Nn]|[Cc][Oo][Pp][Yy]|[Aa][Dd][Dd])[[:space:]] ]] || return 0
  [[ "$rest" == *'<<'* ]] || return 0
  # A native string scanner avoids Bash substring expansion at every character
  # of a long instruction. Keep raw quotes/escapes; never evaluate shell text.
  printf '%s\n' "$rest" | LC_ALL=C awk '
    function fail(message) { print message > "/dev/stderr"; exit 1 }
    function save() { if (word != "") { words[++n] = word; word = "" } }
    BEGIN { single = sprintf("%c", 39); double = "\""; slash = "\\" }
    {
      for (i = 1; i <= length($0); i++) {
        char = substr($0, i, 1)
        if (escaped) { word = word char; escaped = 0; continue }
        if (char == slash && quote != single) {
          word = word char; escaped = 1; continue
        }
        if (quote != "") {
          word = word char
          if (char == quote) quote = ""
          continue
        }
        if (char == single || char == double) {
          quote = char; word = word char
        } else if (char ~ /[[:space:]]/) {
          save()
        } else {
          word = word char
        }
      }
    }
    END {
      if (quote != "" || escaped)
        fail("unfinished quoting in an instruction containing <<; cannot read heredocs safely")
      save()
      for (j = 1; j <= n; j++) {
        word = words[j]
        if (word !~ /^[0-9]*<</) continue
        rest = word
        sub(/^[0-9]*<</, "", rest)
        marker = ""
        if (substr(rest, 1, 1) == "-") { marker = "-"; rest = substr(rest, 2) }
        first = substr(rest, 1, 1)
        if (first == single || first == double) {
          if (length(rest) < 2 || substr(rest, length(rest), 1) != first)
            fail("heredoc delimiter must be wholly quoted or unquoted: " word)
          rest = substr(rest, 2, length(rest)-2)
        }
        if (rest !~ /^[A-Za-z_][A-Za-z0-9_]*$/)
          fail("unsupported heredoc delimiter (use an identifier, wholly quoted or unquoted): " word)
        print marker rest
      }
    }
  '
}
instruction=""
pending=()  # heredoc terminators still to be met, in order; "-" marks <<-
started=no
number=0
while IFS= read -r line || [ -n "$line" ]; do
  number=$((number + 1))
  line="${line%$'\r'}"
  if [ "${#pending[@]}" -gt 0 ]; then
    word="${pending[0]}"
    candidate="$line"
    if [ "${word:0:1}" = "-" ]; then
      word="${word:1}"
      candidate="${line#"${line%%[!$'\t']*}"}"
    fi
    if [ "$candidate" = "$word" ]; then
      pending=("${pending[@]:1}")
    fi
    continue
  fi
  trimmed="${line#"${line%%[![:space:]]*}"}"
  if [ "$started" = no ] && [[ "$trimmed" =~ ^#[[:space:]]*[Ee][Ss][Cc][Aa][Pp][Ee][[:space:]]*= ]]; then
    echo "$dockerfile sets its own escape character (line $number); this script reads only the default backslash" >&2
    exit 1
  fi
  if [ -z "$trimmed" ] || [ "${trimmed:0:1}" = "#" ]; then
    continue  # Docker drops these, also inside a continued instruction
  fi
  started=yes
  body="${line%"${line##*[![:space:]]}"}"
  if [ "${body%\\}" != "$body" ]; then
    instruction+="${body%\\} "
    continue
  fi
  instruction+="$line"
  instruction="${instruction#"${instruction%%[![:space:]]*}"}"
  if [[ "$instruction" =~ ^[Ff][Rr][Oo][Mm][[:space:]] ]]; then
    read_from "$instruction"
  else
    if ! markers="$(heredocs "$instruction")"; then
      exit 1
    fi
    if [ -n "$markers" ]; then
      while IFS= read -r word; do
        pending+=("$word")
      done <<< "$markers"
    fi
  fi
  instruction=""
done < "$dockerfile"
if [ -n "$instruction" ] && [[ "$instruction" =~ ^[Ff][Rr][Oo][Mm][[:space:]] ]]; then
  read_from "$instruction"
fi
if [ "${#pending[@]}" -gt 0 ]; then
  echo "a heredoc in $dockerfile never ends (it waits for ${pending[0]#-}), so the lines after it cannot be read" >&2
  exit 1
fi
if [ "${#problems[@]}" -gt 0 ]; then
  for problem in "${problems[@]}"; do
    echo "$problem (in $dockerfile)" >&2
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
