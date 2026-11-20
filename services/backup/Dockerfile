FROM golang:1.26.9-alpine AS build
ENV GOWORK=off
WORKDIR /src/services/backup
COPY shared/backup/go.mod /src/shared/backup/
COPY services/backup/go.mod services/backup/go.sum ./
RUN go mod download
COPY shared/backup /src/shared/backup
COPY services/backup ./
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/albert-backup ./cmd/albert-backup

FROM alpine:3.23 AS backup
RUN apk add --no-cache ca-certificates tzdata docker-cli docker-cli-compose
COPY --from=build /out/albert-backup /usr/local/bin/albert-backup
ENTRYPOINT ["/usr/local/bin/albert-backup"]
CMD ["serve", "--config", "/etc/openalbert-backup/config.json"]
