"""Fetch one pinned GGUF on explicit container startup; supervise both servers."""
import hashlib
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import urllib.request

REVISION = "ea30d66debe3740a08b573244286791d423d6b3e"
FILENAME = "nemotron-3.5-asr-streaming-0.6b.q8_0.gguf"
SHA256 = "3fc991d3badad7277c11030a7519832cddaf2057aafed6d4b25147e953a070b1"
SIZE = 742090464
URL = f"https://huggingface.co/nvidia/nemotron-3.5-asr-streaming-0.6b/resolve/{REVISION}/{FILENAME}"


def verified(path):
    if not path.is_file() or path.stat().st_size != SIZE:
        return False
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest() == SHA256


def main():
    directory = Path("/models")
    directory.mkdir(exist_ok=True)
    if os.getuid() == 0:
        # Docker creates a fresh bind mount as root. Only the mount directory
        # needs ownership; never recursively chown existing user files.
        os.chown(directory, 10001, 10001)
        os.setgroups([])
        os.setgid(10001)
        os.setuid(10001)
    model = directory / FILENAME
    if not verified(model):
        if model.exists():
            raise RuntimeError("Cached model checksum mismatch; move it aside before restarting")
        if os.environ.get("STT_DOWNLOAD_MODEL", "true").lower() != "true":
            raise RuntimeError("Pinned model is missing and STT_DOWNLOAD_MODEL is disabled")
        print("Downloading pinned Nemotron Q8 model (742090464 bytes)", flush=True)
        partial = directory / (FILENAME + ".partial")
        try:
            with urllib.request.urlopen(URL, timeout=60) as source, partial.open("wb") as target:
                total = 0
                while chunk := source.read(1024 * 1024):
                    total += len(chunk)
                    if total > SIZE:
                        raise RuntimeError("Model download exceeds expected size")
                    target.write(chunk)
            if not verified(partial):
                raise RuntimeError("Downloaded model checksum mismatch")
            partial.replace(model)
        finally:
            partial.unlink(missing_ok=True)
    children = []
    stopping = False

    def stop(*_):
        nonlocal stopping
        stopping = True
        for child in children:
            if child.poll() is None:
                child.terminate()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    # The native endpoint is private to this container. Authentication belongs
    # to the external adapter, so a configured API key is not passed upstream.
    native_env = dict(os.environ)
    native_env.pop("NEMO_SPEECH_HTTP_API_KEY", None)
    try:
        children.append(subprocess.Popen([
            "/opt/nemo/bin/nemo-speech", "serve", "--asr-model", str(model),
            "--asr.backend.gpu", "-1", "--asr.batching.enabled=false",
            "--host", "127.0.0.1", "--port", "8081", "--threads", "2",
            "--max-upload-mb", "21", "--write-timeout", "660", "--no-ui",
        ], env=native_env))
        children.append(subprocess.Popen(["/usr/local/bin/stt-proxy"]))
        while not stopping and all(child.poll() is None for child in children):
            time.sleep(0.25)
        failed = not stopping
    finally:
        stop()
        for child in children:
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        # No request contents, API keys, or download URL credentials in logs.
        detail = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(f"STT startup failed: {detail}", file=sys.stderr)
        sys.exit(1)
