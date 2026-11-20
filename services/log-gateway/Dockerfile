FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY services/log-gateway ./
RUN GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/log-gateway ./cmd/log-gateway

FROM alpine:3.24
RUN apk add --no-cache ca-certificates && addgroup -g 10001 logs && adduser -D -u 10001 -G logs logs
COPY --from=build /out/log-gateway /usr/local/bin/log-gateway
USER logs
EXPOSE 9429
ENTRYPOINT ["/usr/local/bin/log-gateway"]
