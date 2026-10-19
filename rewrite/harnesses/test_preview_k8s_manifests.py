"""Check the one-time manifests of the Kubernetes preview example
(rewrite/examples/preview-k8s) against the commands that rely on them, run the
cleanup script against a fake kubectl, and run the policy check's tool against
a fake one. Nothing here reaches a cluster.

The manifests are read as text, the way the module's other template checks
read theirs: no YAML library is needed or added.
"""
import base64
import contextlib
import datetime
import hashlib
import hmac
import http.client
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest

EXAMPLE = Path(__file__).resolve().parents[1] / "examples" / "preview-k8s"
# The njs command line, to run the router's script. CI's remaining group
# installs nginx.org's (.github/workflows/ci.yml); without one, those tests
# are skipped. The README says how they were run.
NJS = os.environ.get("PREVIEW_NJS") or shutil.which("njs")
# An nginx with the njs and auth_request modules, to run the router's
# configuration on 127.0.0.1 (PREVIEW_NGINX). CI's remaining group installs
# nginx.org's and names it; without one, those tests are skipped.
NGINX = os.environ.get("PREVIEW_NGINX")
# The commands a test starts get the fake's directory and the programs
# named only, a kubeconfig naming no real server and an empty home (as-8):
# test_preview_k8s_example.py has the preparation.
from test_preview_k8s_example import assert_fake_kubectl, isolated_environment  # noqa: E402
CLEANUP_TOOLS = ("ps", "mktemp", "rm", "expr", "cut", "grep")
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

    def test_the_pod_and_deployment_policies_leave_the_pod_s_annotations_alone(self):
        """PREVIEW_POD_ANNOTATIONS adds annotations to the Pod template: no
        validation of the Deployment or the Pod policy reads annotations, so
        none refuses them (the Service policy does, for its own)."""
        policies = {name_of(part): part for part in documents(EXAMPLE / "admission-policy.yaml.example")
                    if kind_of(part) == "ValidatingAdmissionPolicy"}
        for name in ("preview-deployment-shape", "preview-pod-shape"):
            self.assertNotIn("annotation", policies[name].lower(), name)
        self.assertIn("object.metadata.annotations", policies["preview-service-shape"],
                      "the control: the Service policy reads annotations, so the search can find them")

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
        formats = dict(re.findall(r"log_format (\S+) ((?:'[^']*'\s*)+);", config))
        self.assertEqual(sorted(formats), ["preview", "preview_full_relay", "preview_relay"])
        self.assertIn("$uri", formats["preview"])
        self.assertIn("$relay_path", formats["preview_relay"], "the relay's line names the path it decided on")
        for written in formats.values():
            for variable in ("$request ", "$request\"", "$request_uri", "$args", "$query_string", "$arg_", "$http_referer",
                             "$http_cookie", "$cookie_", "$relay_uri", "$relay_key", "$http_authorization"):
                self.assertNotIn(variable, written)
        logs = re.findall(r"access_log ([^;]+);", config)
        self.assertTrue(logs)
        for line in logs:
            self.assertTrue(line.strip() == "off" or line.split()[-1] in formats, line)
        start = config.index("location = /_relay/api {")
        relay = config[start:config.index("location / {", start)]
        self.assertEqual(relay.count("access_log /dev/stdout preview_relay;"), 3,
                         "the relay's location and its two answers write the relay's line")

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
        self.isolated = isolated_environment(self.base, fake, CLEANUP_TOOLS)
        assert_fake_kubectl(self, self.isolated, fake)

    def stamp(self, offset):
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(self.NOW + offset))

    def run_cleanup(self, deployments, services, pods, failing=(), environment=None):
        (self.base / "deployments.txt").write_text("".join("|".join(row) + "\n" for row in deployments))
        (self.base / "services.txt").write_text("".join(name + "\n" for name in services))
        (self.base / "pods.txt").write_text("".join("|".join(row) + "\n" for row in pods))
        if failing:
            (self.base / "failing.txt").write_text(" ".join(failing))
        environment = dict(environment or self.isolated, PREVIEW_NAMESPACE="preview-apps", LANG="C")
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

    def test_a_missing_program_ends_1_before_anything_is_removed(self):
        """rs-18: without grep every Service would look like one without a
        Deployment, without expr no lifetime would end: the script has to
        stop first, saying which program is missing."""
        deployments = [("pv-a", self.stamp(-1), "True", "", "")]
        for missing in ("grep", "expr", "mktemp", "cut", "rm"):
            with self.subTest(missing=missing):
                base = self.base / ("without-" + missing)
                base.mkdir()
                environment = isolated_environment(base, self.fake, [name for name in CLEANUP_TOOLS if name != missing])
                assert_fake_kubectl(self, environment, self.fake)
                self.assertIsNone(shutil.which(missing, path=environment["PATH"]))
                done, removed = self.run_cleanup(deployments, ["pv-a", "pv-b"], [], environment=environment)
                self.assertEqual(done.returncode, 1, done.stdout + done.stderr)
                self.assertEqual(removed, [])
                self.assertIn(missing, done.stderr)

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

        unchanged = [timeless(self.tool.obj(preview, self.tool.Case("", kind, install, None, None, None, annotations),
                                            "preview-apps", NET_IMAGE))
                     for kind, install, annotations in (
                         ("deployment", False, None), ("deployment", True, None), ("service", False, None),
                         ("pod", False, None), ("deployment", False, self.tool.POD_ANNOTATIONS),
                         ("pod", False, self.tool.POD_ANNOTATIONS))]
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
        isolated = isolated_environment(self.base, fake)
        assert_fake_kubectl(self, isolated, fake)
        # The caller's own KUBECONFIG, which dry_run.py has to leave aside.
        environment = dict(isolated, KUBECONFIG=str(self.base / "the-callers-own"))
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



def first_changed(token):
    """The token with the first character of its signature changed."""
    end, signature = token.split(".", 1)
    return "%s.%s%s" % (end, "A" if signature[0] != "A" else "B", signature[1:])


