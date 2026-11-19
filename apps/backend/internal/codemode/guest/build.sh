#!/bin/sh
set -eu

# Run on the host with the WASI SDK 30.0. The worker embeds the build output.
cd "$(dirname "$0")"
sdk=${WASI_SDK_PATH:?Set WASI_SDK_PATH to the extracted WASI SDK 30.0 directory}
if [ "$(sed -n '1p' "$sdk/VERSION")" != "30.0" ]; then
    printf '%s\n' 'Codemode requires WASI SDK 30.0.' >&2
    exit 1
fi
version=0.18.0
checksum=28bcd89aa3683fd2a946537e5ce3f11cc46b5cbf3383c7561aaef3df5ff9f1dd
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

curl -fsSL "https://codeload.github.com/quickjs-ng/quickjs/tar.gz/refs/tags/v$version" -o "$scratch/source.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$checksum" "$scratch/source.tar.gz" | sha256sum -c -
else
    printf '%s  %s\n' "$checksum" "$scratch/source.tar.gz" | shasum -a 256 -c -
fi
tar -xzf "$scratch/source.tar.gz" -C "$scratch"
source="$scratch/quickjs-$version"

"$sdk/bin/clang" --target=wasm32-wasip1 -mexec-model=reactor \
    -O2 -D_GNU_SOURCE -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_SIGNAL \
    -ffile-prefix-map="$source"=/quickjs -ffile-prefix-map="$PWD"=/codemode \
    -Wl,-z,stack-size=2097152 -Wl,--strip-all \
    -I "$source" bridge.c "$source/quickjs.c" "$source/dtoa.c" \
    "$source/libregexp.c" "$source/libunicode.c" \
    -lwasi-emulated-process-clocks -lwasi-emulated-signal \
    -o "$scratch/quickjs.wasm"
install -m 644 "$scratch/quickjs.wasm" quickjs.wasm
