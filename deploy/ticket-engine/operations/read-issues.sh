#!/bin/sh
set -eu
exec python3 -B "$(dirname "$0")/tracker_helper.py" read "$@"
