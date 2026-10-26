#!/usr/bin/env python3
"""Compile on the host; run native Albert UI checks in an owned Tart macOS VM.

There is no host-desktop fallback. CI invokes its own explicit direct-runner
commands instead. Only prepared binaries, allowlisted source and optional image
fixtures enter the read-only share; a fresh output directory is writable.
"""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import tarfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
APP_ROOT = ROOT / "apps/macos"
DEFAULT_IMAGE = "ghcr.io/cirruslabs/macos-golden-gate-xcode:27"
DEFAULT_NAME = "albert-desktop-development"
GUEST_INPUT = "/Volumes/My Shared Files/albert-input"
GUEST_OUTPUT = "/Volumes/My Shared Files/albert-output"
HOST_ENVIRONMENT = {"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL", "DEVELOPER_DIR", "SDKROOT", "TOOLCHAINS"}


class VMError(RuntimeError):
    def __init__(self, message, status=1):
        super().__init__(message)
        self.status = status


class Cancelled(VMError):
    def __init__(self, signum=signal.SIGINT):
        super().__init__(f"Cancelled by signal {signum}", 128 + signum)


class TransportTimeout(VMError):
    pass


def atomic_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".albert-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream, indent=2, ensure_ascii=False)
            stream.write("\n")
        os.replace(temporary, path)  # Replace a guest-created symlink, never follow it.
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)


def finite_timeout(value):
    if not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
        raise VMError("Timeouts must be finite positive numbers")
    return value


@contextlib.contextmanager
def secure_log(path, truncate=False):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise VMError("Host log must be a regular file without other hard links")
        if truncate:
            os.ftruncate(descriptor, 0)
        with os.fdopen(descriptor, "ab", closefd=False) as stream:
            yield stream
    finally:
        os.close(descriptor)


def safe_environment(env=None):
    env = os.environ if env is None else env
    return {key: value for key, value in env.items() if key in HOST_ENVIRONMENT}


class Configuration:
    def __init__(self, env=None, home=None, root=ROOT):
        env = os.environ if env is None else env
        self.home = Path(home or Path.home()).resolve()
        self.root = Path(root).resolve()
        self.name = env.get("ALBERT_VM_NAME", DEFAULT_NAME)
        if not re.fullmatch(r"albert-[A-Za-z0-9-]{1,80}", self.name):
            raise VMError("ALBERT_VM_NAME must start with albert- and contain letters, numbers and hyphens")
        self.image = env.get("ALBERT_VM_IMAGE", DEFAULT_IMAGE)
        if not re.fullmatch(r"ghcr\.io/[A-Za-z0-9_./-]+(?::[A-Za-z0-9_.-]+|@sha256:[0-9a-f]{64})", self.image):
            raise VMError("ALBERT_VM_IMAGE must name a cached ghcr.io OCI image")
        base = self.home / "Library/Application Support/albert-development"
        self.state = Path(env.get("ALBERT_VM_STATE", str(base / "desktop-vm"))).expanduser()
        self.tart_home = Path(env.get("TART_HOME", str(self.home / ".tart"))).expanduser()
        self.lock = base / "desktop-vm.lock"  # One lock across all worktrees and VM names.
        self.owner = self.state / (self.name + ".json")
        self.vm_directory = self.tart_home / "vms" / self.name
        self.environment = safe_environment(env)
        self.environment.update(TART_HOME=str(self.tart_home), TART_NO_AUTO_PRUNE="1")
        explicit = env.get("ALBERT_TART")
        candidates = [explicit] if explicit else [
            str(self.home / "Library/Application Support/albert-development/tart/tart.app/Contents/MacOS/tart"),
            str(self.home / "Library/Application Support/polka-development/tart/tart.app/Contents/MacOS/tart"),
            "/opt/homebrew/bin/tart", "/usr/local/bin/tart", shutil.which("tart", path=env.get("PATH", "")),
        ]
        if explicit and not Path(explicit).is_absolute():
            raise VMError("ALBERT_TART must be an absolute executable path")
        self.tart = next((Path(path) for path in candidates if path and Path(path).is_file() and os.access(path, os.X_OK)), None)
        if self.tart is None:
            raise VMError("Tart is unavailable. Install openai/tools/tart or set ALBERT_TART to tart.app/Contents/MacOS/tart; no host fallback")
        if not self.state.is_absolute() or not self.tart_home.is_absolute():
            raise VMError("VM state and TART_HOME must be absolute paths")
        if ":" in str(self.state) or ":" in str(self.tart_home):
            raise VMError("Tart share/state paths must not contain a colon")


