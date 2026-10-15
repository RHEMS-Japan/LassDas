"""Check the one-time manifests of the Kubernetes preview example
(rewrite/examples/preview-k8s) against the commands that rely on them, run the
cleanup script against a fake kubectl, and run the policy check's tool against
a fake one. Nothing here reaches a cluster.

The manifests are read as text, the way the module's other template checks
read theirs: no YAML library is needed or added.
"""
import contextlib
import datetime
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

EXAMPLE = Path(__file__).resolve().parents[1] / "examples" / "preview-k8s"
# The njs command line, to run the router's script. CI has none, and those
# tests are skipped there; the README says how they were run.
NJS = os.environ.get("PREVIEW_NJS") or shutil.which("njs")
SIGNING_KEY = "fake-router-signing-key-6a5b4c3d2e1f0a9b8c7d6e5f"
MANIFESTS = sorted(EXAMPLE.glob("*.yaml.example"))
PLACEHOLDER = re.compile(r"<([a-z][a-z0-9-]*)>")
NET_IMAGE = "registry.example.com/net@sha256:" + "0" * 64


def load(name):
    specification = importlib.util.spec_from_file_location(name + "_under_test", EXAMPLE / (name + ".py"))
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


def documents(path):
    return [part for part in re.split(r"(?m)^---$", path.read_text()) if re.search(r"(?m)^kind: ", part)]


def kind_of(document):
    return re.search(r"(?m)^kind: (\S+)", document).group(1)


def name_of(document):
    return re.search(r"(?m)^  name: (\S+)", document).group(1)


def cel_list(items):
    return "[" + ", ".join("'%s'" % item for item in items) + "]"


