"""Exercise the real Go desktop API with the real Swift API client on macOS."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def main():
    with tempfile.TemporaryDirectory(prefix="albert-contract-") as directory:
        binary = str(Path(directory) / "testserver")
        subprocess.run(["go", "build", "-o", binary, "./internal/desktopapi/testserver"], cwd=ROOT, check=True)
        server = subprocess.Popen([binary], cwd=ROOT, stderr=subprocess.PIPE, text=True)
        try:
            # The server emits readiness only after binding a random loopback port.
            ready = json.loads(server.stderr.readline())
            env = dict(os.environ, ALBERT_CONTRACT_URL=ready["url"])
            subprocess.run(["swift", "test", "--filter", "LiveAPIContractTests"], cwd=ROOT / "apps/macos", env=env, check=True)
        finally:
            server.terminate()
            try:
                server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
            server.stderr.close()


if __name__ == "__main__":
    main()