@contextlib.contextmanager
def global_lock(path, timeout=600):
    finite_timeout(timeout)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(path, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    deadline = time.monotonic() + timeout
    try:
        while True:
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise VMError("Another Albert VM invocation owns the global lock; no desktop fallback")
                time.sleep(0.05)
        os.ftruncate(descriptor, 0)
        os.write(descriptor, json.dumps({"pid": os.getpid(), "started_at": time.time()}).encode())
        yield
    finally:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        os.close(descriptor)
        # Keep the inode: unlinking a flock file allows a third process to bypass waiters.


@contextlib.contextmanager
def cancellation_signals():
    old = {}
    def cancel(signum, frame):
        raise Cancelled(signum)
    for signum in (signal.SIGINT, signal.SIGTERM):
        old[signum] = signal.signal(signum, cancel)
    try:
        yield
    finally:
        for signum, handler in old.items():
            signal.signal(signum, handler)


def stop_owned(process, grace=3):
    # Signals target only this invocation's unreaped child process group. Never
    # use `tart stop <name>`, which could stop a VM belonging to somebody else.
    old = {}
    for signum in (signal.SIGINT, signal.SIGTERM):
        with contextlib.suppress(ValueError):  # Tests may call transport from a thread.
            old[signum] = signal.signal(signum, signal.SIG_IGN)
    try:
        if process.poll() is None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=grace)
            except subprocess.TimeoutExpired:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=3)
        else:
            process.wait()
    finally:
        for signum, handler in old.items():
            signal.signal(signum, handler)


