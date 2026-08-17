#!/bin/sh

set -eu

if [ "$#" -ne 1 ]; then
	printf 'usage: %s OUTPUT_DIRECTORY\n' "$0" >&2
	exit 2
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
remote_url=$(git -C "$repository_root" config --get remote.origin.url)
repository_label=${remote_url%.git}
repository_label=${repository_label##*/}

case "$repository_label" in
	"" | *[!A-Za-z0-9._-]*)
		printf 'packaging failed: canonical repository label is invalid\n' >&2
		exit 1
		;;
esac

mkdir -p -- "$1"
output_directory=$(CDPATH='' cd -- "$1" && pwd)
work_directory=$(mktemp -d "${TMPDIR:-/tmp}/ticket-ingress-lambda.XXXXXX")
trap 'rm -rf -- "$work_directory"' EXIT HUP INT TERM
archive_name=ticket-ingress-lambda-arm64.zip

(
	cd -- "$repository_root"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-trimpath \
		-buildvcs=false \
		-ldflags='-s -w -buildid=' \
		-o "$work_directory/bootstrap" \
		./cmd/lambda
)

chmod 0755 "$work_directory/bootstrap"

if ! strings "$work_directory/bootstrap" > "$work_directory/bootstrap.strings"; then
	printf 'packaging failed: Lambda binary could not be inspected\n' >&2
	exit 1
fi

if LC_ALL=C grep -F -i -q "$repository_root" "$work_directory/bootstrap.strings" ||
	LC_ALL=C grep -F -i -q "$repository_label" "$work_directory/bootstrap.strings"; then
	printf 'packaging failed: Lambda binary contains a forbidden repository identity\n' >&2
	exit 1
fi

(
	cd -- "$work_directory"
	zip -q -X "$work_directory/$archive_name" bootstrap
)

archive_entries=$(unzip -Z1 "$work_directory/$archive_name")
if ! unzip -p "$work_directory/$archive_name" bootstrap > "$work_directory/archive-bootstrap" ||
	! cmp -s "$work_directory/bootstrap" "$work_directory/archive-bootstrap" ||
	! strings "$work_directory/archive-bootstrap" > "$work_directory/archive-bootstrap.strings"; then
	printf 'packaging failed: Lambda archive could not be inspected\n' >&2
	exit 1
fi

if [ "$archive_entries" != "bootstrap" ] ||
	LC_ALL=C grep -F -i -q "$repository_root" "$work_directory/archive-bootstrap.strings" ||
	LC_ALL=C grep -F -i -q "$repository_label" "$work_directory/archive-bootstrap.strings"; then
	printf 'packaging failed: Lambda archive contents are invalid\n' >&2
	exit 1
fi

mv -f -- "$work_directory/$archive_name" "$output_directory/$archive_name"
shasum -a 256 "$output_directory/$archive_name"
