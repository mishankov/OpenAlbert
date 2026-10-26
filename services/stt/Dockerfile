FROM ubuntu:24.04 AS nemo-build
ARG NEMO_REVISION=2bdfb03b38d5e3b0fa55aeb91a079a9f57385373
ARG BUILD_JOBS=2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git cmake ninja-build build-essential pkg-config libsentencepiece-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init && git remote add origin https://github.com/NVIDIA/NeMo-Speech.cpp.git && git fetch --depth=1 origin ${NEMO_REVISION} && git checkout --detach FETCH_HEAD && git submodule update --init --depth=1 llama.cpp third_party/cpp-httplib
# Build portable CPU code, not instructions discovered on the build host.
RUN scripts/configure.sh cpu-asr -DGGML_NATIVE=OFF -DGGML_AVX512=OFF -DNEMO_SPEECH_BUILD_DIAR=OFF -DNEMO_SPEECH_BUILD_MIC_CAPTURE=OFF -DNEMO_SPEECH_BUILD_HTTP=ON -DCMAKE_INSTALL_LIBDIR=lib && cmake --build --preset cpu-asr --parallel ${BUILD_JOBS} && cmake --install build/cpu-asr --prefix /opt/nemo

FROM golang:1.26.9-alpine AS proxy-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/stt ./services/stt
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /stt-proxy ./services/stt/cmd/stt

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg python3 libsentencepiece0 libgomp1 && rm -rf /var/lib/apt/lists/* && groupadd -g 10001 stt && useradd -u 10001 -g stt -M -s /usr/sbin/nologin stt
COPY --from=nemo-build /opt/nemo /opt/nemo
COPY --from=proxy-build /stt-proxy /usr/local/bin/stt-proxy
COPY services/stt/entrypoint.py /usr/local/bin/stt-entrypoint
ENV LD_LIBRARY_PATH=/opt/nemo/lib OMP_NUM_THREADS=2 STT_MODEL=nemotron-3.5
EXPOSE 8080
ENTRYPOINT ["python3", "/usr/local/bin/stt-entrypoint"]