def execute(arguments, *, env, timeout=30, log=None, check=True, alive=None):
    finite_timeout(timeout)
    with tempfile.TemporaryFile() if log is None else secure_log(log) as output:
        process = None
        try:
            process = subprocess.Popen([str(value) for value in arguments], env=env, stdin=subprocess.DEVNULL,
                                       stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
            deadline = time.monotonic() + timeout
            while process.poll() is None:
                if log is not None and os.fstat(output.fileno()).st_size > 64 << 20:
                    raise VMError("Command log exceeds 64 MiB")
                if alive is not None and alive.poll() is not None:
                    raise VMError("Owned Tart VM exited before the guest command completed")
                if time.monotonic() >= deadline:
                    raise TransportTimeout(f"{Path(arguments[0]).name} {arguments[1] if len(arguments)>1 else ''} timed out")
                time.sleep(0.05)
            process.wait()
            if log is None:
                output.seek(0)
                data = output.read(1024 * 1024 + 1)
                if len(data) > 1024 * 1024:
                    raise VMError("Unexpectedly large transport response")
            else:
                data = b""
            if check and process.returncode:
                text = data[-4096:].decode(errors="replace") if data else f"See {log}"
                raise VMError(f"Command failed ({process.returncode}): {text}", process.returncode if process.returncode > 0 else 1)
            return process.returncode, data
        finally:
            if process is not None:
                stop_owned(process)


def tart(config, arguments, **kwargs):
    return execute([config.tart, *arguments], env=config.environment, **kwargs)


def list_vms(config):
    try:
        value = json.loads(tart(config, ["list", "--format", "json"])[1])
    except (ValueError, TypeError):
        raise VMError("Tart returned an invalid VM list") from None
    if not isinstance(value, list):
        raise VMError("Tart returned an invalid VM list")
    return value


def vm_info(config):
    try:
        value = json.loads(tart(config, ["get", config.name, "--format", "json"])[1])
    except (ValueError, TypeError):
        raise VMError("Tart returned invalid VM configuration") from None
    if not isinstance(value, dict) or type(value.get("Running")) is not bool:
        raise VMError("Tart returned invalid VM running state")
    return value


def vm_identity(config):
    if config.vm_directory.is_symlink():
        raise VMError("Managed VM directory must not be a symlink")
    info = config.vm_directory.stat()
    if not stat.S_ISDIR(info.st_mode):
        raise VMError("Managed VM directory is not a directory")
    return [info.st_dev, info.st_ino]


def require_owned(config):
    try:
        if config.owner.is_symlink():
            raise VMError("Owner marker must not be a symlink")
        value = json.loads(config.owner.read_text())
        if value.get("schema") != 1 or value.get("name") != config.name or value.get("requested_image") != config.image or value.get("uid") != os.getuid() or value.get("directory_identity") != vm_identity(config):
            raise VMError("VM ownership or base image does not match this runner")
        return value
    except (OSError, ValueError, AttributeError):
        raise VMError("VM is not owned by this runner. Run setup with a new ALBERT_VM_NAME; existing VMs are never replaced") from None


def setup(config, lock_timeout=600):
    config.state.mkdir(parents=True, exist_ok=True, mode=0o700)
    with global_lock(config.lock, lock_timeout):
        entries = list_vms(config)
        existing = [entry for entry in entries if entry.get("Source") == "local" and entry.get("Name") == config.name]
        if existing:
            require_owned(config)
        else:
            cached = [entry for entry in entries if entry.get("Source") == "OCI" and entry.get("Name") == config.image]
            if not cached:
                raise VMError("Requested base is not cached. Pull the macOS/Xcode image explicitly, then rerun setup; no automatic download or Polka VM reuse")
            image = config.image
            if "@sha256:" not in image:
                repository = image.rsplit(":", 1)[0]
                immutable = [entry["Name"] for entry in entries if entry.get("Source") == "OCI" and entry.get("Name", "").startswith(repository + "@sha256:")]
                if len(immutable) == 1:
                    image = immutable[0]
            tart(config, ["clone", image, config.name], timeout=600)
            atomic_json(config.owner, {"schema": 1, "name": config.name, "requested_image": config.image, "image": image,
                                       "uid": os.getuid(), "directory_identity": vm_identity(config)})
        if vm_info(config)["Running"]:
            raise VMError("Managed VM is already running outside this invocation; setup will not take it over")
        tart(config, ["set", config.name, "--cpu", "4", "--memory", "12288", "--display", "1920x1080", "--no-display-refit"])
        print(f"Prepared {config.name}. Run doctor to verify its guest desktop. No VM was started.")


SOURCE_FILES = {"apps/macos/Package.swift", "apps/macos/Package.resolved", "apps/macos/version.json"}


def source_snapshot(root, destination):
    """A small reviewable snapshot, not git ls-files or the complete checkout."""
    root, destination = Path(root).resolve(), Path(destination)
    paths = [root / relative for relative in sorted(SOURCE_FILES)]
    for relative in ("apps/macos/Sources", "apps/macos/Tests"):
        directory = root / relative
        if directory.is_symlink():
            raise VMError("Source directories must not be symlinks")
        if directory.exists():
            for current, dirs, files in os.walk(directory, followlinks=False):
                dirs[:] = [name for name in dirs if not name.startswith(".")]
                for name in dirs + files:
                    path = Path(current) / name
                    if path.is_symlink():
                        raise VMError("Source snapshot does not follow symlinks")
                paths.extend(Path(current) / name for name in files if name.endswith(".swift") and not name.startswith("."))
    for source in paths:
        if not source.exists():
            continue
        if source.is_symlink() or not source.is_file() or not source.resolve().is_relative_to(root):
            raise VMError("Unsafe source snapshot path")
        if any(part.startswith(".") for part in source.relative_to(root).parts):
            continue
        target = destination / source.relative_to(root)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)


