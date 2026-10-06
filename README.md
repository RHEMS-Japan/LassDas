# LassDas

Configure a project's workflow, then delegate a ticket through that workflow to
its agreed delivery destination. Roles can investigate, implement, review, ask
questions and report; the operator supplies the tools, permissions and completion
conditions.

The current engine lives in [rewrite/](rewrite/README.md). The previous runner,
its installer and its reusable workflow are no longer part of this source tree.
Their history remains in Git. This is a source retirement, not a migration of
running installations or saved requests.

## Start here

- [Build and start a local bundle](rewrite/START.md).
- [Configure a Kubernetes installation](deploy/ticket-engine/SETUP.md).
- [Engine configuration and observed behavior](rewrite/README.md).
- [Runtime isolation and prerequisites](rewrite/RUNTIME.md).
- [Glossary of the engine's terms](rewrite/GLOSSARY.md).
- [Current development handover](docs/HANDOVER.md).
- [Product direction](docs/PRODUCT_DIRECTION.md).

A new installation needs an explicitly scoped Backlog or GitHub Issues tracker,
a source repository, model endpoints, role tools, and an agreed delivery and
verification procedure. The examples deliberately refuse unedited intake.
Credentials stay outside the repository. A built image or a passing configuration
check is not evidence that a real request has been delivered.

Controller notices and native stop instructions use Japanese; role prompts and
their outputs follow the configured workflow. Confirm the chosen language,
permissions and complete delivery path with a small authorized test request.

## Build and test

The only Go module is in `rewrite/`; use its own Go version requirement.

```sh
GOMAXPROCS=2 go -C rewrite build -p 1 ./...
GOMAXPROCS=2 go -C rewrite vet -p 1 ./...
GOMAXPROCS=2 go -C rewrite test -count=1 -p 1 ./...
cd rewrite
GOMAXPROCS=2 GOFLAGS=-p=1 python3 -B -m unittest discover -s harnesses -p 'test_[a-d]*.py'
GOMAXPROCS=2 GOFLAGS=-p=1 python3 -B -m unittest discover -s harnesses -p 'test_[!a-d]*.py'
```

For shorter foreground runs, split the Python patterns into `test_a*.py`, `test_[b-d]*.py`, `test_[e-v]*.py` and `test_[w-z]*.py`, run them serially and check every exit status; the last group includes the full-suite check without personal settings and must not be omitted.

The [runtime Dockerfile](deploy/pod/Dockerfile) packages this module and its
configured-command tools. Pull requests build and start the packaged engine
without a service connection. The main image workflow publishes an image and,
after verifying anonymous access, writes [the distributor's note](docs/DISTRIBUTION.json).
That note identifies a built image and its source; it does not certify any
installation's rollout.

## Development

This is an evolving project. Do not add backward-compatibility or data-migration
layers. Update the documentation first, write a meaningful failing test, implement
the smallest change, and remove obsolete code and conflicting documentation.
Existing installations and their data require separately authorized operational
changes. No claim of arbitrary unattended completion follows from local tests.

CI also reads every tracked file and the pushed commit messages for the shapes of credentials: GitHub and Slack tokens, OpenRouter, Anthropic and AWS keys, private key blocks, and `apiKey` or `api_key` values ([the scan](.github/scripts/scan-credential-shapes.py)).
A match is named by file or commit and line, never by its text; `python3 .github/scripts/scan-credential-shapes.py tree` runs the same check before a push.
On a match, remove the value and rewrite the commits that carry it, then revoke the key and create a new one: a key that was pushed counts as exposed even after the history is rewritten.
