#!/usr/bin/python3
"""Start and remove a request's preview on Kubernetes.

  preview.py up      run the request's checkout as a preview and write its URL
  preview.py down    remove the request's preview
  preview.py origin  print the origin of the request's preview, one line

An example of the commands the engine's preview setting names: README.md
beside this file says how to install it and what each setting means. It uses
Python's standard library and kubectl, nothing else.

up builds one Deployment and one Service in the preview namespace, hands the
checkout to the Pod over kubectl exec, waits until the application answers,
makes the request's signed URL, opens the public address as the requester
would (the router's password and the signed URL both), and writes three lines
to $TASK_PREVIEW: the signed URL, until when it can be opened and the
identifier of the change it runs. The same change already running is not
started again; only its lifetime is extended, with a new signed URL. down
removes the Deployment and the Service and reads back that they are gone.

What up hands over is the checkout's tracked and untracked, not ignored
files, or, with PREVIEW_PACK_DIR, only what is inside that directory of the
checkout: one git ignores, such as a build's output, carried with that
directory as the root.

origin prints https://<label>.<domain>, the address up gives the request's
preview, from TASK_ISSUE, PREVIEW_NAME and PREVIEW_DOMAIN alone: a build can
put it into what it builds before up runs. It reads no credential and calls
nothing.

Exit status: 0 when the preview is up (up), gone (down) or its origin printed
(origin); 1 otherwise, the reason on the last line of the output; 2 for a
wrong command line, and for origin when a setting it needs is missing or
wrong (the reason on standard error, nothing on standard output). Nothing
is removed after a failure, except a Deployment whose Pod's last rules
container (net) had not ended with status 0 when the wait ended for any
reason: until then the dependency step could still run with public addresses
open. The cleanup and
down remove the rest.

Credentials. The engine passes PREVIEW_K8S_TOKEN (and, to up,
PREVIEW_CHECK_AUTH and PREVIEW_SIGNING_KEY) in the environment and lists their
names in TASK_CREDENTIAL_NAMES. Before anything else this program takes every
listed value out of its environment and starts itself again without them, the
ones it needs passed through a pipe: what /proc/self/environ shows of the running
program then holds no credential. git and kubectl get an environment of their
own without any; kubectl gets only the path of a kubeconfig, written to the
launch's /tmp after git has run and removed at the end. Nothing in the
checkout is read through a symbolic link (README.md, "What it never reads").
"""
import base64
import collections
import datetime
import hashlib
import hmac
import http.client
import json
import os
import re
import selectors
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

COMMANDS = ("up", "down")
TOKEN = "PREVIEW_K8S_TOKEN"
CHECK_AUTH = "PREVIEW_CHECK_AUTH"
SIGNING_KEY = "PREVIEW_SIGNING_KEY"
CREDENTIALS = (TOKEN, CHECK_AUTH, SIGNING_KEY)
SIGNING_KEY_LENGTH = 32
OPEN_PATH = "/_preview/open"
COOKIE = "__Host-preview"
HANDOVER = "--credentials-fd"
HANDOVER_LIMIT = 16 * 1024
# The commands that load the egress rules of the ConfigMap preview-egress.
# The admission policy accepts exactly these and no other: change both
# together (README.md, "The rules image").
NET_COMMANDS = {
    "net-hold": ["iptables-restore", "--wait", "5", "/policy/run.rules"],
    "net-install": ["iptables-restore", "--wait", "5", "/policy/install.rules"],
    "net": ["iptables-restore", "--wait", "5", "/policy/run.rules"],
}
EGRESS_CONFIGMAP = "preview-egress"
LABEL = "preview"
SITE_LABEL = "preview-site"
EXPIRES = "preview-expires-at"
ISSUE = "preview-issue"
CHANGE = "preview-change"
RECEIVE_WAIT = "until [ -e /signal/received ]; do sleep 1; done"
RECEIVE = "tar -x -o -f - -C /src && : > /signal/received"
ENV_FILE_LIMIT = 64 * 1024
ARCHIVE_LIMIT = 1 << 30  # the checkout's emptyDir holds 1 GiB
POLL = 1.0  # the install step is looked at at most this long plus one call apart
# How much longer than the dependency step the wait has to be (r7-2): the
# command has to be still looking when a late step fails, to remove its
# Deployment before the kubelet runs the step again.
INSTALL_MARGIN = 180
CHECK_SECONDS = 60.0
JAPAN = datetime.timezone(datetime.timedelta(hours=9))
# What the router answers itself when it has nothing to forward to (and what
# the load balancer answers when the router is away): waited out, not final.
ROUTER_STATUSES = (404, 502, 503, 504)
# A container waiting for one of these will not start by waiting longer.
STUCK = ("CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "InvalidImageName",
         "CreateContainerConfigError", "CreateContainerError", "RunContainerError")

DNS_LABEL = r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?"
DOMAIN = r"%s(\.%s)+" % (DNS_LABEL, DNS_LABEL)
DOMAIN_DESCRIPTION = "a lowercase domain name such as preview.example.invalid"

Settings = collections.namedtuple(
    "Settings", "workspace result issue site start port ready_path lifetime image memory install "
                "install_seconds env_file domain namespace server ca_file net_image wait_seconds pack_dir "
                "pod_annotations pod_options extra_files result_lines failure_file",
    defaults=(None,) * 6)
Names = collections.namedtuple("Names", "name label host url")


class Failure(Exception):
    """A reason to end with status 1, in one line."""


class Waiting(Exception):
    """An answer the public check waits out."""


def say(text):
    print(text, flush=True)


# -- settings ------------------------------------------------------------

def required(environment, name):
    value = environment.get(name, "")
    if not value.strip():
        raise Failure("%s is not set" % name)
    if "\0" in value:
        raise Failure("%s holds a NUL character" % name)
    return value


def number(environment, name, low, high, default=None):
    value = environment.get(name)
    if value is None and default is not None:
        return default
    if value is None or not re.fullmatch(r"[0-9]{1,6}", value) or not low <= int(value) <= high:
        raise Failure("%s must be a whole number from %d to %d, written in digits only (it is %s)"
                      % (name, low, high, "not set" if value is None else repr(value)))
    return int(value)


def matching(environment, name, pattern, description, default=None):
    value = environment.get(name, default)
    if value is None or not re.fullmatch(pattern, value):
        raise Failure("%s must be %s (it is %s)" % (name, description, "not set" if value is None else repr(value)))
    return value


# An annotation's name: an optional DNS subdomain and /, then 1 to 63
# letters, digits, -, _ and ., starting and ending alphanumeric. Stricter than
# the API, which lowercases the name before it checks it: here the prefix has
# to be lowercase already.
ANNOTATION_NAME = r"([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?"


# The API's limit on the annotations of an object (here the Pod template),
# names and values together, in bytes; preview-change takes its share.
ANNOTATIONS_LIMIT = 262144
CHANGE_LENGTH = 12  # the identifier's hex digits (change_identifier)


def json_type(value):
    """The JSON name of a decoded value's type."""
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, (int, float)):
        return "number"
    return {str: "string", list: "array", dict: "object"}.get(type(value), type(value).__name__)


