#!/usr/bin/env python3
"""Verify encrypted S3 backup and cold restore with disposable Docker resources.

Uses PostgreSQL 17, two independent MinIO servers, and sleeping placeholders for
the application/Temporal processes. All three databases, knowledge and artifact
bytes must survive restore. Requires Docker; never reads the project's .env.
"""
import os
from pathlib import Path
import platform
import subprocess
import tempfile
import uuid


ROOT = Path(__file__).resolve().parents[2]


def main():
    controller_image = os.environ.get("OPENALBERT_TEST_BACKUP_CONTROLLER_IMAGE", "openalbert-ci:backup")
    subprocess.run(["docker", "build", "-f", "services/backup/Dockerfile", "--target", "backup", "-t", controller_image, "."], cwd=ROOT, check=True)
    # The test executes as root on Linux to preserve PostgreSQL ownership. Its
    # bind mount has the same absolute path on the host and in the test runner.
    with tempfile.TemporaryDirectory(prefix="albert-backup-smoke-", dir="/tmp") as directory:
        folder = Path(directory).resolve()
        project = "albert-backup-test-" + uuid.uuid4().hex[:12]
        binary = folder / "backup.test"
        env = dict(os.environ, GOOS="linux", GOARCH="arm64" if platform.machine() in {"arm64", "aarch64"} else "amd64", CGO_ENABLED="0")
        subprocess.run(["go", "test", "-c", "-o", str(binary), "./services/backup"], cwd=ROOT, env=env, check=True)
        command = ["docker", "run", "--rm", "--user", "0", "--mount", f"type=bind,src={folder},dst={folder}",
                   "--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
                   "-e", f"TMPDIR={folder}", "-e", "OPENALBERT_TEST_BACKUP_DOCKER=1",
                   "-e", f"OPENALBERT_TEST_BACKUP_PROJECT={project}",
                   "-e", f"OPENALBERT_TEST_BACKUP_CONTROLLER_IMAGE={controller_image}",
                   "--entrypoint", str(binary), "docker:29-cli", "-test.v", "-test.run", "^TestDockerBackupRestore$", "-test.timeout", "8m"]
        try:
            subprocess.run(command, cwd=ROOT, check=True)
        finally:
            # Also clean up after a test timeout, when Go's Cleanup cannot run.
            for name in (project, project + "-restored"):
                containers = subprocess.check_output(["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={name}"], text=True).split()
                if containers:
                    subprocess.run(["docker", "rm", "-f", *containers], check=True, stdout=subprocess.DEVNULL)
                subprocess.run(["docker", "network", "rm", name + "_default"], capture_output=True)
            subprocess.run(["docker", "rm", "-f", project + "-remote"], capture_output=True)
            # Only this disposable folder is cleaned; Linux container ownership
            # would otherwise prevent the host from removing test data.
            subprocess.run(["docker", "run", "--rm", "--mount", f"type=bind,src={folder},dst=/cleanup",
                            "--entrypoint", "/bin/sh", "docker:29-cli", "-ec", "rm -rf /cleanup/*"], check=True)


if __name__ == "__main__":
    main()