DISPLAY_SOURCE = '''import CoreGraphics
import Darwin
let display = CGMainDisplayID()
let modes = CGDisplayCopyAllDisplayModes(display, nil) as? [CGDisplayMode] ?? []
guard let mode = modes.first(where: { $0.width == 1920 && $0.height == 1080 }) else { exit(1) }
var configuration: CGDisplayConfigRef?
guard CGBeginDisplayConfiguration(&configuration) == .success, let configuration,
 CGConfigureDisplayWithDisplayMode(configuration, display, mode, nil) == .success,
 CGCompleteDisplayConfiguration(configuration, .permanently) == .success else { exit(1) }
let bounds = CGDisplayBounds(display)
guard bounds.width == 1920 && bounds.height == 1080 else { exit(1) }
print("ALBERT_GUEST_DISPLAY_READY 1920x1080")
'''


def compile_display(destination, env):
    destination.mkdir(parents=True, exist_ok=True)
    source = destination / "prepare-display.swift"
    source.write_text(DISPLAY_SOURCE)
    try:
        execute(["/usr/bin/xcrun", "swiftc", source, "-o", destination / "prepare-display"], env=env, timeout=120)
    finally:
        source.unlink(missing_ok=True)


def host_metadata(env):
    version = execute(["/usr/bin/xcodebuild", "-version"], env=env)[1].decode()
    match = re.search(r"^Xcode ([0-9]+(?:\.[0-9]+)*)$", version, re.M)
    if not match:
        raise VMError("Full Xcode is required to prepare native desktop tests")
    sdk = execute(["/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-version"], env=env)[1].decode().strip()
    if not re.fullmatch(r"[0-9]+(?:\.[0-9]+)*", sdk):
        raise VMError("Cannot determine host macOS SDK version")
    return {"xcode": match[1], "xcode_details": version.strip(), "sdk": sdk, "minimum_macos": max(27, int(sdk.split(".")[0]))}


def relocate_test_bundle(bundle, env):
    import release as packaging
    import plistlib
    info = plistlib.loads((bundle / "Contents/Info.plist").read_bytes())
    executable = bundle / "Contents/MacOS" / info["CFBundleExecutable"]
    load = execute(["/usr/bin/otool", "-l", executable], env=env)[1].decode()
    paths = re.findall(r"cmd LC_RPATH\s+cmdsize \d+\s+path (.+?) \(offset \d+\)", load)
    for path in paths:
        if path.startswith("/") and path != "/usr/lib/swift":
            execute(["/usr/bin/install_name_tool", "-delete_rpath", path, executable], env=env)
    if "@loader_path/../../../Frameworks" not in paths:
        execute(["/usr/bin/install_name_tool", "-add_rpath", "@loader_path/../../../Frameworks", executable], env=env)
    packaging.assert_relative_links(bundle)
    execute(["/usr/bin/codesign", "--force", "--sign", "-", "--timestamp=none", bundle], env=env)
    execute(["/usr/bin/codesign", "--verify", "--deep", "--strict", bundle], env=env)
    final = execute(["/usr/bin/otool", "-l", executable], env=env)[1].decode()
    if "/.build/" in final or "/Applications/Xcode" in final:
        raise VMError("XCTest bundle still references host build/SDK paths")


def prepare_host(config, staging, mode, configuration="debug", assets=None):
    env = config.environment
    app_root = config.root / "apps/macos"
    runtime = staging / "runtime"
    runtime.mkdir(parents=True)
    toolchain = host_metadata(env)
    compile_display(runtime, env)
    if mode == "doctor":
        return toolchain
    # Local bundles intentionally have no update credentials or signing identity.
    # safe_environment's allowlist does not include any Albert/Sparkle secret.
    execute([app_root / "build-app.sh", configuration], env=env, timeout=600, log=staging / "host-build.log")
    source_app = app_root / "build/Альберт.app"
    import release as packaging
    packaging.assert_relative_links(source_app)
    shutil.copytree(source_app, runtime / "Albert.app", symlinks=True)
    if mode in {"test", "screenshots"}:
        build = ["swift", "build", "--package-path", app_root, "-c", configuration]
        execute([*build, "--build-tests"], env=env, timeout=600, log=staging / "host-tests-build.log")
        products = Path(execute([*build, "--show-bin-path"], env=env)[1].decode().strip())
        bundles = sorted(products.glob("Albert*.xctest"))
        names = {bundle.name for bundle in bundles}
        if not {"AlbertCoreTests.xctest", "AlbertUITests.xctest"}.issubset(names):
            raise VMError("Host compilation must produce both AlbertCoreTests.xctest and AlbertUITests.xctest")
        tests = runtime / "tests"
        tests.mkdir()
        shutil.copytree(source_app / "Contents/Frameworks", tests / "Frameworks", symlinks=True)
        for bundle in bundles:
            target = tests / bundle.name
            shutil.copytree(bundle, target, symlinks=True)
            relocate_test_bundle(target, env)
    source_snapshot(config.root, staging / "project")
    if mode == "screenshots":
        assets = Path(assets) if assets else config.root / "apps/macos/Fixtures"
        for name in ("lake.jpg", "forest.jpg"):
            source = assets / name
            if source.is_symlink() or not source.is_file():
                raise VMError("Screenshots require regular lake.jpg and forest.jpg fixture files (--assets)")
            target = staging / "fixtures" / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
    return toolchain


