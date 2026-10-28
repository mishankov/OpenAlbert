"""Prepare a user-owned Linux computer; invoked over SSH, never with sudo."""
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile


def prepare():
    if sys.platform != "linux":
        return {"error": "linux_required"}
    if sys.version_info < (3, 9):
        return {"error": "python_version"}
    if not os.access("/bin/bash", os.X_OK):
        return {"error": "bash_required"}
    root = Path.home() / ".openalbert"
    for directory in (root, root / "runners", root / "executions", root / "workspace"):
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        if not os.access(directory, os.W_OK | os.X_OK):
            return {"error": "home_not_writable"}
    digest, action = sys.argv[1:]
    target = root / "runners" / (digest + ".py")
    if target.is_file() and hashlib.sha256(target.read_bytes()).hexdigest() == digest:
        return {"state": "ready"}
    if action == "check":
        return {"state": "install"}
    source = sys.stdin.buffer.read((1 << 20) + 1)
    if len(source) > 1 << 20 or hashlib.sha256(source).hexdigest() != digest:
        return {"error": "invalid_source"}
    compile(source, str(target), "exec")
    fd, temporary = tempfile.mkstemp(dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(source)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
        directory = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return {"state": "ready"}


os.umask(0o077)
try:
    result = prepare()
except Exception:
    # Do not return remote paths, source bytes or OS diagnostics to the model.
    result = {"error": "preparation_failed"}
print(json.dumps(result))
