#!/usr/bin/env python3
"""Local verification and publication. Task is the public command catalogue.

Each invocation owns its reports, image tags and disposable Compose project.
Application .env files are never loaded. Publishing is an explicit command.
"""
import argparse
import contextlib
import datetime
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
REPORTS = ROOT / "reports/local"
COMPONENTS = ("openalbert", "stt", "log-gateway", "minio", "minio-init", "backup")
COMMANDS = ("check", "test", "test:go", "test:computer", "test:backup", "test:web", "test:macos", "images", "verify", "publish",
            "release:macos", "release:macos:draft", "screenshots", "reports")


class GoEvents:
    """Keep full machine-readable events while showing useful test failures live."""
    def __init__(self, directory):
        self.events = (directory / "go-tests.jsonl").open("w")
        self.pending = {}

    def line(self, text):
        try:
            event = json.loads(text)
        except ValueError:
            print(text, end="", flush=True)
            return
        if not isinstance(event, dict) or "Action" not in event:
            print(text, end="", flush=True)
            return
        self.events.write(text)
        self.events.flush()
        action, test, package = event["Action"], event.get("Test"), event.get("Package", "")
        key = (package, test)
        if action == "output":
            if test:
                self.pending.setdefault(key, []).append(event["Output"])
            else:
                print(event["Output"], end="", flush=True)
        elif test and action in {"pass", "skip", "fail"}:
            lines = self.pending.pop(key, [])
            if action == "fail":
                print("".join(lines), end="", flush=True)
                print(f"FAIL {package}/{test}", flush=True)

    def close(self):
        self.events.close()


def output(args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()


def test_environment():
    # Do not inherit optional live-eval settings, application tokens or signing keys.
    names = {"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "DEVELOPER_DIR", "SDKROOT",
             "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH",
             "GOPATH", "GOMODCACHE", "GOCACHE", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB",
             "PLAYWRIGHT_BROWSERS_PATH", "PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH",
             "ALBERT_TART", "ALBERT_VM_NAME", "ALBERT_VM_STATE", "ALBERT_VM_IMAGE", "TART_HOME"}
    return {key: value for key, value in os.environ.items() if key in names}


def stop(process):
    if process.poll() is not None:
        return
    # Let nested runners execute their own cleanup (including the shared VM lease).
    for signum, grace in ((signal.SIGINT, 30), (signal.SIGTERM, 10), (signal.SIGKILL, 5)):
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signum)
        try:
            process.wait(timeout=grace)
            return
        except subprocess.TimeoutExpired:
            pass
    raise RuntimeError("Child process did not stop")


