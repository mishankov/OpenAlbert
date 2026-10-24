#!/usr/bin/env bash
# Run only disposable containers. Never start the Telegram poller or fetch models.
set -euo pipefail

component=${1:?component is required}
image=${2:?image is required}

case "$component" in
  openalbert)
    docker run --rm --entrypoint /bin/sh "$image" -ec \
      'test -x /usr/local/bin/openalbert; test "$(id -u)" = 10001; chromium --version'
    ;;
  stt)
    docker run --rm --entrypoint /bin/sh "$image" -ec \
      'test -x /usr/local/bin/stt-proxy; ffmpeg -version >/dev/null; /opt/nemo/bin/nemo-speech --help >/dev/null; python3 -c "import ast; ast.parse(open(\"/usr/local/bin/stt-entrypoint\").read())"'
    ;;
  log-gateway)
    container=$(docker run -d "$image")
    trap 'docker rm -f "$container" >/dev/null' EXIT
    for ((attempt=0; attempt<30; attempt++)); do
      if docker exec "$container" wget -qO- http://127.0.0.1:9429/healthz; then
        exit 0
      fi
      sleep 1
    done
    docker logs "$container"
    exit 1
    ;;
  minio)
    docker run --rm "$image" --version
    ;;
  minio-init)
    docker run --rm --entrypoint mc "$image" --version
    # Exercise the initializer twice and OpenAlbert's real S3 contract against the
    # same server source used by the published image, on each architecture.
    docker build -f deploy/minio.Dockerfile --target minio -t openalbert-ci:minio-smoke .
    network="openalbert-ci-$(date +%s)-$$"
    container="${network}-minio"
    cleanup() {
      docker rm -f "$container" >/dev/null 2>&1 || true
      docker network rm "$network" >/dev/null 2>&1 || true
    }
    trap cleanup EXIT
    docker network create "$network" >/dev/null
    docker run -d --name "$container" --network "$network" --network-alias minio \
      -p 127.0.0.1::9000 \
      -e MINIO_ROOT_USER=openalbert-ci -e MINIO_ROOT_PASSWORD=ci-only-password \
      openalbert-ci:minio-smoke >/dev/null
    for ((run=0; run<2; run++)); do
      docker run --rm --network "$network" \
        -e S3_ACCESS_KEY=openalbert-ci -e S3_SECRET_KEY=ci-only-password -e S3_BUCKET=openalbert-ci \
        "$image"
    done
    address=$(docker port "$container" 9000/tcp)
    OPENALBERT_TEST_S3_ENDPOINT="$address" \
      OPENALBERT_TEST_S3_ACCESS_KEY=openalbert-ci \
      OPENALBERT_TEST_S3_SECRET_KEY=ci-only-password \
      OPENALBERT_TEST_S3_BUCKET=openalbert-ci \
      go test -race -count=1 ./internal/blob -run '^TestRealMinIO$'
    ;;
  *) echo "Unknown image component: $component" >&2; exit 1 ;;
esac