def doctor_script(toolchain, shared=True):
    checks = f'''set -eu
 test "$(/usr/sbin/sysctl -n kern.hv_vmm_present)" = 1
 test "$(/usr/bin/stat -f %u /dev/console)" = "$(/usr/bin/id -u)"
 test "$(/usr/bin/id -u)" -ne 0
 test "$(/usr/bin/sw_vers -productVersion | /usr/bin/cut -d . -f 1)" -ge {toolchain['minimum_macos']}
 test "$(/usr/bin/xcodebuild -version | /usr/bin/awk '/^Xcode /{{print $2}}')" = {shlex.quote(toolchain['xcode'])}
 test "$(/usr/bin/xcrun --sdk macosx --show-sdk-version)" = {shlex.quote(toolchain['sdk'])}
 /usr/bin/xcrun --find xctest >/dev/null
 /usr/bin/sw_vers
 /usr/bin/xcodebuild -version
'''
    if shared:
        checks += f"test -x {shlex.quote(GUEST_INPUT + '/runtime/prepare-display')}\ntest -w {shlex.quote(GUEST_OUTPUT)}\n"
    checks += "echo ALBERT_GUEST_READY\n"
    return checks


def prepare_guest_script():
    return f'''set -eu
 test "$(/usr/sbin/sysctl -n kern.hv_vmm_present)" = 1
 test "$(/usr/bin/stat -f %u /dev/console)" = "$(/usr/bin/id -u)"
 /usr/bin/defaults write com.apple.WindowManager StandardHideWidgets -bool true
 /usr/bin/defaults write com.apple.WindowManager StageManagerHideWidgets -bool true
 /usr/bin/defaults write com.apple.WindowManager EnableStandardClickToShowDesktop -bool false
 /usr/bin/defaults write com.apple.dock autohide -bool true
 /usr/bin/killall Dock NotificationCenter 2>/dev/null || true
 for service in $(/bin/launchctl list | /usr/bin/awk '$3 ~ /notificationcenterui/ || $3 == "com.apple.Dock.agent" || $3 == "com.apple.WindowManager.agent" {{ print $3 }}'); do
   /bin/launchctl bootout "gui/$(/usr/bin/id -u)/$service"
 done
 {shlex.quote(GUEST_INPUT + '/runtime/prepare-display')}
'''


