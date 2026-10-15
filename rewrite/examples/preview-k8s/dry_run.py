#!/usr/bin/python3
"""Check the admission policy of this example on a cluster, with server-side
dry runs only: nothing is created, changed or removed.

  dry_run.py --kubeconfig FILE --context NAME --namespace PREVIEW-NAMESPACE
             --net-image IMAGE@sha256:DIGEST
             [--admin-kubeconfig FILE --admin-context NAME]
             [--other-namespace NAME]
  dry_run.py --write DIRECTORY --namespace PREVIEW-NAMESPACE --net-image IMAGE@sha256:DIGEST

With --kubeconfig, the identity of the preview commands (the token of
preview-operator) sends, one at a time:

  - the Deployment and the Service preview.py builds, with and without the
    dependency step: these have to pass, or the policy is written wrong;
  - every shape the policy has to refuse (the list below): each has to be
    refused, and refused by the named policy. A refusal for another reason
    (RBAC, the API's own validation, the quota) did not test the policy and
    counts as a failure;
  - kubectl auth can-i for what the identity must not do (create Pods,
    Ingresses, Secrets, ConfigMaps, ServiceAccounts, Roles; anything in
    another namespace) and for what it must (create Deployments and Services,
    exec into Pods).

With --admin-kubeconfig, an identity that may create Pods also sends the
Pod-level cases: the Pod policy is the last line, met when a ReplicaSet
creates a Pod, and the preview identity cannot create Pods to try it.

--write writes every case's object to DIRECTORY as JSON and contacts no
cluster, to read what would be sent.

Both kubeconfig files are named explicitly and only they are used: the
default kubeconfig and KUBECONFIG of the caller are never read.
Exit status: 0 when every check came out as expected, 1 otherwise, 2 for a
wrong command line.
"""
import argparse
import collections
import copy
import datetime
import importlib.util
import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
DEPLOYMENT_POLICY = "preview-deployment-shape"
POD_POLICY = "preview-pod-shape"
SERVICE_POLICY = "preview-service-shape"

Case = collections.namedtuple("Case", "label kind install change policy message")


def load_preview():
    specification = importlib.util.spec_from_file_location("preview", os.path.join(HERE, "preview.py"))
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


def built(preview, namespace, net_image, install):
    """The Deployment and the Service preview.py sends, for a made-up request."""
    settings = preview.Settings(
        workspace="/unused", result="/unused", issue="1", site="dry-run", start="sleep 3600", port=8080,
        ready_path="/", lifetime=1, image="registry.example.com/app:1", memory="64Mi",
        install="true" if install else None, install_seconds=60, env_file=None, domain="preview.example.invalid",
        namespace=namespace, server="https://unused", ca_file="/unused", net_image=net_image, wait_seconds=60)
    names = preview.preview_names("dry-run", "1", "preview.example.invalid")
    expires = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)
    deployment, service = preview.manifests(settings, names, [], expires)
    deployment["spec"]["template"]["metadata"]["annotations"][preview.CHANGE] = "0" * 12
    return deployment, service


def pod_spec(item):
    return item["spec"]["template"]["spec"] if item["kind"] == "Deployment" else item["spec"]


def container(spec, name):
    for entry in spec.get("initContainers", []) + spec.get("containers", []):
        if entry["name"] == name:
            return entry
    raise KeyError(name)


def extra(spec, name):
    """Another init container shaped like receive."""
    entry = copy.deepcopy(container(spec, "receive"))
    entry["name"] = name
    return entry


def change(function):
    """A case's change, applied to the Pod spec of a Deployment or a Pod."""
    def apply(item):
        function(pod_spec(item))
    return apply


def set_install(command, args=None):
    def apply(spec):
        install = container(spec, "install")
        install["command"] = command
        if args is not None:
            install["args"] = args
    return change(apply)


def privileged(spec):
    context = container(spec, "app")["securityContext"]
    del context["allowPrivilegeEscalation"]  # the API refuses privileged with it false
    context["privileged"] = True


