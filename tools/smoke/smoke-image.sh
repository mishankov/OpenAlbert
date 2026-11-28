#!/usr/bin/env bash
# Run only disposable containers. Never start the Telegram poller or fetch models.
set -euo pipefail

component=${1:?component is required}
image=${2:?image is required}

case "$component" in
  backup)
    docker run --rm --entrypoint /bin/sh "$image" -ec \
      'test -x /usr/local/bin/albert-backup; test "$(id -u)" = 0; /usr/local/bin/albert-backup keygen --identity /tmp/backup.agekey'
    ;;
  openalbert)
    docker run --rm --entrypoint /bin/sh "$image" -ec \
      'test -x /usr/local/bin/openalbert; test "$(id -u)" = 10001; chromium --version'
    ;;
  stt)
    docker run --rm --entrypoint /bin/sh "$image" -ec \
      'test -x /usr/local/bin/stt-proxy; ffmpeg -version >/dev/null; /opt/nemo/bin/nemo-speech --help >/dev/null; python3 -c "import ast; ast.parse(open(\"/usr/local/bin/stt-entrypoint\").read())"'
    ;;
  tts)
    container=$(docker run -d --network none --init --read-only --cpus 2 --memory 1g --pids-limit 128 \
      --tmpfs /tmp:rw,noexec,nosuid,size=32m,mode=1777 -e TTS_API_KEY=tts-smoke-only "$image")
    trap 'docker rm -f "$container" >/dev/null' EXIT
    if ! docker exec -i "$container" python3 - < tools/smoke/tts-smoke.py; then
      docker logs "$container"
      exit 1
    fi
    ;;
  *) echo "Unknown image component: $component" >&2; exit 1 ;;
esac
