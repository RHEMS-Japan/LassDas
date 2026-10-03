#!/bin/sh
set -eu
exec python3 -B "$(dirname "$0")/queue_helper.py" copy "$@"
