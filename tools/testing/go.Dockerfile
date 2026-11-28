FROM golang:1.27.4-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends chromium ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY tools/testing/go-modules.sh ./tools/testing/
COPY go.mod go.sum ./
COPY apps/backend/go.mod apps/backend/go.sum ./apps/backend/
COPY services/backup/go.mod services/backup/go.sum ./services/backup/
COPY services/stt/go.mod ./services/stt/
COPY shared/backup/go.mod shared/backup/go.sum ./shared/backup/
RUN sh tools/testing/go-modules.sh mod download
COPY apps/cli ./apps/cli
COPY apps/backend ./apps/backend
COPY services/backup ./services/backup
COPY services/stt ./services/stt
COPY shared/backup ./shared/backup
COPY shared/codemode ./shared/codemode
ENV OPENALBERT_TEST_CHROMIUM=/usr/bin/chromium OPENALBERT_TEST_NO_SANDBOX=true
CMD ["sh", "tools/testing/go-modules.sh", "test", "-race", "-p", "1", "-count=1", "-timeout", "10m", "./..."]
