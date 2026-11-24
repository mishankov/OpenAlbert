"""Prove the local speech endpoint can synthesize and encode Russian audio."""

import json
import os
import subprocess
import time
import urllib.error
import urllib.request


def main():
    base = "http://127.0.0.1:" + os.environ.get("TTS_PORT", "8080")
    for attempt in range(30):
        try:
            with urllib.request.urlopen(base + "/ready", timeout=2) as response:
                assert response.status == 200
            break
        except OSError:
            if attempt == 29:
                raise
            time.sleep(1)

    def speech(text, authorization=""):
        request = urllib.request.Request(
            base + "/v1/audio/speech",
            data=json.dumps({"model": "piper", "voice": "ru_RU-dmitri-medium", "input": text,
                             "response_format": "opus"}, ensure_ascii=False).encode(),
            headers={"Content-Type": "application/json", "Authorization": authorization},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            assert response.headers.get_content_type() == "audio/ogg"
            return response.read(4 << 20)

    def expect_error(status, text, authorization=""):
        try:
            speech(text, authorization)
        except urllib.error.HTTPError as error:
            with error:
                assert error.code == status, f"expected status {status}, got {error.code}"
        else:
            raise AssertionError(f"expected status {status}")

    key = os.environ.get("TTS_API_KEY", "")
    authorization = f"Bearer {key}" if key else ""
    if key:
        expect_error(401, "Привет")
    expect_error(400, "я" * 1001, authorization)

    started = time.monotonic()
    audio = speech("Привет! Встречаемся завтра в половине третьего у станции метро.", authorization)
    elapsed = time.monotonic() - started
    assert 0 < len(audio) < 4 << 20
    probe = subprocess.run(
        ["ffprobe", "-v", "error", "-show_entries", "stream=codec_name,channels", "-of", "json", "-i", "pipe:0"],
        input=audio, check=True, capture_output=True, timeout=5,
    )
    assert json.loads(probe.stdout)["streams"] == [{"codec_name": "opus", "channels": 1}]
    decoded = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", "pipe:0", "-f", "s16le", "pipe:1"],
        input=audio, check=True, capture_output=True, timeout=5,
    ).stdout
    assert len(decoded) > 48000 and any(decoded), "expected audible synthesized speech"
    print(f"Piper Russian speech: mono Opus, {len(audio)} bytes, synthesized in {elapsed:.2f}s")


if __name__ == "__main__":
    main()