def guest_script(mode, identifier):
    if mode not in {"doctor", "test", "smoke", "screenshots", "run"} or not re.fullmatch(r"[a-f0-9]{32}", identifier):
        raise VMError("Invalid prepared guest command")
    # This script is itself shared read-only. The environment is cleared before
    # entering it; all Albert testing/backend variables below are synthetic.
    script = f'''#!/bin/bash
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
export ALBERT_DESKTOP_BACKEND=guest
input={shlex.quote(GUEST_INPUT)}
output={shlex.quote(GUEST_OUTPUT)}
test "$(/usr/sbin/sysctl -n kern.hv_vmm_present)" = 1
test "$(/usr/bin/stat -f %u /dev/console)" = "$(/usr/bin/id -u)"
workspace=$(/usr/bin/mktemp -d "${{TMPDIR:-/tmp}}/albert-desktop.{identifier}.XXXXXX")
cleanup() {{ /bin/rm -rf "$workspace"; }}
trap cleanup EXIT
/bin/mkdir -p "$workspace/runtime" "$workspace/tmp"
/usr/bin/tar -xf "$input/runtime.tar" -C "$workspace/runtime"
export TMPDIR="$workspace/tmp/"
developer=$(/usr/bin/xcode-select -p)
export DYLD_FRAMEWORK_PATH="$workspace/runtime/tests/Frameworks:$developer/Platforms/MacOSX.platform/Developer/Library/Frameworks:$developer/Library/Frameworks"
export DYLD_LIBRARY_PATH="$developer/Platforms/MacOSX.platform/Developer/usr/lib:$developer/usr/lib"
'''
    if mode == "doctor":
        script += "echo ALBERT_VM_GUEST_PASS doctor\n"
    elif mode == "smoke":
        script += '"$workspace/runtime/Albert.app/Contents/MacOS/Albert" --smoke-test\necho ALBERT_VM_GUEST_PASS smoke\n'
    elif mode == "run":
        # Persistent data belongs only to the dedicated guest account, never to
        # the host. Closing the app ends the owned visible VM session.
        script += '"$workspace/runtime/Albert.app/Contents/MacOS/Albert"\necho ALBERT_VM_GUEST_PASS run\n'
    else:
        if mode == "screenshots":
            script += 'export ALBERT_SCREENSHOT_DIR="$output/screenshots"\nexport ALBERT_SCREENSHOT_ASSET_DIR="$input/fixtures"\n'
        script += 'runner=$(/usr/bin/xcrun --find xctest)\nfound=0\nfor bundle in "$workspace/runtime/tests/"*.xctest; do\n test -d "$bundle" || continue\n'
        if mode == "screenshots":
            script += ''' case "$bundle" in *CoreTests.xctest) continue ;; esac
 found=$((found+1))
 "$runner" -XCTest AlbertUITests.NativeScreenshotTests/testCaptureApplicationStates "$bundle"
'''
        else:
            script += ' found=$((found+1))\n "$runner" "$bundle"\n echo "ALBERT_XCTEST_BUNDLE_PASS $(/usr/bin/basename "$bundle")"\n'
        script += f'done\ntest "$found" -gt 0\necho ALBERT_VM_GUEST_PASS {mode}\n'
    return script