def host_network(spec):
    spec["hostNetwork"] = True
    spec["hostUsers"] = True  # the API refuses a host namespace with a user namespace


def insert_init(index, name):
    def apply(spec):
        spec["initContainers"].insert(index if index >= 0 else len(spec["initContainers"]), extra(spec, name))
    return change(apply)


def run_rules(spec):
    container(spec, "net-install")["command"] = ["iptables-restore", "--wait", "5", "/policy/run.rules"]


def without(*path):
    def apply(spec):
        target = spec
        for key in path[:-1]:
            target = target[key]
        del target[path[-1]]
    return change(apply)


def setting(value, *path):
    def apply(spec):
        target = spec
        for key in path[:-1]:
            target = target[key] if not isinstance(key, str) or not key.startswith("@") else container(target, key[1:])
        target[path[-1]] = value
    return change(apply)


def cases():
    """What the policy has to refuse (and what it has to let through), as the
    design's list 9.5 names them, and a few more."""
    D, S = DEPLOYMENT_POLICY, SERVICE_POLICY
    found = [
        Case("preview.py's Deployment without the dependency step", "deployment", False, None, None, None),
        Case("preview.py's Deployment with the dependency step", "deployment", True, None, None, None),
        Case("preview.py's Service", "service", False, None, None, None),
        Case("a privileged container", "deployment", False, change(privileged), D, "privileged"),
        Case("a hostPath volume", "deployment", False,
             change(lambda s: s["volumes"].append({"name": "host", "hostPath": {"path": "/"}})), D, "volumes"),
        Case("hostNetwork", "deployment", False, change(host_network), D, "hostNetwork"),
        Case("hostPID", "deployment", False,
             change(lambda s: s.update(hostPID=True, hostUsers=True)), D, "hostPID"),
        Case("a hostPort", "deployment", False, setting(8080, "@app", "ports", 0, "hostPort"), D, "hostPort"),
        Case("nodeName", "deployment", False, setting("node-1", "nodeName"), D, "nodeName"),
        Case("a toleration of another taint (an engine-only node pool)", "deployment", False,
             setting([{"key": "dedicated", "operator": "Equal", "value": "engine", "effect": "NoSchedule"}],
                     "tolerations"), D, "toleration"),
        Case("a toleration without a key (tolerates every taint)", "deployment", False,
             setting([{"operator": "Exists"}], "tolerations"), D, "toleration"),
        Case("affinity", "deployment", False,
             setting({"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                 {"matchExpressions": [{"key": "node-pool", "operator": "In", "values": ["engine"]}]}]}}},
                     "affinity"), D, "affinity"),
        Case("a nodeSelector of another label", "deployment", False,
             setting({"node-pool": "engine"}, "nodeSelector"), D, "nodeSelector"),
        Case("NET_ADMIN in a container other than the rules containers", "deployment", False,
             setting({"drop": ["ALL"], "add": ["NET_ADMIN"]}, "@app", "securityContext", "capabilities"), D,
             "capability"),
        Case("net with another image", "deployment", False,
             setting("registry.example.com/other@sha256:" + "1" * 64, "@net", "image"), D, "rules image"),
        Case("net with another command", "deployment", False,
             setting(["sh", "-c", "exit 0"], "@net", "command"), D, "rules image"),
        Case("net with args", "deployment", False, setting(["--noflush"], "@net", "args"), D, "rules image"),
        Case("net-install with net's command", "deployment", True, change(run_rules), D, "rules image"),
        Case("the init containers without net", "deployment", False,
             change(lambda s: s["initContainers"].pop()), D, "init containers"),
        Case("an init container after net", "deployment", False, insert_init(-1, "late"), D, "init containers"),
        Case("an init container before net-hold", "deployment", False, insert_init(0, "early"), D,
             "init containers"),
        Case("an init container between receive and net", "deployment", False, insert_init(2, "between"), D,
             "init containers"),
        Case("an init container with restartPolicy Always", "deployment", False,
             setting("Always", "@receive", "restartPolicy"), D, "restartPolicy"),
        Case("install without timeout", "deployment", True, set_install(["sh", "-c", "true"]), D, "install"),
        Case("install without -s KILL", "deployment", True, set_install(["timeout", "60", "sh", "-c", "true"]), D,
             "install"),
        Case("install with -s TERM", "deployment", True,
             set_install(["timeout", "-s", "TERM", "60", "sh", "-c", "true"]), D, "install"),
        Case("install for 0 seconds", "deployment", True,
             set_install(["timeout", "-s", "KILL", "0", "sh", "-c", "true"]), D, "install"),
        Case("install for 481 seconds", "deployment", True,
             set_install(["timeout", "-s", "KILL", "481", "sh", "-c", "true"]), D, "install"),
        Case("install for 30s", "deployment", True,
             set_install(["timeout", "-s", "KILL", "30s", "sh", "-c", "true"]), D, "install"),
        Case("install with args", "deployment", True,
             set_install(["timeout", "-s", "KILL", "60", "sh", "-c", "true"], ["extra"]), D, "install"),
        Case("no seccompProfile on the Pod", "deployment", False, without("securityContext", "seccompProfile"), D,
             "seccompProfile"),
        Case("seccompProfile Unconfined on the Pod", "deployment", False,
             setting({"type": "Unconfined"}, "securityContext", "seccompProfile"), D, "seccompProfile"),
        Case("seccompProfile Unconfined on a container", "deployment", False,
             setting({"type": "Unconfined"}, "@app", "securityContext", "seccompProfile"), D, "seccompProfile"),
        Case("procMount Unmasked", "deployment", False, setting("Unmasked", "@app", "securityContext", "procMount"),
             D, "procMount"),
        Case("priorityClassName system-node-critical", "deployment", False,
             setting("system-node-critical", "priorityClassName"), D, "priorityClassName"),
        Case("a priority of 1000000", "deployment", False, setting(1000000, "priority"), D, "priority"),
        Case("no terminationGracePeriodSeconds (30 by default)", "deployment", False,
             without("terminationGracePeriodSeconds"), D, "terminationGracePeriodSeconds"),
        Case("terminationGracePeriodSeconds 30", "deployment", False, setting(30, "terminationGracePeriodSeconds"),
             D, "terminationGracePeriodSeconds"),
        Case("sysctls", "deployment", False,
             setting([{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}], "securityContext", "sysctls"),
             D, "sysctls"),
        Case("seLinuxOptions", "deployment", False,
             setting({"type": "spc_t"}, "securityContext", "seLinuxOptions"), D, "seLinuxOptions"),
        Case("AppArmor Unconfined on the Pod", "deployment", False,
             setting({"type": "Unconfined"}, "securityContext", "appArmorProfile"), D, "AppArmor"),
        Case("AppArmor Unconfined on a container", "deployment", False,
             setting({"type": "Unconfined"}, "@app", "securityContext", "appArmorProfile"), D, "AppArmor"),
        Case("resourceClaims", "deployment", False,
             setting([{"name": "claim", "resourceClaimTemplateName": "any"}], "resourceClaims"), D, "resourceClaims"),
        Case("a projected ServiceAccount token volume", "deployment", False,
             change(lambda s: s["volumes"].append({"name": "token", "projected": {"sources": [
                 {"serviceAccountToken": {"path": "token"}}]}})), D, "volumes"),
        Case("a Secret volume", "deployment", False,
             change(lambda s: s["volumes"].append({"name": "keys", "secret": {"secretName": "any"}})), D, "volumes"),
        Case("an environment value from a Secret", "deployment", False,
             change(lambda s: container(s, "app")["env"].append(
                 {"name": "KEY", "valueFrom": {"secretKeyRef": {"name": "any", "key": "key"}}})), D, "Secret"),
        Case("an environment from a Secret", "deployment", False,
             setting([{"secretRef": {"name": "any"}}], "@app", "envFrom"), D, "Secret"),
        Case("no hostUsers", "deployment", False, without("hostUsers"), D, "hostUsers"),
        Case("automountServiceAccountToken true", "deployment", False,
             setting(True, "automountServiceAccountToken"), D, "automountServiceAccountToken"),
        Case("another ServiceAccount", "deployment", False, setting("other", "serviceAccountName"), D,
             "ServiceAccount"),
        Case("a runtimeClassName", "deployment", False, setting("other", "runtimeClassName"), D, "runtimeClassName"),
        Case("an application container named net-install", "deployment", False,
             setting("net-install", "@app", "name"), D, "init container names"),
        Case("an application running as root", "deployment", False,
             setting(False, "@app", "securityContext", "runAsNonRoot"), D, "runAsNonRoot"),
        Case("a LoadBalancer Service", "service", False, lambda item: item["spec"].update(type="LoadBalancer"), S,
             "ClusterIP"),
        Case("a NodePort Service", "service", False, lambda item: item["spec"].update(type="NodePort"), S,
             "ClusterIP"),
        Case("an ExternalName Service", "service", False,
             lambda item: item["spec"].update(type="ExternalName", externalName="internal.example.com"), S,
             "ClusterIP"),
        Case("a Service with externalIPs", "service", False,
             lambda item: item["spec"].update(externalIPs=["192.0.2.10"]), S, "externalIPs"),
        Case("a Service without a selector", "service", False, lambda item: item["spec"].pop("selector"), S,
             "select"),
        Case("a Service with an external-dns hostname", "service", False,
             lambda item: item["metadata"].update(annotations={
                 "external-dns.alpha.kubernetes.io/hostname": "preview.example.invalid"}), S, "annotations"),
        Case("a Service with a load balancer annotation", "service", False,
             lambda item: item["metadata"].update(annotations={
                 "service.beta.kubernetes.io/aws-load-balancer-scheme": "internet-facing"}), S, "annotations"),
        Case("preview.py's Pod (as a ReplicaSet would create it)", "pod", False, None, None, None),
        Case("a privileged Pod", "pod", False, change(privileged), POD_POLICY, "privileged"),
        Case("a Pod with an init container before the rules", "pod", False, insert_init(0, "early"), POD_POLICY,
             "init containers"),
        Case("a Pod without seccompProfile", "pod", False, without("securityContext", "seccompProfile"),
             POD_POLICY, "seccompProfile"),
    ]
    return found


def obj(preview, case, namespace, net_image):
    deployment, service = built(preview, namespace, net_image, case.install)
    if case.kind == "service":
        item = service
    elif case.kind == "pod":
        template = deployment["spec"]["template"]
        item = {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": "dry-run-pod", "namespace": namespace,
                             "labels": dict(template["metadata"]["labels"]),
                             "annotations": dict(template["metadata"]["annotations"])},
                "spec": copy.deepcopy(template["spec"])}
    else:
        item = deployment
    if case.change is not None:
        case.change(item)
    return item


