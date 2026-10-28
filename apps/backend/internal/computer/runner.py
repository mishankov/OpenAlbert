#!/usr/bin/python3
"""One-shot SSH protocol and detached, serialized command supervisor (Linux).

The immutable request is committed before spawn. A replay observes that request;
an interrupted/ambiguous start is never replayed. Job records are evidence, not
a tamper-proof boundary against the machine's sudo-capable owner.
"""
import base64
import fcntl
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager

ROOT = Path.home() / ".openalbert" / "executions"
MAX_FILE = 20 << 20
MAX_OUTPUT = 1 << 20  # per stream; further output is drained and discarded
TERMINAL = {"completed", "failed", "cancelled", "timed_out", "unknown"}


def atomic_json(path, value):
    fd, temporary = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


@contextmanager
def locked(path):
    with open(path, "a") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        yield


def identity(pid):
    try:
        # comm may contain spaces and parentheses; fields after its LAST ')' are stable.
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return None
        return [Path("/proc/sys/kernel/random/boot_id").read_text().strip(), fields[19]]
    except (OSError, IndexError):
        return None


def alive(meta):
    return bool(meta.get("worker_identity")) and identity(meta.get("worker_pid", 0)) == meta["worker_identity"]


def job_path(execution_id):
    if not isinstance(execution_id, str) or not re.fullmatch(r"[a-f0-9]{32}", execution_id):
        raise ValueError("invalid execution ID")
    return ROOT / execution_id


def status(job, request):
    if not (job / "request.json").exists():
        return {"execution_id": job.name, "state": "unknown"}
    with locked(job / "control.lock"):
        meta = json.loads((job / "status.json").read_text()) if (job / "status.json").exists() else {"state": "unknown"}
        if meta["state"] not in TERMINAL and not alive(meta):
            meta["state"] = "unknown"
        result = {key: meta[key] for key in ("state", "exit_code", "output_truncated") if key in meta}
    result["execution_id"] = job.name
    for channel in ("stdout", "stderr"):
        offset = request.get(channel + "_offset", 0)
        if type(offset) is not int or offset < 0:
            raise ValueError("invalid output offset")
        data = b""
        size = 0
        if (job / channel).exists():
            with open(job / channel, "rb") as stream:
                size = os.fstat(stream.fileno()).st_size
                offset = min(offset, size)
                stream.seek(offset)
                data = stream.read(2048)
        else:
            offset = 0
        result[channel] = data.decode("utf-8", errors="replace")
        result[channel + "_offset"] = offset + len(data)
        result["has_more"] = result.get("has_more", False) or offset + len(data) < size
    return result


def validate_execution(request):
    command = request.get("command")
    cwd = request.get("cwd") or str(Path.home() / ".openalbert" / "workspace")
    timeout = request.get("timeout_seconds", 300)
    if not isinstance(command, str) or not command.strip() or len(command.encode()) > 32000 or "\0" in command:
        raise ValueError("command must contain 1–32000 bytes without NUL")
    if not isinstance(cwd, str) or not cwd.startswith("/") or len(cwd) > 4096 or "\0" in cwd:
        raise ValueError("cwd must be an absolute path")
    if type(timeout) is not int or not 1 <= timeout <= 86400:
        raise ValueError("timeout_seconds must be 1–86400")
    return {"command": command, "cwd": cwd, "timeout_seconds": timeout}


def start(request):
    spec = validate_execution(request)
    job = job_path(request.get("execution_id"))
    job.mkdir(mode=0o700, parents=True, exist_ok=True)
    with locked(job / "control.lock"):
        if (job / "request.json").exists():
            if json.loads((job / "request.json").read_text()) != spec:
                raise ValueError("execution ID already belongs to another command")
        else:
            # Request first. A crash before spawn remains unknown, never a new start.
            atomic_json(job / "request.json", spec)
            meta = {"state": "queued"}
            atomic_json(job / "status.json", meta)
            worker = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "_worker", job.name],
                                      stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True)
            meta.update(worker_pid=worker.pid, worker_identity=identity(worker.pid))
            atomic_json(job / "status.json", meta)
    until = time.monotonic() + request.get("wait_seconds", 1)
    while True:
        result = status(job, request)
        if result["state"] in TERMINAL or time.monotonic() >= until:
            return result
        time.sleep(0.05)


def update(job, **fields):
    with locked(job / "control.lock"):
        meta = json.loads((job / "status.json").read_text())
        meta.update(fields)
        atomic_json(job / "status.json", meta)


def kill_group(pid, sig):
    try:
        os.killpg(pid, sig)
    except ProcessLookupError:
        return
    except PermissionError:
        pass
    # The normal computer user has passwordless sudo. Signal ordinary sudo
    # descendants as well; another session/process group can still escape.
    try:
        subprocess.run(["sudo", "-n", "/bin/kill", "-" + str(int(sig)), "--", "-" + str(pid)],
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=2, check=False)
    except (OSError, subprocess.TimeoutExpired):
        pass