class GuestLease:
    def __init__(self, config, staging, output, *, visible=False, readiness_timeout=180, probe_timeout=15):
        self.config, self.staging, self.output = config, Path(staging), Path(output)
        self.visible, self.readiness_timeout, self.probe_timeout = visible, readiness_timeout, probe_timeout
        self.process = None
        self.log = None

    def __enter__(self):
        require_owned(self.config)
        if vm_info(self.config)["Running"]:
            raise VMError("VM is already running outside this invocation; refusing to take it over")
        arguments = [self.config.tart, "run", self.config.name]
        if not self.visible:
            arguments += ["--no-graphics"]
        arguments += ["--no-audio", "--no-clipboard", "--no-usb-accessories",
                      f"--dir=albert-input:{self.staging}:ro", f"--dir=albert-output:{self.output}"]
        if ":" in str(self.staging) or ":" in str(self.output):
            raise VMError("Tart share paths must not contain colons")
        self.guest_output = self.output / "guest"
        self.guest_output.mkdir(mode=0o700, exist_ok=True)
        arguments[-1] = f"--dir=albert-output:{self.guest_output}"
        self.log_context = secure_log(self.output / "boot.log")
        self.log = self.log_context.__enter__()
        try:
            self.process = subprocess.Popen([str(v) for v in arguments], env=self.config.environment, stdin=subprocess.DEVNULL,
                                            stdout=self.log, stderr=subprocess.STDOUT, start_new_session=True)
            return self
        except BaseException:
            try:
                if self.process is not None:
                    stop_owned(self.process)
            finally:
                self.log_context.__exit__(*sys.exc_info())
            raise

    def ready(self, metadata):
        deadline = time.monotonic() + self.readiness_timeout
        last = "Guest Agent has not responded"
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise VMError("Owned Tart VM failed to start; see boot.log")
            try:
                code, data = tart(self.config, ["exec", self.config.name, "/bin/sh", "-c", doctor_script(metadata)],
                                  timeout=min(self.probe_timeout, max(0.01, deadline-time.monotonic())), check=False, alive=self.process)
                if code == 0 and b"ALBERT_GUEST_READY" in data:
                    with secure_log(self.output / "doctor.log", truncate=True) as doctor_log:
                        doctor_log.write(data)
                    tart(self.config, ["exec", self.config.name, "/bin/sh", "-c", prepare_guest_script()], timeout=60,
                         log=self.output / "prepare.log", alive=self.process)
                    return
                last = data[-3000:].decode(errors="replace")
            except TransportTimeout as error:
                last = str(error)
            time.sleep(min(0.5, max(0, deadline-time.monotonic())))
        raise VMError(f"Guest desktop/Xcode/SDK did not become ready: {last}. Requires matching host Xcode/SDK, auto-login and Tart Guest Agent in the guest GUI session")

    def run(self, timeout=900):
        # env -i prevents Guest Agent/host process secrets from appearing in
        # XCTest failure diagnostics (which otherwise dump the environment).
        return tart(self.config, ["exec", self.config.name, "/usr/bin/env", "-i", "PATH=/usr/bin:/bin:/usr/sbin:/sbin",
                                  "ALBERT_DESKTOP_BACKEND=guest", "/usr/bin/caffeinate", "-diu", "/bin/bash", GUEST_INPUT + "/run.sh"],
                    timeout=timeout, log=self.output / "tests.log", alive=self.process)

    def __exit__(self, kind, value, traceback):
        try:
            if self.process is not None:
                stop_owned(self.process)
        finally:
            if self.log is not None:
                self.log_context.__exit__(kind, value, traceback)


def manifest(staging):
    result = {}
    for directory, dirs, files in os.walk(staging, followlinks=False):
        for name in dirs + files:
            path = Path(directory) / name
            relative = str(path.relative_to(staging))
            if path.is_symlink():
                if os.path.isabs(os.readlink(path)) or not path.resolve().is_relative_to(staging.resolve()):
                    raise VMError("Prepared input has an external or absolute symlink")
                result[relative] = {"symlink": os.readlink(path)}
            elif path.is_file():
                result[relative] = {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "size": path.stat().st_size}
    return result


def regular_output(path, output):
    path, output = Path(path), Path(output).resolve()
    if path.is_symlink() or not path.resolve().is_relative_to(output) or not path.is_file():
        raise VMError("Guest diagnostics contain an unsafe file path")
    return path


def archive_runtime(staging):
    # virtiofs may not resolve framework symlinks correctly. Unpack on the
    # guest filesystem so framework links retain their meaning.
    with tarfile.open(staging / "runtime.tar", "w") as archive:
        archive.add(staging / "runtime", arcname=".")


