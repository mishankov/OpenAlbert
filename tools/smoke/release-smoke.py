#!/usr/bin/env python3
"""Exercise a fresh published-image deployment with fake Telegram/LLM APIs.

Requires five locally built images; does not contact real providers or download
STT weights. Uses a unique Compose project, temporary data and loopback ports.
"""
import argparse
import http.server
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[2]
COMPONENTS = ("openalbert", "computer", "log-gateway", "minio", "minio-init")
spec = importlib.util.spec_from_file_location("mvp_smoke", ROOT / "tools/smoke/mvp-smoke.py")
mvp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mvp)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def read_json(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.load(response)


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
    state = mvp.FakeState(project, without_search=True, with_computer=True)
    server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), mvp.handler_for(state))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    api_url = f"http://host.docker.internal:{server.server_port}"
    try:
        with tempfile.TemporaryDirectory(prefix="openalbert-release-smoke-") as directory:
            folder = Path(directory)
            (folder / "compose.yaml").write_text((ROOT / "deploy/compose.yaml").read_text())
            settings = {
                "OPENALBERT_IMAGE_PREFIX": "local/openalbert-release-smoke",
                "OPENALBERT_IMAGE_TAG": "test",
                "POSTGRES_PASSWORD": "smoke-only-password",
                "S3_ACCESS_KEY": "smoke-access", "S3_SECRET_KEY": "smoke-only-secret",
                "S3_BUCKET": "smoke-artifacts", "COMPOSE_PROFILES": "", "STT_BASE_URL": "",
                "TELEGRAM_BOT_TOKEN": "smoke-token", "TELEGRAM_ALLOWED_USER_IDS": "42",
                "TELEGRAM_OWNER_USER_ID": "42", "TELEGRAM_BASE_URL": api_url,
                "LLM_API_KEY": "smoke-key", "LLM_MODEL": "smoke-model", "LLM_BASE_URL": api_url + "/v1",
                "SESSION_IDLE_TIMEOUT": "2s", "TASK_MIN_INTERVAL": "1s", "INITIATIVE_INTERVAL": "0",
                "MAX_AGENT_STEPS": "12", "TEMPORAL_TASK_QUEUE": project,
            }
            (folder / ".env").write_text("".join(f"{key}={value}\n" for key, value in settings.items()))
            base = ["docker", "compose", "--env-file", str(folder / ".env"), "-p", project]

            def run(arguments, check=True):
                result = subprocess.run(arguments, cwd=folder, env=env, text=True,
                                        capture_output=True, timeout=240)
                if check and result.returncode:
                    raise RuntimeError(result.stderr or result.stdout)
                return result

            # Render the unmodified standalone file in an empty directory first.
            model = json.loads(run(base + ["config", "--format", "json"]).stdout)
            assert "stt" not in model["services"], "Disabled STT still starts on a clean deployment"
            gateway_port = free_port()
            for name, service in model["services"].items():
                assert "build" not in service, "Published deployment unexpectedly builds source"
                if env.get("DOCKER_DEFAULT_PLATFORM"):
                    service["platform"] = env["DOCKER_DEFAULT_PLATFORM"]
                component = "computer" if name == "computer-init" else name
                if component in images:
                    service["image"] = images[component]
                    service["pull_policy"] = "never"
                service["logging"]["options"]["splunk-url"] = f"http://127.0.0.1:{gateway_port}"
                service["ports"] = [
                    f'127.0.0.1:{gateway_port if name == "log-gateway" else ""}:{port["target"]}'
                    for port in service.get("ports", [])
                ]
            # The rendered model omits inactive profiles; keep the equivalent
            # dependency graph when loading it as a separate Compose file.
            model["services"]["openalbert"]["depends_on"].pop("stt", None)
            if sys.platform.startswith("linux"):
                model["services"]["openalbert"]["extra_hosts"] = {"host.docker.internal": "host-gateway"}
            knowledge = folder / "container_data/knowledge"
            knowledge.mkdir(parents=True)
            # Only this disposable test directory needs to be writable by UID 10001.
            knowledge.chmod(0o777)
            (folder / "runtime.json").write_text(json.dumps(model))
            compose = base + ["-f", str(folder / "runtime.json")]
            started = False
            try:
                print("Starting fresh release stack: PostgreSQL, Temporal, S3, logs and application", flush=True)
                started = True
                run(compose + ["up", "-d", "--no-build", "--wait", "--wait-timeout", "180"])

                def endpoint(service, port):
                    return "http://" + run(compose + ["port", service, str(port)]).stdout.strip()

                app_url = endpoint("openalbert", 8080)
                logs_url = endpoint("victorialogs", 9428)

                def sql(query):
                    return run(compose + ["exec", "-T", "postgres", "psql", "-U", "openalbert",
                                          "-d", "openalbert", "-Atc", query]).stdout.strip()

                assert read_json(app_url + "/healthz")["status"] == "ok"
                mvp.wait_until("two durable Telegram replies", lambda: len(state.sent) >= 2)
                mvp.wait_until("closed sessions analyzed", lambda: sql("SELECT count(*)>0 AND bool_and(analyzed) FROM openalbert_sessions") == "t")
                assert sql("SELECT count(*) FROM openalbert_incoming WHERE handled") == "1"
                assert sql("SELECT count(*) FROM openalbert_tasks WHERE data->>'status'='completed'") == "1"
                assert sql("SELECT count(*) FROM openalbert_artifacts WHERE octet_length(content)=0") == "2"
                assert state.analyses > 0 and len(state.sent) == 2
                assert all(str(message["chat_id"]) == "42" for message in state.sent)
                assert len(read_json(app_url + "/api/inspect/memory/conversations")["conversations"]) > 0
                assert "OpenAlbert" in read_json(app_url + "/api/inspect/memory/soul")["soul"]
                objects = run(compose + ["run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "minio-init", "-ec",
                                        'mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null; mc find "local/$S3_BUCKET"']).stdout.splitlines()
                assert len(objects) == 2, "Input and computer result were not persisted in S3"
                contents = []
                for object_key in objects:
                    contents.append(run(compose + ["run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "minio-init", "-ec",
                                        'mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null; mc cat "$1"',
                                        "sh", object_key]).stdout)
                assert sorted(contents) == sorted([state.document.decode(), state.document.decode().upper()]), "S3 computer result differs"
                memory = run(compose + ["exec", "-T", "openalbert", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"]).stdout
                assert project + " preference" in memory
                before = (state.completions, state.analyses)
                with state.lock:
                    state.replay_once = True
                    state.offsets = []
                print("Restarting application and checking durable state and duplicate prevention", flush=True)
                run(compose + ["restart", "openalbert"])
                # Docker may allocate another ephemeral host port on restart.
                app_url = endpoint("openalbert", 8080)
                mvp.wait_until("health after restart", lambda: read_json(app_url + "/healthz")["status"] == "ok")
                mvp.wait_until("persisted Telegram offset", lambda: len(state.offsets) > 2 and min(state.offsets) >= state.update_id + 1)
                time.sleep(4)
                assert (state.completions, state.analyses) == before and len(state.sent) == 2
                assert sql("SELECT count(*) FROM openalbert_incoming") == "1"
                assert project + " preference" in run(compose + ["exec", "-T", "openalbert", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"]).stdout

                def logged():
                    query = urllib.parse.urlencode({"query": f'task_queue:="{project}" operation:application.started', "limit": "10"})
                    with urllib.request.urlopen(logs_url + "/select/logsql/query?" + query, timeout=5) as response:
                        events = [json.loads(line) for line in response if line.strip()]
                    return events if len(events) >= 2 and all(e.get("service") == "openalbert" for e in events) else None

                mvp.wait_until("Docker driver to gateway to VictoriaLogs", logged, timeout=30)
                print("Fresh release deployment, Temporal tasks, S3, memory, inspector, logs and restart: OK", flush=True)
            except Exception:
                print(run(compose + ["logs", "--tail=30"], check=False).stdout, file=sys.stderr)
                raise
            finally:
                if started:
                    stopped = run(compose + ["down", "--volumes", "--remove-orphans"], check=False)
                    if stopped.returncode:
                        raise RuntimeError("Could not stop disposable stack: " + stopped.stderr)
                    # PostgreSQL data is owned by its container user on Linux.
                    # Delete only this test's bind-mounted temporary data.
                    if (folder / "container_data").exists():
                        subprocess.run(["docker", "run", "--rm", "--entrypoint", "/bin/sh",
                                        "--mount", f"type=bind,src={folder / 'container_data'},dst=/cleanup",
                                        images["minio-init"], "-ec", "rm -rf /cleanup/*"], env=env, check=True)
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