class Kubectl:
    def __init__(self, kubeconfig, context, namespace, home):
        self.base = ["kubectl", "--kubeconfig", kubeconfig, "--context", context, "--namespace", namespace]
        self.environment = {"PATH": os.environ.get("PATH", os.defpath), "LANG": "C.UTF-8", "HOME": home,
                            "KUBECONFIG": kubeconfig}

    def run(self, arguments, data=None):
        done = subprocess.run(self.base + arguments, input=data or b"", capture_output=True, timeout=120,
                              env=self.environment)
        return done.returncode, (done.stdout + done.stderr).decode("utf-8", "replace").strip()

    def dry_run(self, item):
        return self.run(["apply", "--server-side", "--dry-run=server", "--field-manager=preview-dry-run", "-f", "-"],
                        json.dumps(item).encode())


def judge(case, status, output):
    if case.policy is None:
        return status == 0, "accepted" if status == 0 else "refused: " + output.splitlines()[-1][:400]
    if status == 0:
        return False, "accepted, but the policy has to refuse it"
    named = "ValidatingAdmissionPolicy '%s'" % case.policy
    line = next((entry for entry in output.splitlines() if "ValidatingAdmissionPolicy" in entry), output)
    if named not in output:
        return False, "refused, but not by %s: %s" % (case.policy, line[:400])
    if case.message and case.message not in line:
        return False, "refused by %s, but not for %r: %s" % (case.policy, case.message, line[:400])
    return True, "refused by %s" % case.policy


