#!/bin/sh
set -eu

export GOWORK=off
for module in apps/acp apps/backend services/backup services/log-gateway services/stt shared/backup shared/codemode; do
    go -C "$module" "$@"
done
