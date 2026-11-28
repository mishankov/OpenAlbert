#!/usr/bin/env python3
"""Exercise a fresh single-container published-image deployment with fake Telegram/LLM APIs.

Requires the locally built openalbert image; does not contact real providers or download
STT weights. Uses a unique Compose project, temporary data directory and loopback port.
"""
import argparse
import http.server
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[2]
COMPONENTS = ("openalbert",)
spec = importlib.util.spec_from_file_location("mvp_smoke", ROOT / "tools/smoke/mvp-smoke.py")
mvp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mvp)


def read_json(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.load(response)


def query_sqlite(db_path, statement):
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True, timeout=5)
    try:
        row = conn.execute(statement).fetchone()
        return "" if row is None or row[0] is None else str(row[0])
    finally:
        conn.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", action="append", default=[], metavar="COMPONENT=IMAGE",
                        help="Override a local image; defaults to openalbert-ci:COMPONENT")
    args = parser.parse_args()
    images = {name: "openalbert-ci:" + name for name in COMPONENTS}
    for item in args.image:
        name, separator, value = item.partition("=")
        if not separator or name not in images or not value:
            parser.error("--image must identify one of " + ", ".join(COMPONENTS))
        images[name] = value
    env = {key: value for key, value in os.environ.items() if key in {
        "PATH", "HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT",
        "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_DEFAULT_PLATFORM"}}
    for image in images.values():
        subprocess.run(["docker", "image", "inspect", image], env=env, check=True, stdout=subprocess.DEVNULL)

    project = "openalbert-release-smoke-" + uuid.uuid4().hex[:12]
    state = mvp.FakeState(project, without_search=True, with_computer=False)
    server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), mvp.handler_for(state))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    api_url = f"http://host.docker.internal:{server.server_port}"
    try:
        with tempfile.TemporaryDirectory(prefix="openalbert-release-smoke-") as directory:
            folder = Path(directory)
            (folder / "compose.yaml").write_text((ROOT / "deploy/compose.yaml").read_text())
            (folder / "models.toml").write_text(mvp.model_config(api_url + "/v1"))
            settings = {
                "OPENALBERT_IMAGE_PREFIX": "local/openalbert-release-smoke",
                "OPENALBERT_IMAGE_TAG": "test",
                "COMPOSE_PROFILES": "",
                "STT_BASE_URL": "",
                "TELEGRAM_BOT_TOKEN": "smoke-token",
                "TELEGRAM_ALLOWED_USER_IDS": "42",
                "TELEGRAM_OWNER_USER_ID": "42",
                "TELEGRAM_BASE_URL": api_url,
                "LLM_API_KEY": "smoke-key",
                "SESSION_IDLE_TIMEOUT": "2s",
                "TASK_MIN_INTERVAL": "1s",
                "INITIATIVE_INTERVAL": "0",
                "MEMORY_CONSOLIDATION_INTERVAL": "0",
                "MAX_AGENT_STEPS": "12",
            }
            (folder / ".env").write_text("".join(f"{key}={value}\n" for key, value in settings.items()))
            base = ["docker", "compose", "--env-file", str(folder / ".env"), "-p", project]

            def run(arguments, check=True):
                result = subprocess.run(arguments, cwd=folder, env=env, text=True,
                                        capture_output=True, timeout=240)
                if check and result.returncode:
                    raise RuntimeError(result.stderr or result.stdout)
                return result

            model = json.loads(run(base + ["config", "--format", "json"]).stdout)
            assert set(model["services"]) == {"openalbert"}, "Default standalone deployment must run only openalbert"
            service = model["services"]["openalbert"]
            assert "build" not in service, "Published deployment unexpectedly builds source"
            assert service["environment"]["COMPUTER_SSH_ADDRESS"] == ""
            assert service["environment"]["DATA_DIR"] == "/data"
            if env.get("DOCKER_DEFAULT_PLATFORM"):
                service["platform"] = env["DOCKER_DEFAULT_PLATFORM"]
            service["image"] = images["openalbert"]
            service["pull_policy"] = "never"
            service["ports"] = [f'127.0.0.1::{port["target"]}' for port in service.get("ports", [])]
            if "depends_on" in service:
                service["depends_on"].pop("stt", None)
                if not service["depends_on"]:
                    service.pop("depends_on")
            if sys.platform.startswith("linux"):
                service["extra_hosts"] = {"host.docker.internal": "host-gateway"}

            data_dir = folder / "data"
            for sub in (data_dir / "knowledge", data_dir / "artifacts", data_dir / "chatgpt", folder / "backup-state"):
                sub.mkdir(parents=True, exist_ok=True)
            for writable in (data_dir, data_dir / "knowledge", data_dir / "artifacts", data_dir / "chatgpt"):
                writable.chmod(0o777)

            (folder / "runtime.json").write_text(json.dumps(model))
            compose = base + ["-f", str(folder / "runtime.json")]
            db_path = data_dir / "openalbert.db"
            started = False
            try:
                print("Starting fresh single-container release deployment (SQLite + local blobs + built-in logs)", flush=True)
                started = True
                run(compose + ["up", "-d", "--no-build", "--wait", "--wait-timeout", "120"])

                def endpoint(svc, port):
                    return "http://" + run(compose + ["port", svc, str(port)]).stdout.strip()

                app_url = endpoint("openalbert", 8080)

                def sql(query):
                    return query_sqlite(db_path, query)

                assert read_json(app_url + "/healthz")["status"] == "ok"
                mvp.wait_until("two durable Telegram replies", lambda: len(state.sent) >= 2)
                mvp.wait_until("closed sessions analyzed", lambda: sql("SELECT count(*) > 0 AND min(analyzed) = 1 FROM openalbert_sessions") == "1")
                assert sql("SELECT count(*) FROM openalbert_incoming WHERE handled != 0") == "1"
                assert sql("SELECT count(*) FROM openalbert_tasks WHERE json_extract(data, '$.status') = 'completed'") == "1"
                assert state.analyses > 0 and len(state.sent) == 2
                assert all(str(message["chat_id"]) == "42" for message in state.sent)
                assert {"rich" if "rich_message" in message else "ordinary" for message in state.sent} == {"ordinary", "rich"}, "both reply formats must reach Telegram"
                assert len(read_json(app_url + "/api/inspect/memory/conversations")["conversations"]) > 0
                assert "OpenAlbert" in read_json(app_url + "/api/inspect/memory/soul")["soul"]

                artifact_files = [p for p in (data_dir / "artifacts").rglob("*") if p.is_file()]
                assert any(p.read_bytes() == state.document for p in artifact_files), "Input document was not persisted in local blob store"

                memory = run(compose + ["exec", "-T", "openalbert", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"]).stdout
                assert project + " preference" in memory

                before = (state.completions, state.analyses)
                with state.lock:
                    state.replay_once = True
                    state.offsets = []
                print("Restarting application and checking durable SQLite state and duplicate prevention", flush=True)
                run(compose + ["restart", "openalbert"])
                app_url = endpoint("openalbert", 8080)
                mvp.wait_until("health after restart", lambda: read_json(app_url + "/healthz")["status"] == "ok")
                mvp.wait_until("persisted Telegram offset", lambda: len(state.offsets) > 2 and min(state.offsets) >= state.update_id + 1)
                time.sleep(4)
                assert (state.completions, state.analyses) == before and len(state.sent) == 2
                assert sql("SELECT count(*) FROM openalbert_incoming") == "1"
                assert project + " preference" in run(compose + ["exec", "-T", "openalbert", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"]).stdout

                mvp.wait_until("structured logs in SQLite openalbert_logs", lambda: int(sql("SELECT count(*) FROM openalbert_logs") or "0") >= 2, timeout=20)
                print("Fresh single-container release deployment, tasks, local blobs, memory, inspector, SQLite logs and restart: OK", flush=True)
            except Exception:
                print(run(compose + ["logs", "--tail=40"], check=False).stdout, file=sys.stderr)
                raise
            finally:
                if started:
                    stopped = run(compose + ["down", "--volumes", "--remove-orphans"], check=False)
                    if stopped.returncode:
                        raise RuntimeError("Could not stop disposable stack: " + stopped.stderr)
                    if data_dir.exists():
                        subprocess.run(["docker", "run", "--rm", "--entrypoint", "/bin/sh",
                                        "--mount", f"type=bind,src={data_dir},dst=/cleanup",
                                        images["openalbert"], "-ec", "rm -rf /cleanup/*"], env=env, check=False)
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
