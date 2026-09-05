# The investigating designer's read-only identities

These examples give the investigating designer (docs/INVESTIGATING_DESIGNER.md)
the only credentials it runs with. The role has no process of its own: the
kernel executes each `probe` with these identities, records what came back,
and refuses the shapes the catalogue does not declare. The identities are
what make "cannot write, cannot read secrets or customer content" a property
of the system rather than a sentence in a prompt (§3.3, layer 1).

| File | Identity | What it can read | What it can never do |
| --- | --- | --- | --- |
| `k8s-role.yaml` | a ServiceAccount with a namespaced Role | pods, pod logs, deployments, statefulsets, replicasets, services, endpoints, events, ingresses (get/list/watch) | read `secrets` or `configmaps`, mint tokens, `exec` / `attach` / `port-forward` |
| `aws-policy.json` | an IAM role the kernel assumes | `Describe*` / `List*`, metric data | object and item contents, log bodies, secrets and parameters, decrypt, tokens and keys, task definitions and userData, function configuration |
| `postgres-readonly.sql` | a login role | `SELECT` on content-free views in one schema | any table directly, any function (EXECUTE revoked from PUBLIC, defaults kept revoked), any write |

Placeholders (`<consumer-namespace>`, `<db>`, `<app_schema>`, …) are the only
instance-specific values. The files carry nothing that names an instance.

## Where they live

Only the kernel process holds them (§3.4): the kubeconfig context, the AWS
profile and the DSN (`PROBE_DB_DSN`) are named in the consumer's probe
catalogue and exported to the kernel alone. Until the agents run under a
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
| 5 | `SELECT * FROM <app_schema>.<table> LIMIT 1` | `permission denied for table` | live |
| 6 | `SET transaction_read_only = off;` then `UPDATE <app_schema>.<table> SET updated_at = now() WHERE false` | the `SET` succeeds; the `UPDATE` fails with `permission denied for table` — the grant is the guard, not the setting | live |
| 7 | probe `sql.read` with `SELECT 1; UPDATE …` | refused by the kernel: one statement only | `internal/probe` `TestSQLProbeSendsOneReadStatement` |
| 8 | probe `sql.read` with `EXPLAIN ANALYZE DELETE FROM …` | refused by the kernel: `EXPLAIN` is not `SELECT` | `internal/probe` `TestSQLProbeSendsOneReadStatement` |
| 9 | `SELECT dblink_exec('…', 'UPDATE …')` | `permission denied for function` (or the function does not exist) | live |
| 10 | `SELECT <writer_fn>()` where `<writer_fn>` is a `SECURITY DEFINER` function that writes | `permission denied for function` | live |
| 11 | probe `http.timing` against `http://169.254.169.254/` | refused by the kernel at dial time: link-local | `internal/probe` `TestCatalogRefusesOutOfShapeRequests` |

Record the eleven outcomes (command, output, timestamp) with the consumer's
change that applied the identities. Rows 7, 8 and 11 arrive with the probe
package (issue #28); until it lands, the table stands at eight live rows.

## Production

The production identities are built from the same files with the §8 boundary
applied: the views expose counts, timestamps and ids only; pod logs are
limited to namespaces whose containers carry no request bodies; the AWS
policy's Deny list already covers log bodies and stored objects.
