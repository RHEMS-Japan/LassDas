# The investigating designer's read-only identities

> The design document these files refer to lives at
> `docs/INVESTIGATING_DESIGNER.md` once the design pull request (#25) is
> merged; the section numbers below (§3.2, §3.3, §3.4, §8) are its.

These examples give the investigating designer (docs/INVESTIGATING_DESIGNER.md)
the only credentials it runs with. The role has no process of its own: the
kernel executes each `probe` with these identities, records what came back,
and refuses the shapes the catalogue does not declare. The identities are
what make "cannot write, cannot read secrets or customer content" a property
of the system rather than a sentence in a prompt (§3.3, layer 1).

| File | Identity | What it can read | What it can never do |
| --- | --- | --- | --- |
| `k8s-role.yaml` | a ServiceAccount with a namespaced Role | pods, pod logs, deployments, statefulsets, services, endpoints, events, ingresses (get/list/watch) | read `secrets` or `configmaps`, mint tokens, `exec` / `attach` / `port-forward` |
| `aws-policy.json` | an IAM role the kernel assumes | `Describe*` / `List*`, metric data | object and item contents, log bodies, secrets and parameters, decrypt, tokens and keys, task definitions and userData (Spot requests included), VPN customer-gateway configuration, Verified Access client secrets, function configuration |
| `postgres-readonly.sql` | a login role | `SELECT` on content-free views in one schema | any table directly, any function outside `pg_catalog` (EXECUTE revoked from PUBLIC in every schema the role can use, kept revoked for future functions; the catalog's own side-effect functions are refused by the kernel by name, §3.2), any write |

Placeholders (`<consumer-namespace>`, `<db>`, `<app_schema>`, …) are the only
instance-specific values. The files carry nothing that names an instance.

## What the catalogue may ask of the Kubernetes identity

`get` on `pods` and `deployments` returns the pod spec, and with it every
environment value written as a literal — the same class of exposure as
`configmaps`, which the Role leaves out. The consumer's probe catalogue
(§3.3) therefore uses only output shapes that carry no spec body over these
resources: `-o wide`, `custom-columns` over named fields, `rollout status`.
`-o yaml`, `-o json` and `describe` are not put in the catalogue.
`events` messages can carry fragments of a container's probe output as
well. What remains after this rule — a probe shape that returns a spec by
mistake — is caught by the kernel's secret scan alone (§3.3, layer 3); that
is the accepted residual.

## Where they live

Only the kernel process holds them (§3.4): the kubeconfig context, the AWS
profile and the DSN (`PROBE_DB_DSN`) are named in the consumer's probe
catalogue and exported to the kernel alone. The ServiceAccount's credential
reaches the kernel in one of two ways. Either the pod runs as
`lassdas-investigator` — the ServiceAccount then lives in the pod's
namespace, and the RoleBinding in each consumer namespace names it with that
namespace — and the pod spec mounts a projected service-account token
explicitly, with an `expirationSeconds` the kubelet renews; automount is
disabled on the ServiceAccount, so a pod receives the token only when its
spec asks for it. Or the operator mints a token of bounded lifetime
(`kubectl create token lassdas-investigator -n <ns> --duration=…`), writes
it into a kubeconfig, mounts that kubeconfig as a Secret for the kernel (as
the observation session's seed is mounted), and renews it before it expires.
The AWS role is assumed by the kernel's own source identity (the pod's role
or the operator's profile); the trust policy that names who may assume it is
the consumer's and is not part of these files. Until the agents run under a
separate UID (issue #23) another agent in the pod could read these files;
what it would gain is exactly what the identities allow — nothing writable,
nothing secret — which is why the design lets the investigation mode start
before #23 and gates the design mode on it.

## Stage 0: the eleven refusals

Apply the three identities to a staging environment, then run the checks
below with the investigator's credentials. Eight need the live environment;
three are refused by the kernel before anything runs and are pinned by unit
tests instead. Every row must end in the refusal named; a row that
succeeds means the identity is wrong and the role must not be enabled.

| # | Check | Expected refusal | Proved by |
| --- | --- | --- | --- |
| 1 | `kubectl --as=system:serviceaccount:<ns>:lassdas-investigator -n <ns> get secrets` | `Error from server (Forbidden)` | live |
| 2 | `kubectl --as=… -n <ns> exec <pod> -- true` | `Error from server (Forbidden)` on `pods/exec` | live |
| 3 | `aws secretsmanager get-secret-value --secret-id <any>` | `AccessDeniedException` (explicit deny) | live |
| 4 | `aws s3api get-object --bucket <any> --key <any> out` | `AccessDenied` | live |
| 5 | `SELECT * FROM <app_schema>.<table> LIMIT 1` | `permission denied for table <table>` when the role has USAGE on `<app_schema>` (it does when `<app_schema>` is `public`); `permission denied for schema <app_schema>` when it does not | live |
| 6 | `BEGIN; SET TRANSACTION READ WRITE; UPDATE <app_schema>.<table> SET updated_at = now() WHERE false; ROLLBACK;` | `SET TRANSACTION` succeeds; the `UPDATE` fails with `permission denied for table <table>` (or `for schema <app_schema>`, when the role lacks USAGE on it) — the grant is the guard, not the setting. `cannot execute UPDATE in a read-only transaction` means the setting was still on and the grant was never reached: a standalone `SET transaction_read_only = off` has no effect on the next statement | live |
| 7 | probe `sql.read` with `SELECT 1; UPDATE …` | refused by the kernel: one statement only | `internal/probe` `TestSQLProbeSendsOneReadStatement` |
| 8 | probe `sql.read` with `EXPLAIN ANALYZE DELETE FROM …` | refused by the kernel: `EXPLAIN` is not `SELECT` | `internal/probe` `TestSQLProbeSendsOneReadStatement` |
| 9 | `SELECT dblink_exec('…', 'UPDATE …')` | `permission denied for function` (or the function does not exist) | live |
| 10 | `SELECT public.<writer_fn>()` (or `lassdas_ro.<writer_fn>()`) — `<writer_fn>` is a `SECURITY DEFINER` function that writes, created after the script ran by a role that creates functions (`<migration_role>` in `public`, or `<owner_role>` in `lassdas_ro`), so it sits in a schema the investigator has USAGE on and the default privileges are what is tested | `permission denied for function <writer_fn>`. `permission denied for schema …` or `function … does not exist` means the function sits in a schema the role cannot use, and the row proved nothing about EXECUTE | live |
| 11 | probe `http.timing` against `http://169.254.169.254/` | refused by the kernel at dial time: link-local | `internal/probe` `TestCatalogRefusesOutOfShapeRequests` |

Record the eleven outcomes (command, output, timestamp) with the consumer's
change that applied the identities. Rows 7, 8 and 11 arrive with the probe
package (issue #28); until it lands, the table stands at eight live rows.

## Production

The production identities are built from the same files with the §8 boundary
applied: the views expose counts, timestamps and ids only; pod logs are
limited to namespaces whose containers carry no request bodies; the AWS
policy's Deny list already covers log bodies and stored objects.
