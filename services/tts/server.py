"""Serve one pinned Piper voice as Telegram-compatible Ogg/Opus."""

import hmac
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading


VOICE = "ru_RU-dmitri-medium"
MAX_INPUT = 1000
MAX_BODY = 16 << 10
MAX_AUDIO = 4 << 20


def synthesize(text, model_path):
    with tempfile.TemporaryDirectory(prefix="piper-") as directory:
        wav = Path(directory) / "voice.wav"
        subprocess.run(
            [sys.executable, "-m", "piper", "-m", model_path, "-f", wav],
            input=text.encode(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            check=True, timeout=24,
        )
        if not 44 < wav.stat().st_size <= 16 << 20:
            raise ValueError("invalid synthesized WAV size")
        output = subprocess.run(
            ["ffmpeg", "-nostdin", "-v", "error", "-f", "wav", "-i", wav,
             "-ac", "1", "-c:a", "libopus", "-application", "voip", "-b:a", "32k",
             "-fs", str(MAX_AUDIO), "-f", "ogg", "pipe:1"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=True, timeout=4,
        ).stdout
        # An Opus stream needs both header packets and at least one audio page.
        if (not 0 < len(output) < MAX_AUDIO or not output.startswith(b"OggS")
                or b"OpusHead" not in output[:64] or output.count(b"OggS") < 3):
            raise ValueError("invalid synthesized Opus stream")
        return output


class SpeechServer(http.server.ThreadingHTTPServer):
    def __init__(self, address, model_path, api_key):
        super().__init__(address, Handler)
        self.model_path = model_path
        self.authorization = f"Bearer {api_key}".encode("utf-8") if api_key else b""
        self.inference = threading.Lock()
        self.connections = threading.BoundedSemaphore(8)

    def get_request(self):
        request, address = super().get_request()
        request.settimeout(5)
        return request, address

    def process_request(self, request, address):
        if not self.connections.acquire(blocking=False):
            try:
                request.sendall(b"HTTP/1.0 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
            except OSError:
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except Exception:
            self.connections.release()
            raise

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.connections.release()

    def handle_error(self, request, address):
        # Avoid logging request targets, credentials or native error output.
        print("TTS request interrupted", file=sys.stderr, flush=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def respond(self, status, data, mime="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def fail(self, status, code):
        self.respond(status, json.dumps({"error": code}).encode())

    def do_GET(self):
        if self.path in {"/ready", "/healthz"}:
            self.respond(200, b'{"status":"ok"}')
        else:
            self.fail(404, "not_found")

    def do_POST(self):
        if self.path != "/v1/audio/speech":
            self.fail(404, "not_found")
            return
        auth = self.headers.get("Authorization", "").encode("latin-1")
        if self.server.authorization and not hmac.compare_digest(auth, self.server.authorization):
            self.fail(401, "unauthorized")
            return
        lengths = self.headers.get_all("Content-Length", [])
        if len(lengths) != 1 or self.headers.get("Transfer-Encoding"):
            self.fail(400, "invalid_request")
            return
        try:
            length = int(lengths[0])
        except ValueError:
            self.fail(400, "invalid_request")
            return
        if not 0 < length <= MAX_BODY:
            self.fail(413, "request_size")
            return
        try:
            data = json.loads(self.rfile.read(length).decode("utf-8"))
            text = data["input"]
            if not isinstance(text, str) or not text.strip() or len(text) > MAX_INPUT or "\x00" in text:
                raise ValueError
            text.encode("utf-8")
            if (data.get("model", "piper") != "piper" or data.get("voice") != VOICE
                    or data.get("response_format") != "opus"):
                raise ValueError
        except (KeyError, TypeError, ValueError, RecursionError):
            self.fail(400, "invalid_request")
            return
        if not self.server.inference.acquire(blocking=False):
            self.fail(503, "busy")
            return
        try:
            output = synthesize(text, self.server.model_path)
        except (OSError, ValueError, subprocess.SubprocessError):
            print("TTS synthesis failed", file=sys.stderr, flush=True)
            self.fail(502, "synthesis_failed")
        else:
            self.respond(200, output, "audio/ogg")
        finally:
            self.server.inference.release()


def main():
    model = Path(f"/models/{VOICE}.onnx")
    if not model.is_file() or not model.with_suffix(".onnx.json").is_file() or not shutil.which("ffmpeg"):
        sys.exit("TTS model or encoder is missing")
    try:
        synthesize("Готово.", model)
    except (OSError, ValueError, subprocess.SubprocessError):
        sys.exit("TTS startup synthesis failed")
    with SpeechServer(("0.0.0.0", 8080), model, os.environ.get("TTS_API_KEY", "").strip()) as server:
        server.serve_forever()


if __name__ == "__main__":
    main()
