FROM golang:1.26.9-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends chromium ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY apps/backend ./apps/backend
COPY services ./services
ENV OPENALBERT_TEST_CHROMIUM=/usr/bin/chromium OPENALBERT_TEST_NO_SANDBOX=true
CMD ["go", "test", "-race", "-p", "1", "-count=1", "-timeout", "10m", "./..."]
