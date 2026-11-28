FROM golang:1.27.4-alpine AS build
ENV GOWORK=off
WORKDIR /src/services/backup
COPY shared/backup/go.mod shared/backup/go.sum /src/shared/backup/
COPY services/backup/go.mod services/backup/go.sum ./
RUN go mod download
COPY shared/backup /src/shared/backup
COPY services/backup ./
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/albert-backup ./cmd/albert-backup

FROM alpine:3.24 AS backup
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/albert-backup /usr/local/bin/albert-backup
ENTRYPOINT ["/usr/local/bin/albert-backup"]
CMD ["serve", "--config", "/etc/openalbert-backup/config.json"]
