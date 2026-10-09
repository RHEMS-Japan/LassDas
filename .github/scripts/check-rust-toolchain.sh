#!/usr/bin/env bash
# Builds and tests a small crate (rust-toolchain-check/) inside a built runtime
# image the way a configured validation command runs inside a role: a cleared
# environment with the role's PATH, HOME on a writable /tmp, the checkout and
# the image's own files read-only, no network, the image's user. cargo writes
# its build into HOME (CARGO_TARGET_DIR), never into the checkout. It shows
# that rustc links with the image's gcc, that a unit test and a doc test run,
# and which versions the image carries. It also holds cc, gcc, cargo, rustc and
# python3 to resolving inside /usr without any link through /etc (rustc found
# no linker in a role once, when cc was a link through /etc/alternatives).
# Roles now see /etc/alternatives as well, but these five are held to /usr so
# they do not depend on it; check-role-sandbox.sh runs tools that do. Each hop
# is normalized as text, without following later links, so a relative link
# through /etc or to a place outside /usr is caught where it points; resolving
# the whole chain at once would skip a hop through /etc. Anything else fails
# here.
#
# Usage: bash .github/scripts/check-rust-toolchain.sh IMAGE
# image-check.yml runs it on every pull request; image.yml runs it after the
# build and before the push, so an image whose Rust fails it is not pushed.
set -uo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: bash .github/scripts/check-rust-toolchain.sh IMAGE" >&2
  exit 2
fi
crate="$(cd "$(dirname "$0")/rust-toolchain-check" && pwd)" || exit 1
status=0
# Docker mounts a --tmpfs noexec unless told otherwise; a role's HOME is not
# noexec, and cargo test runs the test programs it builds there.
output="$(docker run --rm --network none --platform linux/arm64 --read-only --tmpfs /tmp:rw,exec,nosuid,size=2g \
  -v "$crate:/work:ro" --workdir /work --entrypoint /usr/bin/env "$1" -i \
  PATH=/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin HOME=/tmp/home \
  bash -c 'set -euo pipefail
    mkdir -p "$HOME"
    for program in cc gcc cargo rustc python3; do
      path=$(command -v "$program") || { echo "no $program on the role PATH"; exit 1; }
      hops=0
      while [ -L "$path" ]; do
        next=$(readlink "$path")
        case "$next" in /*) ;; *) next="$(dirname "$path")/$next" ;; esac
        path=$(python3 -c "import os, sys; print(os.path.normpath(os.sep + sys.argv[1].lstrip(os.sep)))" "$next")
        hops=$((hops + 1))
        case "$path" in /etc/*) echo "$program resolves through $path instead of staying inside /usr"; exit 1 ;; esac
        [ "$hops" -lt 16 ] || { echo "$program: too many links"; exit 1; }
      done
      case "$path" in /usr/*) ;; *) echo "$program resolves to $path, outside /usr"; exit 1 ;; esac
      [ -x "$path" ] || { echo "$program resolves to $path, which is not executable"; exit 1; }
      echo "$program -> $path"
    done
    cc --version | head -n 1
    rustc --version; cargo --version; cargo clippy --version; rustfmt --version
    export CARGO_TARGET_DIR="$HOME/target"
    cargo build --locked --offline --all-targets
    cargo test --locked --offline 2>&1' 2>&1)" || status=$?
printf 'exit status %s\n%s\n' "$status" "$output"
if [ "$status" -ne 0 ]; then
  echo "::error::the image did not build and test the small crate with its own Rust"
  exit 1
fi
for line in "test tests::adds ... ok" "test src/lib.rs - add (line 6) ... ok"; do
  if ! grep -qxF "$line" <<<"$output"; then
    echo "::error::the test output above has no line \"$line\""
    exit 1
  fi
done
echo "the image built the small crate and ran its unit test and doc test with its own Rust"
size="$(docker image inspect --format '{{.Size}}' "$1" 2>/dev/null)" && echo "image size: $size bytes"
exit 0
