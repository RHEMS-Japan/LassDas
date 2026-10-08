#!/usr/bin/env bash
# Builds and tests a small crate (rust-toolchain-check/) inside a built runtime
# image the way a configured validation command runs inside a role: a cleared
# environment with the role's PATH, HOME on a writable /tmp, the checkout and
# the image's own files read-only, no network, the image's user. cargo writes
# its build into HOME (CARGO_TARGET_DIR), never into the checkout. It shows
# that rustc links with the image's gcc, that a unit test and a doc test run,
# and which versions the image carries. Anything else fails here.
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
output="$(docker run --rm --network none --platform linux/arm64 --read-only --tmpfs /tmp \
  -v "$crate:/work:ro" --workdir /work --entrypoint /usr/bin/env "$1" -i \
  PATH=/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin HOME=/tmp/home \
  bash -c 'set -euo pipefail
    mkdir -p "$HOME"
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