def pod_annotations_setting(value):
    """PREVIEW_POD_ANNOTATIONS: a JSON object of annotation names to string
    values, added to the Pod template; None when it is not set or empty."""
    if not value:
        return None
    wrong = ("PREVIEW_POD_ANNOTATIONS must be a JSON object of annotation names to string values, "
             "such as {\"example.invalid/keep\": \"true\"}")

    def unique(pairs):
        names = [name for name, _ in pairs]
        if len(set(names)) != len(names):
            raise Failure("%s; a name is given twice" % wrong)
        return dict(pairs)
    try:
        annotations = json.loads(value, object_pairs_hook=unique)
    except ValueError:
        raise Failure("%s (it is not JSON)" % wrong) from None
    except RecursionError:
        raise Failure("%s (it is nested too deeply to read)" % wrong) from None
    if not isinstance(annotations, dict):
        raise Failure("%s (it is a JSON %s)" % (wrong, json_type(annotations)))
    for name, text in annotations.items():
        if not name:
            raise Failure("%s; a name is empty" % wrong)
        if len(name.split("/", 1)[0]) > 253 or not re.fullmatch(ANNOTATION_NAME, name):
            raise Failure("%s; %r is not an annotation name" % (wrong, name))
        if name in (CHANGE, EXPIRES, ISSUE):
            raise Failure("%s; %s is preview.py's own" % (wrong, name))
        if not isinstance(text, str):
            raise Failure("%s; the value of %s is a JSON %s, not a string" % (wrong, name, json_type(text)))
    size = sum(len(name.encode()) + len(text.encode()) for name, text in annotations.items())
    if size + len(CHANGE) + CHANGE_LENGTH > ANNOTATIONS_LIMIT:
        raise Failure("PREVIEW_POD_ANNOTATIONS holds %d bytes of names and values; with %s the Pod's annotations "
                      "would pass the API's limit of %d bytes" % (size, CHANGE, ANNOTATIONS_LIMIT))
    return annotations or None


def option_json(environment, name, expected, default):
    text = environment.get(name)
    if not text:
        return default
    try:
        value = json.loads(text)
    except (ValueError, RecursionError):
        raise Failure("%s is not readable JSON" % name) from None
    if not isinstance(value, expected):
        raise Failure("%s has the wrong JSON type" % name)
    return value


def plain_path(value, absolute=False):
    return (isinstance(value, str) and bool(value) and "\0" not in value
            and value.startswith("/") == absolute
            and all(part not in ("", ".", "..") for part in value.lstrip("/").split("/")))


def pod_options(environment):
    options = option_json(environment, "PREVIEW_POD_OPTIONS", dict, {})

    def require(ok):
        if not ok:
            raise Failure("PREVIEW_POD_OPTIONS has an unsupported field or value; see the optional containers settings")

    def fields(value, allowed):
        require(isinstance(value, dict) and not value.keys() - set(allowed))

    def env(entries):
        require(isinstance(entries, list))
        names = set()
        for entry in entries:
            fields(entry, ("name", "value"))
            name = entry.get("name")
            require(isinstance(name, str) and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name)
                    and name not in names and isinstance(entry.get("value"), str) and "\0" not in entry["value"])
            names.add(name)

    def resources(value):
        fields(value, ("requests", "limits"))
        for quantities in value.values():
            fields(quantities, ("cpu", "memory", "ephemeral-storage"))
            for name, quantity in quantities.items():
                pattern = r"[1-9][0-9]*(m|[.]\d+)?" if name == "cpu" else r"[1-9][0-9]*(Ki|Mi|Gi)"
                require(isinstance(quantity, str) and re.fullmatch(pattern, quantity))

    fields(options, ("containers", "volumes", "resources", "env"))
    env(options.get("env", []))
    resources(options.get("resources", {}))
    volumes = options.get("volumes", [])
    require(isinstance(volumes, list))
    mountable = {"src"}
    for volume in volumes:
        fields(volume, ("name", "emptyDir"))
        name = volume.get("name")
        require(isinstance(name, str) and re.fullmatch(DNS_LABEL, name) and name not in
                mountable | {"policy", "signal", "xtables"})
        empty = volume.get("emptyDir")
        fields(empty, ("sizeLimit", "medium"))
        require(isinstance(empty.get("sizeLimit"), str) and re.fullmatch(r"[1-9][0-9]*(Mi|Gi)", empty["sizeLimit"]))
        require(empty.get("medium", "") in ("", "Memory"))
        mountable.add(name)
    containers = options.get("containers", [])
    require(isinstance(containers, list))
    names = {"app", "receive", "install", *NET_COMMANDS}
    for container in containers:
        fields(container, ("name", "image", "command", "args", "ports", "env", "resources", "volumeMounts", "readinessProbe"))
        name = container.get("name")
        require(isinstance(name, str) and re.fullmatch(DNS_LABEL, name) and name not in names)
        names.add(name)
        require(isinstance(container.get("image"), str) and re.fullmatch(r"\S+", container["image"]))
        for field in ("command", "args"):
            if field in container:
                require(isinstance(container[field], list) and
                        all(isinstance(s, str) and "\0" not in s for s in container[field]))
        env(container.get("env", []))
        resources(container.get("resources", {}))
        ports = container.get("ports", [])
        require(isinstance(ports, list))
        for port in ports:
            fields(port, ("name", "containerPort", "protocol"))
            require(type(port.get("containerPort")) is int and 1 <= port["containerPort"] <= 65535)
            require(port.get("protocol", "TCP") in ("TCP", "UDP"))
        mounts = container.get("volumeMounts", [])
        require(isinstance(mounts, list))
        for mount in mounts:
            fields(mount, ("name", "mountPath", "subPath", "readOnly"))
            require(isinstance(mount.get("name"), str) and mount["name"] in mountable and plain_path(mount.get("mountPath"), True))
            require("subPath" not in mount or plain_path(mount["subPath"]))
            require("readOnly" not in mount or type(mount["readOnly"]) is bool)
        if "readinessProbe" in container:
            probe = container["readinessProbe"]
            fields(probe, ("httpGet", "tcpSocket", "initialDelaySeconds", "periodSeconds", "timeoutSeconds", "failureThreshold", "successThreshold"))
            require(("httpGet" in probe) != ("tcpSocket" in probe))
            for key, value in probe.items():
                if key in ("httpGet", "tcpSocket"):
                    fields(value, ("path", "port") if key == "httpGet" else ("port",))
                    require(type(value.get("port")) is int and 1 <= value["port"] <= 65535)
                    if key == "httpGet":
                        require(isinstance(value.get("path"), str) and re.fullmatch(r"/[\x21-\x7e]*", value["path"]))
                else:
                    require(type(value) is int and value >= (0 if key == "initialDelaySeconds" else 1))
    return options or None


