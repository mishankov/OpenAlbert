#!/usr/bin/env python3
"""Verify encrypted online SQLite S3 backup, retention and restore with zero sidecar DBs.

Runs the real backup container without /var/run/docker.sock against a disposable
in-process S3 mock server. Seeds a live SQLite WAL database, local artifact bytes,
knowledge files and configuration, triggers backups, verifies retention and restores
into a fresh directory. Never reads the project's .env.
"""
import datetime
import email.utils
import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import uuid
import xml.sax.saxutils as xmlutils


ROOT = Path(__file__).resolve().parents[2]


def decode_aws_chunked(raw):
    """Decode STREAMING-AWS4-HMAC-SHA256-PAYLOAD / STREAMING-UNSIGNED-PAYLOAD-TRAILER bodies."""
    out = bytearray()
    pos = 0
    length = len(raw)
    while pos < length:
        line_end = raw.find(b"\r\n", pos)
        if line_end == -1:
            break
        header = raw[pos:line_end].decode("ascii", errors="strict")
        size = int(header.split(";", 1)[0].strip(), 16)
        pos = line_end + 2
        if size == 0:
            break
        out.extend(raw[pos:pos + size])
        pos += size + 2  # skip trailing \r\n
    return bytes(out)


class MockS3Store:
    def __init__(self, bucket):
        self.lock = threading.Lock()
        self.buckets = {bucket: {"versioned": False, "objects": {}}}

    def put(self, bucket, key, data, content_type="application/octet-stream", modified=None):
        etag = hashlib.md5(data).hexdigest()
        now = modified or datetime.datetime.now(datetime.timezone.utc)
        with self.lock:
            self.buckets[bucket]["objects"][key] = {
                "data": data,
                "etag": etag,
                "content_type": content_type,
                "modified": now,
            }
        return etag


