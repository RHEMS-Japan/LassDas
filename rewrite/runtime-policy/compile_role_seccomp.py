"""Compile an operator's syscall profile into a filter the role launcher loads.

This consumes operator policy, never a model's output. It reads a profile in
the [Moby profile format](https://github.com/moby/profiles/blob/main/seccomp/default.json)
and writes one BPF filter for the running machine's native ABI, with every
capability-conditioned rule skipped because roles hold no capability. Rules
naming a syscall this kernel's libseccomp does not know are reported and left
to the default action, which denies them. Alternate ABIs are not compiled, so
they remain denied as well.

    python3 -B compile_role_seccomp.py PROFILE.json /runtime-policy/role-filter.bpf

The output path must not exist. This program changes no host, node or
container policy: the filter only takes effect when the launcher loads it for
a role, and loading it is not by itself a statement that the profile is the
right policy for the work being admitted.
"""
import ctypes
import ctypes.util
import json
import os
from pathlib import Path
import platform
import sys

# libseccomp's action encoding: allow, or return this errno.
ALLOW = 0x7FFF0000
ERRNO = 0x00050000
# The profile's comparison operators, in libseccomp's order.
OPERATORS = {"SCMP_CMP_NE": 1, "SCMP_CMP_LT": 2, "SCMP_CMP_LE": 3, "SCMP_CMP_EQ": 4,
             "SCMP_CMP_GE": 5, "SCMP_CMP_GT": 6, "SCMP_CMP_MASKED_EQ": 7}
MACHINES = {"aarch64": "arm64", "x86_64": "amd64"}
CONDITIONS = {"caps", "arches", "minKernel"}


class Comparison(ctypes.Structure):
    _fields_ = [("arg", ctypes.c_uint), ("op", ctypes.c_int),
                ("datum_a", ctypes.c_uint64), ("datum_b", ctypes.c_uint64)]


def library():
    name = ctypes.util.find_library("seccomp")
    if not name:
        raise RuntimeError("libseccomp is not installed; no filter was written and none was assumed")
    loaded = ctypes.CDLL(name, use_errno=True)
    loaded.seccomp_init.argtypes, loaded.seccomp_init.restype = [ctypes.c_uint32], ctypes.c_void_p
    loaded.seccomp_syscall_resolve_name.argtypes = [ctypes.c_char_p]
    loaded.seccomp_syscall_resolve_name.restype = ctypes.c_int
    loaded.seccomp_rule_add_array.argtypes = [ctypes.c_void_p, ctypes.c_uint32, ctypes.c_int,
                                              ctypes.c_uint, ctypes.POINTER(Comparison)]
    loaded.seccomp_rule_add_array.restype = ctypes.c_int
    loaded.seccomp_export_bpf.argtypes = [ctypes.c_void_p, ctypes.c_int]
    loaded.seccomp_export_bpf.restype = ctypes.c_int
    loaded.seccomp_release.argtypes = [ctypes.c_void_p]
    return loaded


def release(value):
    """Major and minor only; a profile's kernel bound is not a patch level."""
    return tuple(int(part) for part in value.split("-")[0].split(".")[:2])


def kernel_release():
    return release(platform.release())


def action(entry):
    name = entry["action"]
    if name == "SCMP_ACT_ALLOW":
        return ALLOW
    if name == "SCMP_ACT_ERRNO":
        return ERRNO | (entry.get("errnoRet", 1) & 0xFFFF)
    # An unhandled action would silently become something else. Refuse instead.
    raise ValueError("Unsupported profile action for a role filter: " + name)


def applies(entry, architecture, kernel):
    includes, excludes = entry.get("includes", {}), entry.get("excludes", {})
    if set(includes) - CONDITIONS or set(excludes) - CONDITIONS:
        raise ValueError("Unrecognized profile condition; the filter was not written")
    if includes.get("caps"):
        # Roles run with no capability, so a rule that needs one never applies
        # to them; its syscalls stay under the profile's denying default.
        return False
    if includes.get("arches") and architecture not in includes["arches"]:
        return False
    if architecture in excludes.get("arches", []):
        return False
    if includes.get("minKernel") and kernel < release(includes["minKernel"]):
        return False
    if excludes.get("minKernel") and kernel >= release(excludes["minKernel"]):
        return False
    return True


def compile_profile(profile, output):
    if profile.get("defaultAction") != "SCMP_ACT_ERRNO":
        raise ValueError("A role profile must deny by default")
    architecture = MACHINES.get(platform.machine())
    if not architecture:
        raise RuntimeError("This machine's architecture has no profile mapping: " + platform.machine())
    kernel, seccomp = kernel_release(), library()
    default = ERRNO | (profile.get("defaultErrnoRet", 1) & 0xFFFF)
    context = seccomp.seccomp_init(default)
    if not context:
        raise RuntimeError("libseccomp could not start a filter")
    rules, unavailable = 0, []
    try:
        for entry in profile.get("syscalls", []):
            if not applies(entry, architecture, kernel):
                continue
            decision = action(entry)
            if decision == default:
                continue
            comparisons = [Comparison(argument["index"], OPERATORS[argument["op"]],
                                      argument["value"], argument.get("valueTwo", 0))
                           for argument in entry.get("args", [])]
            array = (Comparison * len(comparisons))(*comparisons)
            for name in entry["names"]:
                number = seccomp.seccomp_syscall_resolve_name(name.encode())
                if number < 0:
                    # Unknown here means unavailable, and the default denies it.
                    unavailable.append(name)
                    continue
                code = seccomp.seccomp_rule_add_array(context, decision, number, len(array), array)
                if code != 0:
                    raise RuntimeError("libseccomp refused the rule for %s: %d" % (name, code))
                rules += 1
        descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        try:
            code = seccomp.seccomp_export_bpf(context, descriptor)
            if code != 0:
                raise RuntimeError("libseccomp could not export the filter: %d" % code)
        finally:
            os.close(descriptor)
        return {"native_architecture": architecture, "compiled_rules": rules,
                "unavailable_still_denied": sorted(set(unavailable)), "capabilities": []}
    finally:
        seccomp.seccomp_release(context)


def main(arguments):
    if len(arguments) != 2:
        print(__doc__.strip().splitlines()[0], file=sys.stderr)
        return 2
    profile = json.loads(Path(arguments[0]).read_text())
    print(json.dumps(compile_profile(profile, arguments[1]), sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