def cookie_value(key, host, end):
    """The router's cookie for a host until end, as preview-router.js's open()
    makes it: <end>.<HMAC-SHA256 over "cookie\n<host>\n<end">, base64url
    without padding. Never the URL's token."""
    signature = hmac.new(key.encode(), ("cookie\n%s\n%d" % (host, end)).encode(), hashlib.sha256).digest()
    return "%d.%s" % (end, base64.urlsafe_b64encode(signature).decode().rstrip("="))


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

    def cookie(self, end, host=None, key=SIGNING_KEY):
        return cookie_value(key, host or self.HOST, end)

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
        environment = {}  # njs needs no PATH, and this test starts nothing else
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

    def test_the_cookie_is_signed_apart_from_the_url(self):
        cases = [(SIGNING_KEY, self.HOST, 1800000000), (SIGNING_KEY, "sample-13.preview.example.invalid", 1800000000),
                 ("another-signing-key-0123456789abcdefgh", self.HOST, 1700000123)]
        body = "".join("results.push(preview.signCookie(%s, %s, %s));\n" % (json.dumps(key), json.dumps(host), json.dumps(str(end)))
                       for key, host, end in cases)
        made = self.run_script(body)
        self.assertEqual(["%d.%s" % (end, signature) for (_, _, end), signature in zip(cases, made)],
                         [cookie_value(*case) for case in cases])
        for (key, host, end), signature in zip(cases, made):
            self.assertNotEqual(signature, self.preview.sign(key, host, end), "the cookie would be the URL's token")

    def test_open_turns_a_valid_token_into_the_cookie(self):
        now = int(time.time())
        good = self.token(now + 3600)
        cases = [
            ("no token", njs_request(self.HOST), 403),
            ("another request's token", njs_request(self.HOST, {"t": self.token(now + 3600, "sample-13.preview.example.invalid")}), 403),
            ("an ended token", njs_request(self.HOST, {"t": self.token(now - 1)}), 403),
            ("a changed signature (its last character)",
             njs_request(self.HOST, {"t": good[:-1] + ("A" if good[-1] != "A" else "B")}), 403),
            ("a changed signature (its first character)",
             njs_request(self.HOST, {"t": first_changed(good)}), 403),
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
                    self.assertEqual(attributes[0], "__Host-preview=" + self.cookie(now + 3600))
                    self.assertNotIn(good[good.index(".") + 1:], cookie[0], "the URL's token became the cookie")
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
        good = self.cookie(now + 3600)
        cases = [
            ("no cookie", njs_request(self.HOST), 403),
            ("the cookie", njs_request(self.HOST, cookie="__Host-preview=" + good), 204),
            ("the cookie among others", njs_request(self.HOST, cookie="a=1; __Host-preview=%s; b=2" % good), 204),
            ("the URL's token as the cookie", njs_request(self.HOST, cookie="__Host-preview=" + self.token(now + 3600)), 403),
            ("another host's cookie", njs_request(self.HOST, cookie="__Host-preview=" + self.cookie(
                now + 3600, "sample-13.preview.example.invalid")), 403),
            ("an ended cookie", njs_request(self.HOST, cookie="__Host-preview=" + self.cookie(now - 1)), 403),
            ("a cookie whose signature's first character changed",
             njs_request(self.HOST, cookie="__Host-preview=" + first_changed(good)), 403),
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
                cookie = self.cookie(now + 3600, key=key or "x")
                body = ("var a = %s; preview.open(a); var b = %s; preview.check(b); results.push([a.status, b.status]);"
                        % (njs_request(self.HOST, {"t": token}), njs_request(self.HOST, cookie="__Host-preview=" + cookie)))
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


# README.md's example of a relay ("Relaying an API"), written into the router's
# configuration the way the README says: one line per path.
RELAY_VALUES = {"relay-label-pattern": "sample-example-[0-9]+", "relay-path-prefix": "/api/",
                "relay-service": "api.example-dev", "relay-header": "X-Example-Key", "relay-cleared-header": "X-Act-As"}
RELAY_ALLOWED = ("/api/session", "/api/items", "/api/items/[0-9]+", "/api/projects/[0-9]+/items", "/api/status/[0-9]+",
                 "/api/tags/[^/]+")
RELAY_REFUSED = ("/api/account/keys(/|$)", "/api/items/[0-9]+/export$")
RELAY_QUERIES = (("/api/projects/[0-9]+/items", "owner=demo(&page=[0-9]{1,4})?(&limit=[0-9]{1,4})?"),
                 ("/api/projects/[0-9]+/items", "owner=demo&q=[a-z]{1,20}"),
                 ("/api/tags/[^/]+", "owner=demo"), ("/api/tags/[^/]+", "owner=demo&q=[a-z]{1,20}"))
# The allowed paths that take an identifier in their query, and so are
# refused without one.
RELAY_ID_QUERY_PATHS = ("/api/projects/[0-9]+/items",)
RELAY_KEY = "example-relay-key-0123456789"
REFUSED_SENTENCE = "確認用の画面では使えない操作です"
SAVING_SENTENCE = "確認用の画面では保存できません"
COOKIE_SENTENCE = "この画面は課題に投稿された URL から開いてください"


def router_config():
    """The nginx.conf of router.yaml.example, unindented."""
    router = (EXAMPLE / "router.yaml.example").read_text()
    block = router[router.index("  nginx.conf: |\n") + len("  nginx.conf: |\n"):router.index("  ended.html: |")]
    return "\n".join(line[4:] for line in block.splitlines()) + "\n"


def without_relay(text):
    """The text with every relay block deleted, as the README says to when no
    preview's page reads an API."""
    return re.sub(r"(?ms)^[ \t]*# relay: begin[^\n]*\n.*?^[ \t]*# relay: end(?: full)?\n", "", text)


def fill_relay(config, allowed=RELAY_ALLOWED, refused=RELAY_REFUSED, queries=RELAY_QUERIES,
               id_query_paths=RELAY_ID_QUERY_PATHS, values=None, full=False):
    """The relay's placeholders filled: one line for each path and each query."""
    values = dict(RELAY_VALUES, **(values or {}))
    if not full:
        config = without_full_relay(config)
    lines = []
    for line in config.splitlines():
        if "<relay-id-query-path>" in line:
            lines += [line.replace("<relay-id-query-path>", path) for path in id_query_paths]
        elif "<relay-allowed-query>" in line:
            lines += [line.replace("<relay-allowed-path>", path).replace("<relay-allowed-query>", query)
                      for path, query in queries]
        elif "<relay-allowed-path>" in line:
            lines += [line.replace("<relay-allowed-path>", path) for path in allowed]
        elif "<relay-refused-path>" in line:
            lines += [line.replace("<relay-refused-path>", path) for path in refused]
        else:
            for name, value in values.items():
                line = line.replace("<%s>" % name, value)
            lines.append(line)
    return "\n".join(lines) + "\n"


def without_full_relay(text):
    return re.sub(r"(?ms)^[ \t]*# relay: begin full.*?# relay: end full\n", "", text)


def fill_full_relay(config):
    return fill_relay(config, full=True, values={
        "full-relay-label-pattern": "samplefull-example-[0-9]+",
        "full-relay-path-prefix": "/api/",
        "full-relay-refused-path": "/api/auth/",
        "full-relay-header": "X-Example-Key"})


class RelayTemplateTests(unittest.TestCase):
    """The relay's place in router.yaml.example, read as text."""

    def setUp(self):
        self.router = (EXAMPLE / "router.yaml.example").read_text()
        self.config = without_full_relay(router_config())

    def relay_location(self):
        start = self.config.index("location = /_relay/api {")
        return self.config[start:self.config.index("\n    }\n", start)]

    def test_without_the_relay_nothing_of_it_is_left(self):
        removed = without_relay(self.router)
        self.assertNotEqual(removed, self.router, "no relay block was found; the check removes nothing")
        code = "\n".join(line for line in removed.splitlines() if not line.lstrip().startswith("#"))
        for word in ("relay", "PREVIEW_RELAY_KEY", "Content-Security-Policy"):
            self.assertNotIn(word, code)
        for kept in ("location / {", "auth_request /_preview/check;", "env PREVIEW_SIGNING_KEY;",
                     "secretKeyRef: { name: preview-router-signing, key: key }"):
            self.assertIn(kept, removed)
        self.assertEqual(self.router.count("# relay: begin"), self.router.count("# relay: end"))

    def test_without_full_relay_the_existing_template_is_byte_identical(self):
        self.assertEqual(hashlib.sha256(without_full_relay(self.router).encode()).hexdigest(),
                         'd684c2bb4781746fb5df1fb6eecad2f47d574f4e018b0514f704ad0296642160')

    def test_the_relay_decides_on_the_path_saved_before_the_rewrite(self):
        location = self.config[self.config.index("    location / {"):]
        saved = location.index("set $relay_path $uri;")
        self.assertLess(saved, location.index("rewrite ^ /_relay/api last;"))
        self.assertIn("set $relay_uri $uri$is_args$args;", location)
        maps = re.findall(r'map "([^"]*)" \$relay_(?:refusal|args_refusal|on) ', self.config)
        self.assertEqual(sorted(maps), sorted(["$relay_label:$relay_path", "$request_method:$relay_path",
                                               "$relay_path?$args"]))
        for key in maps:
            self.assertNotIn("$uri", key, "after the rewrite, $uri is the relay's own location")
        self.assertIn("proxy_pass http://<relay-service>.svc.cluster.local$relay_uri;", self.relay_location())

    def test_refusals_come_first_and_everything_else_is_refused(self):
        refusal = self.config[self.config.index('map "$request_method:$relay_path" $relay_refusal {'):]
        refusal = refusal[:refusal.index("\n  }\n")]
        lines = [line.strip() for line in refusal.splitlines()[1:]]
        self.assertTrue(lines[0].startswith('"~^(GET|HEAD):.*[^A-Za-z0-9/._~-]"'), lines[0])
        self.assertTrue(lines[1].startswith('"~^(GET|HEAD):<relay-refused-path>"'), lines[1])
        self.assertEqual(lines[2], '"~^(GET|HEAD):<relay-allowed-path>$" "";')
        self.assertEqual(lines[3], '"~^(GET|HEAD):" "%s";' % REFUSED_SENTENCE)
        self.assertEqual(lines[4], 'default "%s";' % SAVING_SENTENCE)
        query = self.config[self.config.index('map "$relay_path?$args" $relay_args_refusal {'):]
        query = [line.strip() for line in query[:query.index("\n  }\n")].splitlines()[1:]]
        self.assertEqual(query, ['default "%s";' % REFUSED_SENTENCE,
                                 '"~^[^?]*[?][^?]*[?]" "%s";' % REFUSED_SENTENCE,
                                 '"~^<relay-id-query-path>[?]$" "%s";' % REFUSED_SENTENCE,
                                 '"~^[^?]*\\?$" "";', '"~^<relay-allowed-path>[?]<relay-allowed-query>$" "";'])
        self.assertIn('"~^(?:<relay-label-pattern>)$" 1;', self.config, "the whole label has to match")

    def test_the_relay_answers_json_and_sends_no_password_or_cookie(self):
        location = self.relay_location()
        for line in ("internal;", "auth_request /_preview/check;", "error_page 403 = @relay_refused;",
                     "error_page 502 503 504 = @relay_down;", 'if ($relay_refusal != "") { return 403; }',
                     'if ($relay_args_refusal != "") { return 403; }', 'if ($relay_key = "") { return 503; }',
                     "proxy_set_header Host <relay-service>.svc.cluster.local;", 'proxy_set_header Authorization "";',
                     'proxy_set_header Cookie "";', 'proxy_set_header <relay-cleared-header> "";',
                     "proxy_set_header <relay-header> $relay_key;", "proxy_hide_header Set-Cookie;"):
            self.assertIn(line, location)
        self.assertNotIn("proxy_intercept_errors", location, "the API's own answers go to the page as they are")
        self.assertIn("error_log stderr crit;", location, "an API that does not answer would log the query")
        for name in ("@relay_refused", "@relay_down"):
            named = self.config[self.config.index("location %s {" % name):].splitlines()[0]
            self.assertIn("default_type application/json;", named)
        self.assertIn("add_header Content-Security-Policy $relay_csp always;", self.config)
        self.assertIn("""map $relay_label $relay_csp { default ""; 1 "connect-src 'self'"; }""", self.config)

    def test_the_relay_asks_for_the_cookie_alone(self):
        location = self.relay_location()
        self.assertIn("auth_basic off;", location, "a page's own Authorization would fail the password")
        self.assertIn("auth_request /_preview/check;", location, "the signed URL's cookie stays required")
        server = self.config[self.config.index("server_name *.<preview-domain>;"):]
        self.assertEqual(server.count("auth_basic off;"), 3, "the two pages and the relay, nothing else")
        self.assertNotIn("auth_basic off;", server[server.index("    location / {"):])
        self.assertNotIn("satisfy any", self.config)
        bare = without_relay(self.config)
        self.assertEqual(bare[bare.index("server_name *.<preview-domain>;"):].count("auth_basic off;"), 2,
                         "without the relay, the password guards every path but the router's own pages")

    def test_the_key_is_the_router_s_alone(self):
        self.assertIn("env PREVIEW_RELAY_KEY;", self.config)
        self.assertIn("js_set $relay_key preview.relayKey;", self.config)
        self.assertIn("- name: PREVIEW_RELAY_KEY\n              valueFrom:\n"
                      "                secretKeyRef: { name: preview-relay, key: key, optional: true }", self.router)
        script = (EXAMPLE / "preview-router.js").read_text()
        self.assertIn("return process.env.PREVIEW_RELAY_KEY || '';", script)
        preview = (EXAMPLE / "preview.py").read_text()
        self.assertNotIn("RELAY", preview, "the preview command neither reads nor passes the relay's key")

    def test_the_readme_s_example_is_the_one_the_tests_run(self):
        readme = (EXAMPLE / "README.md").read_text()
        section = readme[readme.index("## Relaying an API"):]
        example = section[section.index("```nginx\n") + len("```nginx\n"):]
        example = example[:example.index("```")]
        filled = [line.strip() for line in fill_relay(self.config).splitlines()]
        lines = [line.strip() for line in example.splitlines() if line.strip()]
        self.assertGreater(len(lines), 10)
        for line in lines:
            self.assertIn(line, filled)
        for line in ("proxy_pass http://api.example-dev.svc.cluster.local$relay_uri;", 'proxy_set_header X-Act-As "";'):
            self.assertIn(line, filled)
            self.assertIn("`%s`" % line, section)

    def test_the_egress_rules_stay_as_they_were(self):
        router = [line for line in rules(EXAMPLE / "router.yaml.example", "router.rules") if line.startswith("-A")]
        self.assertEqual(router[-2:], ["-A OUTPUT -d <service-cidr> -p tcp --dport 80 -j ACCEPT", "-A OUTPUT -j REJECT"])


class StandIn(http.server.BaseHTTPRequestHandler):
    """A stand-in for the API, or for a preview's Pod, on 127.0.0.1: it
    keeps what it received and says which it is."""

    protocol_version = "HTTP/1.1"

    def log_message(self, *arguments):
        pass

    def answer(self):
        self.server.received.append({"method": self.command, "target": self.path,
                                     "headers": {name.lower(): value for name, value in self.headers.items()}})
        status = 200
        found = re.match(r"^/api/status/([0-9]{3})$", self.path)
        if found and self.server.role == "api":
            status = int(found.group(1))
        body = json.dumps({"from": self.server.role, "target": self.path}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json" if self.server.role == "api" else "text/html")
        self.send_header("Set-Cookie", "session=from-%s; Path=/" % self.server.role)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    do_GET = do_HEAD = do_POST = answer


def stand_in(role):
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), StandIn)
    server.role, server.received = role, []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


@unittest.skipUnless(NGINX, "no nginx with njs and auth_request is named (set PREVIEW_NGINX); the relay is not run")
class RelayNginxTests(unittest.TestCase):
    """The router's configuration from router.yaml.example, with README.md's
    example relay filled in, run by nginx on 127.0.0.1 with stand-ins for
    the API and the preview's Pod. Only the addresses, the files' places and
    the module line are changed to run it here."""

    HOST = "sample-example-12.preview.example.invalid"
    PASSWORD = "requester-password"

    @classmethod
    def setUpClass(cls):
        cls.preview = load("preview")
        version = subprocess.run([NGINX, "-V"], capture_output=True, text=True, timeout=30).stderr
        cls.njs_built_in = "--add-module=" in version and "njs" in version
        cls.api, cls.pod = stand_in("api"), stand_in("pod")

    @classmethod
    def tearDownClass(cls):
        for server in (cls.api, cls.pod):
            server.shutdown()
            server.server_close()

    def setUp(self):
        self.api.received.clear()
        self.pod.received.clear()

    def start(self, config=None, key=RELAY_KEY, api_port=None, full_key=None):
        """nginx with this configuration (by default the filled example),
        on a free port; it is stopped when the test ends."""
        temporary = tempfile.TemporaryDirectory(prefix="preview-relay-")
        self.addCleanup(temporary.cleanup)
        base = Path(temporary.name)
        for directory in ("js", "pages", "auth", "conf", "tmp"):
            (base / directory).mkdir()
        shutil.copy(EXAMPLE / "preview-router.js", base / "js" / "preview-router.js")
        router = (EXAMPLE / "router.yaml.example").read_text()
        for page in ("ended.html", "denied.html"):
            (base / "pages" / page).write_text("\n".join(rules(EXAMPLE / "router.yaml.example", page)) + "\n")
        (base / "auth" / "htpasswd").write_text("requester:{PLAIN}%s\n" % self.PASSWORD)
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        text = fill_relay(router_config()) if config is None else config
        text = (text.replace("<preview-domain>", "preview.example.invalid").replace("<preview-namespace>", "preview-apps")
                .replace("<cluster-dns-ip>", "127.0.0.1")
                .replace("listen 8080 default_server;", "listen 127.0.0.1:%d default_server;" % port)
                .replace("listen 8080;", "listen 127.0.0.1:%d;" % port)
                # /tmp/ first: the places put in after it may lie under /tmp.
                .replace("/tmp/", str(base / "tmp") + "/").replace("/etc/preview-router/", str(base) + "/")
                .replace("access_log /dev/stdout ", "access_log %s/access.log " % base)
                .replace("http://pv-$preview_label.preview-apps.svc.cluster.local",
                         "http://127.0.0.1:%d" % self.pod.server_address[1])
                .replace("http://api.example-dev.svc.cluster.local$relay_uri",
                         "http://127.0.0.1:%d$relay_uri" % (api_port or self.api.server_address[1])))
        if self.njs_built_in:
            text = text.replace("load_module /etc/nginx/modules/ngx_http_js_module.so;\n", "")
        (base / "conf" / "nginx.conf").write_text(text)
        environment = {"PREVIEW_SIGNING_KEY": SIGNING_KEY}
        if key is not None:
            environment["PREVIEW_RELAY_KEY"] = key
        if full_key is not None:
            environment["PREVIEW_FULL_RELAY_KEY"] = full_key
        log = open(base / "nginx.stderr", "w")
        self.addCleanup(log.close)
        process = subprocess.Popen([NGINX, "-p", str(base), "-e", str(base / "error.log"), "-c",
                                    str(base / "conf" / "nginx.conf"), "-g", "daemon off;"],
                                   env=environment, stdout=log, stderr=subprocess.STDOUT)

        def stop():
            process.terminate()
            process.wait(timeout=10)
        self.addCleanup(stop)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if process.poll() is not None:
                self.fail("nginx ended %s: %s" % (process.returncode, (base / "nginx.stderr").read_text()))
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=1):
                    break
            except OSError:
                time.sleep(0.05)
        self.port = port
        self.base = base

    def access_log(self):
        return (self.base / "access.log").read_text()

    def token(self, host):
        end = int(time.time()) + 3600
        return "%d.%s" % (end, self.preview.sign(SIGNING_KEY, host, end))

    def ask(self, method, target, host=None, password=True, cookie=True, headers=None):
        host = host or self.HOST
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        connection.putrequest(method, target, skip_host=True, skip_accept_encoding=True)
        sent = {"Host": host}
        if password:
            sent["Authorization"] = "Basic " + base64.b64encode(("requester:" + self.PASSWORD).encode()).decode()
        if cookie:
            sent["Cookie"] = "__Host-preview=%s; app=1" % cookie_value(SIGNING_KEY, host, int(time.time()) + 3600)
        sent.update(headers or {})
        for name, value in sent.items():
            connection.putheader(name, value)
        connection.endheaders()
        response = connection.getresponse()
        body = response.read().decode()
        headers = {name.lower(): value for name, value in response.getheaders()}
        connection.close()
        return response.status, headers.get("content-type", ""), body, headers

    def refused(self, answer, sentence, status=403):
        code, kind, body, _ = answer
        self.assertEqual((code, kind), (status, "application/json"), body)
        self.assertEqual(json.loads(body), {"error": sentence})

    def test_full_relay_uses_only_its_own_key_and_host_bound_token(self):
        self.start(config=fill_full_relay(router_config()), full_key="disposable-test-value")
        host = "samplefull-example-12.preview.example.invalid"
        answer = self.ask("POST", "/api/items", host=host, password=False,
                          headers={"Origin": "https://" + host, "Authorization": "Bearer browser-value",
                                   "X-Impersonate-UID": "untrusted", "X-API-Key": "untrusted"})
        self.assertEqual(answer[0], 200, answer)
        self.assertEqual(self.api.received, [], "the development API must receive nothing")
        received = self.pod.received[-1]
        self.assertEqual(received["method"], "POST")
        self.assertEqual(received["target"], "/api/items")
        self.assertEqual(received["headers"].get("x-example-key"), "disposable-test-value")
        expected = hmac.new(SIGNING_KEY.encode(), b"relay:samplefull-example-12", hashlib.sha256).hexdigest()
        self.assertEqual(received["headers"].get("x-preview-relay"), expected)
        for name in ("authorization", "cookie", "x-impersonate-uid", "x-api-key"):
            self.assertNotIn(name, received["headers"])
        self.assertNotIn(RELAY_KEY, str(received))
        self.assertNotIn("set-cookie", answer[3])
        self.assertEqual(self.ask("GET", "/", host=host)[3].get("content-security-policy"), "connect-src 'self'")

    def test_full_relay_refuses_foreign_missing_origin_cookie_and_auth_path(self):
        self.start(config=fill_full_relay(router_config()), full_key="disposable-test-value")
        host = "samplefull-example-12.preview.example.invalid"
        for headers in ({}, {"Origin": "null"}, {"Origin": "https://other.preview.example.invalid"},
                        {"Origin": "https://" + host + ".invalid"},
                        {"Origin": "https://" + host, "origin": "https://" + host}):
            self.assertEqual(self.ask("POST", "/api/items", host=host, headers=headers)[0], 403)
        self.assertEqual(self.ask("POST", "/api/items")[0], 403, "page remains read-only")
        self.assertEqual(self.ask("GET", "/api/items", host=host, cookie=False)[0], 403)
        self.assertEqual(self.ask("GET", "/api/auth/login", host=host)[0], 403)
        self.assertEqual(self.pod.received, [])
        self.assertEqual(self.api.received, [])

    def test_full_relay_missing_key_never_falls_back_to_read_only_key(self):
        self.start(config=fill_full_relay(router_config()))
        self.assertEqual(self.ask("GET", "/api/items", host="samplefull-example-12.preview.example.invalid")[0], 503)
        self.assertEqual(self.pod.received, [])
        self.assertEqual(self.api.received, [])

    def test_full_relay_page_proxy_refuses_wrong_token_and_strips_valid_one(self):
        host = "samplefull-example-12.preview.example.invalid"
        expected = hmac.new(SIGNING_KEY.encode(), b"relay:samplefull-example-12", hashlib.sha256).hexdigest()
        with socket.socket() as probe:
            probe.bind(("localhost", 0))
            proxy_port = probe.getsockname()[1]
        config = fill_full_relay(router_config()).replace(
            "http://pv-$preview_label.<preview-namespace>.svc.cluster.local$full_relay_uri",
            "http://%s:%d$full_relay_uri" % (socket.gethostbyname("localhost"), proxy_port))
        page_proxy = '''
  server {
    listen localhost:%d;
    location /api/ {
      if ($http_x_preview_relay != "%s") { return 403; }
      proxy_set_header X-Preview-Relay "";
      proxy_pass http://localhost:%d;
    }
  }
''' % (proxy_port, expected, self.api.server_address[1])
        at = config.rfind("\n}")
        config = config[:at] + page_proxy + config[at:]
        # Literal localhost upstream: no resolver or cluster service is used.
        self.start(config=config, full_key="disposable-test-value")
        self.assertEqual(self.ask("POST", "/api/items", host=host,
                                  headers={"Origin": "https://" + host})[0], 200)
        self.assertNotIn("x-preview-relay", self.api.received[-1]["headers"])
        self.assertEqual(self.api.received[-1]["headers"]["x-example-key"], "disposable-test-value")
        self.api.received.clear()
        router_port, self.port = self.port, proxy_port
        try:
            for token in ("", "wrong", expected + "x"):
                self.assertEqual(self.ask("POST", "/api/items", host=host, password=False,
                                          cookie=False, headers={"X-Preview-Relay": token})[0], 403)
            self.assertEqual(self.api.received, [])
        finally:
            self.port = router_port

    def test_sign_in_keeps_the_public_origin_behind_tls_termination(self):
        self.start()
        token = self.token(self.HOST)
        code, _, _, headers = self.ask("GET", "/_preview/open?t=" + token, cookie=False)
        self.assertEqual(code, 302)
        self.assertEqual(headers["location"], "/", "the internal HTTP listener must not become the browser's destination")
        cookie = headers["set-cookie"].split(";", 1)[0]
        self.assertTrue(cookie.startswith("__Host-preview="), cookie)
        self.assertNotIn(token[token.index(".") + 1:], cookie, "the URL's token became the cookie")
        code, _, body, _ = self.ask("GET", headers["location"], cookie=False, headers={"Cookie": cookie})
        self.assertEqual(code, 200, body)
        self.assertEqual(json.loads(body)["from"], "pod")

    def test_an_allowed_get_reaches_the_api_with_the_key_and_nothing_of_the_browser(self):
        self.start()
        code, kind, body, headers = self.ask("GET", "/api/items/7", headers={"X-Act-As": "someone-else"})
        self.assertEqual((code, kind), (200, "application/json"), body)
        self.assertEqual(json.loads(body), {"from": "api", "target": "/api/items/7"})
        self.assertNotIn("set-cookie", headers, "the API's cookie reaches the page")
        received = self.api.received[-1]["headers"]
        self.assertEqual(received.get("x-example-key"), RELAY_KEY)
        self.assertEqual(received.get("host"), "api.example-dev.svc.cluster.local")
        for name in ("authorization", "cookie"):
            self.assertNotIn(name, received, "%s reached the API" % name)
        self.assertEqual(received.get("x-act-as", ""), "", "the cleared header reached the API")
        self.assertEqual(self.ask("HEAD", "/api/session")[0], 200)
        self.assertEqual(self.pod.received, [], "the relayed paths reached the Pod")
        self.ask("GET", "/api/projects/3/items?owner=demo&page=2")
        self.refused(self.ask("GET", "/api/other?owner=demo"), REFUSED_SENTENCE)
        written = self.access_log()
        for line in ('"GET /api/items/7 HTTP/1.1" 200', '"HEAD /api/session HTTP/1.1" 200',
                     '"GET /api/projects/3/items HTTP/1.1" 200', '"GET /api/other HTTP/1.1" 403'):
            self.assertIn(line, written)
        for word in ("/_relay/api", "owner=", "page=", RELAY_KEY, self.PASSWORD):
            self.assertNotIn(word, written)

    def test_the_relay_asks_for_the_cookie_and_not_the_password(self):
        """A page's own code may write Authorization (a token of its own): the
        relay's paths need the signed URL's cookie alone, and the pages and
        the signed URL still need the password."""
        self.start()
        for headers in (None, {"Authorization": "Bearer page-token"}):
            with self.subTest(headers=headers):
                code, kind, body, _ = self.ask("GET", "/api/items/7", password=False, headers=headers)
                self.assertEqual((code, kind), (200, "application/json"), body)
                self.assertEqual(json.loads(body)["from"], "api")
                received = self.api.received[-1]["headers"]
                self.assertNotIn("authorization", received, "the page's Authorization reached the API")
                self.assertEqual(received.get("x-example-key"), RELAY_KEY)
        sent = len(self.api.received)
        self.refused(self.ask("GET", "/api/items/7", cookie=False), COOKIE_SENTENCE)
        self.refused(self.ask("GET", "/api/items/7", password=False, cookie=False), COOKIE_SENTENCE)
        self.refused(self.ask("GET", "/api/items/7", password=False, cookie=False,
                              headers={"Authorization": "Bearer page-token"}), COOKIE_SENTENCE)
        # Whoever reads the URL can send its token as the cookie: refused.
        token = "__Host-preview=" + self.token(self.HOST)
        for password in (False, True):
            with self.subTest(token_as_cookie_with_password=password):
                self.refused(self.ask("GET", "/api/items/7", password=password, cookie=False, headers={"Cookie": token}),
                             COOKIE_SENTENCE)
        self.assertEqual(self.ask("GET", "/index.html", cookie=False, headers={"Cookie": token})[0], 403)
        self.assertEqual(len(self.api.received), sent, "a request without the cookie reached the API")
        for headers in (None, {"Authorization": "Bearer page-token"}):
            with self.subTest(page=headers):
                self.assertEqual(self.ask("GET", "/index.html", password=False, headers=headers)[0], 401)
        self.assertEqual(self.ask("GET", "/_preview/open?t=" + self.token(self.HOST), password=False, cookie=False)[0], 401)
        self.assertEqual(self.ask("GET", "/api/items/7", host="sample-stg-example-12.preview.example.invalid",
                                  password=False)[0], 401, "a label that is not relayed keeps the password")
        # The cookie /_preview/open returned, alone, reads the relayed API.
        code, _, _, headers = self.ask("GET", "/_preview/open?t=" + self.token(self.HOST), cookie=False)
        self.assertEqual(code, 302)
        opened = headers["set-cookie"].split(";", 1)[0]
        code, kind, body, _ = self.ask("GET", "/api/items/7", password=False, cookie=False, headers={"Cookie": opened})
        self.assertEqual((code, kind), (200, "application/json"), body)
        self.assertEqual(self.pod.received, [])
        self.assertNotIn("page-token", self.access_log())

    def test_only_this_preview_s_live_cookie_opens_the_relay(self):
        """Without the password the cookie is the relay's only guard: an ended
        cookie, another host's, one signed wrongly or with another key, one of
        another shape, the URL's token and two of the name are refused, with
        or without the password, and nothing reaches the API."""
        self.start()
        now = int(time.time())
        good = cookie_value(SIGNING_KEY, self.HOST, now + 3600)
        refused = (
            ("an ended cookie", cookie_value(SIGNING_KEY, self.HOST, now - 1)),
            ("another relayed host's cookie", cookie_value(SIGNING_KEY, "sample-example-13.preview.example.invalid", now + 3600)),
            ("a changed signature", first_changed(good)),
            ("another key", cookie_value("another-signing-key-0123456789abcdefgh", self.HOST, now + 3600)),
            ("padding", good + "="),
            ("an end with a leading zero", "0" + good),
            ("no end", good[good.index("."):]),
            ("the URL's token", self.token(self.HOST)),
        )
        for label, value in refused:
            for password in (False, True):
                with self.subTest(label, password=password):
                    self.refused(self.ask("GET", "/api/items/7", password=password, cookie=False,
                                          headers={"Cookie": "__Host-preview=" + value}), COOKIE_SENTENCE)
        for password in (False, True):
            with self.subTest("two cookies of the name", password=password):
                self.refused(self.ask("GET", "/api/items/7", password=password, cookie=False,
                                      headers={"Cookie": "__Host-preview=%s; __Host-preview=%s" % (good, good)}),
                             COOKIE_SENTENCE)
        self.assertEqual(self.api.received, [])
        # The control: the same request with this preview's cookie goes through.
        code, _, body, _ = self.ask("GET", "/api/items/7", password=False, cookie=False, headers={"Cookie": "__Host-preview=" + good})
        self.assertEqual(code, 200, body)

    def test_what_is_not_allowed_is_refused_before_anything_is_sent(self):
        self.start()
        for method, target, sentence in (
                ("GET", "/api/other", REFUSED_SENTENCE),
                ("GET", "/api/items/7/comments", REFUSED_SENTENCE),
                ("GET", "/api/account/keys", REFUSED_SENTENCE),
                ("GET", "/api/account/keys/1", REFUSED_SENTENCE),
                ("GET", "/api/items/7/export", REFUSED_SENTENCE),
                ("POST", "/api/items", SAVING_SENTENCE),
                ("PUT", "/api/items/7", SAVING_SENTENCE),
                ("DELETE", "/api/items/7", SAVING_SENTENCE),
                ("OPTIONS", "/api/items", SAVING_SENTENCE)):
            with self.subTest(method=method, target=target):
                self.refused(self.ask(method, target), sentence)
        self.assertEqual(self.api.received, [])

    def test_a_refused_path_stays_refused_when_it_is_also_allowed(self):
        self.start(fill_relay(router_config(), allowed=RELAY_ALLOWED + ("/api/account/keys", "/api/items/[0-9]+/export")))
        self.refused(self.ask("GET", "/api/account/keys"), REFUSED_SENTENCE)
        self.refused(self.ask("GET", "/api/items/7/export"), REFUSED_SENTENCE)
        self.assertEqual(self.api.received, [])
        # The check sees the difference: without the refusal, the same path goes through.
        self.start(fill_relay(router_config(), allowed=RELAY_ALLOWED + ("/api/account/keys",), refused=("/api/none$",)))
        self.assertEqual(self.ask("GET", "/api/account/keys")[0], 200)

    def test_the_path_decided_on_is_the_normalized_path_that_is_sent(self):
        self.start()
        for target in ("/api/items/../account/keys", "/api/account%2Fkeys", "/api/session/../account/keys/",
                       "/api/items/7%2Fexport", "//api//account//keys", "/api/items/a%20b", "/api/items/%2e%2e/account/keys"):
            with self.subTest(target=target):
                self.refused(self.ask("GET", target), REFUSED_SENTENCE)
        # [^/]+ of an allowed path takes any character but "/"; the line
        # before the refused paths keeps what would need encoding again.
        for target in ("/api/tags/a%20b", "/api/tags/x%3Fy", "/api/tags/x%23y", "/api/tags/x%25y", "/api/tags/x%0Ay"):
            with self.subTest(target=target):
                self.refused(self.ask("GET", target), REFUSED_SENTENCE)
        self.assertEqual(self.api.received, [])
        code, _, body, _ = self.ask("GET", "/api/other/../items/7")
        self.assertEqual(code, 200, body)
        self.assertEqual(self.api.received[-1]["target"], "/api/items/7")
        self.assertEqual(self.ask("GET", "/api/tags/x-y_1.2~")[0], 200)
        self.assertEqual(self.api.received[-1]["target"], "/api/tags/x-y_1.2~")

    def test_the_query_is_none_or_the_form_listed_for_its_path(self):
        self.start()
        # The target sent, or None where the request is refused.
        for target, sent in (
                ("/api/projects/3/items", None),
                ("/api/projects/3/items?", None),
                ("/api/projects/3/items?owner=demo", "/api/projects/3/items?owner=demo"),
                ("/api/projects/3/items?owner=demo&page=2&limit=50", "/api/projects/3/items?owner=demo&page=2&limit=50"),
                ("/api/projects/3/items?owner=demo&q=abc", "/api/projects/3/items?owner=demo&q=abc"),
                ("/api/projects/3/items?owner=demo&q=abc&page=1", None),
                ("/api/projects/3/items?owner=demo&q=a%62c", None),
                ("/api/projects/3/items?owner=other", None),
                ("/api/projects/3/items?owner=demo&owner=other", None),
                ("/api/projects/3/items?owner=demo&page=1&page=2", None),
                ("/api/projects/3/items?owner=dem%6F", None),
                ("/api/projects/3/items?%6Fwner=demo", None),
                ("/api/projects/3/items?owner=demo&limit=5&page=1", None),
                ("/api/projects/3/items?owner=demo&sort=name", None),
                ("/api/projects/3/items?page=1", None),
                ("/api/items?page=1", None),
                ("/api/items?", "/api/items"),
                ("/api/session?x", None),
                # A query with a second "?" answers 403.
                ("/api/tags/3", "/api/tags/3"),
                ("/api/tags/3?owner=demo", "/api/tags/3?owner=demo"),
                ("/api/tags/3?owner=demo&q=abc", "/api/tags/3?owner=demo&q=abc"),
                ("/api/tags/3?owner=other&z=?owner=demo", None),
                ("/api/tags/3?sort=name?owner=demo", None),
                ("/api/tags/3?owner=other&z=?owner=demo&q=abc", None),
                ("/api/tags/3?owner=demo?", None),
                ("/api/tags/3??owner=demo", None),
                ("/api/projects/3/items?owner=other&z=?owner=demo", None)):
            with self.subTest(target=target):
                if sent:
                    code, _, body, _ = self.ask("GET", target)
                    self.assertEqual(code, 200, body)
                    self.assertEqual(self.api.received[-1]["target"], sent)
                else:
                    before = len(self.api.received)
                    self.refused(self.ask("GET", target), REFUSED_SENTENCE)
                    self.assertEqual(len(self.api.received), before)

    def test_a_second_question_mark_is_refused_with_both_query_fields_written(self):
        self.start(fill_relay(router_config(), id_query_paths=RELAY_ID_QUERY_PATHS + ("/api/tags/[^/]+",)))
        for target in ("/api/tags/3?owner=other&z=?owner=demo", "/api/tags/3??owner=demo",
                       "/api/tags/3?owner=demo&q=abc?"):
            with self.subTest(target=target):
                self.refused(self.ask("GET", target), REFUSED_SENTENCE)
        self.refused(self.ask("GET", "/api/tags/3"), REFUSED_SENTENCE)
        self.assertEqual(self.api.received, [])
        self.assertEqual(self.ask("GET", "/api/tags/3?owner=demo")[0], 200)

    def test_an_api_that_does_not_answer_leaves_no_address_or_query_in_the_error_log(self):
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            closed = probe.getsockname()[1]
        target = "/api/projects/3/items?owner=demo&page=2"

        def logged(config):
            self.start(config, api_port=closed)
            self.refused(self.ask("GET", target), "API に届きません", status=503)
            time.sleep(0.2)
            return "".join((self.base / name).read_text() for name in ("error.log", "nginx.stderr")
                           if (self.base / name).exists())
        config = fill_relay(router_config())
        written = logged(config)
        for word in ("owner=demo", "/api/projects/3", ":%d" % closed, RELAY_KEY):
            self.assertNotIn(word, written)
        self.assertIn('"GET /api/projects/3/items HTTP/1.1" 503', self.access_log())
        # The check sees it: without the line, the error log has the query.
        quiet = "      error_log stderr crit;\n"
        self.assertEqual(config.count(quiet), 1)
        self.assertIn("owner=demo", logged(config.replace(quiet, "")))

    def test_only_the_labels_that_match_whole_are_relayed(self):
        self.start()
        for host in ("sample-stg-example-12.preview.example.invalid", "sample-12.preview.example.invalid",
                     "other-1.preview.example.invalid", "xsample-example-12.preview.example.invalid",
                     "sample-example-12x.preview.example.invalid"):
            with self.subTest(host=host):
                code, kind, body, headers = self.ask("GET", "/api/items/7", host=host)
                self.assertEqual((code, json.loads(body)["from"]), (200, "pod"))
                self.assertNotIn("content-security-policy", headers)
        self.assertEqual(self.api.received, [])
        code, _, _, headers = self.ask("GET", "/index.html")
        self.assertEqual(code, 200)
        self.assertEqual(headers.get("content-security-policy"), "connect-src 'self'")

    def test_every_refusal_is_json_and_never_the_router_s_pages(self):
        self.start()
        self.refused(self.ask("GET", "/api/items/7", cookie=False), COOKIE_SENTENCE)
        self.refused(self.ask("GET", "/api/other", password=False, cookie=False), REFUSED_SENTENCE)
        self.refused(self.ask("GET", "/api/items/7", password=False, cookie=False), COOKIE_SENTENCE)
        code, kind, body, _ = self.ask("GET", "/api/status/401")
        self.assertEqual((code, kind, json.loads(body)["from"]), (401, "application/json", "api"))
        self.assertEqual(self.ask("GET", "/api/status/503")[:2], (503, "application/json"))
        self.assertEqual(json.loads(self.ask("GET", "/api/status/503")[2])["from"], "api")
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            closed = probe.getsockname()[1]
        self.start(api_port=closed)
        self.refused(self.ask("GET", "/api/items/7"), "API に届きません", status=503)
        self.start(key=None)
        self.refused(self.ask("GET", "/api/items/7"), "API に届きません", status=503)
        self.refused(self.ask("GET", "/api/other"), REFUSED_SENTENCE)

    def test_the_checks_see_the_difference_without_the_relay_s_pieces(self):
        """Positive controls: each piece the tests above rely on, taken out,
        changes what they see."""
        config = fill_relay(router_config())
        self.start(fill_relay(without_relay(router_config())))
        code, _, body, _ = self.ask("GET", "/api/items/7")
        self.assertEqual((code, json.loads(body)["from"]), (200, "pod"), "without the relay the Pod answers")
        bare = config
        for line in ("      error_page 403 = @relay_refused;\n", "      error_page 502 503 504 = @relay_down;\n"):
            self.assertEqual(bare.count(line), 1, line)
            bare = bare.replace(line, "")
        self.start(bare)
        code, kind, body, _ = self.ask("GET", "/api/other")
        self.assertEqual((code, kind), (403, "text/html"), "without the relay's error_page the router's page answers")
        self.assertIn("この画面は課題に投稿された URL から開いてください", body)
        self.start(config.replace('map "$request_method:$relay_path" $relay_refusal', 'map "$request_method:$uri" $relay_refusal', 1))
        self.refused(self.ask("GET", "/api/items/7"), REFUSED_SENTENCE)
        # Without the line that refuses a second "?", the same request is not refused.
        second = '    "~^[^?]*[?][^?]*[?]" "%s";\n' % REFUSED_SENTENCE
        self.assertEqual(config.count(second), 1)
        self.start(config.replace(second, ""))
        code, _, body, _ = self.ask("GET", "/api/tags/3?owner=other&z=?owner=demo")
        self.assertEqual(code, 200, body)
        self.assertEqual(self.api.received[-1]["target"], "/api/tags/3?owner=other&z=?owner=demo")
        # A path that needs its query, listed below the line that lets an
        # empty query through, lets one through.
        needs = '    "~^/api/projects/[0-9]+/items[?]$" "%s";\n' % REFUSED_SENTENCE
        empty = '    "~^[^?]*\\?$" "";\n'
        self.assertEqual((config.count(needs), config.count(empty)), (1, 1))
        self.start(config.replace(needs, "").replace(empty, empty + needs))
        self.assertEqual(self.ask("GET", "/api/projects/3/items")[0], 200)
        # The pages keep the password: auth_basic off in the location / or on
        # the server lets a page through without it.
        page_off = config.replace("    location / {\n", "    location / {\n      auth_basic off;\n")
        server_off = config.replace('    auth_basic "preview";\n', "    auth_basic off;\n")
        self.assertEqual((config.count("    location / {\n"), config.count('    auth_basic "preview";\n')), (1, 1))
        for name, changed in (("location /", page_off), ("server", server_off)):
            with self.subTest(auth_basic_off_in=name):
                self.start(changed)
                self.assertEqual(self.ask("GET", "/index.html", password=False)[0], 200)
        # Without the relay's auth_request, a request with the password and
        # no cookie reaches the API.
        start = config.index("    location = /_relay/api {")
        check = "      auth_request /_preview/check;\n"
        at = config.index(check, start)
        self.assertLess(at, config.index("    location / {", start))
        self.start(config[:at] + config[at + len(check):])
        code, _, body, _ = self.ask("GET", "/api/items/7", cookie=False)
        self.assertEqual(code, 200, body)
        # Without the relay's "auth_basic off;", a page's own Authorization,
        # or none, answers 401 on the relay's paths.
        off = "      auth_basic off;\n"
        self.assertEqual(config.count(off), 1)
        self.start(config.replace(off, ""))
        self.assertEqual(self.ask("GET", "/api/items/7", password=False, headers={"Authorization": "Bearer page-token"})[0], 401)
        self.assertEqual(self.ask("GET", "/api/items/7", password=False)[0], 401)


if __name__ == "__main__":
    unittest.main()
