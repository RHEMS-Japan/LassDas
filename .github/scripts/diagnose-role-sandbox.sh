#!/usr/bin/env bash
# Temporary: which container settings let bubblewrap set up a user namespace
# on this runner. Prints one line per attempt; never fails the job.
set -u
image=$1
echo "docker: $(docker info --format '{{.SecurityOptions}} userns={{.ID}}' 2>&1 | cut -c1-200)"
probe='echo "id=$(id -u):$(id -g) uid_map=$(tr -s " " < /proc/self/uid_map | tr "\n" ";") userns_clone=$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null) apparmor_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null) caps=$(grep CapBnd /proc/self/status)"; bwrap --unshare-user --disable-userns --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib --proc /proc --dev /dev --tmpfs /tmp --share-net --die-with-parent /bin/sh -c "echo inside: $(id -u) ok" 2>&1'
try() {
  local label=$1; shift
  local out status=0
  out="$(docker run --rm --network none --platform linux/arm64 "$@" --entrypoint /bin/sh "$image" -c "$probe" 2>&1)" || status=$?
  printf '%s -> exit %s | %s\n' "$label" "$status" "$(echo "$out" | tr '\n' ' ' | cut -c1-400)"
}
base=(--read-only --tmpfs /tmp:rw,exec,nosuid,size=256m --security-opt seccomp=unconfined --security-opt apparmor=unconfined --security-opt systempaths=unconfined)
try "baseline (cap-drop ALL)" "${base[@]}" --cap-drop ALL
try "no cap-drop" "${base[@]}"
try "cap-drop ALL + userns=host" "${base[@]}" --cap-drop ALL --userns=host
try "no cap-drop + userns=host" "${base[@]}" --userns=host
try "cap-drop ALL, no systempaths" --read-only --tmpfs /tmp:rw,exec,nosuid,size=256m --security-opt seccomp=unconfined --security-opt apparmor=unconfined --cap-drop ALL
try "cap-drop ALL, writable root" --tmpfs /tmp:rw,exec,nosuid,size=256m --security-opt seccomp=unconfined --security-opt apparmor=unconfined --security-opt systempaths=unconfined --cap-drop ALL
try "privileged (diagnostic only)" --privileged
exit 0
