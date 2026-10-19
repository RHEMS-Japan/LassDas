# 0013. Dependency caches are shared only within an opted-in request

## Decision

The operator may enable intake.dependency_cache (default false). The watcher
then owns TASK_DEPENDENCY_CACHE, a dependency-cache sibling of that request's
workspace. The Linux launcher mounts only that directory writable, through
the existing no-symlink file-descriptor mounts, and redirects npm, pnpm and
Go caches there. Role homes, credentials, model state and checkouts retain
their existing boundaries. No other request or parent directory is mounted.
pnpm's store is selected with npm_config_store_dir, not PNPM_HOME, which would
also share global installs and executable directories.

## Alternatives and reason

Keeping separate caches repeats downloads and storage. Sharing installed
node_modules would also share mutable install state and native build outputs,
so it is not done. A machine-wide cache would cross request boundaries and
is also not done. No old data is migrated.

The cache is writable by roles of the same request and is untrusted input,
not a review result or proof that dependencies are safe. An operator requiring
dependency-cache isolation between those roles must leave the option off.
Existing writable-checkout and read-only Git metadata rules remain unchanged.
Normal finished-request cleanup removes an existing shared cache regardless of
the current option, before the old home-cleanup marker is checked. Turning the
option off therefore does not leave earlier caches behind. Requests that never
created a shared cache gain no cleanup writes or removals.

## Status and sources

Proposed in this change, following an offline local npm/Go size comparison.
`python3 -B harnesses/measure_dependency_cache.py` repeats the comparison;
`--pnpm /path/to/pnpm` also checks pnpm's store reuse and separate global installs.
The fixture uses only its local package server and temporary homes, not user
configuration or registry credentials.
See README "Optional request-local dependency caches" and RUNTIME "Linux role
launch". Real namespace checks still require Linux; portable command-construction
tests do not prove kernel confinement.
