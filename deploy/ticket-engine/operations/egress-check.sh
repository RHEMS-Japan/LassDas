#!/bin/sh
set -eu
exec python3 -B "$(dirname "$0")/network_probe.py" egress "$@"
