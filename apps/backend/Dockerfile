FROM golang:1.27.4-alpine AS build
ENV GOWORK=off
WORKDIR /src/apps/backend
COPY shared/backup/go.mod /src/shared/backup/
COPY apps/backend/go.mod apps/backend/go.sum ./
RUN go mod download
COPY shared/backup /src/shared/backup
COPY apps/backend ./
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/openalbert ./cmd/openalbert

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata chromium font-dejavu && addgroup -g 10001 openalbert && adduser -D -u 10001 -G openalbert openalbert && mkdir -p /data/knowledge && chown -R openalbert:openalbert /data
COPY --from=build /out/openalbert /usr/local/bin/openalbert
USER openalbert
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/openalbert"]