def rules(path, key):
    """The lines of one rules file held in a ConfigMap."""
    text = path.read_text()
    start = text.index("  %s: |\n" % key) + len("  %s: |\n" % key)
    lines = []
    for line in text[start:].splitlines():
        if not line.startswith("    "):
            break
        lines.append(line.strip())
    return lines


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.preview = load("preview")
        self.policy = (EXAMPLE / "admission-policy.yaml.example").read_text()
        self.readme = (EXAMPLE / "README.md").read_text()

    def built(self, install):
        settings = self.preview.Settings(
            workspace="/unused", result="/unused", issue="1", site="sample", start="sleep 1", port=8080,
            ready_path="/", lifetime=1, image="registry.example.com/app:1", memory="64Mi",
            install="true" if install else None, install_seconds=60, env_file=None, domain="preview.example.invalid",
            namespace="preview-apps", server="https://unused", ca_file="/unused", net_image=NET_IMAGE,
            wait_seconds=60)
        names = self.preview.preview_names("sample", "1", "preview.example.invalid")
        return self.preview.manifests(settings, names, [], datetime.datetime.now(datetime.timezone.utc))

    def test_every_placeholder_is_explained_and_used(self):
        table = self.readme[self.readme.index("## Values a person fills in"):]
        table = table[:table.index("\n## ", 1)]
        explained = set(re.findall(r"^\| `<([a-z][a-z0-9-]*)>` \|", table, re.M))
        used = set()
        for path in MANIFESTS:
            # Values only: not the comments, not the lines of the page the
            # router serves (each starts with an HTML tag), not nginx's
            # named groups (?<name>).
            for line in path.read_text().splitlines():
                if not line.lstrip().startswith(("#", "<")):
                    used |= set(re.findall(r"(?<!\?)<([a-z][a-z0-9-]*)>", line))
        self.assertTrue(used, "no placeholder was found; the check reads nothing")
        self.assertEqual(used - explained, set(), "a placeholder of the manifests is not in the README's table")
        self.assertEqual(explained - used - {"region", "api-server"}, set(), "the table explains an unused value")

    def test_the_policy_accepts_exactly_the_commands_and_the_order_the_script_sends(self):
        commands = self.preview.NET_COMMANDS
        self.assertIn("name: runRules\n      expression: \"%s\"" % cel_list(commands["net"]), self.policy)
        self.assertIn("name: installRules\n      expression: \"%s\"" % cel_list(commands["net-install"]), self.policy)
        self.assertEqual(commands["net-hold"], commands["net"], "the policy gives net-hold the running rules")
        for install in (False, True):
            deployment, _ = self.built(install)
            order = [container["name"] for container in deployment["spec"]["template"]["spec"]["initContainers"]]
            self.assertIn("variables.initNames == %s" % cel_list(order), self.policy)
            if install:
                command = deployment["spec"]["template"]["spec"]["initContainers"][3]["command"]
                self.assertEqual(command[:3] + command[4:6], ["timeout", "-s", "KILL", "sh", "-c"])
        self.assertIn("int(c.command[3]) >= 1 && int(c.command[3]) <= 480", self.policy)
        self.assertIn("c.command[2] == 'KILL'", self.policy)
        self.assertIn("v.configMap.name == '%s'" % self.preview.EGRESS_CONFIGMAP, self.policy)
        router = (EXAMPLE / "router.yaml.example").read_text()
        self.assertIn('command: ["iptables-restore", "--wait", "5", "/policy/router.rules"]', router,
                      "the router's rules container has to use the previews' program (README, step 4)")
        self.assertEqual(commands["net"][:3], ["iptables-restore", "--wait", "5"])

    def test_the_deployment_and_the_pod_policies_share_their_validations(self):
        policies = {name_of(part): part for part in documents(EXAMPLE / "admission-policy.yaml.example")
                    if kind_of(part) == "ValidatingAdmissionPolicy"}
        self.assertEqual(set(policies), {"preview-deployment-shape", "preview-pod-shape", "preview-service-shape"})

        def shared(text):
            body = text[text.index("    - name: checkNodeName"):]
            return body[body.index("\n    - name: ", 1):]

        self.assertEqual(shared(policies["preview-deployment-shape"]), shared(policies["preview-pod-shape"]))
        self.assertIn('expression: "object.spec.template.spec"', policies["preview-deployment-shape"])
        self.assertIn('expression: "object.spec"', policies["preview-pod-shape"])
        self.assertIn('resources: ["pods", "pods/ephemeralcontainers"]', policies["preview-pod-shape"])
        for name, part in policies.items():
            self.assertIn("failurePolicy: Fail", part, name)
        bindings = [part for part in documents(EXAMPLE / "admission-policy.yaml.example")
                    if kind_of(part) == "ValidatingAdmissionPolicyBinding"]
        self.assertEqual(sorted(re.search(r"policyName: (\S+)", part).group(1) for part in bindings),
                         sorted(policies))
        for part in bindings:
            self.assertIn("validationActions: [Deny]", part)
            self.assertIn("kubernetes.io/metadata.name: <preview-namespace>", part)

    def test_only_the_quota_the_limits_the_rules_and_the_role_are_put_in_the_preview_namespace(self):
        found = []
        for path in MANIFESTS:
            for part in documents(path):
                match = re.search(r"(?m)^  namespace: (\S+)", part)
                if match and match.group(1) == "<preview-namespace>":
                    found.append((kind_of(part), name_of(part)))
        self.assertTrue(found)
        for kind, name in found:
            self.assertIn(kind, ("ResourceQuota", "LimitRange", "ConfigMap", "Role", "RoleBinding"), name)
            if kind == "ConfigMap":
                self.assertEqual(name, "preview-egress")
        for path in MANIFESTS:
            for part in documents(path):
                if kind_of(part) == "Secret":
                    self.assertIn("namespace: <router-namespace>", part)

    def test_the_router_needs_the_password_and_the_signed_url(self):
        router = (EXAMPLE / "router.yaml.example").read_text()
        config = router[router.index("  nginx.conf: |"):router.index("  ended.html: |")]
        self.assertIn("load_module /etc/nginx/modules/ngx_http_js_module.so;", config)
        self.assertIn("env PREVIEW_SIGNING_KEY;", config)
        self.assertIn("js_import preview from preview-router.js;", config)
        self.assertIn("location = /_preview/open { error_log stderr crit; js_content preview.open; }", config,
                      "a mistyped password would log the request line, with its token")
        self.assertIn("location = /_preview/check { internal; js_content preview.check; }", config)
        self.assertIn("location ^~ /_preview/ { return 404; }", config)
        preview_server = config[config.index("server_name *.<preview-domain>;"):]
        self.assertIn("auth_basic_user_file", preview_server)
        self.assertNotIn("satisfy any", config, "the password and the signed URL are both required")
        location = preview_server[preview_server.index("location / {"):]
        self.assertIn("auth_request /_preview/check;", location)
        self.assertIn('proxy_set_header Authorization "";', location)
        self.assertIn("proxy_set_header Cookie $preview_other_cookies;", location)
        self.assertIn("js_header_filter preview.filterCookies;", location)
        self.assertIn("proxy_cookie_domain ~.+ $host;", location,
                      "the application's cookies have to be bound to the preview's host")
        self.assertIn("js_set $preview_other_cookies preview.otherCookies;", config)
        self.assertIn("secretKeyRef: { name: preview-router-signing, key: key }", router)
        self.assertIn("name: preview-router-js", router)

    def test_the_router_logs_no_query_and_no_referer(self):
        router = (EXAMPLE / "router.yaml.example").read_text()
        config = router[router.index("  nginx.conf: |"):router.index("  ended.html: |")]
        formats = re.findall(r"log_format (\S+) ((?:'[^']*'\s*)+);", config)
        self.assertEqual([name for name, _ in formats], ["preview"])
        written = formats[0][1]
        self.assertIn("$uri", written)
        for variable in ("$request ", "$request\"", "$request_uri", "$args", "$query_string", "$arg_", "$http_referer",
                         "$http_cookie", "$cookie_"):
            self.assertNotIn(variable, written)
        logs = re.findall(r"access_log ([^;]+);", config)
        self.assertTrue(logs)
        for line in logs:
            self.assertTrue(line.strip() == "off" or line.split()[-1] == "preview", line)

    def test_the_router_drops_the_password_and_makes_no_dns_record(self):
        router = (EXAMPLE / "router.yaml.example").read_text()
        self.assertIn('proxy_set_header Authorization "";', router)
        self.assertIn("auth_basic_user_file /etc/preview-router/auth/htpasswd;", router)
        self.assertIn("proxy_pass http://pv-$preview_label.<preview-namespace>.svc.cluster.local;", router)
        code = "\n".join(line for line in router.splitlines() if not line.lstrip().startswith("#"))
        self.assertNotIn("external-dns", code)
        ingress = next(part for part in documents(EXAMPLE / "router.yaml.example") if kind_of(part) == "Ingress")
        annotations = ingress[ingress.index("  annotations:\n"):ingress.index("\nspec:")].splitlines()[1:]
        self.assertTrue(annotations)
        for line in annotations:
            self.assertTrue(line.strip().startswith("alb.ingress.kubernetes.io/"), line)
        self.assertIn('- host: "*.<preview-domain>"', ingress)

    def test_the_egress_rules(self):
        egress = EXAMPLE / "preview-namespace.yaml.example"
        running = rules(egress, "run.rules")
        self.assertEqual([line for line in running if line.startswith("-A")],
                         ["-A OUTPUT -o lo -j ACCEPT", "-A OUTPUT -p tcp ! --syn -j ACCEPT", "-A OUTPUT -j REJECT"])
        installing = [line for line in rules(egress, "install.rules") if line.startswith("-A")]
        self.assertEqual(installing[-1], "-A OUTPUT -j ACCEPT")
        refused = [line.split()[3] for line in installing if line.endswith("-j REJECT")]
        for block in ("<service-cidr>", "10.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
                      "100.64.0.0/10", "127.0.0.0/8"):
            self.assertIn(block, refused)
        accepted = [line for line in installing[:-1] if line.endswith("-j ACCEPT")]
        self.assertEqual(accepted, ["-A OUTPUT -o lo -j ACCEPT",
                                    "-A OUTPUT -d <cluster-dns-ip> -p udp --dport 53 -j ACCEPT",
                                    "-A OUTPUT -d <cluster-dns-ip> -p tcp --dport 53 -j ACCEPT"])
        router = [line for line in rules(EXAMPLE / "router.yaml.example", "router.rules") if line.startswith("-A")]
        self.assertEqual(router, ["-A OUTPUT -o lo -j ACCEPT", "-A OUTPUT -p tcp --sport 8080 ! --syn -j ACCEPT",
                                  "-A OUTPUT -d <cluster-dns-ip> -p udp --dport 53 -j ACCEPT",
                                  "-A OUTPUT -d <cluster-dns-ip> -p tcp --dport 53 -j ACCEPT",
                                  "-A OUTPUT -d <service-cidr> -p tcp --dport 80 -j ACCEPT",
                                  "-A OUTPUT -j REJECT"])
        for path in (egress, EXAMPLE / "router.yaml.example"):
            self.assertNotIn("--noflush", path.read_text(), "a later rules file has to replace the earlier one")

    def test_the_preview_identity_gets_no_pods_secrets_or_cluster_wide_rights(self):
        access = documents(EXAMPLE / "access.yaml.example")
        role = next(part for part in access if kind_of(part) == "Role" and name_of(part) == "preview-operator")
        resources = re.findall(r'resources: \["([^"]+)"\]', role)
        self.assertEqual(sorted(resources), sorted(["deployments", "services", "pods", "pods/exec", "pods/log",
                                                    "replicasets", "endpointslices", "events"]))
        pods = role[role.index('resources: ["pods"]'):]
        self.assertTrue(pods.split("\n")[1].strip() == 'verbs: ["get", "list"]', "pods may only be read")
        self.assertIn('resources: ["pods/exec"]\n    verbs: ["create"]', role)
        for part in access:
            self.assertNotIn(kind_of(part), ("ClusterRole", "ClusterRoleBinding"))
        account = next(part for part in access if kind_of(part) == "ServiceAccount"
                       and name_of(part) == "preview-operator")
        self.assertIn("automountServiceAccountToken: false", account)


