#!/usr/bin/env bash
# Builds and tests the small crate through the real role launcher inside a built
# runtime image: linux_role.py starts bubblewrap with the mounts a role gets
# (/usr, the named /etc files, /etc/alternatives, a read-only workspace, a
# private home), so a program the image carries but a role cannot run fails
# here and not in the cluster (rustc found no `cc` in a role once: cc was a
# link through /etc/alternatives). The container runs as the image's user with
# every capability dropped, seccomp and AppArmor unconfined and /proc unmasked,
# the settings the shipped StatefulSet gives the engine container. Without the
# compiled role syscall policy the launcher's sandbox is the plain bubblewrap
# one; the mounts are the same. The launcher is given --network inherit, as
# the shipped verify processes are: with every capability dropped, bubblewrap
# cannot bring up loopback in a network namespace of its own (the first run
# failed on "loopback: Failed RTM_NEWADDR"), and the container has no network.
# On a runner whose kernel.apparmor_restrict_unprivileged_userns is 1, the
# uid map write fails ("setting up uid map: Permission denied") whatever the
# container's settings; the workflows set that sysctl to 0 first. The
# container has a memory limit, as the engine's has in a cluster, so the
# launcher runs the build under its memory guard (check-role-memory-guard.sh
# checks the guard itself); a launch without the guard fails this check.
#
# Usage: bash .github/scripts/check-role-sandbox.sh IMAGE
# image-check.yml runs it on every pull request; image.yml runs it after the
# build and before the push.
set -uo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: bash .github/scripts/check-role-sandbox.sh IMAGE" >&2
  exit 2
fi
crate="$(cd "$(dirname "$0")/rust-toolchain-check" && pwd)" || exit 1
status=0
output="$(docker run --rm --network none --platform linux/arm64 --read-only --tmpfs /tmp:rw,exec,nosuid,size=2g \
  --memory 4g --memory-swap 4g --cap-drop ALL --security-opt seccomp=unconfined --security-opt apparmor=unconfined --security-opt systempaths=unconfined \
  -v "$crate:/work:ro" --workdir /work --entrypoint /usr/bin/env "$1" -i \
  PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin TASK_WORKSPACE=/work TASK_HOME=/tmp/home \
  bash -c 'set -euo pipefail
    mkdir -p "$TASK_HOME"
    python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py --runtime /opt/ticket-automation/bundle --network inherit -- \
      /bin/sh -c "set -eu; cd /work; cc --version | head -n 1; readlink -f /usr/bin/cc; export CARGO_TARGET_DIR=\$HOME/target; cargo build --locked --offline --all-targets && cargo test --locked --offline"' 2>&1)" || status=$?
printf 'exit status %s\n%s\n' "$status" "$output"
if [ "$status" -ne 0 ]; then
  echo "::error::a role inside the image could not build and test the small crate"
  exit 1
fi
if grep -q 'The memory guard is off' <<<"$output"; then
  echo "::error::the launcher ran without its memory guard in a container with a memory limit"
  exit 1
fi
for line in "test tests::adds ... ok" "test src/lib.rs - add (line 6) ... ok"; do
  if ! grep -qxF "$line" <<<"$output"; then
    echo "::error::the test output above has no line \"$line\""
    exit 1
  fi
done
echo "a role inside the image built the small crate and ran its unit test and doc test"
