#!/usr/bin/env python3
"""Actual Compose MVP smoke. Uses local fake APIs; never contacts real Telegram/LLM/Yandex.
With --with-browser, Chromium visits public example.com.

Requires the local development infrastructure from compose.yaml and a built jarvis image.
Creates an isolated application DB, knowledge volume and artifact bucket, then removes them.
Does not alter the documentation server or stop infrastructure. Refuses a running Jarvis.
"""
import argparse
import base64
import datetime
import http.server
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]


def run(args, env, **kwargs):
    return subprocess.run(args, cwd=ROOT, env=env, check=True, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs).stdout.strip()


def wait_until(description, condition, timeout=60):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = condition()
            if value:
                return value
        except Exception as error:
            last = type(error).__name__
        time.sleep(.4)
    raise AssertionError(f"Timed out: {description}" + (f" ({last})" if last else ""))


class FakeState:
    def __init__(self, marker, without_search=False, with_browser=False):
        self.marker = marker
        self.without_search = without_search
        self.with_browser = with_browser
        self.update_id = int(time.time() * 1000)
        self.document = ("Local document for " + marker).encode()
        self.sent = []
        self.offsets = []
        self.searches = 0
        self.analyses = 0
        self.completions = 0
        self.replay_once = False
        self.lock = threading.Lock()

    def update(self):
        return {"update_id": self.update_id, "message": {
            "message_id": 100, "date": int(time.time()),
            "from": {"id": 42, "is_bot": False}, "chat": {"id": 42, "type": "private"},
            "caption": self.marker,
            "document": {"file_id": "local-document", "file_name": "smoke.txt",
                         "mime_type": "text/plain", "file_size": len(self.document)}}}

    @staticmethod
    def call(name, args, suffix):
        return {"id": "call_" + suffix, "type": "function", "function": {
            "name": name, "arguments": json.dumps(args)}}

    def model(self, body):
        with self.lock:
            self.completions += 1
        messages = body["messages"]
        if self.without_search:
            names = {tool["function"]["name"] for tool in body.get("tools", [])}
            assert "web_search" not in names, "disabled web_search exposed to model"
            if body.get("tools"):
                assert "web_read" in names, "public URL reader was removed"
        if messages[0]["content"].startswith("Review a closed conversation"):
            with self.lock:
                self.analyses += 1
            return {"role": "assistant", "content": json.dumps({"summary": self.marker,
                "memories": [{"content": self.marker + " analyzed preference", "kind": "preference"}]})}
        last_user = max(i for i, message in enumerate(messages) if message["role"] == "user")
        user_text = messages[last_user]["content"]
        if not isinstance(user_text, str):
            user_text = " ".join(item.get("text", "") for item in user_text)
        tools = {message.get("tool_call_id"): json.loads(message["content"])
                 for message in messages[last_user + 1:] if message["role"] == "tool"}
        browser_setup = self.with_browser and not user_text.startswith("Background task step")
        if browser_setup:
            if "call_browser_open" not in tools:
                return {"role": "assistant", "tool_calls": [self.call("browser_open", {"url": "https://example.com"}, "browser_open")]}
            opened = tools["call_browser_open"]
            assert "error" not in opened and opened.get("title") == "Example Domain" and bool(opened.get("text")), "public browser navigation failed"
            if "call_browser_shot" not in tools:
                return {"role": "assistant", "tool_calls": [self.call("browser_screenshot", {"page_id": opened["page_id"]}, "browser_shot")]}
            assert tools["call_browser_shot"].get("artifact_id"), "browser screenshot was not saved"
        # A final empty assistant response ends the tool loop after persisted results.
        if any(message["role"] == "tool" for message in messages[last_user + 1:]) and (not browser_setup or "call_memory" in tools):
            return {"role": "assistant", "content": ""}
        if user_text.startswith("Background task step"):
            task = json.loads(user_text.split("Task: ", 1)[1])
            calls = [self.call("task_update", {"id": task["id"], "status": "completed",
                      "result": self.marker + " task persisted"}, "task_done"),
                     self.call("send_message", {"text": self.marker + ": completed"}, "notify_done")]
        else:
            assert self.marker in user_text, "unexpected model request"
            later = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=5)).isoformat()
            calls = [self.call("memory_update", {"content": self.marker + " preference", "kind": "preference"}, "memory"),
                     self.call("task_create", {"title": self.marker, "goal": self.marker,
                               "next_run_at": later}, "task"),
                     self.call("send_message", {"text": self.marker + ": accepted"}, "notify")]
            if not self.without_search:
                calls.insert(0, self.call("web_search", {"query": self.marker}, "search"))
        return {"role": "assistant", "content": "", "tool_calls": calls}