class CleanupTests(unittest.TestCase):
    """cleanup.sh with a fake kubectl that answers its three reads from files
    and records its deletions, and a fake date at a fixed moment. The
    jsonpath of the reads is the cluster's to evaluate; these tests check what
    the script does with the lines."""

    NOW = 1_800_000_000  # 2027-01-15T08:00:00Z

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="preview-cleanup-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        fake = self.base / "bin"
        fake.mkdir()
        (fake / "kubectl").write_text("#!%s\n" % sys.executable + r'''
import sys
from pathlib import Path
HERE = Path(__file__).resolve().parent.parent
arguments = sys.argv[1:]
with open(HERE / "calls.txt", "a") as handle:
    handle.write(" ".join(arguments) + "\n")
verb = next(argument for argument in arguments if not argument.startswith("-") and argument != "preview-apps")
if verb == "get":
    kind = arguments[arguments.index("get") + 1]
    path = HERE / (kind + ".txt")
    sys.stdout.write(path.read_text() if path.exists() else "")
elif verb == "delete":
    name = arguments[arguments.index("-l") + 1].split("=", 1)[1]
    failing = HERE / "failing.txt"
    if failing.exists() and name in failing.read_text().split():
        print("Error from server: refused", file=sys.stderr)
        sys.exit(1)
    with open(HERE / "deleted.txt", "a") as handle:
        handle.write(arguments[arguments.index("delete") + 1] + " " + name + "\n")
else:
    sys.exit("fake kubectl: unexpected %r" % arguments)
''')
        (fake / "date").write_text("#!%s\n" % sys.executable + r'''
import sys, time
NOW = %d
arguments = sys.argv[1:]
if arguments == ["+%%s"]:
    print(NOW)
elif arguments[:2] == ["-u", "-d"] and arguments[2].startswith("@"):
    print(time.strftime(arguments[3][1:], time.gmtime(int(arguments[2][1:]))))
else:
    sys.exit("fake date: unexpected %%r" %% arguments)
''' % self.NOW)
        for path in fake.iterdir():
            path.chmod(0o755)
        self.fake = fake

    def stamp(self, offset):
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(self.NOW + offset))

    def run_cleanup(self, deployments, services, pods, failing=()):
        (self.base / "deployments.txt").write_text("".join("|".join(row) + "\n" for row in deployments))
        (self.base / "services.txt").write_text("".join(name + "\n" for name in services))
        (self.base / "pods.txt").write_text("".join("|".join(row) + "\n" for row in pods))
        if failing:
            (self.base / "failing.txt").write_text(" ".join(failing))
        environment = {"PATH": str(self.fake) + os.pathsep + os.environ.get("PATH", os.defpath),
                       "PREVIEW_NAMESPACE": "preview-apps", "LANG": "C"}
        done = subprocess.run(["/bin/sh", str(EXAMPLE / "cleanup.sh")], env=environment, capture_output=True,
                              text=True, timeout=60)
        deleted = (self.base / "deleted.txt").read_text().split("\n") if (self.base / "deleted.txt").exists() else []
        return done, sorted(line.split()[1] for line in deleted if line)

    def test_it_removes_what_is_past_its_lifetime_never_ready_or_restarted(self):
        hour, minute = 3600, 60
        deployments = [
            ("pv-fresh", self.stamp(hour), "True", self.stamp(-2 * hour), self.stamp(-3 * hour)),
            ("pv-expired", self.stamp(-1), "True", self.stamp(-2 * hour), self.stamp(-3 * hour)),
            ("pv-unreadable", "soon", "True", "", self.stamp(-hour)),
            ("pv-no-lifetime", "", "True", "", self.stamp(-hour)),
            ("pv-stale", self.stamp(hour), "False", self.stamp(-31 * minute), self.stamp(-hour)),
            ("pv-recent", self.stamp(hour), "False", self.stamp(-29 * minute), self.stamp(-hour)),
            ("pv-never", self.stamp(hour), "", "", self.stamp(-31 * minute)),
            ("pv-new", self.stamp(hour), "", "", self.stamp(-5 * minute)),
            ("pv-restarted", self.stamp(hour), "False", self.stamp(-minute), self.stamp(-2 * minute)),
            ("pv-installing", self.stamp(hour), "False", self.stamp(-minute), self.stamp(-2 * minute)),
        ]
        pods = [("pv-restarted", "1"), ("pv-installing", "0"), ("pv-fresh", "")]
        services = ["pv-fresh", "pv-expired", "pv-orphan", "pv-recent"]
        done, removed = self.run_cleanup(deployments, services, pods)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(removed, sorted(["pv-expired", "pv-unreadable", "pv-no-lifetime", "pv-stale", "pv-never",
                                          "pv-restarted", "pv-orphan"]))
        calls = (self.base / "calls.txt").read_text()
        for line in calls.splitlines():
            self.assertIn("--namespace preview-apps", line)
        for line in calls.splitlines():
            if " delete " in " %s " % line:
                self.assertIn("deployment,service", line)
                self.assertIn("--wait=false", line)
        self.assertIn("removed", done.stdout.splitlines()[-1])

    def test_a_removal_that_fails_ends_1_and_the_rest_go_on(self):
        deployments = [("pv-a", self.stamp(-1), "True", "", ""), ("pv-b", self.stamp(-1), "True", "", "")]
        done, removed = self.run_cleanup(deployments, [], [], failing=("pv-a",))
        self.assertEqual(done.returncode, 1)
        self.assertEqual(removed, ["pv-b"])

    def test_nothing_to_remove(self):
        done, removed = self.run_cleanup([], [], [])
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(removed, [])
        self.assertIn("nothing to remove", done.stdout)


