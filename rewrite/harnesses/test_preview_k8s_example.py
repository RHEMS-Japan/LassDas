"""Exercise the Kubernetes preview example (rewrite/examples/preview-k8s)
against a fake cluster. A fake kubectl and a recording wrapper around git
stand first on PATH; no cluster, registry or other host is reached.

Several checks come with a positive control: a copy of the script with one
safeguard taken out, which has to show the leak the check looks for. A check
whose control shows nothing would pass without looking.
"""
import base64
import contextlib
import datetime
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest

EXAMPLE = Path(__file__).resolve().parents[1] / "examples" / "preview-k8s"
TOOL = EXAMPLE / "preview.py"
# Made-up values, none of them shaped like a real key. Every check for a leak
# looks for these exact strings.
TOKEN = "fake-cluster-token-4b1e9d0c"
CHECK_PASSWORD = "fake-check-password-93a7"
CHECK_AUTH = "preview-check:" + CHECK_PASSWORD
CHECK_HEADER = base64.b64encode(CHECK_AUTH.encode()).decode()
LEAKS = (TOKEN, CHECK_PASSWORD, CHECK_HEADER)
NET_IMAGE = "registry.example.com/net@sha256:" + "0" * 64
POD_POLICY_REFUSAL = ("pods \"pv-sample-12-6d4cf-x2v9p\" is forbidden: ValidatingAdmissionPolicy "
                      "'preview-pod-shape' with binding 'preview-pod-shape' denied request: "
                      "init containers must be net-hold, receive, net")

# The fake cluster. It keeps its state next to itself, because the script
# gives kubectl no environment of its own to point it elsewhere. Every call is
# recorded with its environment and its parent's (the script's) starting
# environment. scenario.json decides what the cluster does:
#   apply_error  text      apply prints it (with @TOKEN@ replaced by the
#                          kubeconfig's token, as a kubectl that echoed its
#                          credential would) and fails
#   apply_block  true      apply never returns
#   pod          name      what the Pod does once the checkout has arrived:
#                          ready, never_ready, crash, install_fails,
#                          install_restarts; failed_create makes no Pod at all
#   delete_stuck true      delete leaves everything in place
FAKE_CLUSTER = r'''
import json
import os
from pathlib import Path
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
OUTCOME = {"ready": "ready", "never_ready": "unready", "crash": "crashing",
           "install_fails": "install_failed", "install_restarts": "install_restarted"}


def parent_environment():
    parent = os.getppid()
    try:
        return Path("/proc/%d/environ" % parent).read_bytes().decode("utf-8", "replace")
    except OSError:
        return subprocess.run(["ps", "-wwE", "-o", "command=", "-p", str(parent)],
                              capture_output=True, text=True).stdout


def load(name, default):
    path = HERE / name
    return json.loads(path.read_text()) if path.exists() else default


def option(arguments, flag):
    return arguments[arguments.index(flag) + 1] if flag in arguments else ""


def pod_name(name):
    return name + "-6d4cf-x2v9p"


def pod_object(deployment, pod):
    if pod is None or pod["phase"] == "failed_create":
        return None
    template = deployment["spec"]["template"]
    names = [container["name"] for container in template["spec"].get("initContainers", [])]
    phase = pod["phase"]
    done = {"terminated": {"exitCode": 0, "reason": "Completed"}}
    statuses = []
    for index, name in enumerate(names):
        status = {"name": name, "restartCount": 0, "state": {"waiting": {"reason": "PodInitializing"}}}
        if phase == "receiving":
            if name == "net-hold":
                status["state"] = done
            elif name == "receive":
                status["state"] = {"running": {}}
        elif phase in ("install_failed", "install_restarted"):
            if index < names.index("install"):
                status["state"] = done
            elif name == "install" and phase == "install_failed":
                status["state"] = {"terminated": {"exitCode": 137, "reason": "Error"}}
            elif name == "install":
                status.update(restartCount=1, state={"running": {}},
                              lastState={"terminated": {"exitCode": 137, "reason": "Error"}})
        else:
            status["state"] = done
        statuses.append(status)
    if phase in ("ready", "unready"):
        application = {"running": {}}
    elif phase == "crashing":
        application = {"waiting": {"reason": "CrashLoopBackOff", "message": "back-off restarting failed container"}}
    else:
        application = {"waiting": {"reason": "PodInitializing"}}
    return {"apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": pod_name(deployment["metadata"]["name"]),
                         "labels": dict(template["metadata"]["labels"]),
                         "annotations": dict(template["metadata"]["annotations"]),
                         "creationTimestamp": "2026-01-01T00:00:00Z"},
            "spec": template["spec"],
            "status": {"phase": "Running" if phase in ("ready", "unready") else "Pending",
                       "conditions": [{"type": "Ready", "status": "True" if phase == "ready" else "False"}],
                       "initContainerStatuses": statuses,
                       "containerStatuses": [{"name": "app", "restartCount": 0, "ready": phase == "ready",
                                              "state": application}]}}


def replicaset_object(deployment, pod):
    conditions = []
    if pod is not None and pod["phase"] == "failed_create":
        conditions.append({"type": "ReplicaFailure", "status": "True", "reason": "FailedCreate",
                           "message": POD_POLICY_REFUSAL})
    return {"apiVersion": "apps/v1", "kind": "ReplicaSet",
            "metadata": {"name": deployment["metadata"]["name"] + "-6d4cf",
                         "labels": dict(deployment["spec"]["template"]["metadata"]["labels"])},
            "spec": {"template": deployment["spec"]["template"]},
            "status": {"conditions": conditions}}


def get(state, arguments):
    kinds = arguments[0].split(",")
    key, _, value = option(arguments, "-l").partition("=")
    items = []
    for kind in kinds:
        if kind.startswith("deployment"):
            items += [item for name, item in state["deployments"].items() if key == "preview" and name == value]
        elif kind.startswith("service"):
            items += [item for name, item in state["services"].items() if key == "preview" and name == value]
        elif kind.startswith("replicaset"):
            items += [replicaset_object(item, state["pods"].get(name))
                      for name, item in state["deployments"].items() if key == "preview" and name == value]
        elif kind.startswith("pod"):
            for name, item in state["deployments"].items():
                if key == "preview" and name == value:
                    pod = pod_object(item, state["pods"].get(name))
                    if pod is not None:
                        items.append(pod)
        elif kind.startswith("endpointslice"):
            if key == "kubernetes.io/service-name" and value in state["services"]:
                pod = state["pods"].get(value)
                ready = value in state["deployments"] and pod is not None and pod["phase"] == "ready"
                endpoints = [{"addresses": ["10.1.2.3"], "conditions": {"ready": ready}}] if pod else []
                items.append({"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
                              "metadata": {"name": value + "-abcde"}, "endpoints": endpoints})
    if option(arguments, "-o") == "name":
        for item in items:
            prefix = {"Deployment": "deployment.apps", "Service": "service"}.get(item["kind"], item["kind"].lower())
            print("%s/%s" % (prefix, item["metadata"]["name"]))
    else:
        print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
    return 0


def apply(scenario, state, data):
    count = len(list(HERE.glob("applied-*.json")))
    (HERE / ("applied-%d.json" % count)).write_bytes(data)
    if scenario.get("apply_block"):
        (HERE / "apply-started").write_text("")
        time.sleep(3600)
    if scenario.get("apply_error"):
        message = scenario["apply_error"]
        if "@TOKEN@" in message:
            config = json.loads(Path(os.environ["KUBECONFIG"]).read_text())
            message = message.replace("@TOKEN@", config["users"][0]["user"]["token"])
        print(message, file=sys.stderr)
        return 1
    for item in json.loads(data)["items"]:
        name = item["metadata"]["name"]
        if item["kind"] == "Deployment":
            state["deployments"][name] = item
            change = item["spec"]["template"]["metadata"]["annotations"]["preview-change"]
            pod = state["pods"].get(name)
            if pod is None or pod["change"] != change:
                phase = "failed_create" if scenario.get("pod") == "failed_create" else "receiving"
                state["pods"][name] = {"change": change, "phase": phase}
        elif item["kind"] == "Service":
            state["services"][name] = item
        print("%s/%s serverside-applied" % (item["kind"].lower(), name))
    return 0


def execute(scenario, state, arguments, data):
    target = option(arguments, "-i")
    for name, pod in state["pods"].items():
        if pod_name(name) == target and pod["phase"] == "receiving" and option(arguments, "-c") == "receive":
            state["streams"] += 1
            (HERE / ("stream-%d.tar" % state["streams"])).write_bytes(data)
            pod["phase"] = OUTCOME[scenario.get("pod", "ready")]
            return 0
    print("error: unable to upgrade connection: container not found (\"receive\")", file=sys.stderr)
    return 1


def annotate(state, arguments):
    name = arguments[1]
    key, _, value = arguments[2].partition("=")
    if name not in state["deployments"]:
        print("Error from server (NotFound): deployments.apps \"%s\" not found" % name, file=sys.stderr)
        return 1
    state["deployments"][name]["metadata"].setdefault("annotations", {})[key] = value
    return 0


def delete(scenario, state, arguments):
    value = option(arguments, "-l").partition("=")[2]
    for kind in arguments[0].split(","):
        if scenario.get("delete_stuck"):
            continue
        if kind.startswith("deployment"):
            state["deployments"].pop(value, None)
            state["pods"].pop(value, None)
        elif kind.startswith("service"):
            state["services"].pop(value, None)
    return 0


def main():
    scenario = load("scenario.json", {})
    state = load("state.json", {"deployments": {}, "services": {}, "pods": {}, "streams": 0})
    arguments = sys.argv[1:]
    while arguments and arguments[0].startswith("--"):
        arguments.pop(0)
    verb = arguments[0] if arguments else ""
    data = sys.stdin.buffer.read() if verb in ("apply", "exec") else b""
    config = os.environ.get("KUBECONFIG", "")
    with open(HERE / "calls.jsonl", "a", encoding="utf-8") as handle:
        handle.write(json.dumps({
            "argv": sys.argv[1:], "verb": verb, "time": time.time(), "environment": dict(os.environ),
            "parent": parent_environment(), "kubeconfig": config,
            "kubeconfig_mode": oct(os.stat(config).st_mode & 0o777) if config and os.path.exists(config) else ""}) + "\n")
    status = 2
    if verb == "apply":
        status = apply(scenario, state, data)
    elif verb == "get":
        status = get(state, arguments[1:])
    elif verb == "exec":
        status = execute(scenario, state, arguments[1:], data)
    elif verb == "annotate":
        status = annotate(state, arguments[1:])
    elif verb == "delete":
        status = delete(scenario, state, arguments[1:])
    else:
        print("fake cluster: unknown verb %r" % verb, file=sys.stderr)
    (HERE / "state.json").write_text(json.dumps(state))
    sys.exit(status)


POD_POLICY_REFUSAL = @REFUSAL@
main()
'''

