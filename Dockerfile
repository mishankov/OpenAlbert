FROM golang:1.26.9-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/jarvis ./cmd/jarvis

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata chromium font-dejavu && addgroup -g 10001 jarvis && adduser -D -u 10001 -G jarvis jarvis && mkdir -p /data/knowledge && chown -R jarvis:jarvis /data
COPY --from=build /out/jarvis /usr/local/bin/jarvis
USER jarvis
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/jarvis"]