class DryRunToolTests(unittest.TestCase):
    def setUp(self):
        self.tool = load("dry_run")
        temporary = tempfile.TemporaryDirectory(prefix="preview-dry-run-test-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)

    def test_the_readme_lists_every_case(self):
        readme = (EXAMPLE / "README.md").read_text()
        for case in self.tool.cases():
            self.assertIn("| %s |" % case.label, readme)

    def test_written_cases_are_preview_py_s_objects_with_one_change_each(self):
        with contextlib.redirect_stdout(io.StringIO()):
            status = self.tool.main(["--write", str(self.base / "cases"), "--namespace", "preview-apps",
                                     "--net-image", NET_IMAGE])
        self.assertEqual(status, 0)
        written = sorted((self.base / "cases").glob("*.json"))
        self.assertEqual(len(written), len(self.tool.cases()))
        preview = self.tool.load_preview()
        def timeless(item):
            # The lifetime is now + 1 hour, to the second: it moves while the
            # cases are written and built again.
            item = json.loads(json.dumps(item))
            item.get("metadata", {}).get("annotations", {}).pop("preview-expires-at", None)
            return json.dumps(item, sort_keys=True)

        unchanged = [timeless(self.tool.obj(preview, self.tool.Case("", kind, install, None, None, None),
                                            "preview-apps", NET_IMAGE))
                     for kind, install in (("deployment", False), ("deployment", True), ("service", False),
                                           ("pod", False))]
        for path in written:
            entry = json.loads(path.read_text())
            text = timeless(entry["object"])
            same = text in unchanged
            if entry["expect"] == "accepted":
                self.assertTrue(same, entry["case"])
            else:
                self.assertFalse(same, "%s sends the unchanged object" % entry["case"])

    def test_a_case_passes_only_when_its_own_policy_refuses_it_for_its_rule(self):
        case = self.tool.Case("x", "deployment", False, None, "preview-deployment-shape", "privileged")
        refused = ("The deployments \"pv\" is invalid: : ValidatingAdmissionPolicy 'preview-deployment-shape' with "
                   "binding 'preview-deployment-shape' denied request: privileged containers are not allowed")
        self.assertTrue(self.tool.judge(case, 1, refused)[0])
        self.assertFalse(self.tool.judge(case, 0, "deployment.apps/pv serverside-applied (server dry run)")[0])
        self.assertFalse(self.tool.judge(case, 1, refused.replace("privileged containers", "hostPort"))[0])
        self.assertFalse(self.tool.judge(case, 1, "Error from server (Forbidden): deployments.apps is forbidden: "
                                                  "User cannot create resource")[0])
        self.assertFalse(self.tool.judge(case, 1, refused.replace("preview-deployment-shape", "other"))[0])
        accepted = self.tool.Case("y", "deployment", False, None, None, None)
        self.assertTrue(self.tool.judge(accepted, 0, "deployment.apps/pv serverside-applied (server dry run)")[0])
        self.assertFalse(self.tool.judge(accepted, 1, refused)[0])

    def test_it_uses_only_the_named_kubeconfig_and_sends_only_dry_runs(self):
        fake = self.base / "bin"
        fake.mkdir()
        (fake / "kubectl").write_text("#!%s\n" % sys.executable + r'''
import json, os, sys
from pathlib import Path
with open(Path(__file__).resolve().parent.parent / "calls.jsonl", "a") as handle:
    handle.write(json.dumps({"argv": sys.argv[1:], "KUBECONFIG": os.environ.get("KUBECONFIG")}) + "\n")
sys.stdin.read()
print("yes" if "can-i" in sys.argv else "applied (server dry run)")
''')
        (fake / "kubectl").chmod(0o755)
        environment = dict(os.environ, PATH=str(fake) + os.pathsep + os.environ.get("PATH", ""),
                           KUBECONFIG=str(self.base / "the-callers-own"))
        done = subprocess.run([sys.executable, "-B", str(EXAMPLE / "dry_run.py"), "--kubeconfig",
                               str(self.base / "operator.kubeconfig"), "--context", "preview",
                               "--admin-kubeconfig", str(self.base / "admin.kubeconfig"), "--admin-context", "admin",
                               "--namespace", "preview-apps", "--net-image", NET_IMAGE, "--settle-seconds", "2"],
                              env=environment, capture_output=True, text=True, timeout=300)
        self.assertEqual(done.returncode, 1, "a cluster that accepts everything has to fail the check")
        self.assertIn("FAIL  a privileged container: accepted, but the policy has to refuse it", done.stdout)
        self.assertIn("PASS  preview.py's Deployment without the dependency step", done.stdout)
        self.assertIn("the policy did not refuse a nodeName within 2 seconds", done.stdout)
        calls = [json.loads(line) for line in (self.base / "calls.jsonl").read_text().splitlines()]
        self.assertGreater(len(calls), len(self.tool.cases()))
        for call in calls:
            argv = call["argv"]
            self.assertEqual(argv[0], "--kubeconfig")
            self.assertIn(argv[1], (str(self.base / "operator.kubeconfig"), str(self.base / "admin.kubeconfig")))
            self.assertEqual(argv[2], "--context")
            self.assertEqual(call["KUBECONFIG"], argv[1], "the caller's KUBECONFIG reached kubectl")
            verb = argv[6]
            self.assertIn(verb, ("apply", "auth"))
            if verb == "apply":
                self.assertIn("--dry-run=server", argv)
            else:
                self.assertEqual(argv[7], "can-i")



def njs_request(host, args=None, cookie=None):
    return ("request(%s, %s, %s)" % (json.dumps(host), json.dumps(args or {}),
                                    "undefined" if cookie is None else json.dumps(cookie)))


@unittest.skipUnless(NJS, "the njs command line is not installed (set PREVIEW_NJS); the router's script is not run")
class RouterScriptTests(unittest.TestCase):
    """preview-router.js run by the njs command line with a stand-in for
    nginx's request object. The tokens are made by preview.py's sign(): the
    router has to take what the preview command writes."""

    HOST = "sample-12.preview.example.invalid"

    def setUp(self):
        self.preview = load("preview")
        temporary = tempfile.TemporaryDirectory(prefix="preview-njs-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)

    def token(self, end, host=None, key=SIGNING_KEY):
        return "%d.%s" % (end, self.preview.sign(key, host or self.HOST, end))

    def run_script(self, body, key=SIGNING_KEY):
        harness = self.base / "harness.js"
        harness.write_text("""import preview from 'preview-router.js';
function request(host, args, cookie) {
    var r = {variables: {host: host}, args: args, headersIn: {}, headersOut: {}, status: 0, location: null};
    r.return = function (code, text) { r.status = code; r.location = text === undefined ? null : text; };
    if (cookie !== undefined) { r.headersIn.Cookie = cookie; }
    return r;
}
var results = [];
""" + body + "\nconsole.log(JSON.stringify(results));\n")
        environment = {"PATH": os.environ.get("PATH", os.defpath)}
        if key is not None:
            environment["PREVIEW_SIGNING_KEY"] = key
        done = subprocess.run([NJS, "-m", "-p", str(EXAMPLE), str(harness)], env=environment, capture_output=True,
                              text=True, timeout=60)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        return json.loads(done.stdout.strip().splitlines()[-1])

    def test_it_signs_what_the_preview_command_signs(self):
        from test_preview_k8s_example import SIGNATURES
        body = "".join("results.push(preview.sign(%s, %s, %s));\n" % (json.dumps(key), json.dumps(host), json.dumps(end))
                       for key, host, end, _ in SIGNATURES)
        self.assertEqual(self.run_script(body), [entry[3] for entry in SIGNATURES])

    def test_open_turns_a_valid_token_into_the_cookie(self):
        now = int(time.time())
        good = self.token(now + 3600)
        cases = [
            ("no token", njs_request(self.HOST), 403),
            ("another request's token", njs_request(self.HOST, {"t": self.token(now + 3600, "sample-13.preview.example.invalid")}), 403),
            ("an ended token", njs_request(self.HOST, {"t": self.token(now - 1)}), 403),
            ("a changed signature", njs_request(self.HOST, {"t": good[:-1] + ("A" if good[-1] != "A" else "B")}), 403),
            ("a changed end", njs_request(self.HOST, {"t": str(now + 7200) + good[good.index("."):]}), 403),
            ("an end with a leading zero", njs_request(self.HOST, {"t": "0" + good}), 403),
            ("padding", njs_request(self.HOST, {"t": good + "="}), 403),
            ("another key", njs_request(self.HOST, {"t": self.token(now + 3600, key="another-signing-key-0123456789abcdefgh")}), 403),
            ("a valid token", njs_request(self.HOST, {"t": good}), 302),
        ]
        body = "".join("var r%d = %s; preview.open(r%d); results.push([r%d.status, r%d.location, r%d.headersOut]);\n"
                       % (index, call, index, index, index, index) for index, (_, call, _) in enumerate(cases))
        results = self.run_script(body)
        for (label, _, status), (got, location, headers) in zip(cases, results):
            with self.subTest(label):
                self.assertEqual(got, status)
                if status == 302:
                    self.assertEqual(location, "/")
                    cookie = headers.get("Set-Cookie", [])
                    self.assertEqual(len(cookie), 1)
                    attributes = [part.strip() for part in cookie[0].split(";")]
                    self.assertEqual(attributes[0], "__Host-preview=" + good)
                    self.assertEqual(set(attributes[1:]) - {a for a in attributes[1:] if a.startswith("Max-Age=")},
                                     {"Path=/", "Secure", "HttpOnly", "SameSite=Lax"})
                    age = int(next(a for a in attributes if a.startswith("Max-Age="))[len("Max-Age="):])
                    self.assertTrue(3590 <= age <= 3600, age)
                    self.assertFalse(any(a.lower().startswith("domain") for a in attributes))
                    self.assertEqual(headers.get("Referrer-Policy"), "no-referrer")
                    self.assertEqual(headers.get("Cache-Control"), "no-store")
                else:
                    self.assertNotIn("Set-Cookie", headers)

    def test_every_other_path_needs_the_cookie_of_this_host(self):
        now = int(time.time())
        good = self.token(now + 3600)
        cases = [
            ("no cookie", njs_request(self.HOST), 403),
            ("the cookie", njs_request(self.HOST, cookie="__Host-preview=" + good), 204),
            ("the cookie among others", njs_request(self.HOST, cookie="a=1; __Host-preview=%s; b=2" % good), 204),
            ("another host's cookie", njs_request(self.HOST, cookie="__Host-preview=" + self.token(
                now + 3600, "sample-13.preview.example.invalid")), 403),
            ("an ended cookie", njs_request(self.HOST, cookie="__Host-preview=" + self.token(now - 1)), 403),
            ("two cookies of the name", njs_request(self.HOST, cookie="__Host-preview=%s; __Host-preview=x" % good), 403),
            ("a cookie of another name", njs_request(self.HOST, cookie="preview=" + good), 403),
        ]
        body = "".join("var r%d = %s; preview.check(r%d); results.push(r%d.status);\n" % (index, call, index, index)
                       for index, (_, call, _) in enumerate(cases))
        for (label, _, status), got in zip(cases, self.run_script(body)):
            with self.subTest(label):
                self.assertEqual(got, status)

    def test_without_a_key_of_32_characters_nothing_opens(self):
        now = int(time.time())
        for key in (None, "", "short-key"):
            with self.subTest(key=key):
                token = self.token(now + 3600, key=key or "x")
                body = ("var a = %s; preview.open(a); var b = %s; preview.check(b); results.push([a.status, b.status]);"
                        % (njs_request(self.HOST, {"t": token}), njs_request(self.HOST, cookie="__Host-preview=" + token)))
                self.assertEqual(self.run_script(body, key=key), [[403, 403]])

    def test_the_application_never_sees_or_sets_the_router_cookie(self):
        body = """
results.push(preview.otherCookies(request('h', {}, 'a=1; __Host-preview=tok; b=2')));
results.push(preview.otherCookies(request('h', {}, '__Host-preview=tok')));
results.push(preview.otherCookies(request('h', {})));
var r = request('h', {}); r.headersOut['Set-Cookie'] = ['__Host-preview=evil; Path=/', 'session=1; Path=/'];
preview.filterCookies(r); results.push(r.headersOut['Set-Cookie']);
var s = request('h', {}); s.headersOut['Set-Cookie'] = ['__Host-preview=evil'];
preview.filterCookies(s); results.push(s.headersOut['Set-Cookie'] === undefined);
"""
        self.assertEqual(self.run_script(body), ["a=1; b=2", "", "", ["session=1; Path=/"], True])

    def test_the_application_cannot_set_cookies_for_another_domain(self):
        """r7-3 and r8-3: after nginx's proxy_cookie_domain, a Set-Cookie
        whose Domain attributes (any of them, any letter case, spaces around
        "=") name anything but this host is dropped; the host itself, with or
        without a leading dot, stays, and so does a cookie without Domain."""
        host = self.HOST
        cases = [
            ("no Domain", "session=1; Path=/", True),
            ("Domain is the host", "a=1; Domain=%s; Path=/" % host, True),
            ("the host with a leading dot, in capitals", "a=1; Domain=.%s" % host.upper(), True),
            ("spaces around = and the host", "a=1; domain = %s" % host, True),
            ("the parent domain", "a=1; Domain=example.invalid", False),
            ("the parent domain with a leading dot", "a=1; Domain=.example.invalid", False),
            ("two Domains, the last the parent", "a=1; Domain=%s; Domain=example.invalid" % host, False),
            ("two Domains, the first the parent", "a=1; Domain=example.invalid; Domain=%s" % host, False),
            ("a space before =", "a=1; Domain =example.invalid", False),
            ("DOMAIN in capitals", "a=1; DOMAIN=example.invalid", False),
            ("another preview's host", "a=1; Domain=sample-13.preview.example.invalid", False),
            ("an empty Domain", "a=1; Domain=", False),
            ("the router's own name", "__Host-preview=evil; Path=/", False),
        ]
        body = "".join("var r%d = request(%s, {}); r%d.headersOut['Set-Cookie'] = [%s]; preview.filterCookies(r%d); "
                       "results.push(r%d.headersOut['Set-Cookie'] === undefined ? [] : r%d.headersOut['Set-Cookie']);\n"
                       % (index, json.dumps(host), index, json.dumps(value), index, index, index)
                       for index, (_, value, _) in enumerate(cases))
        for (label, value, kept), got in zip(cases, self.run_script(body)):
            with self.subTest(label):
                self.assertEqual(got, [value] if kept else [])
        mixed = ("var m = request(%s, {}); m.headersOut['Set-Cookie'] = ['a=1; Domain=example.invalid', 'b=2', "
                 "'c=3; Domain=%s']; preview.filterCookies(m); results.push(m.headersOut['Set-Cookie']);"
                 % (json.dumps(host), host))
        self.assertEqual(self.run_script(mixed), [["b=2", "c=3; Domain=%s" % host]])


if __name__ == "__main__":
    unittest.main()
