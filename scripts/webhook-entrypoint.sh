#!/bin/sh
set -eu
dir="${FW_WEBHOOK_DIR:-/opt/family-workspace}"
git config --global --add safe.directory "$dir"
cd "$dir"
exec /usr/local/bin/github-webhook