def read_settings(environment, command):
    """The settings either command reads, checked before anything runs."""
    workspace = required(environment, "TASK_WORKSPACE")
    issue = required(environment, "TASK_ISSUE")
    site = required(environment, "PREVIEW_NAME")
    namespace = matching(environment, "PREVIEW_NAMESPACE", r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?",
                         "a namespace name")
    server = matching(environment, "PREVIEW_K8S_SERVER", r"https://[^\s/]+(/\S*)?",
                      "the API server's https:// address")
    ca_file = required(environment, "PREVIEW_K8S_CA_FILE")
    if not os.path.isabs(ca_file) or not os.path.isfile(ca_file):
        raise Failure("PREVIEW_K8S_CA_FILE must name an existing file by its absolute path")
    wait_seconds = number(environment, "PREVIEW_WAIT_SECONDS", 1, 3600, default=420 if command == "up" else 60)
    if command == "down":
        return Settings(workspace, None, issue, site, None, None, None, None, None, None, None, None, None,
                        None, namespace, server, ca_file, None, wait_seconds)
    domain = matching(environment, "PREVIEW_DOMAIN", DOMAIN, DOMAIN_DESCRIPTION)
    result = required(environment, "TASK_PREVIEW")
    if not os.path.isabs(result):
        raise Failure("TASK_PREVIEW must be an absolute path")
    start = required(environment, "PREVIEW_START")
    port = number(environment, "PREVIEW_PORT", 1, 65535)
    ready_path = matching(environment, "PREVIEW_READY_PATH", r"/[\x21-\x7e]*",
                          "a path starting with / without spaces")
    lifetime = number(environment, "PREVIEW_LIFETIME_HOURS", 1, 720)
    image = matching(environment, "PREVIEW_IMAGE", r"\S+", "an image name without spaces")
    memory = matching(environment, "PREVIEW_MEMORY", r"[1-9][0-9]{0,5}(Mi|Gi)", "a size such as 512Mi or 1Gi",
                      default="512Mi")
    install = environment.get("PREVIEW_INSTALL") or None
    if install is not None and "\0" in install:
        raise Failure("PREVIEW_INSTALL holds a NUL character")
    # Checked also without PREVIEW_INSTALL: a wrong value is a wrong setting.
    # 0 would mean "no limit" to timeout, so it is refused like any value
    # outside 1 to 480.
    install_seconds = number(environment, "PREVIEW_INSTALL_SECONDS", 1, 480, default=240)
    if install is not None and wait_seconds < install_seconds + INSTALL_MARGIN:
        raise Failure("PREVIEW_WAIT_SECONDS (%d) must be at least PREVIEW_INSTALL_SECONDS (%d) + %d, so that the "
                      "command still watches the dependency step when it ends; raise the first or lower the second"
                      % (wait_seconds, install_seconds, INSTALL_MARGIN))
    env_file = environment.get("PREVIEW_ENV_FILE") or None
    if env_file is not None:
        parts = env_file.split("/")
        if env_file.startswith("/") or any(part in ("", ".", "..") for part in parts):
            raise Failure("PREVIEW_ENV_FILE must be a path inside the checkout, relative to it, without . or ..")
    pack_dir = environment.get("PREVIEW_PACK_DIR") or None
    if pack_dir is not None:
        written = pack_dir
        if pack_dir.endswith("/") and not pack_dir.endswith("//"):
            pack_dir = pack_dir[:-1]  # one trailing / names the same directory
        parts = pack_dir.split("/")
        if "\0" in pack_dir or pack_dir.startswith("/") or any(part in ("", ".", "..") for part in parts):
            raise Failure("PREVIEW_PACK_DIR must be a directory inside the checkout, relative to it, with no "
                          "leading or doubled / and no . or .. part (it is %r)" % written)
    net_image = matching(environment, "PREVIEW_NET_IMAGE", r"[^\s@]+@sha256:[0-9a-f]{64}",
                         "the rules image named by its digest (name@sha256:...)")
    pod_annotations = pod_annotations_setting(environment.get("PREVIEW_POD_ANNOTATIONS"))
    options = pod_options(environment)
    extra = option_json(environment, "PREVIEW_EXTRA_FILES", dict, {})
    if extra and install is not None:
        raise Failure("PREVIEW_EXTRA_FILES cannot be combined with PREVIEW_INSTALL: the install step can reach public addresses")
    for name, source in extra.items():
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name) or not plain_path(source, True):
            raise Failure("PREVIEW_EXTRA_FILES must map plain file names to absolute file paths")
        if os.path.commonpath((os.path.realpath(workspace), os.path.realpath(source))) == os.path.realpath(workspace):
            raise Failure("PREVIEW_EXTRA_FILES must be outside the checkout")
    lines = option_json(environment, "PREVIEW_RESULT_LINES", list, [])
    if (not all(isinstance(line, str) and not any(ord(c) < 32 or 127 <= ord(c) < 160 or
            0x202a <= ord(c) <= 0x202e or 0x2066 <= ord(c) <= 0x2069 for c in line) for line in lines)
            or len("\n".join(lines).encode()) > 8192):
        raise Failure("PREVIEW_RESULT_LINES must be plain text lines totaling at most 8 KiB")
    failure_file = environment.get("PREVIEW_FAILURE_FILE") or None
    if failure_file and not plain_path(failure_file):
        raise Failure("PREVIEW_FAILURE_FILE must be a plain path relative to /src")
    return Settings(workspace, result, issue, site, start, port, ready_path, lifetime, image, memory, install,
                    install_seconds, env_file, domain, namespace, server, ca_file, net_image, wait_seconds,
                    pack_dir, pod_annotations, options, extra or None, lines or None, failure_file)


def preview_names(site, issue, domain=None):
    """The object name, the host label, the host and the URL of a request's
    preview (the last two None without a domain). Long names are cut and
    given a part of their own hash, so two request numbers that differ only
    at the end still get two previews."""
    if not re.fullmatch(r"[a-z]([a-z0-9-]{0,18}[a-z0-9])?", site):
        raise Failure("PREVIEW_NAME must be 1 to 20 lowercase letters, digits and hyphens, "
                      "start with a letter and not end with a hyphen (it is %r)" % site)
    part = re.sub(r"[^a-z0-9]+", "-", issue.lower()).strip("-")
    if not part:
        raise Failure("the request number %r has no letter or digit to name a preview by" % issue)
    label = site + "-" + part
    if len(label) > 60:
        label = label[:51].rstrip("-") + "-" + hashlib.sha256(label.encode()).hexdigest()[:8]
    host = label + "." + domain if domain else None
    return Names("pv-" + label, label, host, "https://%s/" % host if host else None)


# -- what is read from the checkout ---------------------------------------

def lookup(root, relative):
    """What a path of the checkout is, found without following a symbolic
    link anywhere on the way: ("file", open descriptor), ("link", its target
    text) or (None, why it is left out)."""
    parts = relative.split("/")
    if any(part in ("", ".", "..") for part in parts):
        return None, "it is not a plain relative path"
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in parts[:-1]:
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            except OSError:
                return None, "a directory on its way is a symbolic link or missing"
            os.close(directory)
            directory = child
        try:
            status = os.stat(parts[-1], dir_fd=directory, follow_symlinks=False)
        except OSError:
            return None, "it is missing"
        if stat.S_ISLNK(status.st_mode):
            return "link", os.readlink(parts[-1], dir_fd=directory)
        if not stat.S_ISREG(status.st_mode):
            return None, "it is neither a regular file nor a symbolic link"
        try:
            descriptor = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
        except OSError:
            return None, "it changed while it was read"
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            os.close(descriptor)
            return None, "it changed while it was read"
        return "file", descriptor
    finally:
        os.close(directory)


def read_env_file(root, relative):
    """The KEY=VALUE lines of PREVIEW_ENV_FILE, read as lookup reads. A link
    is refused, not followed."""
    kind, found = lookup(root, relative)
    if kind == "link":
        raise Failure("PREVIEW_ENV_FILE %s is a symbolic link; it was not read" % relative)
    if kind is None:
        raise Failure("PREVIEW_ENV_FILE %s could not be read: %s" % (relative, found))
    with os.fdopen(found, "rb") as handle:
        data = handle.read(ENV_FILE_LIMIT + 1)
    if len(data) > ENV_FILE_LIMIT:
        raise Failure("PREVIEW_ENV_FILE %s is larger than 64 KiB" % relative)
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        raise Failure("PREVIEW_ENV_FILE %s is not UTF-8 text" % relative) from None
    entries = {}
    for index, line in enumerate(text.split("\n"), 1):
        line = line[:-1] if line.endswith("\r") else line
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key) or "\0" in value:
            raise Failure("PREVIEW_ENV_FILE %s line %d is not KEY=VALUE" % (relative, index))
        entries[key] = value
    return list(entries.items())


def tool_environment(environment, home):
    """What git and kubectl start with: no credential and nothing of the
    engine's but the path and the language."""
    return {"PATH": environment.get("PATH") or os.defpath, "LANG": environment.get("LANG") or "C.UTF-8",
            "HOME": home}