# Records every git call, with its environment, its parent's starting
# environment and whether anything under the launch's temporary directory
# holds the cluster token yet, then runs the real git.
FAKE_GIT = r'''
import json
import os
from pathlib import Path
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent


def parent_environment():
    parent = os.getppid()
    try:
        return Path("/proc/%d/environ" % parent).read_bytes().decode("utf-8", "replace")
    except OSError:
        return subprocess.run(["ps", "-wwE", "-o", "command=", "-p", str(parent)],
                              capture_output=True, text=True).stdout


def token_on_disk():
    for path in Path(@TMP@).rglob("*"):
        try:
            if path.is_file() and @TOKEN@.encode() in path.read_bytes():
                return True
        except OSError:
            pass
    return False


with open(HERE / "git-calls.jsonl", "a", encoding="utf-8") as handle:
    handle.write(json.dumps({"argv": sys.argv[1:], "time": time.time(), "environment": dict(os.environ),
                             "parent": parent_environment(), "token_on_disk": token_on_disk()}) + "\n")
os.execv(@GIT@, [@GIT@] + sys.argv[1:])
'''


def load_tool(path=TOOL, name="preview_k8s_example"):
    specification = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


class PreviewExampleTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="preview-k8s-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.checkout, self.home, self.tmp = self.base / "checkout", self.base / "home", self.base / "tmp"
        self.fake, self.outside = self.base / "fake", self.base / "outside"
        for path in (self.checkout, self.home, self.tmp, self.fake, self.outside):
            path.mkdir()
        self.real_git = shutil.which("git")
        self.assertIsNotNone(self.real_git, "these tests need git")
        self.git("init", "-q")
        (self.checkout / "site").mkdir()
        (self.checkout / "site" / "index.html").write_text("<h1>Welcome</h1>\n")
        (self.checkout / ".env.preview").write_text("GREETING=hello\n")
        self.git("add", "-A")
        # A file outside the checkout that holds the token: what a link to
        # /proc/self/environ or to another secret would let a reader see.
        (self.outside / "environ").write_text("PREVIEW_K8S_TOKEN=%s\n" % TOKEN)
        (self.outside / "inner.txt").write_text("LEAKED=%s\n" % TOKEN)
        self.ca = self.base / "ca.crt"
        self.ca.write_text("fake certificate authority for these tests\n")
        self.write_fake("kubectl", FAKE_CLUSTER.replace("@REFUSAL@", repr(POD_POLICY_REFUSAL)))
        self.write_fake("git", FAKE_GIT.replace("@GIT@", repr(self.real_git)).replace(
            "@TMP@", repr(str(self.tmp))).replace("@TOKEN@", repr(TOKEN)))
        self.scenario({})
        self.tool = load_tool()
        self.checks = []

    # -- helpers ---------------------------------------------------------

    def git(self, *arguments):
        subprocess.run([self.real_git, "-C", str(self.checkout), *arguments], check=True,
                       stdin=subprocess.DEVNULL, capture_output=True)

    def write_fake(self, name, source):
        path = self.fake / name
        path.write_text("#!%s\n%s" % (sys.executable, source))
        path.chmod(0o755)

    def scenario(self, values):
        (self.fake / "scenario.json").write_text(json.dumps(values))

    def reset_cluster(self):
        for path in self.fake.iterdir():
            if path.name not in ("kubectl", "git", "scenario.json"):
                path.unlink()

    def environment(self, **changes):
        environment = {
            "PATH": str(self.fake) + os.pathsep + os.environ.get("PATH", os.defpath),
            "LANG": "C.UTF-8",
            "TASK_WORKSPACE": str(self.checkout), "TASK_HOME": str(self.home), "TASK_ISSUE": "12",
            "TASK_PREVIEW": str(self.home / "task-preview.txt"), "TMPDIR": str(self.tmp),
            "PREVIEW_NAME": "sample",
            "PREVIEW_START": 'python3 -m http.server "$PORT" --bind 0.0.0.0 --directory site',
            "PREVIEW_PORT": "8080", "PREVIEW_READY_PATH": "/", "PREVIEW_LIFETIME_HOURS": "72",
            "PREVIEW_IMAGE": "python:3.12-slim", "PREVIEW_DOMAIN": "preview.example.com",
            "PREVIEW_NAMESPACE": "preview-apps", "PREVIEW_K8S_SERVER": "https://cluster.example.com",
            "PREVIEW_K8S_CA_FILE": str(self.ca), "PREVIEW_NET_IMAGE": NET_IMAGE,
        }
        environment.update(changes)
        return {key: value for key, value in environment.items() if value is not None}

    def with_credentials(self, environment):
        return dict(environment, PREVIEW_K8S_TOKEN=TOKEN, PREVIEW_CHECK_AUTH=CHECK_AUTH,
                    TASK_CREDENTIAL_NAMES="PREVIEW_CHECK_AUTH:PREVIEW_K8S_TOKEN")

    def credentials(self):
        return {"PREVIEW_K8S_TOKEN": TOKEN, "PREVIEW_CHECK_AUTH": CHECK_AUTH}

    def passing_check(self, url, auth, **_):
        self.checks.append((url, auth))
        return "401 without credentials, 200 with them"

    def up(self, tool=None, check=None, **changes):
        """The up command in this process, its credentials handed over as
        after the restart without them."""
        tool = tool or self.tool
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            status = tool.up(self.environment(**changes), self.credentials(), check=check or self.passing_check)
        return status, output.getvalue()

    def down(self, **changes):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            status = self.tool.down(self.environment(**changes), {"PREVIEW_K8S_TOKEN": TOKEN})
        return status, output.getvalue()

    def launch(self, *arguments, tool=TOOL, environment=None):
        """The command as the engine starts it: a process of its own, the
        credentials in its environment."""
        environment = environment if environment is not None else self.with_credentials(self.environment())
        return subprocess.Popen([sys.executable, "-B", str(tool), *arguments], cwd=self.checkout, env=environment,
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, start_new_session=True)

    def run_tool(self, *arguments, tool=TOOL, environment=None):
        process = self.launch(*arguments, tool=tool, environment=environment)
        self.addCleanup(lambda: process.poll() is None and os.killpg(process.pid, signal.SIGKILL))
        output, _ = process.communicate(timeout=120)
        return process.returncode, output

    def mutated(self, replacements, name):
        """A copy of the script with one safeguard taken out."""
        source = TOOL.read_text()
        for old, new in replacements:
            self.assertIn(old, source, "the safeguard the control removes is not in the script any more")
            source = source.replace(old, new)
        path = self.base / ("%s.py" % name)
        path.write_text(source)
        return path

    def calls(self):
        path = self.fake / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def git_calls(self):
        path = self.fake / "git-calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def verbs(self):
        return [call["verb"] for call in self.calls()]

    def applied(self):
        found = sorted(self.fake.glob("applied-*.json"), key=lambda path: int(path.stem.split("-")[1]))
        return [json.loads(path.read_text()) for path in found]

    def applied_deployments(self):
        return [item for document in self.applied() for item in document["items"] if item["kind"] == "Deployment"]

    def state(self):
        return json.loads((self.fake / "state.json").read_text())

    def streams(self):
        return sorted(self.fake.glob("stream-*.tar"))

    def assertNoLeak(self, text, where):
        for value in LEAKS:
            self.assertNotIn(value, text, "a credential reached %s" % where)

    def files_holding(self, root, value):
        found = []
        for path in Path(root).rglob("*"):
            if path.is_file() and not path.is_symlink() and value.encode() in path.read_bytes():
                found.append(path)
        return found

    # -- names -----------------------------------------------------------

    def test_names_come_from_the_site_and_the_request_number(self):
        names = self.tool.preview_names("sample", "12", "preview.example.com")
        self.assertEqual((names.name, names.host, names.url),
                         ("pv-sample-12", "sample-12.preview.example.com", "https://sample-12.preview.example.com/"))
        self.assertEqual(self.tool.preview_names("demo", "PROJ-81", "preview.example.com").name, "pv-demo-proj-81")
        self.assertEqual(self.tool.preview_names("demo", "Proj_81 x", "preview.example.com").name, "pv-demo-proj-81-x")
        label = re.compile(r"[a-z]([-a-z0-9]*[a-z0-9])?")
        long_issue = "VERYLONGPROJECTKEY" * 4 + "-1234"
        first = self.tool.preview_names("sample", long_issue, "preview.example.com")
        second = self.tool.preview_names("sample", long_issue[:-1] + "5", "preview.example.com")
        for names in (first, second):
            self.assertLessEqual(len(names.name), 63)
            self.assertLessEqual(len(names.host.split(".")[0]), 63)
            self.assertRegex(names.name, label)
            self.assertTrue(label.fullmatch(names.name))
            self.assertEqual(names.name, "pv-" + names.host.split(".")[0])
        self.assertNotEqual(first.name, second.name, "two long request numbers that differ at the end collide")
        self.assertEqual(first, self.tool.preview_names("sample", long_issue, "preview.example.com"))
        for site, issue in (("Sample", "1"), ("9sample", "1"), ("sample-", "1"), ("sam_ple", "1"),
                            ("sample", "---"), ("sample", "")):
            with self.subTest(site=site, issue=issue), self.assertRaises(self.tool.Failure):
                self.tool.preview_names(site, issue, "preview.example.com")

    # -- what up builds --------------------------------------------------

    def test_up_builds_a_deployment_and_a_service_of_the_fixed_shape(self):
        before = datetime.datetime.now(datetime.timezone.utc)
        status, output = self.up()
        self.assertEqual(status, 0, output)
        documents = self.applied()
        self.assertEqual(len(documents), 1)
        self.assertEqual([item["kind"] for item in documents[0]["items"]], ["Deployment", "Service"])
        deployment, service = documents[0]["items"]
        self.assertEqual(deployment["metadata"]["name"], "pv-sample-12")
        self.assertEqual(deployment["metadata"]["namespace"], "preview-apps")
        self.assertEqual(deployment["spec"]["strategy"], {"type": "Recreate"})
        self.assertEqual(deployment["spec"]["replicas"], 1)
        annotations = deployment["metadata"]["annotations"]
        expires = datetime.datetime.strptime(annotations["preview-expires-at"], "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=datetime.timezone.utc)
        self.assertLessEqual(before.replace(microsecond=0) + datetime.timedelta(hours=72), expires)
        self.assertLessEqual(expires, datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=72))
        self.assertEqual(annotations["preview-issue"], "12")
        template = deployment["spec"]["template"]
        self.assertRegex(template["metadata"]["annotations"]["preview-change"], r"^[0-9a-f]{12}$")
        self.assertNotIn("preview-expires-at", template["metadata"]["annotations"],
                         "a lifetime in the Pod template would replace the Pod whenever it is extended")
        self.assertEqual(template["metadata"]["labels"]["preview"], "pv-sample-12")
        self.assertEqual(deployment["spec"]["selector"], {"matchLabels": {"preview": "pv-sample-12"}})
        pod = template["spec"]
        self.assertIs(pod["automountServiceAccountToken"], False)
        self.assertIs(pod["enableServiceLinks"], False)
        self.assertIs(pod["hostUsers"], False)
        self.assertEqual(pod["securityContext"]["seccompProfile"], {"type": "RuntimeDefault"})
        self.assertIs(pod["securityContext"]["runAsNonRoot"], True)
        # The cluster's Priority admission refuses a Pod whose preemptionPolicy
        # differs from its class's (PreemptLowerPriority without a class), so
        # Never would keep every Pod from being created. Priority 0 and no
        # class are what keep a preview from displacing other Pods.
        self.assertNotIn("preemptionPolicy", pod)
        self.assertNotIn("priority", pod)
        self.assertNotIn("priorityClassName", pod)
        self.assertEqual(pod["terminationGracePeriodSeconds"], 5)
        for absent in ("nodeName", "hostNetwork", "hostPID", "hostIPC", "tolerations", "affinity",
                       "runtimeClassName", "serviceAccountName", "ephemeralContainers", "resourceClaims"):
            self.assertNotIn(absent, pod)
        self.assertNotIn("sysctls", pod["securityContext"])
        for volume in pod["volumes"]:
            self.assertTrue("emptyDir" in volume or volume.get("configMap", {}).get("name") == "preview-egress", volume)
        self.assertEqual([container["name"] for container in pod["initContainers"]], ["net-hold", "receive", "net"])
        self.assertEqual([container["name"] for container in pod["containers"]], ["app"])
        for container in pod["initContainers"] + pod["containers"]:
            context = container["securityContext"]
            self.assertIs(context["allowPrivilegeEscalation"], False, container["name"])
            self.assertNotIn("privileged", context)
            self.assertNotIn("restartPolicy", container)
            self.assertIn("memory", container["resources"]["limits"])
            self.assertIn("ephemeral-storage", container["resources"]["limits"])
            for variable in container.get("env", []):
                self.assertEqual(set(variable), {"name", "value"}, "a value read from another object")
            self.assertNotIn("envFrom", container)
            for port in container.get("ports", []):
                self.assertNotIn("hostPort", port)
            if container["name"].startswith("net"):
                self.assertEqual(container["image"], NET_IMAGE)
                self.assertEqual(container["command"], self.tool.NET_COMMANDS[container["name"]])
                self.assertNotIn("args", container)
                self.assertEqual(context["capabilities"], {"drop": ["ALL"], "add": ["NET_ADMIN"]})
            else:
                self.assertIs(context["runAsNonRoot"], True, container["name"])
                self.assertEqual(context["capabilities"], {"drop": ["ALL"]})
                self.assertEqual(container["image"], "python:3.12-slim")
        application = pod["containers"][0]
        self.assertEqual(application["command"], ["sh", "-c", 'python3 -m http.server "$PORT" --bind 0.0.0.0 --directory site'])
        self.assertEqual(application["resources"]["limits"]["memory"], "512Mi")
        self.assertEqual(application["readinessProbe"]["httpGet"], {"path": "/", "port": 8080})
        self.assertEqual(service["spec"]["type"], "ClusterIP")
        self.assertEqual(service["spec"]["selector"], {"preview": "pv-sample-12"})
        self.assertEqual(service["spec"]["ports"], [{"name": "http", "port": 80, "targetPort": 8080, "protocol": "TCP"}])
        self.assertNotIn("annotations", service["metadata"])
        self.assertNotIn("externalIPs", service["spec"])
        self.assertEqual(len(self.streams()), 1)

    def test_install_runs_between_its_own_rules_and_under_a_hard_time_limit(self):
        status, output = self.up(PREVIEW_INSTALL="npm ci", PREVIEW_INSTALL_SECONDS="120", PREVIEW_MEMORY="768Mi")
        self.assertEqual(status, 0, output)
        pod = self.applied_deployments()[-1]["spec"]["template"]["spec"]
        self.assertEqual([container["name"] for container in pod["initContainers"]],
                         ["net-hold", "receive", "net-install", "install", "net"])
        install = pod["initContainers"][3]
        self.assertEqual(install["command"], ["timeout", "-s", "KILL", "120", "sh", "-c", "npm ci"])
        self.assertNotIn("args", install)
        self.assertEqual(install["resources"]["limits"]["memory"], "768Mi")
        self.assertEqual(pod["initContainers"][2]["command"], self.tool.NET_COMMANDS["net-install"])
        self.assertNotEqual(self.tool.NET_COMMANDS["net-install"], self.tool.NET_COMMANDS["net"])
        self.assertEqual(self.tool.NET_COMMANDS["net-hold"], self.tool.NET_COMMANDS["net"])
        self.reset_cluster()
        status, output = self.up(PREVIEW_INSTALL="npm ci")
        self.assertEqual(status, 0, output)
        install = self.applied_deployments()[-1]["spec"]["template"]["spec"]["initContainers"][3]
        self.assertEqual(install["command"][:4], ["timeout", "-s", "KILL", "300"], "the default limit is 300 seconds")

    def test_a_bad_install_time_limit_ends_before_anything_reaches_the_cluster(self):
        for value in ("0", "481", "30s", "", "-5", "1e2", " 30", "3.5"):
            with self.subTest(value=value):
                self.reset_cluster()
                status, output = self.up(PREVIEW_INSTALL="npm ci", PREVIEW_INSTALL_SECONDS=value)
                self.assertEqual(status, 1, output)
                self.assertEqual(self.calls(), [], "the cluster was called")
                self.assertIn("PREVIEW_INSTALL_SECONDS", output.strip().splitlines()[-1])
        for value in ("1", "480"):
            with self.subTest(value=value):
                self.reset_cluster()
                status, output = self.up(PREVIEW_INSTALL="npm ci", PREVIEW_INSTALL_SECONDS=value)
                self.assertEqual(status, 0, output)

    def test_settings_are_checked_before_anything_reaches_the_cluster(self):
        for name, value in (("PREVIEW_PORT", "0"), ("PREVIEW_PORT", "http"), ("PREVIEW_READY_PATH", "health"),
                            ("PREVIEW_READY_PATH", "/a b"), ("PREVIEW_LIFETIME_HOURS", "0"),
                            ("PREVIEW_NET_IMAGE", "registry.example.com/net:latest"), ("PREVIEW_MEMORY", "lots"),
                            ("PREVIEW_DOMAIN", "Preview.Example.com"), ("PREVIEW_K8S_SERVER", "http://cluster.example.com"),
                            ("PREVIEW_ENV_FILE", "../outside/environ"), ("PREVIEW_ENV_FILE", "/etc/hosts"),
                            ("PREVIEW_START", None), ("PREVIEW_IMAGE", None), ("TASK_PREVIEW", None),
                            ("PREVIEW_K8S_CA_FILE", str(self.base / "missing.crt"))):
            with self.subTest(name=name, value=value):
                self.reset_cluster()
                status, output = self.up(**{name: value})
                self.assertEqual(status, 1, output)
                self.assertEqual(self.calls(), [])
                self.assertIn(name, output.strip().splitlines()[-1])

    def test_a_different_change_or_setting_gets_a_new_identifier(self):
        status, output = self.up()
        self.assertEqual(status, 0, output)
        first = self.applied_deployments()[-1]["spec"]["template"]["metadata"]["annotations"]["preview-change"]
        status, output = self.up(PREVIEW_START="python3 -m http.server \"$PORT\" --directory site")
        self.assertEqual(status, 0, output)
        second = self.applied_deployments()[-1]["spec"]["template"]["metadata"]["annotations"]["preview-change"]
        self.assertNotEqual(first, second, "changing only PREVIEW_START kept the identifier")
        (self.checkout / "site" / "index.html").write_text("<h1>ようこそ</h1>\n")
        status, output = self.up(PREVIEW_START="python3 -m http.server \"$PORT\" --directory site")
        self.assertEqual(status, 0, output)
        third = self.applied_deployments()[-1]["spec"]["template"]["metadata"]["annotations"]["preview-change"]
        self.assertNotEqual(second, third, "changing only the checkout kept the identifier")
        self.assertEqual(len(self.streams()), 3, "every new change has to be handed over")

    def test_the_same_change_again_only_extends_the_lifetime(self):
        status, output = self.up()
        self.assertEqual(status, 0, output)
        first = self.state()["deployments"]["pv-sample-12"]
        self.assertEqual(len(self.streams()), 1)
        applied = len(self.applied_deployments())
        time.sleep(1.1)
        status, output = self.up()
        self.assertEqual(status, 0, output)
        self.assertEqual(len(self.streams()), 1, "the checkout was handed over again for the same change")
        self.assertEqual(len(self.applied_deployments()), applied, "the Deployment was applied again")
        self.assertIn("annotate", self.verbs())
        second = self.state()["deployments"]["pv-sample-12"]
        self.assertEqual(first["spec"], second["spec"])
        self.assertGreater(second["metadata"]["annotations"]["preview-expires-at"],
                           first["metadata"]["annotations"]["preview-expires-at"])
        self.assertTrue((self.home / "task-preview.txt").exists())

    def test_the_result_file_has_the_url_the_end_and_the_identifier(self):
        status, output = self.up()
        self.assertEqual(status, 0, output)
        lines = (self.home / "task-preview.txt").read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 3)
        self.assertEqual(lines[0], "https://sample-12.preview.example.com/")
        self.assertRegex(lines[1], r"^この画面は \d{4}-\d{2}-\d{2} \d{2}:\d{2} \(日本時間\) まで開けます$")
        change = self.applied_deployments()[-1]["spec"]["template"]["metadata"]["annotations"]["preview-change"]
        self.assertEqual(lines[2], "動いている変更の識別子: " + change)
        expires = self.applied_deployments()[-1]["metadata"]["annotations"]["preview-expires-at"]
        local = datetime.datetime.strptime(expires, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=datetime.timezone.utc).astimezone(datetime.timezone(datetime.timedelta(hours=9)))
        self.assertIn(local.strftime("%Y-%m-%d %H:%M"), lines[1])
        self.assertEqual(self.checks, [("https://sample-12.preview.example.com/", CHECK_AUTH)])
        self.assertNoLeak((self.home / "task-preview.txt").read_text(), "the result file")

    def test_the_check_uses_the_ready_path_on_the_public_url(self):
        status, output = self.up(PREVIEW_READY_PATH="/health?full=1")
        self.assertEqual(status, 0, output)
        self.assertEqual(self.checks, [("https://sample-12.preview.example.com/health?full=1", CHECK_AUTH)])

    # -- failures --------------------------------------------------------

    def test_a_failed_public_check_ends_1_and_leaves_the_preview_for_the_cleanup(self):
        def failing(url, auth, **_):
            raise self.tool.Failure("the router answered 502: the application is not reachable behind it")
        status, output = self.up(check=failing)
        self.assertEqual(status, 1)
        self.assertEqual(output.strip().splitlines()[-1],
                         "the router answered 502: the application is not reachable behind it")
        self.assertNotIn("delete", self.verbs())
        self.assertFalse((self.home / "task-preview.txt").exists())

    def test_a_failed_install_removes_the_deployment(self):
        for scenario in ("install_fails", "install_restarts"):
            with self.subTest(scenario=scenario):
                self.reset_cluster()
                self.scenario({"pod": scenario})
                started = time.monotonic()
                status, output = self.up(PREVIEW_INSTALL="npm ci", PREVIEW_INSTALL_SECONDS="60",
                                         PREVIEW_WAIT_SECONDS="20")
                self.assertEqual(status, 1, output)
                self.assertLess(time.monotonic() - started, 30)
                deletes = [call["argv"] for call in self.calls() if call["verb"] == "delete"]
                self.assertEqual(len(deletes), 1)
                self.assertIn("deployment", deletes[0][deletes[0].index("delete") + 1])
                self.assertIn("preview=pv-sample-12", deletes[0])
                self.assertNotIn("pv-sample-12", self.state()["deployments"])
                last = output.strip().splitlines()[-1]
                self.assertIn("PREVIEW_INSTALL", last)
                self.assertIn("137", last)
                self.assertFalse((self.home / "task-preview.txt").exists())

    def test_a_refusal_by_the_policy_is_named_in_the_last_line(self):
        refusal = ("Error from server (Forbidden): error when creating \"STDIN\": deployments.apps \"pv-sample-12\" "
                   "is forbidden: ValidatingAdmissionPolicy 'preview-deployment-shape' with binding "
                   "'preview-deployment-shape' denied request: install must be timeout -s KILL <1-480> sh -c")
        self.scenario({"apply_error": refusal})
        status, output = self.up()
        self.assertEqual(status, 1)
        self.assertIn("ValidatingAdmissionPolicy 'preview-deployment-shape'", output.strip().splitlines()[-1])
        self.reset_cluster()
        self.scenario({"pod": "failed_create"})
        started = time.monotonic()
        status, output = self.up(PREVIEW_WAIT_SECONDS="20")
        self.assertEqual(status, 1)
        self.assertLess(time.monotonic() - started, 30, "a Pod the policy refused was waited for")
        self.assertIn("ValidatingAdmissionPolicy 'preview-pod-shape'", output.strip().splitlines()[-1])
        self.assertEqual(self.streams(), [])

    def test_a_preview_that_never_gets_ready_ends_at_the_wait_limit(self):
        for scenario, word in (("never_ready", "ready"), ("crash", "CrashLoopBackOff")):
            with self.subTest(scenario=scenario):
                self.reset_cluster()
                self.scenario({"pod": scenario})
                started = time.monotonic()
                status, output = self.up(PREVIEW_WAIT_SECONDS="3")
                self.assertEqual(status, 1, output)
                self.assertLess(time.monotonic() - started, 30)
                self.assertIn(word, output.strip().splitlines()[-1])

    # -- the settings file in the checkout -------------------------------

    def test_the_settings_file_becomes_the_application_environment(self):
        (self.checkout / ".env.preview").write_text("GREETING=hello world\n# a comment\n\nPORT=9999\nEMPTY=\nURL=a=b\n")
        status, output = self.up(PREVIEW_ENV_FILE=".env.preview")
        self.assertEqual(status, 0, output)
        pod = self.applied_deployments()[-1]["spec"]["template"]["spec"]
        environment = {item["name"]: item["value"] for item in pod["containers"][0]["env"]}
        self.assertEqual(environment["GREETING"], "hello world")
        self.assertEqual(environment["EMPTY"], "")
        self.assertEqual(environment["URL"], "a=b")
        self.assertEqual(environment["PORT"], "8080", "the script's PORT has to win")

    def test_an_unreadable_settings_file_ends_1_before_the_cluster(self):
        for content in (b"NOT A SETTING\n", b"1KEY=value\n", b"KEY=" + b"x" * (64 * 1024), b"KEY=\xff\n"):
            with self.subTest(content=content[:20]):
                self.reset_cluster()
                (self.checkout / ".env.preview").write_bytes(content)
                status, output = self.up(PREVIEW_ENV_FILE=".env.preview")
                self.assertEqual(status, 1, output)
                self.assertEqual(self.calls(), [])
                self.assertIn("PREVIEW_ENV_FILE", output.strip().splitlines()[-1])
        (self.checkout / ".env.preview").write_bytes(b"KEY=" + b"x" * (64 * 1024 - 5) + b"\n")
        self.reset_cluster()
        status, output = self.up(PREVIEW_ENV_FILE=".env.preview")
        self.assertEqual(status, 0, "a file of exactly 64 KiB is allowed: " + output)
        self.reset_cluster()
        status, output = self.up(PREVIEW_ENV_FILE="missing.env")
        self.assertEqual(status, 1, output)

    def test_a_linked_settings_file_is_not_read(self):
        """9.2: the settings file as a link to /proc/self/environ, and under a
        linked directory. The control reads through the link and shows the
        token in the Deployment."""
        os.remove(self.checkout / ".env.preview")
        os.symlink("/proc/self/environ", self.checkout / ".env.preview")
        os.symlink(str(self.outside), self.checkout / "conf")
        for setting in (".env.preview", "conf/environ"):
            with self.subTest(setting=setting):
                self.reset_cluster()
                status, output = self.run_tool("up", environment=self.with_credentials(
                    self.environment(PREVIEW_ENV_FILE=setting)))
                self.assertEqual(status, 1, output)
                self.assertEqual(self.applied_deployments(), [], "a Deployment was sent")
                self.assertNotIn("apply", self.verbs())
                self.assertIn("PREVIEW_ENV_FILE", output.strip().splitlines()[-1])
                self.assertNoLeak(output, "the output")
        os.remove(self.checkout / ".env.preview")
        os.symlink(str(self.outside / "environ"), self.checkout / ".env.preview")
        following = self.mutated([("os.O_NOFOLLOW", "0"), ("follow_symlinks=False", "follow_symlinks=True")],
                                 "following")
        self.reset_cluster()
        self.scenario({"apply_error": "refused"})
        self.run_tool("up", tool=following, environment=self.with_credentials(
            self.environment(PREVIEW_ENV_FILE=".env.preview")))
        deployments = self.applied_deployments()
        self.assertEqual(len(deployments), 1, "the control did not get as far as the cluster")
        self.assertIn(TOKEN, json.dumps(deployments[0]), "the control read nothing through the link")

    # -- the checkout ----------------------------------------------------

    def test_the_archive_keeps_links_as_links_and_reads_nothing_through_them(self):
        """9.2: x -> /proc/self/environ, d -> an absolute directory (as d -> /
        would be), d2 -> ../.. style. d and d2 were real directories in the
        index first, so git still lists the paths under them."""
        for name in ("d", "d2"):
            (self.checkout / name).mkdir()
            (self.checkout / name / "inner.txt").write_text("tracked before the link\n")
        self.git("add", "-A")
        for name in ("d", "d2"):
            shutil.rmtree(self.checkout / name)
        os.symlink(str(self.outside), self.checkout / "d")
        os.symlink("../outside", self.checkout / "d2")
        os.symlink("/proc/self/environ", self.checkout / "x")
        os.symlink(str(self.outside / "environ"), self.checkout / "y")
        (self.checkout / "run.sh").write_text("#!/bin/sh\n")
        (self.checkout / "run.sh").chmod(0o755)
        environment = self.environment()

        def archive(tool):
            destination = self.base / ("%s.tar" % tool.__name__)
            paths = tool.checkout_paths(str(self.checkout), environment)
            self.assertIn("d/inner.txt", paths, "git no longer lists the path under the link; the case tests nothing")
            digest = tool.write_tar(str(self.checkout), paths, str(destination))
            self.assertRegex(digest, r"^[0-9a-f]{64}$")
            return destination

        destination = archive(self.tool)
        with tarfile.open(destination) as opened:
            members = {member.name: member for member in opened.getmembers()}
        self.assertTrue(members["x"].issym())
        self.assertEqual(members["x"].linkname, "/proc/self/environ")
        self.assertEqual(members["x"].size, 0)
        self.assertTrue(members["y"].issym())
        self.assertNotIn("d/inner.txt", members)
        self.assertNotIn("d2/inner.txt", members)
        self.assertTrue(members["site/index.html"].isfile())
        self.assertEqual(members["run.sh"].mode, 0o755)
        self.assertEqual(members["site/index.html"].mode, 0o644)
        self.assertNotIn(".git", {name.split("/")[0] for name in members})
        self.assertNotIn(TOKEN.encode(), destination.read_bytes())
        following = load_tool(self.mutated([("os.O_NOFOLLOW", "0"), ("follow_symlinks=False", "follow_symlinks=True")],
                                           "following"), "following")
        self.assertIn(TOKEN.encode(), archive(following).read_bytes(), "the control read nothing through the links")

    def test_the_same_checkout_gives_the_same_archive(self):
        paths = self.tool.checkout_paths(str(self.checkout), self.environment())
        first = self.tool.write_tar(str(self.checkout), paths, str(self.base / "one.tar"))
        time.sleep(1.1)
        os.utime(self.checkout / "site" / "index.html")
        second = self.tool.write_tar(str(self.checkout), paths, str(self.base / "two.tar"))
        self.assertEqual(first, second)

    # -- credentials -----------------------------------------------------

    def test_no_child_receives_a_credential_and_the_output_carries_none(self):
        self.scenario({"apply_error": "error: the server echoed a credential: @TOKEN@"})
        status, output = self.run_tool("up")
        self.assertEqual(status, 1, output)
        self.assertNoLeak(output, "the output")
        self.assertIn("[credential]", output, "the fake's echo did not reach the redaction")
        git_calls, kube_calls = self.git_calls(), self.calls()
        self.assertTrue(git_calls, "git was not called")
        self.assertIn("apply", [call["verb"] for call in kube_calls])
        for call in git_calls:
            self.assertNoLeak(json.dumps(call["environment"]), "git's environment")
            self.assertNotIn("KUBECONFIG", call["environment"])
            self.assertFalse(call["token_on_disk"], "the kubeconfig existed while git ran")
        for call in kube_calls:
            self.assertNoLeak(json.dumps(call["environment"]), "kubectl's environment")
            self.assertIn("KUBECONFIG", call["environment"])
            config = Path(call["kubeconfig"])
            self.assertTrue(config.is_relative_to(self.tmp), "the kubeconfig is not in the launch's /tmp")
            self.assertFalse(config.is_relative_to(self.home))
            self.assertEqual(call["kubeconfig_mode"], "0o600")
        self.assertLess(max(call["time"] for call in git_calls), min(call["time"] for call in kube_calls))
        self.assertEqual(self.files_holding(self.home, TOKEN), [])
        self.assertEqual(self.files_holding(self.tmp, TOKEN), [], "the kubeconfig was left after the launch")

    def test_the_running_script_holds_no_credential_in_its_environment(self):
        """9.2: after the restart the script's own starting environment (what
        /proc/self/environ shows) has no credential. The control skips the
        restart and shows the token there."""
        self.scenario({"apply_error": "refused"})
        status, output = self.run_tool("up")
        self.assertEqual(status, 1, output)
        parents = [call["parent"] for call in self.git_calls() + self.calls()]
        self.assertTrue(parents and all(parents), "the parent's environment could not be read")
        for parent in parents:
            self.assertNoLeak(parent, "the running script's environment")
        staying = self.mutated([("os.execve(executable, arguments, dict(os.environ))",
                                 "return run(command, held, dict(os.environ))")], "staying")
        self.reset_cluster()
        status, output = self.run_tool("up", tool=staying)
        self.assertEqual(status, 1, output)
        self.assertTrue(any(TOKEN in call["parent"] for call in self.git_calls()),
                        "the control's environment showed no token")

    def test_the_kubeconfig_never_reaches_the_home_directory_even_when_killed(self):
        """9.2: kill the launch during apply, as the engine's time limit does.
        The control writes its kubeconfig under the home directory."""
        def killed(tool):
            self.reset_cluster()
            self.scenario({"apply_block": True})
            process = self.launch("up", tool=tool)
            self.addCleanup(lambda: process.poll() is None and os.killpg(process.pid, signal.SIGKILL))
            deadline = time.monotonic() + 60
            while not (self.fake / "apply-started").exists():
                self.assertIsNone(process.poll(), process.stdout.read() if process.poll() is not None else "")
                self.assertLess(time.monotonic(), deadline, "apply never started")
                time.sleep(0.05)
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=30)
            process.stdout.close()

        killed(TOOL)
        self.assertTrue(self.files_holding(self.tmp, TOKEN), "no kubeconfig had been written before apply")
        self.assertEqual(self.files_holding(self.home, TOKEN), [])
        killed(self.mutated([('environment.get("TMPDIR") or "/tmp"', 'environment["TASK_HOME"]')], "homeward"))
        self.assertTrue(self.files_holding(self.home, TOKEN), "the control left nothing in the home directory")

    # -- the public check ------------------------------------------------

    def serve(self, anonymous, authorized):
        """A local stand-in for the router: one status without credentials,
        another with the check user's."""
        expected = "Basic " + CHECK_HEADER

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                code = authorized if self.headers.get("Authorization") == expected else anonymous
                self.send_response(code)
                if 300 <= code < 400:
                    self.send_header("Location", "http://127.0.0.1:9/elsewhere")
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *arguments):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return "http://127.0.0.1:%d/" % server.server_address[1]

    def test_the_public_check_needs_401_without_and_an_answer_with_credentials(self):
        for anonymous, authorized, passes, word in (
                (401, 200, True, ""), (401, 204, True, ""), (401, 302, True, ""),
                (200, 200, False, "without credentials"), (403, 200, False, "without credentials"),
                (401, 404, False, "404"), (401, 502, False, "502"), (401, 503, False, "503"),
                (401, 401, False, "PREVIEW_CHECK_AUTH"), (401, 500, False, "500")):
            with self.subTest(anonymous=anonymous, authorized=authorized):
                url = self.serve(anonymous, authorized)
                if passes:
                    self.tool.check_public_url(url, CHECK_AUTH, seconds=0.5, pause=0.05)
                    continue
                with self.assertRaises(self.tool.Failure) as caught:
                    self.tool.check_public_url(url, CHECK_AUTH, seconds=0.5, pause=0.05)
                self.assertIn(word, str(caught.exception))
                self.assertNoLeak(str(caught.exception), "the check's reason")

    def test_the_public_check_waits_for_the_router_to_find_the_application(self):
        answers = [502, 503, 200]
        expected = "Basic " + CHECK_HEADER

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                if self.headers.get("Authorization") != expected:
                    self.send_response(401)
                else:
                    self.send_response(answers.pop(0) if len(answers) > 1 else answers[0])
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *arguments):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.tool.check_public_url("http://127.0.0.1:%d/" % server.server_address[1], CHECK_AUTH,
                                   seconds=10, pause=0.05)
        self.assertEqual(answers, [200])

    # -- down and the exit statuses --------------------------------------

    def test_down_removes_the_deployment_and_the_service_and_can_run_again(self):
        status, output = self.up()
        self.assertEqual(status, 0, output)
        self.assertIn("pv-sample-12", self.state()["services"])
        for attempt in range(2):
            with self.subTest(attempt=attempt):
                status, output = self.down()
                self.assertEqual(status, 0, output)
                self.assertEqual(self.state()["deployments"], {})
                self.assertEqual(self.state()["services"], {})
        self.scenario({"delete_stuck": True})
        status, output = self.up()
        self.assertEqual(status, 0, output)
        status, output = self.down(PREVIEW_WAIT_SECONDS="2")
        self.assertEqual(status, 1, output)
        self.assertIn("pv-sample-12", output.strip().splitlines()[-1])

    def test_down_needs_neither_the_checkout_nor_the_application_settings(self):
        status, output = self.up()
        self.assertEqual(status, 0, output)
        self.reset_cluster()
        output = io.StringIO()
        environment = {key: value for key, value in self.environment().items()
                       if not key.startswith("PREVIEW_") or key in (
                           "PREVIEW_NAME", "PREVIEW_NAMESPACE", "PREVIEW_K8S_SERVER", "PREVIEW_K8S_CA_FILE")}
        environment.pop("TASK_PREVIEW")
        environment["TASK_WORKSPACE"] = str(self.base / "gone")
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            status = self.tool.down(environment, {"PREVIEW_K8S_TOKEN": TOKEN})
        self.assertEqual(status, 0, output.getvalue())
        self.assertEqual(self.git_calls(), [])

    def test_exit_statuses(self):
        cases = [
            ("no command", [], None, 2),
            ("an unknown command", ["start"], None, 2),
            ("up without the cluster token", ["up"], dict(self.with_credentials(self.environment()), PREVIEW_K8S_TOKEN=""), 1),
            ("up without the check user", ["up"], {key: value for key, value in self.with_credentials(
                self.environment()).items() if key != "PREVIEW_CHECK_AUTH"}, 1),
            ("up refused by the cluster", ["up"], None, 1),
            ("down", ["down"], None, 0),
            ("down again", ["down"], None, 0),
        ]
        self.scenario({"apply_error": "refused"})
        for label, arguments, environment, expected in cases:
            with self.subTest(label):
                status, output = self.run_tool(*arguments, environment=environment)
                self.assertEqual(status, expected, output)
                self.assertNoLeak(output, "the output")
                if expected == 1:
                    self.assertTrue(output.strip(), "a failure without a reason")


if __name__ == "__main__":
    unittest.main()