def handler_for(state):
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def json(self, value):
            data = json.dumps(value).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path.endswith("/smoke.txt"):
                self.send_response(200)
                self.send_header("Content-Length", str(len(state.document)))
                self.end_headers()
                self.wfile.write(state.document)
            else:
                self.send_error(404)

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
            method = self.path.rsplit("/", 1)[-1]
            if method == "getUpdates":
                offset = body.get("offset", 0)
                with state.lock:
                    state.offsets.append(offset)
                    replay = state.replay_once
                    state.replay_once = False
                updates = [state.update()] if offset <= state.update_id or replay else []
                if not updates:
                    time.sleep(.2)
                self.json({"ok": True, "result": updates})
            elif method == "getFile":
                self.json({"ok": True, "result": {"file_path": "documents/smoke.txt", "file_size": len(state.document)}})
            elif method == "sendChatAction":
                assert body.get("action") == "typing"
                self.json({"ok": True, "result": True})
            elif method == "sendRichMessage":
                assert "markdown" in body.get("rich_message", {}), "reply is not Rich Markdown"
                assert "text" not in body and "entities" not in body, "legacy text formatting leaked"
                with state.lock:
                    state.sent.append(body)
                    count = len(state.sent)
                self.json({"ok": True, "result": {"message_id": count}})
            elif self.path == "/v1/chat/completions":
                response = state.model(body)
                self.json({"choices": [{"message": response, "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": 50, "completion_tokens": 20}})
            elif self.path == "/search":
                with state.lock:
                    state.searches += 1
                xml = '<yandexsearch><response><results><grouping><group><doc><url>https://example.com</url><title>Local result</title><passages><passage>Smoke result</passage></passages></doc></group></grouping></results></response></yandexsearch>'
                self.json({"rawData": base64.b64encode(xml.encode()).decode()})
            else:
                self.send_error(404)
    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=18089)
    parser.add_argument("--host-address", default="host.docker.internal",
                        help="Host name reachable from containers; Docker Desktop default")
    parser.add_argument("--without-search", action="store_true", help="Prove startup without any Yandex credentials")
    parser.add_argument("--with-browser", action="store_true", help="Exercise real Chromium on public example.com and screenshot persistence")
    args = parser.parse_args()
    env = dict(os.environ)
    env.setdefault("POSTGRES_PORT", "15432")
    compose = ["docker", "compose"]
    if run(compose + ["ps", "-q", "jarvis"], env):
        raise SystemExit("Stop your Jarvis container before running isolated smoke; it will not be replaced.")
    marker = "mvp-smoke-" + uuid.uuid4().hex[:12]
    db = marker.replace("-", "_")
    volume = "jarvis_" + db
    bucket = marker
    # Every external key/endpoint is replaced. No dotenv secret is read by fake APIs.
    env.update(TELEGRAM_BOT_TOKEN="smoke-token", TELEGRAM_ALLOWED_USER_IDS="42", TELEGRAM_OWNER_USER_ID="42",
               TELEGRAM_ALLOWED_GROUP_IDS="", LLM_API_KEY="smoke-key", LLM_MODEL="smoke-model",
               YANDEX_API_KEY="smoke-key", YANDEX_FOLDER_ID="smoke-folder",
               SESSION_IDLE_TIMEOUT="2s", INITIATIVE_INTERVAL="0", TASK_MIN_INTERVAL="1s",
               MAX_AGENT_STEPS="12", TELEGRAM_BASE_URL=f"http://{args.host_address}:{args.port}",
               LLM_BASE_URL=f"http://{args.host_address}:{args.port}/v1",
               YANDEX_SEARCH_URL=f"http://{args.host_address}:{args.port}/search",
               TEMPORAL_TASK_QUEUE=marker)
    if args.without_search:
        env.update(YANDEX_API_KEY="", YANDEX_FOLDER_ID="", YANDEX_SEARCH_URL="")
    state = FakeState(marker, args.without_search, args.with_browser)
    server = http.server.ThreadingHTTPServer(("0.0.0.0", args.port), handler_for(state))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    created = False
    app_created = False
    try:
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "jarvis", "-d", "postgres", "-c", f'CREATE DATABASE "{db}"'], env)
        created = True
        run(compose + ["run", "--rm", "--no-deps", "-e", "S3_BUCKET=" + bucket, "minio-init"], env)
        with tempfile.TemporaryDirectory(prefix="jarvis-smoke-") as temp:
            # JSON is also valid YAML. Overrides only the application service, no infrastructure recreation.
            password = env.get("POSTGRES_PASSWORD", "jarvis-local-dev")
            override = {"services": {"jarvis": {"labels": {"jarvis.smoke": marker},
                "environment": {"DATABASE_URL": f"postgres://jarvis:{urllib.parse.quote(password, safe='')}@postgres:5432/{db}?sslmode=disable", "S3_BUCKET": bucket},
                "volumes": [{"type": "volume", "source": "smoke_knowledge", "target": "/data/knowledge"}]}},
                "volumes": {"smoke_knowledge": {"name": volume}}}
            path = Path(temp) / "override.json"
            path.write_text(json.dumps(override))
            cmd = compose + ["-f", "compose.yaml", "-f", str(path)]
            def sql(query):
                return run(compose + ["exec", "-T", "postgres", "psql", "-U", "jarvis", "-d", db, "-Atc", query], env)
            def health():
                with urllib.request.urlopen("http://127.0.0.1:8080/healthz", timeout=2) as response:
                    return json.load(response).get("status") == "ok"
            print("Starting isolated Jarvis container with local fake APIs", flush=True)
            app_created = True
            run(cmd + ["up", "-d", "--no-deps", "--no-build", "jarvis"], env)
            wait_until("application health", health)
            wait_until("two Telegram replies", lambda: len(state.sent) >= 2)
            wait_until("closed sessions analyzed", lambda: sql("SELECT count(*) > 0 AND bool_and(analyzed) FROM jarvis_sessions") == "t")
            assert sql("SELECT count(*) FROM jarvis_incoming WHERE handled") == "1", "incoming message not persisted exactly once"
            assert sql("SELECT count(*) FROM jarvis_tasks WHERE data->>'status'='completed'") == "1", "task not completed"
            assert sql("SELECT count(*) FROM jarvis_artifacts WHERE octet_length(content)=0") == str(2 if args.with_browser else 1), "artifact body should live in S3"
            assert len(state.sent) == 2 and all(str(message["chat_id"]) == "42" for message in state.sent), "unexpected deliveries"
            assert state.searches == (0 if args.without_search else 1), "Yandex adapter was not called exactly once"
            assert state.analyses > 0, "post-session analysis did not run"
            knowledge = run(cmd + ["exec", "-T", "jarvis", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"], env)
            assert marker + " preference" in knowledge, "automatic memory not saved"
            # Read the actual object from the isolated bucket through mc; no direct API shortcuts.
            objects = run(compose + ["run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "minio-init", "-c",
                          'mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null && mc find "$1"', "sh", "local/" + bucket], env).splitlines()
            assert len(objects) == (2 if args.with_browser else 1), "unexpected artifact object count"
            document_id = sql("SELECT id FROM jarvis_artifacts WHERE data->>'name'='smoke.txt'")
            document_key = "local/" + bucket + "/artifacts/" + hashlib.sha256(b"personal:42").hexdigest() + "/" + hashlib.sha256(document_id.encode()).hexdigest()
            assert document_key in objects, "document object not found"
            if args.with_browser:
                assert sql("SELECT count(*) FROM jarvis_artifacts WHERE data->>'mime'='image/png'") == "1", "browser screenshot metadata missing"
                processes = run(cmd + ["exec", "-T", "jarvis", "sh", "-c", "ps -o comm"], env).splitlines()
                assert not any(name.strip() in ("chromium", "chrome") for name in processes), "Chromium survived closed session"
            content = run(compose + ["run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "minio-init", "-c",
                          'mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null && mc cat "$1"', "sh", document_key], env)
            assert content == state.document.decode(), "S3 artifact differs from downloaded document"
            old_completions = state.completions
            old_analyses = state.analyses
            with state.lock:
                state.replay_once = True
                state.offsets = []
            print("Restarting Jarvis and replaying the same Telegram update", flush=True)
            run(cmd + ["restart", "jarvis"], env)
            wait_until("health after restart", health)
            wait_until("persisted Telegram offset after restart", lambda: len(state.offsets) > 2 and min(state.offsets) >= state.update_id + 1)
            time.sleep(4)
            assert len(state.sent) == 2, "restart duplicated Telegram delivery"
            assert state.completions == old_completions and state.analyses == old_analyses, "restart repeated finished model work"
            assert sql("SELECT count(*) FROM jarvis_incoming") == "1", "duplicate incoming inserted"
            knowledge = run(cmd + ["exec", "-T", "jarvis", "sh", "-c", "find /data/knowledge -type f -exec cat {} \\;"], env)
            assert marker + " preference" in knowledge, "memory did not survive restart"
            def logs_for(expression, limit=100):
                query = urllib.parse.urlencode({"query": expression, "limit": str(limit)})
                with urllib.request.urlopen("http://localhost:9428/select/logsql/query?" + query, timeout=3) as response:
                    return [json.loads(line) for line in response if line.strip()]
            def logged():
                return logs_for(f'task_queue:"{marker}" operation:application.started', 10)
            events = wait_until("application wide events in VictoriaLogs", lambda: logged() if len(logged()) >= 2 else None, timeout=20)
            assert all(event["result"] == "success" and "duration_ms" in event for event in events)
            assert all(event.get("service") == "jarvis" for event in events), "container service metadata missing"
            turn_id = sql("SELECT data->>'turn_id' FROM jarvis_outgoing WHERE data->>'trigger'='message' LIMIT 1")
            assert turn_id, "reply trace missing from persistent outbox"
            def traced():
                rows = logs_for(f'turn_id:"{turn_id}"')
                operations = {row.get("operation") for row in rows}
                return rows if {"turn.prepare", "llm.complete", "tool.execute", "turn.complete", "telegram.deliver"} <= operations else None
            traced_events = wait_until("correlated turn events in VictoriaLogs", traced, timeout=20)
            summary = next(row for row in traced_events if row.get("operation") == "turn.complete")
            assert float(summary["processing_ms"]) > 0 and int(summary["model_calls"]) >= 2
            assert summary["delivery_state"] == "queued", "processing claimed actual delivery"
            assert all(row.get("_msg", "").startswith("[jarvis]") and row.get("level") in ("info", "warn", "error") for row in traced_events), "normalized message or level missing"
            print(json.dumps({"result": "passed", "incoming": 1, "deliveries": 2, "task": "completed", "search_requests": state.searches,
                              "session_analyses": state.analyses, "s3_document": "verified", "memory": "survived restart",
                              "duplicate_delivery": False, "browser": "page, screenshot and process cleanup verified" if args.with_browser else "not exercised", "wide_events": len(events), "traced_events": len(traced_events), "telegram_format": "rich_markdown"}, ensure_ascii=False), flush=True)
    except Exception:
        if app_created:
            print(run(compose + ["logs", "--tail=60", "jarvis"], env), flush=True)
        raise
    finally:
        if app_created:
            subprocess.run(compose + ["stop", "jarvis"], cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(compose + ["rm", "-f", "jarvis"], cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        server.shutdown()
        server.server_close()
        if created:
            run(compose + ["exec", "-T", "postgres", "psql", "-U", "jarvis", "-d", "postgres", "-c", f'DROP DATABASE "{db}" WITH (FORCE)'], env)
        subprocess.run(["docker", "volume", "rm", volume], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(compose + ["run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "minio-init", "-c",
                       'mc alias set local http://minio:9000 "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null && mc rb --force "$1"', "sh", "local/" + bucket],
                       cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
