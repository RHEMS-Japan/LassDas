#!/bin/sh
# 旧 setup の互換入口。新しいローカル init へ引き継ぐ。
set -eu
lassdas_init_repo_root=$(pwd)
cd "$(dirname "$0")"
if ! command -v go >/dev/null 2>&1; then
	printf 'go が見つかりません。https://go.dev/dl/ から入れてから再実行してください。\n' >&2
	exit 1
fi
printf 'setup は init に移りました。次回から lassdas init を使えます。\n' >&2
exec go run ./cmd/lassdas init --repo-root "$lassdas_init_repo_root" "$@"
