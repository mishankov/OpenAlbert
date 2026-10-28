#!/usr/bin/env python3
"""Exercise the real SSH adapter on an isolated, disposable Ubuntu computer."""
import argparse
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]


def run(args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="openalbert-dev:computer")
    args = parser.parse_args()
    name = "openalbert-computer-test-" + uuid.uuid4().hex[:10]
    # Docker-managed volumes avoid root-owned temporary bind directories on Linux.
    volumes = [name + "-" + kind for kind in ("client", "server", "home", "workspace")]
    for volume in volumes:
        run(["docker", "volume", "create", volume], stdout=subprocess.DEVNULL)
    try:
        mounts = ["-v", volumes[0] + ":/client", "-v", volumes[1] + ":/server"]
        for _ in range(2):  # initialization preserves an existing identity
            run(["docker", "run", "--rm", *mounts, args.image, "initialize"])
        start = ["docker", "run", "-d", "--name", name, "-p", "127.0.0.1::22",
                 "-v", volumes[1] + ":/etc/openalbert-ssh:ro", "-v", volumes[2] + ":/home/albert",
                 "-v", volumes[3] + ":/workspace", args.image]
        run(start, stdout=subprocess.DEVNULL)
        with tempfile.TemporaryDirectory(prefix="openalbert-ssh-test-") as directory:
            folder = Path(directory)
            # Copy through a stopped holder; never print key bytes into logs.
            holder = name + "-keys"
            run(["docker", "create", "--name", holder, *mounts, args.image], stdout=subprocess.DEVNULL)
            try:
                run(["docker", "cp", holder + ":/client/id_ed25519", str(folder / "key")])
            finally:
                run(["docker", "rm", holder], stdout=subprocess.DEVNULL)

            def environment():
                address = subprocess.check_output(["docker", "port", name, "22/tcp"], text=True).strip()
                env = dict(os.environ, OPENALBERT_TEST_COMPUTER_ADDRESS=address,
                           OPENALBERT_TEST_COMPUTER_KEY=str(folder / "key"))
                return env

            def ready():
                for _ in range(40):
                    result = subprocess.run(["docker", "exec", name, "sh", "-c", "/usr/sbin/sshd -t && test -f /run/sshd.pid"],
                                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    if result.returncode == 0:
                        return
                    time.sleep(0.1)
                raise RuntimeError("SSH server did not start")

            ready()
            run(["go", "test", "-race", "-count=1", "./apps/backend/internal/computer", "-run", "^TestRealComputer$"], env=environment())
            # Only this disposable fixture enables a synthetic password. The
            # optional user-computer template remains public-key-only by default.
            run(["docker", "exec", name, "sh", "-ec",
                 "useradd --create-home --shell /bin/bash worker; "
                 "printf 'worker:synthetic-smoke-password\\n' | chpasswd; "
                 "sed -i 's/^PasswordAuthentication no/PasswordAuthentication yes/; "
                 "s/^AuthenticationMethods publickey/AuthenticationMethods any/; "
                 "s/^AllowUsers albert/AllowUsers albert worker/' /etc/ssh/sshd_config; "
                 "kill -HUP $(cat /run/sshd.pid)"])
            env = environment()
            env["OPENALBERT_TEST_COMPUTER_PASSWORD"] = "synthetic-smoke-password"
            run(["go", "test", "-race", "-count=1", "./apps/backend/internal/computer", "-run", "^TestRealComputerPassword$"], env=env)
            # Ensure the command started before destroying its supervisor.
            time.sleep(0.3)
            run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL)
            run(start, stdout=subprocess.DEVNULL)
            ready()
            env = environment()
            env["OPENALBERT_TEST_COMPUTER_RESTARTED"] = "1"
            run(["go", "test", "-race", "-count=1", "./apps/backend/internal/computer", "-run", "^TestRealComputerAfterRestart$"], env=env)
            print("Computer bootstrap, updates, key/password auth, transfers, retries, cancellation and recreation: OK")
    except BaseException:
        subprocess.run(["docker", "logs", name], check=False)
        raise
    finally:
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for volume in volumes:
            subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