def worker(job):
    # Request handlers use a separate lock; status/cancel never wait for a job.
    with open(ROOT / "machine.lock", "a") as machine:
        while True:
            try:
                fcntl.flock(machine, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if (job / "cancel").exists():
                    update(job, state="cancelled")
                    return
                time.sleep(0.1)
        if (job / "cancel").exists():
            update(job, state="cancelled")
            return
        spec = json.loads((job / "request.json").read_text())
        # Synchronize with the request handler that recorded this worker's identity.
        update(job, state="running")
        process = None
        try:
            process = subprocess.Popen(["/bin/bash", "-c", spec["command"]], cwd=spec["cwd"],
                                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, start_new_session=True)
            drain(job, process, spec["timeout_seconds"])
        except Exception:
            if process is not None:
                kill_group(process.pid, signal.SIGKILL)
                process.wait()
            (job / "stderr").write_text("Command could not be supervised; check cwd and machine state.\n")
            update(job, state="failed")


def drain(job, process, timeout):
    selector = selectors.DefaultSelector()
    outputs = {"stdout": open(job / "stdout", "wb"), "stderr": open(job / "stderr", "wb")}
    sizes = {"stdout": 0, "stderr": 0}
    for channel in outputs:
        pipe = getattr(process, channel)
        os.set_blocking(pipe.fileno(), False)
        selector.register(pipe, selectors.EVENT_READ, channel)
    deadline = time.monotonic() + timeout
    reason = None
    stopped_at = None
    truncated = False
    forced = False
    cleaned_orphans = False
    try:
        while selector.get_map() or process.poll() is None:
            now = time.monotonic()
            if reason is None and process.poll() is None and ((job / "cancel").exists() or now >= deadline):
                reason = "cancelled" if (job / "cancel").exists() else "timed_out"
                stopped_at = now
                kill_group(process.pid, signal.SIGTERM)
            if stopped_at is not None and now - stopped_at >= 1 and not forced:
                kill_group(process.pid, signal.SIGKILL)
                forced = True
            if process.poll() is not None and not cleaned_orphans:
                # Background descendants cannot retain the machine lock forever.
                kill_group(process.pid, signal.SIGKILL)
                cleaned_orphans = True
            for key, _ in selector.select(0.1):
                data = os.read(key.fileobj.fileno(), 65536)
                if not data:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                channel = key.data
                remaining = MAX_OUTPUT - sizes[channel]
                if len(data) > remaining:
                    truncated = True
                kept = data[:remaining]
                outputs[channel].write(kept)
                outputs[channel].flush()
                sizes[channel] += len(kept)
        code = process.wait()
        for stream in outputs.values():
            stream.flush()
            os.fsync(stream.fileno())
        update(job, state=reason or ("completed" if code == 0 else "failed"),
               exit_code=code, output_truncated=truncated)
    finally:
        selector.close()
        for stream in outputs.values():
            stream.close()


def transfer_path(request):
    raw = request.get("path")
    if not isinstance(raw, str) or not raw.startswith("/") or len(raw) > 4096 or "\0" in raw:
        raise ValueError("path must be absolute")
    path = Path(raw).resolve()
    if not any(path.is_relative_to(root) and path != root for root in (Path("/workspace"), Path.home())):
        raise ValueError("file transfer is limited to /workspace and the user's home")
    return path


def transfer(request):
    path = transfer_path(request)
    if request["action"] == "upload":
        data = base64.b64decode(request.get("data", ""), validate=True)
        if len(data) > MAX_FILE:
            raise ValueError("file exceeds 20 MiB")
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(dir=path.parent)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            # No overwrite: replay only succeeds for identical existing bytes.
            try:
                os.link(temporary, path)
            except FileExistsError:
                if not path.is_file() or path.stat().st_size != len(data) or path.read_bytes() != data:
                    raise ValueError("destination already exists with different content; use another path")
        finally:
            os.unlink(temporary)
        return {"path": str(path), "size": len(data)}
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NONBLOCK), "rb") as stream:
        import stat
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError("only regular files can be downloaded")
        data = stream.read(MAX_FILE + 1)
    if len(data) > MAX_FILE:
        raise ValueError("file exceeds 20 MiB")
    return {"path": str(path), "size": len(data), "data": base64.b64encode(data).decode()}


def main():
    os.umask(0o077)
    ROOT.mkdir(parents=True, exist_ok=True)
    if len(sys.argv) == 3 and sys.argv[1] == "_worker":
        worker(job_path(sys.argv[2]))
        return
    try:
        raw = sys.stdin.buffer.read((29 << 20) + 1)
        if len(raw) > 29 << 20:
            raise ValueError("request too large")
        request = json.loads(raw)
        action = request["action"]
        if type(request.get("wait_seconds", 1)) is not int or not 0 <= request.get("wait_seconds", 1) <= 5:
            raise ValueError("wait_seconds must be 0–5")
        if action == "exec":
            result = start(request)
        elif action in ("status", "cancel"):
            job = job_path(request.get("execution_id"))
            if action == "cancel" and job.is_dir():
                (job / "cancel").touch()
                # Allow the worker to persist its actual outcome. Cancellation is best effort.
                until = time.monotonic() + 2
                while status(job, request)["state"] not in TERMINAL and time.monotonic() < until:
                    time.sleep(0.05)
            result = status(job, request)
        elif action in ("upload", "download"):
            result = transfer(request)
        else:
            raise ValueError("unknown computer action")
    except Exception as error:
        # OS messages may contain file contents/credentials; use a fixed error there.
        result = {"error": str(error) if isinstance(error, ValueError) else "computer operation failed"}
    print(json.dumps(result))


if __name__ == "__main__":
    main()
