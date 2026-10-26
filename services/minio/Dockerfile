# MinIO's former public images are unavailable. Build the unmodified upstream
# sources at fixed commits; keep their licenses and source references in images.
FROM golang:1.26.9-alpine AS source
RUN apk add --no-cache git

FROM source AS minio-build
# RELEASE.2025-10-15T17-29-55Z
ARG MINIO_REVISION=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
WORKDIR /src
RUN git init && git remote add origin https://github.com/minio/minio.git && git fetch --depth=1 origin ${MINIO_REVISION} && git checkout --detach FETCH_HEAD
RUN CGO_ENABLED=0 go build -trimpath -ldflags="$(go run buildscripts/gen-ldflags.go)" -o /out/minio .

FROM source AS mc-build
# RELEASE.2025-08-13T08-35-41Z
ARG MC_REVISION=7394ce0dd2a80935aded936b09fa12cbb3cb8096
WORKDIR /src
RUN git init && git remote add origin https://github.com/minio/mc.git && git fetch --depth=1 origin ${MC_REVISION} && git checkout --detach FETCH_HEAD
RUN CGO_ENABLED=0 go build -trimpath -ldflags="$(go run buildscripts/gen-ldflags.go)" -o /out/mc .

FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates tzdata

FROM runtime AS minio
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      io.openalbert.upstream.source="https://github.com/minio/minio/tree/9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a"
COPY --from=minio-build /out/minio /usr/local/bin/minio
COPY --from=minio-build /src/LICENSE /usr/share/licenses/minio/LICENSE
EXPOSE 9000 9001
ENTRYPOINT ["minio"]
CMD ["server", "/data", "--console-address", ":9001"]

FROM runtime AS minio-init
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      io.openalbert.upstream.source="https://github.com/minio/mc/tree/7394ce0dd2a80935aded936b09fa12cbb3cb8096"
COPY --from=mc-build /out/mc /usr/local/bin/mc
COPY --from=mc-build /src/LICENSE /src/CREDITS /usr/share/licenses/mc/
COPY services/minio/init.sh /init.sh
ENTRYPOINT ["/bin/sh", "/init.sh"]
