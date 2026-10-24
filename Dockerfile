FROM golang:1.26.9-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/openalbert ./cmd/openalbert

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata chromium font-dejavu && addgroup -g 10001 openalbert && adduser -D -u 10001 -G openalbert openalbert && mkdir -p /data/knowledge && chown -R openalbert:openalbert /data
COPY --from=build /out/openalbert /usr/local/bin/openalbert
USER openalbert
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/openalbert"]
