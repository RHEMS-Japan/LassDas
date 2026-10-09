#!/usr/bin/env bash
# The role launcher's memory guard inside a built runtime image, in a container
# with a memory limit (the engine's StatefulSet gives it one, so in a cluster
# every role is launched through the guard):
#
# 1. A role process that keeps allocating is stopped by the launcher before
#    the kernel runs out of memory for the container: the launcher's line
#    names it on the role's standard error (the record) and on the process's
#    own standard error (what the tool that started it shows), the role's
#    shell goes on to its next command, the launcher ends 0 and the kernel
#    stopped nothing (memory.events oom_kill 0). Then two processes of one
#    role grow side by side, as a build tool's two compilers do: both are
#    stopped in turn, and the kernel still stopped nothing.
# 2. The launcher's tests (rewrite/harnesses/test_linux_role.py) run in the
#    image, where bubblewrap is: the tests of the guard and of the launch it
#    supervises are skipped in the Python job of ci.yml, which has none. A
#    skipped test fails this check.
#
# The container runs as check-role-sandbox.sh's does (the image's user, every
# capability dropped, seccomp, AppArmor and system paths unconfined). The
# runner's kernel.apparmor_restrict_unprivileged_userns must be 0, as for that
# script.
#
# Usage: bash .github/scripts/check-role-memory-guard.sh IMAGE
set -uo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: bash .github/scripts/check-role-memory-guard.sh IMAGE" >&2
  exit 2
fi
harnesses="$(cd "$(dirname "$0")/../../rewrite/harnesses" && pwd)" || exit 1
container=(--rm --network none --platform linux/arm64 --read-only --tmpfs /tmp:rw,exec,nosuid,size=64m
  --cap-drop ALL --security-opt seccomp=unconfined --security-opt apparmor=unconfined
  --security-opt systempaths=unconfined)

status=0
output="$(docker run -i "${container[@]}" --memory 1g --memory-swap 1g --entrypoint /usr/bin/env "$1" -i \
  PATH=/usr/local/bin:/usr/bin:/bin TASK_WORKSPACE=/tmp/work TASK_HOME=/tmp/home bash -s 2>&1 <<'CHECK'
set -u
mkdir -p "$TASK_WORKSPACE" "$TASK_HOME"
# 64 MiB more every PAUSE seconds, up to 4 GiB: four times the limit.
cat > "$TASK_WORKSPACE/allocate.py" <<'PY'
import sys, time
held = []
while len(held) < 64:
    block = bytearray(64 << 20)
    block[::4096] = b"\x01" * (len(block) // 4096)
    held.append(block)
    time.sleep(float(sys.argv[1]))
print("the allocator was never stopped")
PY
# The allocator's standard error is a pipe to the "tool" (sed), as a compiler's is to its build tool.
cat > "$TASK_WORKSPACE/role.sh" <<'ROLE'
python3 -B allocate.py 0.1 2>&1 | sed -u 's/^/tool| /'
echo "the allocator ended with ${PIPESTATUS[0]}"
ROLE
cat > "$TASK_WORKSPACE/two.sh" <<'ROLE'
python3 -B allocate.py 0.05 & first=$!
python3 -B allocate.py 0.05 & second=$!
wait $first; a=$?; wait $second; b=$?
echo "the two allocators ended with $a and $b"
ROLE
python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py --network none -- /bin/bash role.sh </dev/null
echo "launcher exit status $?"
echo "kernel out-of-memory kills in this container: $(sed -n 's/^oom_kill //p' /sys/fs/cgroup/memory.events)"
python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py --network none -- /bin/bash two.sh </dev/null 2>&1 |
  sed -u 's/^/two| /'
echo "two: launcher exit status ${PIPESTATUS[0]}"
echo "two: kernel out-of-memory kills in this container: $(sed -n 's/^oom_kill //p' /sys/fs/cgroup/memory.events)"
CHECK
)" || status=$?
printf 'exit status %s\n%s\n' "$status" "$output"
if [ "$status" -ne 0 ]; then
  echo "::error::the container of the memory guard check ended with $status"
  exit 1
fi
for pattern in '^\(launcher\) Stopped python3 \(pid [0-9]+, [0-9]+ MiB resident' \
               '^tool\| \(launcher\) Stopped python3 \(pid [0-9]+, [0-9]+ MiB resident' \
               '^the allocator ended with 137$' '^launcher exit status 0$' \
               '^kernel out-of-memory kills in this container: 0$' \
               '^two\| the two allocators ended with 137 and 137$' '^two: launcher exit status 0$' \
               '^two: kernel out-of-memory kills in this container: 0$'; do
  if ! grep -qE "$pattern" <<<"$output"; then
    echo "::error::the output above has no line matching $pattern"
    exit 1
  fi
done
if grep -q 'The memory guard is off' <<<"$output"; then
  echo "::error::the launcher ran without its memory guard in a container with a memory limit"
  exit 1
fi
stops="$(grep -cE '^two\| \(launcher\) Stopped python3 ' <<<"$output")"
if [ "$stops" -ne 2 ]; then
  echo "::error::the two growing processes were stopped $stops times, not once each"
  exit 1
fi
echo "the launcher stopped the role's allocating processes, said why, and the kernel stopped nothing"

status=0
output="$(docker run "${container[@]}" --memory 2g --memory-swap 2g -v "$harnesses:/harnesses:ro" --workdir /harnesses \
  --entrypoint /usr/bin/env "$1" -i PATH=/usr/local/bin:/usr/bin:/bin \
  python3 -B -m unittest -v test_linux_role 2>&1)" || status=$?
printf 'exit status %s\n%s\n' "$status" "$output"
if [ "$status" -ne 0 ]; then
  echo "::error::the launcher's tests failed in the image"
  exit 1
fi
if grep -qE 'skipped|^OK \(' <<<"$output"; then
  echo "::error::a launcher test was skipped in the image, where every one of them can run"
  exit 1
fi
echo "the launcher's tests ran in the image, none skipped"
