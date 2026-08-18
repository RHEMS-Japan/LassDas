#!/bin/sh
# LassDas setup - thin launcher. The real wizard is Go (cmd/setup): checks
# arrive first, the interview and provisioning run there.
set -eu
cd "$(dirname "$0")"
if ! command -v go >/dev/null 2>&1; then
	printf 'go が見つかりません。https://go.dev/dl/ から入れてから再実行してください。\n' >&2
	exit 1
fi
exec go run ./cmd/setup "$@"