def s3_handler(store):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def _parse_target(self):
            parsed = urllib.parse.urlsplit(self.path)
            parts = [urllib.parse.unquote(p) for p in parsed.path.lstrip("/").split("/", 1)]
            bucket = parts[0] if parts and parts[0] else ""
            key = parts[1] if len(parts) > 1 else ""
            query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
            return bucket, key, query

        def _read_body(self):
            te = (self.headers.get("Transfer-Encoding") or "").lower()
            if "chunked" in te:
                buf = bytearray()
                while True:
                    line = self.rfile.readline()
                    if not line:
                        break
                    size = int(line.split(b";", 1)[0].strip(), 16)
                    if size == 0:
                        while True:
                            trailer = self.rfile.readline()
                            if not trailer or trailer == b"\r\n":
                                break
                        break
                    buf.extend(self.rfile.read(size))
                    self.rfile.read(2)
                raw = bytes(buf)
            else:
                length = int(self.headers.get("Content-Length") or "0")
                raw = self.rfile.read(length) if length > 0 else b""
            sha_hdr = self.headers.get("x-amz-content-sha256") or ""
            if sha_hdr.startswith("STREAMING-"):
                return decode_aws_chunked(raw)
            return raw

        def _send(self, status, body=b"", content_type="application/xml", headers=None):
            self.send_response(status)
            if content_type:
                self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            for k, v in (headers or {}).items():
                self.send_header(k, v)
            self.end_headers()
            if body and self.command != "HEAD":
                self.wfile.write(body)

        def _error(self, status, code, message):
            body = (
                f'<?xml version="1.0" encoding="UTF-8"?>'
                f'<Error><Code>{xmlutils.escape(code)}</Code>'
                f'<Message>{xmlutils.escape(message)}</Message></Error>'
            ).encode()
            self._send(status, body)

        def do_HEAD(self):
            bucket, key, _ = self._parse_target()
            with store.lock:
                b = store.buckets.get(bucket)
                if b is None:
                    self._send(404)
                    return
                if not key:
                    self._send(200)
                    return
                obj = b["objects"].get(key)
            if obj is None:
                self._send(404)
                return
            self._send(200, b"", content_type=obj["content_type"], headers={
                "ETag": f'"{obj["etag"]}"',
                "Content-Length": str(len(obj["data"])),
                "Last-Modified": email.utils.format_datetime(obj["modified"], usegmt=True),
                "Accept-Ranges": "bytes",
            })

        def do_GET(self):
            bucket, key, query = self._parse_target()
            with store.lock:
                b = store.buckets.get(bucket)
                if b is None:
                    self._error(404, "NoSuchBucket", "bucket not found")
                    return
                if not key:
                    if "versioning" in query:
                        if b["versioned"]:
                            xml = '<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>'
                        else:
                            xml = '<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>'
                        self._send(200, xml.encode())
                        return
                    if "location" in query:
                        xml = '<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>'
                        self._send(200, xml.encode())
                        return
                    prefix = query.get("prefix", [""])[0]
                    url_encode = query.get("encoding-type", [""])[0] == "url"
                    items = sorted((k, dict(v)) for k, v in b["objects"].items() if k.startswith(prefix))
                else:
                    obj = b["objects"].get(key)
                    if obj is None:
                        self._error(404, "NoSuchKey", "key not found")
                        return
                    obj = dict(obj)
            if not key:
                enc = (lambda s: urllib.parse.quote(s, safe="/")) if url_encode else (lambda s: s)
                parts = [
                    '<?xml version="1.0" encoding="UTF-8"?>',
                    '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">',
                    f'<Name>{xmlutils.escape(bucket)}</Name>',
                    f'<Prefix>{xmlutils.escape(enc(prefix))}</Prefix>',
                    f'<KeyCount>{len(items)}</KeyCount>',
                    '<MaxKeys>1000</MaxKeys>',
                    '<IsTruncated>false</IsTruncated>',
                ]
                if url_encode:
                    parts.append('<EncodingType>url</EncodingType>')
                for k, v in items:
                    ts = v["modified"].astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")
                    parts.append(
                        f'<Contents><Key>{xmlutils.escape(enc(k))}</Key>'
                        f'<LastModified>{ts}</LastModified>'
                        f'<ETag>&quot;{v["etag"]}&quot;</ETag>'
                        f'<Size>{len(v["data"])}</Size>'
                        f'<StorageClass>STANDARD</StorageClass></Contents>'
                    )
                parts.append('</ListBucketResult>')
                self._send(200, "".join(parts).encode())
                return

            data = obj["data"]
            range_hdr = self.headers.get("Range")
            headers = {
                "ETag": f'"{obj["etag"]}"',
                "Last-Modified": email.utils.format_datetime(obj["modified"], usegmt=True),
                "Accept-Ranges": "bytes",
            }
            if range_hdr and range_hdr.startswith("bytes="):
                spec = range_hdr[len("bytes="):].split(",", 1)[0]
                start_s, end_s = spec.split("-", 1)
                start = int(start_s) if start_s else 0
                end = int(end_s) if end_s else len(data) - 1
                end = min(end, len(data) - 1)
                chunk = data[start:end + 1]
                headers["Content-Range"] = f"bytes {start}-{end}/{len(data)}"
                self._send(206, chunk, content_type=obj["content_type"], headers=headers)
                return
            self._send(200, data, content_type=obj["content_type"], headers=headers)

        def do_PUT(self):
            bucket, key, _ = self._parse_target()
            data = self._read_body()
            with store.lock:
                if bucket not in store.buckets:
                    self._error(404, "NoSuchBucket", "bucket not found")
                    return
            if not key:
                self._send(200)
                return
            etag = store.put(bucket, key, data, self.headers.get("Content-Type") or "application/octet-stream")
            self._send(200, b"", content_type="", headers={"ETag": f'"{etag}"'})

        def do_DELETE(self):
            bucket, key, _ = self._parse_target()
            with store.lock:
                b = store.buckets.get(bucket)
                if b is not None and key:
                    b["objects"].pop(key, None)
            self._send(204, b"", content_type="")

        def do_POST(self):
            bucket, _, query = self._parse_target()
            body = self._read_body()
            if "delete" in query:
                keys = re.findall(rb"<Key>(.*?)</Key>", body)
                with store.lock:
                    b = store.buckets.get(bucket)
                    if b is not None:
                        for raw_key in keys:
                            b["objects"].pop(xmlutils.unescape(raw_key.decode()), None)
                self._send(200, b'<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>')
                return
            self._error(405, "MethodNotAllowed", "unsupported POST")

    return Handler


def wait_until(description, condition, timeout=60):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = condition()
            if value:
                return value
        except Exception as err:
            last = err
        time.sleep(0.25)
    raise RuntimeError(f"Timed out waiting for {description}" + (f" ({last})" if last else ""))


