#!/bin/sh
set -eu

export GOWORK=off
for module in apps/backend services/backup services/log-gateway services/stt shared/backup; do
    go -C "$module" "$@"
done
