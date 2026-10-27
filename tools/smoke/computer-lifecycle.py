#!/usr/bin/env python3
"""Verify that app updates leave a separately provisioned user computer intact."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import uuid

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    args = parser.parse_args()
    project = "albert-lifecycle-" + uuid.uuid4().hex[:12]
    env = {key: value for key, value in os.environ.items() if key in {
        "PATH", "HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT",
        "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_DEFAULT_PLATFORM"}}
    with tempfile.TemporaryDirectory(prefix=project) as directory:
        root = Path(directory)
        computer = root / "user-computer"
        shutil.copytree(ROOT / "services/computer", computer)
        env.update(COMPUTER_IMAGE=args.image, COMPUTER_NETWORK_NAME=project,
                   COMPUTER_DIRECTORY=str(computer), COMPUTER_SSH_ADDRESS="computer:22")
        user = ["docker", "compose", "--env-file", "/dev/null", "-p", project + "-computer",
                "-f", str(computer / "compose.yaml")]
        # A disposable app stand-in exercises the actual attachment override.
        # No real application credentials, Telegram process or user data are used.
        app_file = root / "app.json"
        app_file.write_text(json.dumps({"services": {"openalbert": {
            "image": args.image, "entrypoint": ["sleep", "infinity"],
        }}}))
        app = ["docker", "compose", "--env-file", "/dev/null", "-p", project + "-app",
               "-f", str(app_file), "-f", str(computer / "app.compose.yaml")]

        def run(command):
            return subprocess.check_output(command, env=env, text=True).strip()

        try:
            run(user + ["up", "-d", "--no-build", "--wait", "--wait-timeout", "60"])
            container = run(user + ["ps", "-q", "computer"])
            run(["docker", "exec", container, "sh", "-ec",
                 "printf system-change > /usr/local/albert-update-sentinel; "
                 "printf work-change > /workspace/update-sentinel"])
            run(app + ["up", "-d", "--no-build"])
            run(app + ["exec", "-T", "openalbert", "python3", "-c",
                       "import socket; socket.create_connection(('computer', 22), timeout=5).close()"])
            run(app + ["up", "-d", "--no-build", "--force-recreate", "--remove-orphans"])
            run(app + ["down", "--volumes", "--remove-orphans"])
            assert run(user + ["ps", "-q", "computer"]) == container, "App update replaced the computer"
            assert run(["docker", "exec", container, "cat", "/usr/local/albert-update-sentinel"]) == "system-change"
            assert run(["docker", "exec", container, "cat", "/workspace/update-sentinel"]) == "work-change"
            print("App recreation/removal preserves the user computer, system changes and workspace: OK")
        finally:
            try:
                run(app + ["down", "--volumes", "--remove-orphans"])
            finally:
                # Clear only this test's bind data (root-owned on Linux).
                try:
                    run(user + ["down", "--volumes", "--remove-orphans"])
                finally:
                    run(["docker", "run", "--rm", "--network", "none", "--entrypoint", "sh",
                         "--mount", f"type=bind,src={computer},dst=/cleanup", args.image,
                         "-ec", "rm -rf /cleanup/client /cleanup/server /cleanup/home /cleanup/workspace"])


if __name__ == "__main__":
    main()