def main():
    env = {key: value for key, value in os.environ.items() if key in {
        "PATH", "HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT",
        "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_DEFAULT_PLATFORM"}}
    controller_image = os.environ.get("OPENALBERT_TEST_BACKUP_CONTROLLER_IMAGE", "openalbert-ci:backup")
    subprocess.run(["docker", "build", "-f", "services/backup/Dockerfile", "-t", controller_image, "."],
                   cwd=ROOT, env=env, check=True)

    bucket = "backups"
    prefix = "smoke/"
    store = MockS3Store(bucket)
    server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), s3_handler(store))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    host_args = ["--add-host=host.docker.internal:host-gateway"] if sys.platform.startswith("linux") else []
    container = "albert-backup-smoke-" + uuid.uuid4().hex[:12]

    try:
        with tempfile.TemporaryDirectory(prefix="albert-backup-smoke-", dir="/tmp") as directory:
            folder = Path(directory).resolve()
            inst = folder / "installation"
            keys_dir = folder / "keys"
            restore_parent = folder / "restore"
            for sub in (inst / "data/knowledge", inst / "data/artifacts/smoke", inst / "data/chatgpt",
                        inst / "backup-config", inst / "backup-state", inst / "backup-work",
                        keys_dir, restore_parent):
                sub.mkdir(parents=True, exist_ok=True)

            env_content = "TELEGRAM_BOT_TOKEN=smoke-never-leak-secret\nTELEGRAM_ALLOWED_USER_IDS=42\n"
            models_toml = "# synthetic model config preserved across online backup\n"
            models_env = "ANALYSIS_LLM_API_KEY=synthetic-analysis-key\n"
            artifact_body = b"artifact preserved across online SQLite backup and restore"
            soul_body = "Albert remembers"

            (inst / "compose.yaml").write_text((ROOT / "deploy/compose.yaml").read_text())
            (inst / ".env").write_text(env_content)
            (inst / ".env").chmod(0o600)
            (inst / "models.toml").write_text(models_toml)
            (inst / "models.env").write_text(models_env)
            (inst / "models.env").chmod(0o600)
            (inst / "data/knowledge/SOUL.md").write_text(soul_body)
            (inst / "data/artifacts/smoke/doc.txt").write_bytes(artifact_body)
            (inst / "data/chatgpt/credentials.json").write_text('{"synthetic": true}\n')

            db_path = inst / "data/openalbert.db"
            conn = sqlite3.connect(str(db_path))
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA wal_autocheckpoint=0")
            conn.execute("CREATE TABLE backup_probe (id INTEGER PRIMARY KEY, value TEXT NOT NULL)")
            conn.execute("INSERT INTO backup_probe (value) VALUES ('online-wal-preserved')")
            conn.commit()

            try:
                keygen = subprocess.run(
                    ["docker", "run", "--rm",
                     "--mount", f"type=bind,src={keys_dir},dst=/keys",
                     controller_image, "keygen", "--identity", "/keys/albert-backup.agekey"],
                    env=env, text=True, capture_output=True, check=True,
                )
                match = re.search(r"age1[0-9a-z]+", keygen.stdout)
                assert match, f"Could not parse public age recipient from: {keygen.stdout!r}"
                recipient = match.group(0)

                cfg = json.loads((ROOT / "deploy/backup.example.json").read_text())
                cfg.update({
                    "root": "/installation",
                    "compose_file": "compose.yaml",
                    "work_dir": "/var/lib/openalbert-backup",
                    "enabled": True,
                    "keep_days": 2,
                    "s3_endpoint": f"host.docker.internal:{server.server_port}",
                    "s3_region": "us-east-1",
                    "s3_secure": False,
                    "s3_bucket": bucket,
                    "s3_prefix": prefix,
                    "credentials_file": "/etc/openalbert-backup/s3.json",
                    "age_recipient": recipient,
                })
                if "notice_seconds" in cfg:
                    cfg["notice_seconds"] = 0
                (inst / "backup-config/config.json").write_text(json.dumps(cfg, indent=2) + "\n")
                (inst / "backup-config/s3.json").write_text(json.dumps({
                    "access_key": "test-access", "secret_key": "test-only-secret",
                }) + "\n")
                (inst / "backup-config/s3.json").chmod(0o600)

                subprocess.run([
                    "docker", "run", "-d", "--name", container, "--init", "--read-only",
                    "--tmpfs", "/tmp", "-e", "BACKUP_CONTAINER=true", *host_args,
                    "--mount", f"type=bind,src={inst},dst=/installation,readonly",
                    "--mount", f"type=bind,src={inst / 'data'},dst=/installation/data",
                    "--mount", f"type=bind,src={inst / 'backup-config'},dst=/etc/openalbert-backup,readonly",
                    "--mount", f"type=bind,src={inst / 'backup-state'},dst=/installation/backup-state",
                    "--mount", f"type=bind,src={inst / 'backup-work'},dst=/var/lib/openalbert-backup",
                    controller_image,
                ], env=env, check=True, stdout=subprocess.DEVNULL)

                status_file = inst / "backup-state/status.json"

                def read_status():
                    return json.loads(status_file.read_text()) if status_file.exists() else None

                wait_until("backup controller idle", lambda: (read_status() or {}).get("phase") == "idle")

                def trigger_and_wait():
                    prev = (read_status() or {}).get("run_id")
                    subprocess.run(["docker", "exec", container, "/usr/local/bin/albert-backup", "request"],
                                   env=env, check=True, stdout=subprocess.DEVNULL)

                    def done():
                        s = read_status()
                        if not s or s.get("run_id") == prev:
                            return None
                        if s.get("phase") == "failed":
                            logs = subprocess.run(["docker", "logs", "--tail", "40", container],
                                                  env=env, text=True, capture_output=True).stdout
                            raise RuntimeError(f"Backup failed: {s}\n{logs}")
                        return s if s.get("phase") == "completed" else None

                    return wait_until("backup completion", done, timeout=90)

                first = trigger_and_wait()
                assert first.get("last_success"), f"Missing last_success in status: {first}"
                raw_status = status_file.read_text()
                assert "smoke-never-leak-secret" not in raw_status and "synthetic-analysis-key" not in raw_status

                last_copy = first["last_success"]
                archive_bytes = store.buckets[bucket]["objects"][last_copy["key"]]["data"]
                for idx, day in enumerate(("2020-01-01", "2020-01-02"), start=1):
                    old_id = f"2020010{idx}T000000Z-0000000000000000"
                    old_key = f"{prefix}copies/{old_id}/archive.tar.gz.age"
                    old_meta = dict(last_copy, id=old_id, day=day,
                                    created_at=f"{day}T00:00:00Z", key=old_key)
                    store.put(bucket, old_key, archive_bytes)
                    store.put(bucket, f"{prefix}copies/{old_id}/complete.json",
                              json.dumps(old_meta).encode(), "application/json")

                second = trigger_and_wait()
                listed = json.loads(subprocess.check_output(
                    ["docker", "exec", container, "/usr/local/bin/albert-backup", "list"],
                    env=env, text=True,
                ))
                assert all(c["day"] != "2020-01-01" for c in listed), f"Expired date survived rotation: {listed}"
                assert len(listed) == 3, f"Expected 3 copies (2 today + 2020-01-02), got {len(listed)}: {listed}"
                assert f"{prefix}copies/20200101T000000Z-0000000000000000/archive.tar.gz.age" not in store.buckets[bucket]["objects"]

                latest_id = second["last_success"]["id"]
                subprocess.run([
                    "docker", "run", "--rm", *host_args,
                    "--mount", f"type=bind,src={inst / 'backup-config'},dst=/etc/openalbert-backup,readonly",
                    "--mount", f"type=bind,src={keys_dir / 'albert-backup.agekey'},dst=/run/recovery.agekey,readonly",
                    "--mount", f"type=bind,src={restore_parent},dst=/restore",
                    controller_image, "restore", "--id", latest_id,
                    "--identity", "/run/recovery.agekey", "--destination", "/restore/openalbert",
                ], env=env, check=True)

                restored = restore_parent / "openalbert"
                assert (restored / ".env").read_text() == env_content
                assert (restored / "models.toml").read_text() == models_toml
                assert (restored / "models.env").read_text() == models_env
                assert (restored / "data/knowledge/SOUL.md").read_text() == soul_body
                assert (restored / "data/artifacts/smoke/doc.txt").read_bytes() == artifact_body
                for excluded in ("data/chatgpt", "backup-state", "backup-work", "backup-config"):
                    assert not (restored / excluded).exists(), f"Excluded directory restored: {excluded}"

                restored_db = sqlite3.connect(str(restored / "data/openalbert.db"))
                try:
                    assert restored_db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
                    rows = [r[0] for r in restored_db.execute("SELECT value FROM backup_probe").fetchall()]
                    assert rows == ["online-wal-preserved"], f"Unexpected restored SQLite rows: {rows}"
                finally:
                    restored_db.close()

                print("Online SQLite encrypted S3 backup, retention and restore (no Docker socket): OK", flush=True)
            finally:
                conn.close()
                subprocess.run(["docker", "rm", "-f", container], env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                subprocess.run([
                    "docker", "run", "--rm", "--entrypoint", "/bin/sh",
                    "--mount", f"type=bind,src={folder},dst=/cleanup",
                    controller_image, "-ec", "rm -rf /cleanup/*",
                ], env=env, check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