def git(root, environment, arguments, what):
    """git in the checkout with no configuration but the repository's own and
    no credential; the finished process."""
    home = tempfile.mkdtemp(prefix="preview-git-", dir=environment.get("TMPDIR") or "/tmp")
    try:
        variables = dict(tool_environment(environment, home), GIT_CONFIG_NOSYSTEM="1",
                         GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0")
        try:
            return subprocess.run(["git", "-c", "core.fsmonitor=false", "-C", root, *arguments], env=variables,
                                  stdin=subprocess.DEVNULL, capture_output=True, timeout=300)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise Failure("%s: %s" % (what, error)) from None
    finally:
        shutil.rmtree(home, ignore_errors=True)


def checkout_paths(root, environment):
    """The tracked and the untracked, not ignored paths of the checkout."""
    done = git(root, environment, ["ls-files", "-z", "-co", "--exclude-standard"],
               "git ls-files could not list the checkout")
    if done.returncode != 0:
        raise Failure("git ls-files could not list the checkout: " + last_line(done.stderr))
    return sorted({os.fsdecode(path) for path in done.stdout.split(b"\0") if path})


def pack_paths(root, relative, environment):
    """The files and links inside PREVIEW_PACK_DIR, relative to it. The
    directory itself has to be one git ignores (git check-ignore; git does not
    count a directory with a tracked file in it as ignored, nor one only whose
    contents a pattern such as dist/* ignores): what it holds is then never
    part of a delivery, only of the preview. Every directory on the way
    to it and under it is opened without following a symbolic link; a link
    inside is listed as a link, to be carried as one."""
    name = "PREVIEW_PACK_DIR %s" % relative
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in relative.split("/"):
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            except OSError:
                raise Failure("%s is not a directory of the checkout: %s on its way is a symbolic link, missing "
                              "or not a directory" % (name, part)) from None
            os.close(directory)
            directory = child
        found = []

        def walk(descriptor, prefix):
            with os.scandir(descriptor) as listing:
                entries = sorted(listing, key=lambda entry: entry.name)
            for entry in entries:
                path = prefix + entry.name
                if entry.is_symlink():
                    found.append(path)
                elif entry.is_dir(follow_symlinks=False):
                    try:
                        child = os.open(entry.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                    except OSError:
                        continue  # changed while it was read: left out, as write_tar leaves it out
                    try:
                        walk(child, path + "/")
                    finally:
                        os.close(child)
                elif entry.is_file(follow_symlinks=False):
                    found.append(path)

        walk(directory, "")
    finally:
        os.close(directory)
    ignored = git(root, environment, ["check-ignore", "-q", "--", relative], "git check-ignore could not run")
    if ignored.returncode == 1:
        raise Failure("git does not ignore %s itself (a tracked file inside, or a pattern such as dist/* that "
                      "ignores only what is inside): only a directory git ignores as a whole is carried to the "
                      "preview alone" % name)
    if ignored.returncode != 0:
        raise Failure("git check-ignore could not read %s: %s" % (relative, last_line(ignored.stderr)))
    if not found:
        raise Failure("%s holds nothing to carry to the preview" % name)
    return found


def write_tar(root, paths, destination, inside=None, extra_files=None):
    """The checkout as a tar file at destination, the same bytes for the same
    files: links stay links, regular files are read through lookup, anything
    else and anything under a linked directory is left out. With inside (a
    directory of the checkout, PREVIEW_PACK_DIR), paths are relative to it and
    it is the archive's root. Returns the sha256 of the file."""
    total = 0
    what = "PREVIEW_PACK_DIR %s" % inside if inside else "the checkout"
    with tarfile.open(destination, "w", format=tarfile.GNU_FORMAT) as archive:
        for relative in paths:
            kind, found = lookup(root, inside + "/" + relative if inside else relative)
            if kind is None:
                continue
            info = tarfile.TarInfo(relative)
            info.mtime, info.uid, info.gid, info.uname, info.gname = 0, 0, 0, "", ""
            if kind == "link":
                info.type, info.linkname, info.mode = tarfile.SYMTYPE, found, 0o777
                archive.addfile(info)
                continue
            with os.fdopen(found, "rb") as handle:
                status = os.fstat(handle.fileno())
                info.size = status.st_size
                info.mode = 0o755 if status.st_mode & 0o111 else 0o644
                total += info.size
                if total > ARCHIVE_LIMIT:
                    raise Failure("%s holds more than the preview's 1 GiB" % what)
                archive.addfile(info, handle)
        for name, source in sorted((extra_files or {}).items()):
            if os.path.lexists(os.path.join(root, inside or "", name)):
                raise Failure("PREVIEW_EXTRA_FILES collides with a source path")
            kind, found = lookup("/", source[1:])
            if kind != "file":
                raise Failure("PREVIEW_EXTRA_FILES must name regular files without symbolic links")
            with os.fdopen(found, "rb") as handle:
                info = tarfile.TarInfo(name)
                info.size, info.mode = os.fstat(handle.fileno()).st_size, 0o600
                total += info.size
                if total > ARCHIVE_LIMIT:
                    raise Failure("the source and PREVIEW_EXTRA_FILES together exceed 1 GiB")
                archive.addfile(info, handle)
    digest = hashlib.sha256()
    with open(destination, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


# -- what up asks the cluster for -------------------------------------------

def stamp(moment):
    return moment.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def manifests(settings, names, entries, expires):
    """The Deployment and the Service of a request's preview: the shape the
    admission policy of README.md, "The shape the cluster accepts", lets
    through."""
    labels = {LABEL: names.name, SITE_LABEL: settings.site}
    values = {"HOME": "/tmp"}
    values.update(entries)
    values["PORT"] = str(settings.port)
    variables = [{"name": key, "value": value} for key, value in values.items()]
    working = {"requests": {"cpu": "100m", "memory": settings.memory, "ephemeral-storage": "64Mi"},
               "limits": {"cpu": "500m", "memory": settings.memory, "ephemeral-storage": "2Gi"}}
    small = {"requests": {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "1Mi"},
             "limits": {"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "16Mi"}}

    def unprivileged(read_only):
        return {"runAsNonRoot": True, "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": read_only,
                "capabilities": {"drop": ["ALL"]}}

    def rules(name):
        return {"name": name, "image": settings.net_image, "imagePullPolicy": "IfNotPresent",
                "command": list(NET_COMMANDS[name]),
                "securityContext": {"runAsUser": 0, "runAsGroup": 0, "runAsNonRoot": False,
                                    "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                    "capabilities": {"drop": ["ALL"], "add": ["NET_ADMIN"]}},
                "resources": small,
                "volumeMounts": [{"name": "policy", "mountPath": "/policy", "readOnly": True},
                                 {"name": "xtables", "mountPath": "/run"}]}

    source = {"name": "src", "mountPath": "/src"}
    initial = [rules("net-hold"),
               {"name": "receive", "image": settings.image, "command": ["sh", "-c", RECEIVE_WAIT],
                "securityContext": unprivileged(True), "resources": small,
                "volumeMounts": [source, {"name": "signal", "mountPath": "/signal"}]}]
    if settings.install:
        initial += [rules("net-install"),
                    {"name": "install", "image": settings.image,
                     "command": install_command(settings.install_seconds, settings.install),
                     "workingDir": "/src", "env": variables, "securityContext": unprivileged(False),
                     "resources": working, "volumeMounts": [source]}]
    initial.append(rules("net"))
    application = {"name": "app", "image": settings.image, "command": ["sh", "-c", settings.start],
                   "workingDir": "/src", "env": variables,
                   "ports": [{"name": "http", "containerPort": settings.port, "protocol": "TCP"}],
                   "readinessProbe": {"httpGet": {"path": settings.ready_path, "port": settings.port},
                                      "periodSeconds": 5, "timeoutSeconds": 3, "failureThreshold": 3},
                   "securityContext": unprivileged(False), "resources": working, "volumeMounts": [source]}
    options = settings.pod_options or {}
    if options.get("env"):
        app_values = dict(values)
        app_values.update((entry["name"], entry["value"]) for entry in options["env"])
        application["env"] = [{"name": key, "value": value} for key, value in app_values.items()]
    if options.get("resources"):
        application["resources"] = {kind: dict(quantities, **options["resources"].get(kind, {}))
                                    for kind, quantities in working.items()}
    extra_containers = [dict(container, securityContext=unprivileged(False))
                        for container in options.get("containers", [])]
    # No preemptionPolicy and no priority: the cluster's Priority admission
    # refuses a Pod that names a preemptionPolicy other than its class's, and
    # without a class a Pod has priority 0, below nothing that runs.
    pod = {"automountServiceAccountToken": False, "enableServiceLinks": False, "hostUsers": False,
           "terminationGracePeriodSeconds": 5,
           "nodeSelector": {"kubernetes.io/arch": "amd64"},
           "securityContext": {"runAsNonRoot": True, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000,
                               "seccompProfile": {"type": "RuntimeDefault"}},
           "initContainers": initial, "containers": [application] + extra_containers,
           "volumes": [{"name": "src", "emptyDir": {"sizeLimit": "1Gi"}},
                       {"name": "signal", "emptyDir": {"sizeLimit": "1Mi"}},
                       {"name": "xtables", "emptyDir": {"sizeLimit": "1Mi"}},
                       {"name": "policy", "configMap": {"name": EGRESS_CONFIGMAP}}] + options.get("volumes", [])}
    deployment = {"apiVersion": "apps/v1", "kind": "Deployment",
                  "metadata": {"name": names.name, "namespace": settings.namespace, "labels": labels,
                               "annotations": {EXPIRES: stamp(expires), ISSUE: settings.issue}},
                  "spec": {"replicas": 1, "revisionHistoryLimit": 1, "strategy": {"type": "Recreate"},
                           "selector": {"matchLabels": {LABEL: names.name}},
                           "template": {"metadata": {"labels": dict(labels),
                                                     "annotations": dict(settings.pod_annotations or {})},
                                        "spec": pod}}}
    service = {"apiVersion": "v1", "kind": "Service",
               "metadata": {"name": names.name, "namespace": settings.namespace, "labels": dict(labels)},
               "spec": {"type": "ClusterIP", "selector": {LABEL: names.name},
                        "ports": [{"name": "http", "port": 80, "targetPort": settings.port, "protocol": "TCP"}]}}
    return deployment, service


def install_command(seconds, command):
    # KILL, not timeout's default TERM: a step that ignores TERM would keep
    # running, and with it the window in which it can reach public addresses.
    return ["timeout", "-s", "KILL", str(seconds), "sh", "-c", command]


def change_identifier(archive_digest, template):
    """The first 12 hex digits of the hash of the checkout's archive and of
    the Pod template (start command, image, port, install step, settings
    file, PREVIEW_POD_ANNOTATIONS): a new value for any change of either."""
    digest = hashlib.sha256(b"preview change\n" + archive_digest.encode() + b"\n")
    digest.update(json.dumps(template, sort_keys=True, separators=(",", ":")).encode())
    return digest.hexdigest()[:CHANGE_LENGTH]


# -- talking to the cluster ------------------------------------------------

def last_line(data):
    text = data.decode("utf-8", "replace") if isinstance(data, bytes) else data
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    if not lines:
        return "(no message)"
    named = [line for line in lines if "ValidatingAdmissionPolicy" in line]
    return (named or lines)[-1][:600]


def private_directory(environment):
    # The launcher's /tmp: this launch's memory, gone with its sandbox also
    # when the launch is killed. Never the home directory, which outlives the
    # launch and is kept with the request.
    return tempfile.mkdtemp(prefix="preview-", dir=environment.get("TMPDIR") or "/tmp")


def write_kubeconfig(directory, settings, token):
    path = os.path.join(directory, "kubeconfig")
    config = {"apiVersion": "v1", "kind": "Config", "current-context": "preview",
              "clusters": [{"name": "preview", "cluster": {"server": settings.server,
                                                           "certificate-authority": settings.ca_file}}],
              "users": [{"name": "preview", "user": {"token": token}}],
              "contexts": [{"name": "preview", "context": {"cluster": "preview", "user": "preview",
                                                           "namespace": settings.namespace}}]}
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as handle:
        json.dump(config, handle)
    return path


class Cluster:
    """kubectl, in an environment that holds the kubeconfig's path and no
    credential, with its cache in the launch's private directory."""

    def __init__(self, settings, environment, directory, kubeconfig):
        self.environment = dict(tool_environment(environment, directory), KUBECONFIG=kubeconfig)
        self.base = ["kubectl", "--cache-dir=" + os.path.join(directory, "cache"),
                     "--namespace=" + settings.namespace]

    def run(self, arguments, *, data=None, source=None, timeout=90, limited=True, output_limit=None):
        command = self.base + (["--request-timeout=30s"] if limited else []) + arguments
        try:
            if output_limit is not None:
                # head inside the Pod is untrusted too. Bound the pipe here,
                # discard stderr and reap kubectl on overflow or timeout.
                with subprocess.Popen(command, env=self.environment, stdin=subprocess.DEVNULL,
                                      stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as process:
                    output = bytearray()
                    deadline = time.monotonic() + timeout
                    try:
                        with selectors.DefaultSelector() as selector:
                            selector.register(process.stdout, selectors.EVENT_READ)
                            while len(output) < output_limit:
                                if not selector.select(max(0, deadline - time.monotonic())):
                                    raise subprocess.TimeoutExpired(command, timeout)
                                chunk = os.read(process.stdout.fileno(), output_limit - len(output))
                                if not chunk:
                                    break
                                output.extend(chunk)
                        if len(output) == output_limit:
                            process.kill()
                        process.wait(timeout=max(0, deadline - time.monotonic()))
                        return subprocess.CompletedProcess(command, process.returncode, bytes(output), b"")
                    finally:
                        if process.poll() is None:
                            process.kill()
                        process.wait()
            if source is not None:
                return subprocess.run(command, env=self.environment, stdin=source, capture_output=True,
                                      timeout=timeout)
            return subprocess.run(command, env=self.environment, input=data or b"", capture_output=True,
                                  timeout=timeout)
        except FileNotFoundError:
            raise Failure("kubectl was not found on PATH") from None
        except subprocess.TimeoutExpired:
            raise Failure("kubectl %s did not finish within %d seconds" % (arguments[0], timeout)) from None
        except OSError as error:
            raise Failure("kubectl could not be started: %s" % (error.strerror or error)) from None

    def call(self, arguments, what):
        done = self.run(arguments)
        if done.returncode != 0:
            raise Failure("%s failed: %s" % (what, last_line(done.stderr)))
        return done.stdout

    def items(self, kinds, selector):
        output = self.call(["get", kinds, "-l", selector, "-o", "json"], "reading the %s" % kinds)
        try:
            return json.loads(output).get("items", [])
        except ValueError:
            raise Failure("kubectl get %s printed no JSON" % kinds) from None

    def apply(self, objects):
        data = json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}).encode()
        done = self.run(["apply", "--server-side", "--force-conflicts", "--field-manager=preview", "-f", "-"],
                        data=data)
        if done.returncode != 0:
            raise Failure("the cluster refused the preview: " + last_line(done.stderr))

    def remove(self, kinds, name):
        """None when the objects are removed or were not there, else why not."""
        try:
            done = self.run(["delete", kinds, "-l", "%s=%s" % (LABEL, name), "--ignore-not-found", "--wait=false"])
        except Failure as failure:
            return str(failure)
        return None if done.returncode == 0 else last_line(done.stderr)


def condition(item, kind):
    for entry in item.get("status", {}).get("conditions", []) or []:
        if entry.get("type") == kind:
            return entry
    return {}


def pods_of(items, change):
    """The Pods of this change that are not being removed, newest first."""
    found = [item for item in items if item.get("kind") == "Pod"
             and (item.get("metadata", {}).get("annotations") or {}).get(CHANGE) == change
             and not item.get("metadata", {}).get("deletionTimestamp")]
    return sorted(found, key=lambda item: item["metadata"].get("creationTimestamp", ""), reverse=True)


def creation_refused(items, change):
    """The reason the ReplicaSet of this change could not create its Pod (a
    policy or the quota refused it), if it could not."""
    for item in items:
        template = item.get("spec", {}).get("template", {}).get("metadata", {})
        if item.get("kind") == "ReplicaSet" and (template.get("annotations") or {}).get(CHANGE) == change:
            failure = condition(item, "ReplicaFailure")
            if failure.get("status") == "True":
                return failure.get("message") or failure.get("reason") or "ReplicaFailure"
    return None


def statuses(pod, key):
    return {entry.get("name"): entry for entry in pod.get("status", {}).get(key, []) or []}


def terminated_code(entry, field):
    return ((entry.get(field) or {}).get("terminated") or {}).get("exitCode")


def install_failure(pod):
    """Why the dependency step failed, if it did: it ended with a status
    other than 0, or it was started again."""
    entry = statuses(pod, "initContainerStatuses").get("install")
    if entry is None:
        return None
    codes = [code for code in (terminated_code(entry, "state"), terminated_code(entry, "lastState"))
             if code not in (None, 0)]
    restarts = entry.get("restartCount", 0) or 0
    if not codes and restarts < 1:
        return None
    text = "the dependency step (PREVIEW_INSTALL) ended with status %s" % (codes[0] if codes else "unknown")
    if 137 in codes:
        text += " (137: stopped at PREVIEW_INSTALL_SECONDS or for memory)"
    if restarts:
        text += " and was started again %d time(s)" % restarts
    return text


def stuck(pod):
    """Why a container of the Pod will not start, if one will not."""
    initial = statuses(pod, "initContainerStatuses")
    for name, entry in initial.items():
        code = terminated_code(entry, "state")
        if name != "install" and ((code not in (None, 0)) or (entry.get("restartCount", 0) or 0) >= 1):
            return "the setup container %s failed (status %s)" % (
                name, code if code is not None else terminated_code(entry, "lastState"))
    for name, entry in list(initial.items()) + list(statuses(pod, "containerStatuses").items()):
        waiting = (entry.get("state") or {}).get("waiting") or {}
        if waiting.get("reason") in STUCK:
            return "the container %s cannot start: %s %s" % (name, waiting["reason"], waiting.get("message", ""))
    return None


def past_the_last_rules(pod):
    """Whether the Pod's last rules container (net) has ended with status 0:
    from then on its running rules are in place and no dependency step can
    run in it any more. net still running, or ended otherwise, is not."""
    if pod is None:
        return False
    state = statuses(pod, "initContainerStatuses").get("net", {}).get("state") or {}
    return (state.get("terminated") or {}).get("exitCode") == 0


def describe(pod):
    if pod is None:
        return "no Pod of this change exists yet"
    waiting = []
    for name, entry in list(statuses(pod, "initContainerStatuses").items()) + \
            list(statuses(pod, "containerStatuses").items()):
        state = entry.get("state") or {}
        if "running" in state:
            waiting.append("%s running" % name)
        elif "waiting" in state:
            waiting.append("%s waiting (%s)" % (name, state["waiting"].get("reason", "")))
    return "the Pod is %s and not ready: %s" % (pod.get("status", {}).get("phase", "unknown"), ", ".join(waiting))


def endpoint_ready(cluster, name):
    for item in cluster.items("endpointslices", "kubernetes.io/service-name=" + name):
        for endpoint in item.get("endpoints") or []:
            if (endpoint.get("conditions") or {}).get("ready") is not False and endpoint.get("addresses"):
                return True
    return False


def ready_pod(cluster, names, change):
    for pod in pods_of(cluster.items("pods", "%s=%s" % (LABEL, names.name)), change):
        if condition(pod, "Ready").get("status") == "True":
            return True
    return False


def wait_until_ready(cluster, settings, names, change, archive):
    """Hands the checkout to the Pod of this change and waits until it is
    ready behind its Service, within PREVIEW_WAIT_SECONDS. Whatever ends the
    wait early (a failed or restarted dependency step, a failed setup
    container, a failed read, the end of the wait) removes the Deployment
    first unless its Pod's last rules container (net) has ended with status
    0: until then the kubelet could run the dependency step again, with
    public addresses open, and nobody would be looking any more."""
    seen = []
    try:
        watch(cluster, settings, names, change, archive, seen)
    except (Failure, OSError) as failure:
        if seen and past_the_last_rules(seen[-1]):
            if settings.failure_file:
                failure = Failure(startup_diagnostic(cluster, settings, seen[-1], archive))
            raise failure
        problem = cluster.remove("deployment", names.name)
        raise Failure(str(failure) + ("; the Deployment was removed, as its last rules had not ended with status 0"
                                      if problem is None else "; removing the Deployment failed: " + problem)) from None


def startup_diagnostic(cluster, settings, pod, archive):
    fallback = "application did not start"
    if pod is None or not past_the_last_rules(pod):
        return fallback
    names = {"app"} | {c["name"] for c in (settings.pod_options or {}).get("containers", [])}
    for name, entry in statuses(pod, "containerStatuses").items():
        reason = (entry.get("state", {}).get("waiting") or {}).get("reason")
        if name in names and reason in STUCK:
            fallback += " (%s: %s)" % (name, reason)
            break
    try:
        done = cluster.run(["exec", pod["metadata"]["name"], "-c", "app", "--", "head", "-c", "1025",
                            "/src/" + settings.failure_file], timeout=10, output_limit=1025)
        if done.returncode or len(done.stdout) > 1024:
            return fallback
        value = json.loads(done.stdout)
        if not isinstance(value, dict) or value.keys() - {"stage", "container", "file", "code"}:
            return fallback
        stage, container = value.get("stage"), value.get("container")
        if stage not in ("snapshot", "migration", "seed", "start") or not isinstance(container, str) or container not in names:
            return fallback
        message = "%s: %s failed" % (container, stage)
        if stage == "migration":
            filename, code = value.get("file"), value.get("code")
            if not isinstance(filename, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+[.]up[.]sql", filename):
                return fallback
            if not isinstance(code, str) or not re.fullmatch(r"[0-9]{4,5}|client", code):
                return fallback
            with tarfile.open(archive) as packed:
                member = packed.getmember("migrations/" + filename)
                if not member.isfile():
                    return fallback
            message += " (%s, error %s)" % (filename, code)
        elif "file" in value or "code" in value:
            return fallback
        return message if len(message) <= 300 else fallback
    except (Failure, OSError, ValueError, KeyError, RecursionError, tarfile.TarError):
        return fallback


def watch(cluster, settings, names, change, archive, seen):
    """The rounds of wait_until_ready, about a second apart: each appends
    the Pod it found (or None) to seen."""
    deadline = time.monotonic() + settings.wait_seconds
    handed = False
    while True:
        try:
            items = cluster.items("deployments,replicasets,pods", "%s=%s" % (LABEL, names.name))
        except Failure as failure:
            raise Failure("reading the preview's Pod failed: %s" % failure) from None
        refused = creation_refused(items, change)
        found = pods_of(items, change)
        pod = found[0] if found else None
        seen.append(pod)
        if refused:
            raise Failure("the cluster refused to create the preview's Pod: " + refused)
        progressed = False
        if pod is not None:
            failed = install_failure(pod)
            if failed:
                raise Failure(failed)
            blocked = stuck(pod)
            if blocked:
                raise Failure(blocked)
            receive = statuses(pod, "initContainerStatuses").get("receive", {})
            state = receive.get("state") or {}
            if not handed and "running" in state:
                hand_over_checkout(cluster, pod["metadata"]["name"], archive, deadline)
                handed = progressed = True
            elif "terminated" in state:
                handed = True  # an earlier launch handed this change over
            if condition(pod, "Ready").get("status") == "True" and endpoint_ready(cluster, names.name):
                return
        if progressed:
            continue
        if time.monotonic() >= deadline:
            raise Failure("the preview was not ready within PREVIEW_WAIT_SECONDS (%d): %s"
                          % (settings.wait_seconds, describe(pod)))
        time.sleep(POLL)


def hand_over_checkout(cluster, pod, archive, deadline):
    with open(archive, "rb") as source:
        done = cluster.run(["exec", "-i", pod, "-c", "receive", "--", "sh", "-c", RECEIVE], source=source,
                           timeout=max(30, int(deadline - time.monotonic())), limited=False)
    if done.returncode != 0:
        raise Failure("handing the checkout to the preview failed: " + last_line(done.stderr))


# -- the public check -----------------------------------------------------

class KeepRedirects(urllib.request.HTTPRedirectHandler):
    """A redirect is an answer of its own: the check does not follow it."""

    def redirect_request(self, *arguments, **keywords):
        return None


def status_of(opener, url, authorization=None):
    request = urllib.request.Request(url, headers={"User-Agent": "preview-check"})
    if authorization:
        request.add_header("Authorization", authorization)
    try:
        with opener.open(request, timeout=10) as response:
            return response.status
    except urllib.error.HTTPError as error:
        error.close()
        return error.code


def opened_cookie(opener, url, authorization):
    """The status of the signed URL and the value of the cookie it set."""
    request = urllib.request.Request(url, headers={"User-Agent": "preview-check", "Authorization": authorization})
    try:
        with opener.open(request, timeout=10) as response:
            return response.status, None
    except urllib.error.HTTPError as error:
        status, cookies = error.code, error.headers.get_all("Set-Cookie") or []
        error.close()
    for cookie in cookies:
        name, _, rest = cookie.partition("=")
        if name.strip() == COOKIE:
            return status, rest.split(";", 1)[0].strip()
    return status, None


def check_public_url(url, auth, *, signed, seconds=CHECK_SECONDS, pause=2.0):
    """Opens the address as the requester would (5.5 step 6):
    (a) without the password, which the router must answer with 401;
    (b) with the check user's password but without the signed URL's cookie,
        which the router must answer with 403;
    (c) the signed URL with the password, which must answer 302 and set the
        cookie;
    (d) the ready path with both, which the application must answer with 200
        to 399.
    What the router and the load balancer answer when they have nothing to
    forward to yet (404, 502, 503, 504), and an unreachable address, are
    waited out for seconds, in (a) as in (d). The signed URL
    itself is never part of a reason: a reason ends up in the history."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), KeepRedirects)
    authorization = "Basic " + base64.b64encode(auth.encode("utf-8")).decode("ascii")
    deadline = time.monotonic() + seconds
    while True:
        try:
            anonymous = status_of(opener, url)
            if anonymous in ROUTER_STATUSES:
                raise Waiting("%s answered %d without credentials: the router is not there yet" % (url, anonymous))
            if anonymous != 401:
                raise Failure("%s answered %d without credentials; the router's password check must answer 401"
                              % (url, anonymous))
            unsigned = status_of(opener, url, authorization)
            if unsigned == 401:
                raise Failure("the router refused the check user (401): PREVIEW_CHECK_AUTH does not match its "
                              "password file")
            if unsigned != 403:
                raise Failure("%s answered %d to the check user without the signed URL's cookie; the router's "
                              "signature check must answer 403" % (url, unsigned))
            status, cookie = opened_cookie(opener, signed, authorization)
            if status != 302:
                raise Failure("the router refused the signed URL (%d): its signing key and PREVIEW_SIGNING_KEY differ, "
                              "or the clocks do" % status if status == 403 else
                              "the signed URL answered %d instead of 302" % status)
            if not cookie:
                raise Failure("the signed URL answered 302 without setting the cookie %s" % COOKIE)
            request = urllib.request.Request(url, headers={"User-Agent": "preview-check", "Authorization": authorization,
                                                           "Cookie": "%s=%s" % (COOKIE, cookie)})
            try:
                with opener.open(request, timeout=10) as response:
                    answer = response.status
            except urllib.error.HTTPError as error:
                error.close()
                answer = error.code
        except Waiting as waiting:
            observed = str(waiting)
        except (OSError, http.client.HTTPException) as error:
            observed = "%s could not be reached: %s" % (url, getattr(error, "reason", None) or error)
        else:
            if 200 <= answer < 400:
                return ("%s answered 401 without credentials, 403 without the signed URL, and %d with both"
                        % (url, answer))
            if answer in (401, 403):
                raise Failure("the router refused the check user with the signed URL's cookie (%d)" % answer)
            if answer not in ROUTER_STATUSES:
                raise Failure("the application answered %d at %s" % (answer, url))
            observed = "%s answered %d with both: the router does not reach the application" % (url, answer)
        if time.monotonic() >= deadline:
            raise Failure(observed)
        time.sleep(pause)


def sign(key, host, end):
    """The signature of the router's check (README.md, "The signed URL"):
    HMAC-SHA256 with the key over the lowercase host, a newline and the end
    in Unix seconds, base64url without "=" padding, as njs writes it."""
    digest = hmac.new(key.encode("utf-8"), ("%s\n%s" % (host, end)).encode("utf-8"), hashlib.sha256).digest()
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")


def signed_url(key, host, end):
    return "https://%s%s?t=%d.%s" % (host, OPEN_PATH, end, sign(key, host, end))


# -- the two commands -------------------------------------------------------

def write_result(path, lines):
    temporary = "%s.%d.tmp" % (path, os.getpid())
    with open(temporary, "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines) + "\n")
    os.replace(temporary, path)


def credential(credentials, name):
    value = credentials.get(name) or ""
    if not value:
        raise Failure("%s was not given to this command (the role's secrets must name it)" % name)
    return value


def redactor(credentials):
    values = set()
    for value in credentials.values():
        if value:
            values.update({value, base64.b64encode(value.encode()).decode()})
            if ":" in value:
                values.add(value.split(":", 1)[1])
    values = sorted((value for value in values if len(value) >= 4), key=len, reverse=True)

    def redact(text):
        for value in values:
            text = text.replace(value, "[credential]")
        return text
    return redact


def up(environment, credentials, *, check=None):
    check = check or check_public_url
    redact = redactor(credentials)
    directory = None
    try:
        settings = read_settings(environment, "up")
        token = credential(credentials, TOKEN)
        auth = credential(credentials, CHECK_AUTH)
        if ":" not in auth or auth.startswith(":"):
            raise Failure("PREVIEW_CHECK_AUTH must be user:password")
        signing_key = credential(credentials, SIGNING_KEY)
        if len(signing_key) < SIGNING_KEY_LENGTH:
            raise Failure("PREVIEW_SIGNING_KEY must be at least %d characters (openssl rand -base64 32 makes 44)"
                          % SIGNING_KEY_LENGTH)
        names = preview_names(settings.site, settings.issue, settings.domain)
        entries = read_env_file(settings.workspace, settings.env_file) if settings.env_file else []
        directory = private_directory(environment)
        if settings.pack_dir:
            paths = pack_paths(settings.workspace, settings.pack_dir, environment)
        else:
            paths = checkout_paths(settings.workspace, environment)
        archive = os.path.join(directory, "checkout.tar")
        archive_digest = write_tar(settings.workspace, paths, archive, settings.pack_dir, settings.extra_files)
        expires = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0) + \
            datetime.timedelta(hours=settings.lifetime)
        deployment, service = manifests(settings, names, entries, expires)
        change = change_identifier(archive_digest, deployment["spec"]["template"])
        deployment["spec"]["template"]["metadata"]["annotations"][CHANGE] = change
        # The kubeconfig only now, after git: nothing started before it can
        # read it, and kubectl runs nothing of the checkout.
        cluster = Cluster(settings, environment, directory, write_kubeconfig(directory, settings, token))
        say("preview %s: change %s from %d paths" % (names.name, change, len(paths)))
        if ready_pod(cluster, names, change):
            cluster.call(["annotate", "deployment", names.name, "%s=%s" % (EXPIRES, stamp(expires)), "--overwrite"],
                         "extending the lifetime")
            cluster.apply([service])
            say("the same change is running already; its lifetime now ends at %s" % stamp(expires))
        else:
            cluster.apply([deployment, service])
            wait_until_ready(cluster, settings, names, change, archive)
        # The URL ends with the preview's lifetime: a lifetime extended gets
        # a new URL, and the old one stops opening at the old end.
        opening = signed_url(signing_key, names.host, int(expires.timestamp()))
        observed = check("https://" + names.host + settings.ready_path, auth, signed=opening)
        write_result(settings.result, [
            opening,
            "この画面は %s (日本時間) まで開けます" % expires.astimezone(JAPAN).strftime("%Y-%m-%d %H:%M"),
            "動いている変更の識別子: %s" % change] + (settings.result_lines or []))
        say(redact("preview up: %s" % observed))
        return 0
    except Failure as failure:
        say(redact(str(failure)))
        return 1
    except OSError as error:
        # Anything the system refused (a file, a program): the reason as the
        # last line, never a traceback.
        say(redact("the preview command stopped: %s" % error))
        return 1
    finally:
        if directory is not None:
            shutil.rmtree(directory, ignore_errors=True)


def down(environment, credentials):
    redact = redactor(credentials)
    directory = None
    try:
        settings = read_settings(environment, "down")
        token = credential(credentials, TOKEN)
        names = preview_names(settings.site, settings.issue, settings.domain)
        directory = private_directory(environment)
        cluster = Cluster(settings, environment, directory, write_kubeconfig(directory, settings, token))
        problem = cluster.remove("deployment,service", names.name)
        if problem is not None:
            raise Failure("removing %s failed: %s" % (names.name, problem))
        deadline = time.monotonic() + settings.wait_seconds
        while True:
            left = cluster.call(["get", "deployment,service", "-l", "%s=%s" % (LABEL, names.name), "-o", "name"],
                                "reading back what is left").decode("utf-8", "replace").split()
            if not left:
                say("preview %s: nothing of it is left" % names.name)
                return 0
            if time.monotonic() >= deadline:
                raise Failure("preview %s is still there after it was removed: %s" % (names.name, ", ".join(left)))
            time.sleep(POLL)
    except Failure as failure:
        say(redact(str(failure)))
        return 1
    except OSError as error:
        # Anything the system refused (a file, a program): the reason as the
        # last line, never a traceback.
        say(redact("the preview command stopped: %s" % error))
        return 1
    finally:
        if directory is not None:
            shutil.rmtree(directory, ignore_errors=True)


def run(command, credentials, environment):
    return up(environment, credentials) if command == "up" else down(environment, credentials)


def origin(environment):
    """Prints the origin of the request's preview, https://<label>.<domain>,
    named as up names it (preview_names), from TASK_ISSUE, PREVIEW_NAME and
    PREVIEW_DOMAIN. Reads no credential and calls nothing. 2, with the reason
    on standard error and nothing on standard output, when one of them is
    missing or wrong."""
    try:
        issue = required(environment, "TASK_ISSUE")
        site = required(environment, "PREVIEW_NAME")
        domain = matching(environment, "PREVIEW_DOMAIN", DOMAIN, DOMAIN_DESCRIPTION)
        names = preview_names(site, issue, domain)
    except Failure as failure:
        print(failure, file=sys.stderr, flush=True)
        return 2
    print("https://" + names.host, flush=True)
    return 0


# -- step 0: the credentials out of the environment ---------------------------

def take_credentials(environ):
    """Removes every credential the engine lists (and the two this program
    reads, listed or not) from environ; returns the two."""
    names = {name for name in environ.get("TASK_CREDENTIAL_NAMES", "").split(":") if name} | set(CREDENTIALS)
    held = {}
    for name in sorted(names):
        value = environ.pop(name, None)
        if value is not None and name in CREDENTIALS:
            held[name] = value
    environ.pop("TASK_CREDENTIAL_NAMES", None)
    return held


def pass_on(held):
    """A pipe holding the credentials, its reading end left open across exec."""
    data = json.dumps(held).encode()
    if len(data) > HANDOVER_LIMIT:
        raise Failure("the credentials are larger than %d bytes" % HANDOVER_LIMIT)
    reader, writer = os.pipe()
    try:
        view = memoryview(data)
        while view:
            view = view[os.write(writer, view):]
    finally:
        os.close(writer)
    os.set_inheritable(reader, True)
    return reader


def receive(descriptor):
    chunks = []
    while True:
        chunk = os.read(descriptor, 65536)
        if not chunk:
            break
        chunks.append(chunk)
    os.close(descriptor)
    try:
        held = json.loads(b"".join(chunks) or b"{}")
    except ValueError:
        raise Failure("the credentials handed over could not be read") from None
    return {name: value for name, value in held.items() if name in CREDENTIALS and isinstance(value, str)}


def hand_over(command):
    held = take_credentials(os.environ)
    try:
        reader = pass_on(held)
    except (OSError, Failure) as error:
        say("the credentials could not be handed over: %s" % error)
        return 1
    executable = sys.executable
    if not executable:
        say("the Python interpreter's own path is unknown, so the program cannot start again without credentials")
        return 1
    arguments = [executable, "-I", "-B", os.path.abspath(__file__), HANDOVER, str(reader), command]
    # Start again without the credentials: what /proc/self/environ shows is
    # the environment a program was started with, not what it removed since.
    try:
        os.execve(executable, arguments, dict(os.environ))
    except OSError as error:
        say("the program could not start again without credentials: %s" % error)
        return 1


def usage():
    print("usage: preview.py up|down|origin", file=sys.stderr)
    return 2


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if argv[:1] == [HANDOVER]:
        if len(argv) != 3 or not argv[1].isdigit() or argv[2] not in COMMANDS:
            return usage()
        try:
            credentials = receive(int(argv[1]))
        except (OSError, Failure) as error:
            say("the credentials could not be received: %s" % error)
            return 1
        left = sorted(name for name in CREDENTIALS + ("TASK_CREDENTIAL_NAMES",) if name in os.environ)
        if left:
            say("credentials are still in the environment after the restart: %s" % ", ".join(left))
            return 1
        return run(argv[2], credentials, dict(os.environ))
    if argv == ["origin"]:
        # No credential is read or needed: nothing to hand over.
        return origin(dict(os.environ))
    if len(argv) != 1 or argv[0] not in COMMANDS:
        return usage()
    return hand_over(argv[0])


if __name__ == "__main__":
    sys.exit(main())