def run_prepared(config, mode, *, configuration="debug", timeout=900, lock_timeout=600, assets=None, output_base=None,
                 prepare=prepare_host, readiness_timeout=180, probe_timeout=15):
    if mode not in {"doctor", "test", "smoke", "screenshots", "run"}:
        raise VMError("Unknown guest mode")
    finite_timeout(timeout)
    finite_timeout(lock_timeout)
    finite_timeout(readiness_timeout)
    finite_timeout(probe_timeout)
    identifier = uuid.uuid4().hex
    base = Path(output_base) if output_base else config.root / "apps/macos/build/vm"
    output = base.resolve() / identifier
    output.mkdir(parents=True, mode=0o700)
    result = {"schema": 1, "mode": mode, "vm": config.name, "ok": False, "compilation": "host", "visible": mode == "run"}
    started = time.monotonic()
    try:
        with global_lock(config.lock, lock_timeout):
            owner = require_owned(config)
            result["image"] = owner["image"]
            with tempfile.TemporaryDirectory(prefix="albert-vm-input-") as temporary:
                staging = Path(temporary)
                prepared_at = time.monotonic()
                try:
                    toolchain = prepare(config, staging, mode, configuration, assets)
                finally:
                    result["host_preparation_seconds"] = time.monotonic()-prepared_at
                    for name in ("host-build.log", "host-tests-build.log"):
                        source = staging / name
                        if source.exists():
                            shutil.move(str(source), output / name)
                archive_runtime(staging)
                result["host_toolchain"] = toolchain
                (staging / "run.sh").write_text(guest_script(mode, identifier))
                (staging / "run.sh").chmod(0o500)
                atomic_json(output / "inputs.json", manifest(staging))
                vm_started = time.monotonic()
                with GuestLease(config, staging, output, visible=mode == "run", readiness_timeout=readiness_timeout, probe_timeout=probe_timeout) as lease:
                    lease.ready(toolchain)
                    tests_started = time.monotonic()
                    lease.run(timeout=timeout)
                    result["guest_seconds"] = time.monotonic()-tests_started
                result["vm_seconds"] = time.monotonic()-vm_started
                log_path = regular_output(output / "tests.log", output)
                if log_path.stat().st_size > 64 << 20:
                    raise VMError("Guest test log exceeds 64 MiB")
                log = log_path.read_text(errors="replace")
                if f"ALBERT_VM_GUEST_PASS {mode}" not in log:
                    raise VMError("Guest did not confirm required validation; see tests.log")
                if mode == "smoke" and "ALBERT_SMOKE_PASS" not in log:
                    raise VMError("Native keyboard smoke did not confirm success")
                if mode == "test":
                    if not all(f"ALBERT_XCTEST_BUNDLE_PASS {name}.xctest" in log for name in ("AlbertCoreTests", "AlbertUITests")) or not re.search(r"Test Suite '(?:AlbertUITests\.)?InterfaceTests' passed", log):
                        raise VMError("XCTest did not confirm both bundles and the native keyboard InterfaceTests suite")
                if mode == "screenshots":
                    for name in ("conversation-light.png", "conversation-dark.png", "composer-voice-ready.png", "composer-voice-recording.png"):
                        file = regular_output(output / "guest/screenshots" / name, output)
                        if file.stat().st_size > 64 << 20:
                            raise VMError("Guest screenshot exceeds 64 MiB")
                        with file.open("rb") as screenshot:
                            signature = screenshot.read(8)
                        if signature != b"\x89PNG\r\n\x1a\n":
                            raise VMError("Guest did not produce a required native screenshot")
                result["ok"] = True
    except BaseException as error:
        result["failure"] = str(error)
        raise
    finally:
        result["total_seconds"] = time.monotonic()-started
        atomic_json(output / "result.json", result)
        print(f"VM diagnostics: {output}")
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["setup", "doctor", "test", "smoke", "screenshots", "run"])
    parser.add_argument("--configuration", choices=["debug", "release"], default="debug")
    parser.add_argument("--timeout", type=float, help="guest operation deadline, seconds (default 900; run 8 hours)")
    parser.add_argument("--lock-timeout", type=float, default=600)
    parser.add_argument("--assets", type=Path, help="screenshots only: directory with lake.jpg and forest.jpg")
    parser.add_argument("--output", type=Path, help="parent directory for a fresh diagnostics subdirectory")
    args = parser.parse_args()
    try:
        if platform.system() != "Darwin" or platform.machine() != "arm64":
            raise VMError("Local desktop VM checks require Apple silicon macOS and full Xcode; no host fallback")
        timeout = args.timeout if args.timeout is not None else (8*3600 if args.command == "run" else 900)
        finite_timeout(timeout)
        finite_timeout(args.lock_timeout)
        if args.assets and args.command != "screenshots":
            raise VMError("--assets is accepted only for explicit screenshots")
        config = Configuration()
        with cancellation_signals():
            if args.command == "setup":
                setup(config, args.lock_timeout)
            else:
                run_prepared(config, args.command, configuration=args.configuration, timeout=timeout,
                             lock_timeout=args.lock_timeout, assets=args.assets, output_base=args.output)
    except (VMError, OSError) as error:
        print(f"Desktop VM failed: {error}", file=sys.stderr)
        return error.status if isinstance(error, VMError) else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