PERMISSIONS = [
    # (verb, resource, namespace key, expected answer)
    ("create", "deployments.apps", "preview", "yes"),
    ("patch", "deployments.apps", "preview", "yes"),
    ("delete", "services", "preview", "yes"),
    ("create", "pods/exec", "preview", "yes"),
    ("create", "pods", "preview", "no"),
    ("create", "ingresses.networking.k8s.io", "preview", "no"),
    ("create", "secrets", "preview", "no"),
    ("get", "secrets", "preview", "no"),
    ("create", "configmaps", "preview", "no"),
    ("create", "serviceaccounts", "preview", "no"),
    ("create", "roles.rbac.authorization.k8s.io", "preview", "no"),
    ("patch", "pods/ephemeralcontainers", "preview", "no"),
    ("create", "deployments.apps", "other", "no"),
    ("create", "services", "other", "no"),
]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--kubeconfig")
    parser.add_argument("--context")
    parser.add_argument("--admin-kubeconfig")
    parser.add_argument("--admin-context")
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--other-namespace", default="default")
    parser.add_argument("--net-image", required=True)
    parser.add_argument("--write")
    arguments = parser.parse_args(argv)
    preview = load_preview()
    every = cases()
    if arguments.write:
        os.makedirs(arguments.write, exist_ok=True)
        for index, case in enumerate(every):
            name = "%02d-%s-%s.json" % (index, case.kind, "".join(ch if ch.isalnum() else "-" for ch in case.label)[:60])
            with open(os.path.join(arguments.write, name), "w") as handle:
                json.dump({"case": case.label, "expect": case.policy or "accepted", "message": case.message,
                           "object": obj(preview, case, arguments.namespace, arguments.net_image)}, handle, indent=2)
        print("wrote %d cases to %s" % (len(every), arguments.write))
        return 0
    if not arguments.kubeconfig or not arguments.context:
        parser.error("--kubeconfig and --context are required unless --write is given")
    if bool(arguments.admin_kubeconfig) != bool(arguments.admin_context):
        parser.error("--admin-kubeconfig and --admin-context go together")
    failures = 0
    with tempfile.TemporaryDirectory(prefix="preview-dry-run-") as home:
        preview_identity = Kubectl(arguments.kubeconfig, arguments.context, arguments.namespace, home)
        admin = Kubectl(arguments.admin_kubeconfig, arguments.admin_context, arguments.namespace, home) \
            if arguments.admin_kubeconfig else None
        for case in every:
            sender = admin if case.kind == "pod" else preview_identity
            if sender is None:
                print("SKIP  %s: needs --admin-kubeconfig" % case.label)
                continue
            status, output = sender.dry_run(obj(preview, case, arguments.namespace, arguments.net_image))
            passed, detail = judge(case, status, output)
            failures += not passed
            print("%s  %s: %s" % ("PASS" if passed else "FAIL", case.label, detail), flush=True)
        for verb, resource, where, expected in PERMISSIONS:
            namespace = arguments.namespace if where == "preview" else arguments.other_namespace
            subresource = []
            if "/" in resource:
                resource, sub = resource.split("/", 1)
                subresource = ["--subresource=" + sub]
            _, answer = preview_identity.run(["auth", "can-i", verb, resource, *subresource,
                                              "--namespace=" + namespace])
            answer = answer.splitlines()[0].strip() if answer else ""
            passed = answer == expected
            failures += not passed
            print("%s  can-i %s %s%s in %s: %s (expected %s)" % (
                "PASS" if passed else "FAIL", verb, resource, "/" + subresource[0].split("=")[1] if subresource else "",
                namespace, answer or "no answer", expected), flush=True)
    print("%d check(s) failed" % failures if failures else "every check came out as expected")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