class Run:
    def __init__(self, command, directory=None):
        self.id = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + uuid.uuid4().hex[:8]
        self.directory = Path(directory) if directory else REPORTS / self.id
        self.directory.mkdir(parents=True, mode=0o700)
        self.env = test_environment()
        self.data = {"command": command, "status": "running", "started_at": self.id,
                     "revision": output(["git", "rev-parse", "HEAD"]),
                     "dirty": bool(output(["git", "status", "--porcelain"])), "steps": [], "images": {}}
        self.repository = "openalbert-local-" + self.id.lower()
        self.started = time.monotonic()
        self.save()

    def save(self):
        self.data["seconds"] = round(time.monotonic() - self.started, 2)
        target = self.directory / "result.json"
        temporary = target.with_suffix(".tmp")
        temporary.write_text(json.dumps(self.data, ensure_ascii=False, indent=2) + "\n")
        temporary.replace(target)
        rows = [f"# {self.data['command']}: {self.data['status']}", "",
                f"Commit: `{self.data['revision']}` · dirty: {self.data['dirty']} · {self.data['seconds']} s", "",
                "| Step | Status | Seconds | Log |", "| --- | --- | ---: | --- |"]
        for step in self.data["steps"]:
            rows.append(f"| {step['name']} | {step['status']} | {step.get('seconds', '')} | [log]({step['log']}) |")
        if self.data.get("failure"):
            rows += ["", self.data["failure"]]
        for arch, images in self.data["images"].items():
            rows += ["", f"Images ({arch}):", "", *[f"- `{image}`" for image in images.values()]]
        artifacts = [path for path in ("go-tests.jsonl", "docs/html/index.html", "inspector/html/index.html",
                                      "docs/junit.xml", "inspector/junit.xml") if (self.directory / path).exists()]
        artifacts += [str(path.relative_to(self.directory)) for path in sorted((self.directory / "macos").glob("*/result.json"))]
        artifacts += [str(path.relative_to(self.directory)) for path in sorted((self.directory / "release").glob("*"))]
        if artifacts:
            rows += ["", "Artifacts:", "", *[f"- [{path}]({path})" for path in artifacts]]
        (self.directory / "summary.md").write_text("\n".join(rows) + "\n")

    def command(self, name, args, *, cwd=ROOT, env=None, timeout=1200):
        logfile = f"{len(self.data['steps']) + 1:02d}-{re.sub('[^a-zA-Z0-9_-]', '-', name)}.log"
        step = {"name": name, "status": "running", "log": logfile}
        self.data["steps"].append(step)
        self.save()
        print(f"\n▶ {name}", flush=True)
        started = time.monotonic()
        process = None
        go_events = GoEvents(self.directory) if name == "go-race-integration" else None
        try:
            with (self.directory / logfile).open("w") as log:
                process = subprocess.Popen([str(value) for value in args], cwd=cwd,
                                           env=self.env if env is None else env,
                                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                           start_new_session=True)

                def stream():
                    for line in iter(process.stdout.readline, b""):
                        text = line.decode(errors="replace")
                        log.write(text)
                        log.flush()
                        if go_events:
                            go_events.line(text)
                        else:
                            print(text, end="", flush=True)

                reader = threading.Thread(target=stream, daemon=True)
                reader.start()
                try:
                    status = process.wait(timeout=timeout)
                finally:
                    stop(process)
                    reader.join(timeout=10)
                    process.stdout.close()
                if status:
                    raise RuntimeError(f"{name} failed (exit {status}); see {logfile}")
            step["status"] = "passed"
        except BaseException as error:
            step["status"] = "cancelled" if isinstance(error, KeyboardInterrupt) else "failed"
            step["failure"] = str(error)
            raise
        finally:
            if go_events:
                go_events.close()
            step["seconds"] = round(time.monotonic() - started, 2)
            self.save()
            print(f"{step['status']}: {name} ({step['seconds']} s)", flush=True)

    def check(self):
        self.command("format", [sys.executable, "tools/local.py", "--format-check"])
        self.command("vet", ["go", "vet", "./..."])
        self.command("local-runner-tests", [sys.executable, "-m", "unittest", "discover", "-s", "tools", "-p", "test_*.py", "-v"])
        self.command("compose", [sys.executable, "tools/checks/check-compose.py"])
        self.command("test-compose", ["docker", "compose", "--project-directory", str(ROOT), "--env-file", "/dev/null", "-f", "deploy/compose.test.yaml", "config", "-q"])
        self.command("bake-config", ["docker", "buildx", "bake", "-f", "deploy/docker-bake.hcl", "--print"])

    def go(self):
        compose = ["docker", "compose", "--project-directory", str(ROOT), "--env-file", "/dev/null", "-f", "deploy/compose.test.yaml", "-p", self.repository]
        try:
            self.command("go-services", compose + ["up", "-d", "--wait", "--wait-timeout", "90", "postgres", "victorialogs"])
            self.command("go-race-integration", compose + ["run", "--build", "--rm", "go"], timeout=1800)
        finally:
            try:
                self.command("go-service-logs", compose + ["logs", "--no-color"], timeout=60)
            finally:
                self.command("go-cleanup", compose + ["down", "--volumes", "--remove-orphans", "--rmi", "local"], timeout=120)

    def computer(self):
        image = self.repository + ":computer-test"
        built = False
        try:
            self.command("computer-test-build", ["docker", "build", "-t", image, "services/computer"], timeout=1200)
            built = True
            self.command("computer-lifecycle", [sys.executable, "tools/smoke/computer-lifecycle.py", "--image", image], timeout=300)
            self.command("computer-ssh", [sys.executable, "tools/smoke/computer-smoke.py", "--image", image], timeout=900)
        finally:
            if built:
                self.command("computer-test-cleanup", ["docker", "image", "rm", image], timeout=60)

    def backup(self):
        self.command("backup-restore", [sys.executable, "tools/smoke/backup-smoke.py"], timeout=1200)

    def web(self):
        self.command("npm-install", ["npm", "ci"], cwd=ROOT / "docs")
        self.command("playwright-install", ["./node_modules/.bin/playwright", "install", "chromium"], cwd=ROOT / "docs")
        self.command("docs-tests", ["npm", "test"], cwd=ROOT / "docs")
        self.command("docs-build", ["npm", "run", "build"], cwd=ROOT / "docs")
        for suite, config, variable in (("docs", "docs/playwright.config.mjs", "DOCS_TEST_PORT"),
                                        ("inspector", "apps/backend/internal/inspector/playwright.config.mjs", "INSPECTOR_TEST_PORT")):
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            env = dict(self.env, PLAYWRIGHT_HTML_OPEN="never", PLAYWRIGHT_HTML_OUTPUT_DIR=str(self.directory / suite / "html"),
                       PLAYWRIGHT_JUNIT_OUTPUT_FILE=str(self.directory / suite / "junit.xml"))
            env[variable] = str(port)
            self.command(suite + "-browser", ["./docs/node_modules/.bin/playwright", "test", "-c", config,
                                             "--reporter=list,html,junit", "--output", self.directory / suite / "results"], env=env)

    def macos(self):
        if sys.platform != "darwin":
            raise RuntimeError("macOS verification requires a Mac with the managed Tart VM; no stage is skipped")
        # Signature/tamper tests require Sparkle's pinned signing tools. Resolve
        # them before Python discovery so a fresh checkout cannot skip that suite.
        self.command("macos-resolve", ["swift", "package", "--package-path", "apps/macos", "resolve"])
        self.command("macos-tooling", [sys.executable, "-m", "unittest", "discover", "-s", "apps/macos", "-p", "test_*.py", "-v"])
        self.command("go-swift-contract", [sys.executable, "tools/checks/check-contract.py"])
        self.vm("test")
        self.vm("smoke", "--configuration", "release")

    def vm(self, mode, *args):
        self.command("macos-" + mode, [sys.executable, "apps/macos/desktop_vm.py", mode,
                                     "--output", self.directory / "macos", *args], timeout=2400)

    def images(self, arch, deployment=False):
        env = dict(self.env, LOCAL_REPOSITORY=self.repository, ARCH=arch, REVISION=self.data["revision"],
                   DOCKER_DEFAULT_PLATFORM="linux/" + arch)
        self.command("images-" + arch, ["docker", "buildx", "bake", "-f", "deploy/docker-bake.hcl", "--load",
                                       "--metadata-file", self.directory / ("images-" + arch + ".json")], env=env, timeout=7200)
        images = {name: f"{self.repository}:{name}-{arch}" for name in COMPONENTS}
        self.data["images"][arch] = images
        self.save()
        env["MINIO_SMOKE_IMAGE"] = images["minio"]
        for component, image in images.items():
            self.command(f"smoke-{component}-{arch}", ["bash", "tools/smoke/smoke-image.sh", component, image], env=env, timeout=900)
        if deployment:
            args = [sys.executable, "tools/smoke/release-smoke.py"]
            for component, image in images.items():
                if component not in {"stt", "backup"}:
                    args += ["--image", component + "=" + image]
            self.command("deployment-" + arch, args, env=env, timeout=1200)

    def native_arch(self):
        arch = output(["docker", "info", "--format", "{{.Architecture}}"], self.env)
        aliases = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}
        if arch not in aliases:
            raise RuntimeError("Unsupported Docker engine architecture: " + arch)
        return aliases[arch]

    def clean_checkout(self, master=False):
        if output(["git", "status", "--porcelain"], self.env):
            raise RuntimeError("Publication requires a clean checkout; commit the exact sources first")
        if master and output(["git", "branch", "--show-current"], self.env) != "master":
            raise RuntimeError("Image publication is allowed only from master")
        if output(["git", "rev-parse", "HEAD"], self.env) != self.data["revision"]:
            raise RuntimeError("HEAD changed during verification")

    def tests(self):
        self.go()
        self.backup()
        self.web()
        self.macos()

    def publish(self):
        prefix = os.environ.get("OPENALBERT_IMAGE_PREFIX", "")
        if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/[a-z0-9][a-z0-9_./-]*", prefix):
            raise RuntimeError("Set OPENALBERT_IMAGE_PREFIX to a lowercase registry/owner/repository")
        self.clean_checkout(master=True)
        self.check()
        self.tests()
        for arch in ("amd64", "arm64"):
            self.images(arch, deployment=True)
        self.clean_checkout(master=True)
        # Upload only after BOTH architectures, deployment tests and macOS passed.
        for component in COMPONENTS:
            image = prefix + "/" + component
            sha = "sha-" + self.data["revision"]
            variants = []
            for arch in ("amd64", "arm64"):
                remote = f"{image}:{sha}-{arch}"
                self.command("tag-" + component + "-" + arch, ["docker", "tag", self.data["images"][arch][component], remote])
                self.command("push-" + component + "-" + arch, ["docker", "push", remote], timeout=1800)
                variants.append(remote)
            self.command("manifest-" + component, ["docker", "buildx", "imagetools", "create", "--tag", image + ":" + sha,
                                                  "--tag", image + ":master", *variants])

    def release_macos(self, draft=False):
        self.clean_checkout()
        # Signing secrets are passed only to the existing isolated signing helper.
        env = dict(self.env)
        names = ("ALBERT_UPDATE_FEED_URL", "SPARKLE_PUBLIC_KEY", "ALBERT_SIGNING_CERT_SHA1", "RELEASE_REPOSITORY", "RELEASE_TAG")
        env.update({name: os.environ[name] for name in names if name in os.environ})
        self.command("release-config", [sys.executable, "apps/macos/release.py", "check-config"], env=env)
        self.command("release-history", [sys.executable, "tools/macos_release.py", "history", "--output", self.directory / "release-history"], env=env)
        self.macos()
        signing = dict(env)
        signing.update({name: os.environ[name] for name in ("ALBERT_SIGNING_P12", "ALBERT_SIGNING_PASSWORD", "SPARKLE_PRIVATE_KEY") if name in os.environ})
        self.command("release-signing", [sys.executable, "apps/macos/release-signing.py"], env=signing, timeout=1800)
        self.vm("smoke", "--app", ROOT / "apps/macos/build/Альберт.app")
        import shutil
        shutil.copytree(ROOT / "apps/macos/build/release", self.directory / "release")
        self.clean_checkout()
        if draft:
            self.command("release-draft", [sys.executable, "tools/macos_release.py", "draft", "--artifacts", self.directory / "release"], env=env)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", nargs="?", choices=COMMANDS)
    parser.add_argument("--format-check", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.format_check:
        files = output(["gofmt", "-l", "apps/backend", "services"])
        if files:
            print(files)
            return 1
        return 0
    if args.command == "reports":
        latest = REPORTS / "latest.txt"
        if not latest.exists():
            parser.error("No local runs yet")
        summary = REPORTS / latest.read_text().strip() / "summary.md"
        print(summary.read_text())
        print(summary)
        return 0
    if not args.command:
        parser.error("Choose a command, or run task --list")

    def cancel(signum, frame):
        raise KeyboardInterrupt(f"Signal {signum}")

    signal.signal(signal.SIGTERM, cancel)
    run = Run(args.command)
    code = 0
    try:
        actions = {"check": run.check, "test": run.tests, "test:go": run.go, "test:computer": run.computer, "test:backup": run.backup, "test:web": run.web, "test:macos": run.macos,
                   "images": lambda: run.images(run.native_arch()), "verify": lambda: (run.check(), run.tests(), run.images(run.native_arch(), deployment=True)),
                   "publish": run.publish, "release:macos": run.release_macos, "release:macos:draft": lambda: run.release_macos(draft=True),
                   "screenshots": lambda: run.vm("screenshots")}
        actions[args.command]()
        run.data["status"] = "passed"
    except KeyboardInterrupt as error:
        run.data.update(status="cancelled", failure=str(error) or "Interrupted")
        code = 130
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        run.data.update(status="failed", failure=str(error))
        print(str(error), file=sys.stderr)
        code = 1
    finally:
        run.save()
        temporary = REPORTS / (run.id + ".latest")
        temporary.write_text(run.id + "\n")
        temporary.replace(REPORTS / "latest.txt")
        print(f"\n{run.data['status']}: {run.data['command']} — {run.data['seconds']} s\nReport: {run.directory / 'summary.md'}")
    return code


if __name__ == "__main__":
    sys.exit(main())
