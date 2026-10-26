#!/bin/sh
set -eu
# Retry connection without echoing credentials. The job is safe to re-run.
attempt=0
until mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    echo 'MinIO did not become ready within 120 seconds' >&2
    exit 1
  fi
  sleep 2
done
mc mb --ignore-existing "local/$S3_BUCKET"
echo 'Artifact bucket ready'
