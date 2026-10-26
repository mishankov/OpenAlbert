#!/usr/bin/env python3
"""Build relocatable native bundles and authenticate immutable Sparkle archives.

No command publishes a release. Private update keys are used only on stdin to
Sparkle's signing tool; child build commands receive a scrubbed environment.
"""
from __future__ import annotations

import argparse
import base64
import binascii
import datetime
import email.utils
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parent
APP_NAME = "Альберт.app"
APP_ID = "ru.mishankov.albert.desktop"
SPARKLE_NS = "http://www.andymatuschak.org/xml-namespaces/sparkle"
ET.register_namespace("sparkle", SPARKLE_NS)
SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$")
SECRETS = {"SPARKLE_PRIVATE_KEY", "ALBERT_SIGNING_P12", "ALBERT_SIGNING_PASSWORD"}
MACHO = {bytes.fromhex(v) for v in ("feedface", "feedfacf", "cefaedfe", "cffaedfe", "cafebabe", "bebafeca", "cafebabf", "bfbafeca")}


class ReleaseError(ValueError):
    pass


def public_environment(env=None):
    return {k: v for k, v in (os.environ if env is None else env).items() if k not in SECRETS}


def run(args, *, input=None, env=None, capture=False):
    result = subprocess.run([str(v) for v in args], input=input, env=public_environment(env),
                            stdout=subprocess.PIPE if capture else None, stderr=subprocess.PIPE if capture else None)
    if result.returncode:
        # Tool output may contain supplied secrets. Never replay captured signing output.
        raise ReleaseError(f"{Path(args[0]).name} failed (exit {result.returncode})")
    return result.stdout if capture else b""


def canonical_base64(value, length, name):
    try:
        decoded = base64.b64decode(value, validate=True)
    except (ValueError, binascii.Error, TypeError):
        raise ReleaseError(f"{name} must be canonical base64 ({length} bytes)") from None
    if len(decoded) != length or base64.b64encode(decoded).decode() != value:
        raise ReleaseError(f"{name} must be canonical base64 ({length} bytes)")
    return decoded


def metadata(path=ROOT / "version.json"):
    try:
        value = json.loads(Path(path).read_text())
    except (OSError, ValueError):
        raise ReleaseError("Cannot read version.json") from None
    if not isinstance(value, dict) or set(value) != {"version", "build"} or not isinstance(value["version"], str) or not SEMVER.fullmatch(value["version"]):
        raise ReleaseError("version.json requires a semver version and positive integer build")
    match = SEMVER.fullmatch(value["version"])
    if match[4] and any(part.isdigit() and len(part) > 1 and part[0] == "0" for part in match[4].split(".")):
        raise ReleaseError("Semver prerelease numbers must not have leading zeroes")
    if type(value["build"]) is not int or not 0 < value["build"] <= 2147483647:
        raise ReleaseError("build must be a positive integer below 2147483648")
    return value


def update_configuration(env=None, required=False):
    env = os.environ if env is None else env
    feed, key = env.get("ALBERT_UPDATE_FEED_URL", ""), env.get("SPARKLE_PUBLIC_KEY", "")
    if bool(feed) != bool(key) or (required and not feed):
        raise ReleaseError("Set ALBERT_UPDATE_FEED_URL and SPARKLE_PUBLIC_KEY together")
    if not feed:
        return None
    url = urllib.parse.urlsplit(feed)
    if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment:
        raise ReleaseError("ALBERT_UPDATE_FEED_URL must be an HTTPS URL without credentials, query or fragment")
    canonical_base64(key, 32, "SPARKLE_PUBLIC_KEY")
    return {"feed": feed, "public_key": key}


def release_configuration(meta, env=None):
    env = os.environ if env is None else env
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", meta["version"]):
        raise ReleaseError("The stable update feed requires a final major.minor.patch version without prerelease or build metadata")
    update = update_configuration(env, required=True)
    repository = env.get("RELEASE_REPOSITORY", env.get("GITHUB_REPOSITORY", "mishankov/OpenAlbert"))
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", repository):
        raise ReleaseError("RELEASE_REPOSITORY must be owner/repository")
    expected_feed = f"https://github.com/{repository}/releases/latest/download/appcast.xml"
    if update["feed"] != expected_feed:
        raise ReleaseError("Release feed must match this repository's latest appcast.xml")
    tag = env.get("RELEASE_TAG", f"macos-v{meta['version']}")
    if tag != f"macos-v{meta['version']}":
        raise ReleaseError("RELEASE_TAG must be macos-v<version.json version>")
    fingerprint = env.get("ALBERT_SIGNING_CERT_SHA1", "")
    if not re.fullmatch(r"[0-9A-Fa-f]{40}", fingerprint):
        raise ReleaseError("ALBERT_SIGNING_CERT_SHA1 must pin the persistent certificate (40 hex digits)")
    return {**update, "repository": repository, "tag": tag, "fingerprint": fingerprint.upper(), "arch": "arm64"}


def info_plist(meta, update=None):
    value = {"CFBundleIdentifier": APP_ID, "CFBundleName": "Альберт", "CFBundleDisplayName": "Альберт",
             "CFBundleExecutable": "Albert", "CFBundlePackageType": "APPL", "CFBundleShortVersionString": meta["version"],
             "CFBundleVersion": str(meta["build"]), "LSMinimumSystemVersion": "14.0", "NSHighResolutionCapable": True,
             "NSPrincipalClass": "NSApplication", "NSAppTransportSecurity": {"NSAllowsArbitraryLoads": True},
             "NSMicrophoneUsageDescription": "Запись голосовых сообщений для Альберта."}
    if update:
        value.update(SUFeedURL=update["feed"], SUPublicEDKey=update["public_key"], SUEnableAutomaticChecks=True,
                     SUAutomaticallyUpdate=False, SUAllowsAutomaticUpdates=False, SUEnableSystemProfiling=False,
                     SUVerifyUpdateBeforeExtraction=True, SUScheduledCheckInterval=21600)
    return value


def assert_relative_links(app):
    root = Path(app).resolve()
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in dirs + files:
            path = Path(directory) / name
            if path.is_symlink():
                if os.path.isabs(os.readlink(path)) or not path.resolve().is_relative_to(root) or not path.exists():
                    raise ReleaseError("Bundle contains an external, absolute, or broken symlink")


def macho_targets(app):
    targets = []
    for directory, dirs, files in os.walk(app, followlinks=False):
        for name in files:
            path = Path(directory) / name
            if not path.is_symlink():
                with path.open("rb") as stream:
                    if stream.read(4) in MACHO:
                        targets.append(path)
        for name in dirs:
            path = Path(directory) / name
            if not path.is_symlink() and path.suffix in {"app", ".app", ".framework", ".xpc"}:
                targets.append(path)
    return sorted(targets, key=lambda p: (-len(p.parts), str(p))) + [Path(app)]


def requirement(identifier, fingerprint):
    return f'identifier "{identifier}" and certificate leaf = H"{fingerprint}"'


def target_identifier(path, app):
    if path == app:
        return APP_ID
    plist = path / ("Resources/Info.plist" if path.suffix == ".framework" else "Contents/Info.plist")
    if plist.exists():
        value = plistlib.loads(plist.read_bytes()).get("CFBundleIdentifier")
        if value:
            return value
    result = subprocess.run(["/usr/bin/codesign", "--display", "--verbose=2", str(path)], capture_output=True, env=public_environment())
    match = re.search(r"^Identifier=(.+)$", (result.stdout + result.stderr).decode(errors="replace"), re.M)
    return match[1] if match else APP_ID + "." + re.sub(r"[^A-Za-z0-9.-]", "-", str(path.relative_to(app)))


def sign_bundle(app, env=None):
    env = os.environ if env is None else env
    fingerprint = env.get("ALBERT_SIGNING_CERT_SHA1", "")
    keychain = env.get("ALBERT_SIGNING_KEYCHAIN", "")
    if bool(fingerprint) != bool(keychain):
        raise ReleaseError("Stable signing requires ALBERT_SIGNING_CERT_SHA1 and an isolated ALBERT_SIGNING_KEYCHAIN")
    if fingerprint and not re.fullmatch(r"[A-Fa-f0-9]{40}", fingerprint):
        raise ReleaseError("Invalid signing certificate fingerprint")
    fingerprint = fingerprint.upper()
    for target in macho_targets(app):
        args = ["/usr/bin/codesign", "--force", "--sign", fingerprint or "-", "--timestamp=none", "--options", "0"]
        if fingerprint:
            identifier = target_identifier(target, app)
            args += ["--keychain", keychain, "--identifier", identifier, "--requirements", "=designated => " + requirement(identifier, fingerprint)]
        if "Sparkle.framework" in str(target) and (target.name == "Downloader.xpc" or target.name == "Downloader"):
            args += ["--preserve-metadata=entitlements"]
        if target == Path(app):
            args += ["--entitlements", str(ROOT / "Albert.entitlements")]
        run(args + [str(target)], env=env, capture=True)
    run(["/usr/bin/codesign", "--verify", "--deep", "--strict", str(app)], env=env, capture=True)
    if fingerprint:
        verify_identity(app, fingerprint)


def verify_identity(app, fingerprint):
    run(["/usr/bin/codesign", "--verify", "--strict", "-R", "=" + requirement(APP_ID, fingerprint), str(app)], capture=True)
    # codesign writes requirements to stderr, so inspect both streams separately.
    result = subprocess.run(["/usr/bin/codesign", "--display", "-r-", str(app)], capture_output=True, env=public_environment())
    details = (result.stdout + result.stderr).decode(errors="replace")
    if result.returncode or "cdhash" in details.lower() or fingerprint not in details.upper():
        raise ReleaseError("Application must have a stable certificate-based designated requirement")


def relocate_executable(executable):
    load = run(["/usr/bin/otool", "-l", str(executable)], capture=True).decode()
    paths = re.findall(r"cmd LC_RPATH\s+cmdsize \d+\s+path (.+?) \(offset \d+\)", load)
    for path in paths:
        if "/.build/" in path or path.startswith("/") and not path.startswith(("/usr/lib/", "/System/Library/")):
            run(["/usr/bin/install_name_tool", "-delete_rpath", path, str(executable)], capture=True)
    if "@executable_path/../Frameworks" not in paths:
        run(["/usr/bin/install_name_tool", "-add_rpath", "@executable_path/../Frameworks", str(executable)], capture=True)
    load = run(["/usr/bin/otool", "-l", str(executable)], capture=True).decode()
    if "/.build/" in load:
        raise ReleaseError("App still references the Swift build directory")


def build(configuration):
    if configuration not in {"debug", "release"}:
        raise ReleaseError("Usage: build-app.sh [debug|release]")
    meta, update = metadata(), update_configuration()
    args = ["swift", "build", "--package-path", str(ROOT), "-c", configuration]
    arch = os.environ.get("ALBERT_BUILD_ARCH")
    if arch:
        if arch not in {"arm64", "x86_64"}:
            raise ReleaseError("ALBERT_BUILD_ARCH must be arm64 or x86_64")
        args += ["--arch", arch]
    run(args)
    binaries = Path(run(args + ["--show-bin-path"], capture=True).decode().strip())
    framework = binaries / "Sparkle.framework"
    if not framework.exists():
        raise ReleaseError("Swift build did not produce Sparkle.framework")
    app = ROOT / "build" / APP_NAME
    if app.exists():
        shutil.rmtree(app)
    for folder in ("MacOS", "Resources", "Frameworks"):
        (app / "Contents" / folder).mkdir(parents=True)
    executable = app / "Contents/MacOS/Albert"
    shutil.copy2(binaries / "Albert", executable)
    shutil.copytree(framework, app / "Contents/Frameworks/Sparkle.framework", symlinks=True)
    for resource in binaries.glob("*.bundle"):
        shutil.copytree(resource, app / "Contents/Resources" / resource.name, symlinks=True)
    license_path = ROOT / ".build/artifacts/sparkle/Sparkle/LICENSE"
    if license_path.is_file():
        shutil.copy2(license_path, app / "Contents/Resources/Sparkle-LICENSE")
    (app / "Contents/Info.plist").write_bytes(plistlib.dumps(info_plist(meta, update)))
    relocate_executable(executable)
    assert_relative_links(app)
    sign_bundle(app)
    print(app)
    return app


VERIFY_SOURCE = '''import Foundation
import CryptoKit
let args = CommandLine.arguments
if args.count != 4 { exit(2) }
do {
 let key = try Curve25519.Signing.PublicKey(rawRepresentation: Data(base64Encoded: args[1])!)
 let signature = Data(base64Encoded: args[2])!
 let bytes = try Data(contentsOf: URL(fileURLWithPath: args[3]), options: .mappedIfSafe)
 if !key.isValidSignature(signature, for: bytes) { exit(1) }
} catch { exit(1) }
'''


def signature_verifier(directory=None):
    output = Path(directory) if directory else ROOT / "build/release-tools"
    output.mkdir(parents=True, exist_ok=True)
    source, executable = output / "verify-ed25519.swift", output / "verify-ed25519"
    if not source.exists() or source.read_text() != VERIFY_SOURCE or not executable.exists():
        source.write_text(VERIFY_SOURCE)
        run(["xcrun", "swiftc", "-O", str(source), "-o", str(executable)], capture=True)
    return executable


def verify_signature(archive, public_key, signature, verifier=None):
    canonical_base64(public_key, 32, "SPARKLE_PUBLIC_KEY")
    canonical_base64(signature, 64, "Ed25519 signature")
    run([str(verifier or signature_verifier()), public_key, signature, str(archive)], capture=True)


def sign_archive(archive, private_key, signer=None):
    if not private_key:
        raise ReleaseError("SPARKLE_PRIVATE_KEY is required")
    # Only known export formats, never a filename or accidental environment text.
    try:
        raw = base64.b64decode(private_key, validate=True)
    except (ValueError, binascii.Error):
        raise ReleaseError("SPARKLE_PRIVATE_KEY must be an exported Sparkle Ed25519 secret") from None
    if len(raw) not in {32, 96} or base64.b64encode(raw).decode() != private_key:
        raise ReleaseError("SPARKLE_PRIVATE_KEY must be an exported Sparkle Ed25519 secret")
    signer = signer or ROOT / ".build/artifacts/sparkle/Sparkle/bin/sign_update"
    signature = run([str(signer), "--ed-key-file", "-", "-p", str(archive)], input=private_key.encode(), capture=True).decode().strip()
    canonical_base64(signature, 64, "Ed25519 signature")
    return signature


def archive_name(meta):
    return f"Albert-{meta['version']}-arm64.zip"


def archive_url(meta, config):
    return f"https://github.com/{config['repository']}/releases/download/{urllib.parse.quote(config['tag'], safe='')}/{archive_name(meta)}"


def appcast(meta, config, signature, size):
    canonical_base64(signature, 64, "Ed25519 signature")
    if type(size) is not int or size <= 0:
        raise ReleaseError("Archive must contain bytes")
    rss = ET.Element("rss", version="2.0")
    channel = ET.SubElement(rss, "channel")
    ET.SubElement(channel, "title").text = "Альберт"
    ET.SubElement(channel, "link").text = f"https://github.com/{config['repository']}/releases"
    item = ET.SubElement(channel, "item")
    ET.SubElement(item, "title").text = "Альберт " + meta["version"]
    ET.SubElement(item, "pubDate").text = email.utils.format_datetime(datetime.datetime.now(datetime.timezone.utc), usegmt=True)
    for key, value in {"version": str(meta["build"]), "shortVersionString": meta["version"], "minimumSystemVersion": "14.0.0", "hardwareRequirements": "arm64"}.items():
        ET.SubElement(item, "{" + SPARKLE_NS + "}" + key).text = value
    ET.SubElement(item, "enclosure", {"url": archive_url(meta, config), "length": str(size), "type": "application/octet-stream", "{" + SPARKLE_NS + "}edSignature": signature})
    ET.indent(rss)
    return ET.tostring(rss, encoding="utf-8", xml_declaration=True)


def verify_appcast(path, archive, meta, config, verifier=None):
    xml = Path(path).read_bytes()
    if b"<!DOCTYPE" in xml.upper() or b"<!ENTITY" in xml.upper():
        raise ReleaseError("Appcast must not contain a DTD or entity declarations")
    try:
        rss = ET.fromstring(xml)
    except ET.ParseError:
        raise ReleaseError("Invalid appcast XML") from None
    if rss.tag != "rss" or len(rss.findall("channel")) != 1 or len(rss.findall("./channel/item")) != 1:
        raise ReleaseError("Appcast must have exactly one channel and item")
    item = rss.find("./channel/item")
    for key, value in {"version": str(meta["build"]), "shortVersionString": meta["version"], "minimumSystemVersion": "14.0.0", "hardwareRequirements": "arm64"}.items():
        elements = item.findall("{" + SPARKLE_NS + "}" + key)
        if len(elements) != 1 or elements[0].text != value:
            raise ReleaseError("Appcast metadata does not match the built application")
    enclosures = item.findall("enclosure")
    if len(enclosures) != 1:
        raise ReleaseError("Appcast must have exactly one archive enclosure")
    enclosure = enclosures[0]
    if enclosure.get("url") != archive_url(meta, config) or enclosure.get("length") != str(Path(archive).stat().st_size) or enclosure.get("type") != "application/octet-stream":
        raise ReleaseError("Appcast URL or archive size does not match the ZIP")
    verify_signature(archive, config["public_key"], enclosure.get("{" + SPARKLE_NS + "}edSignature", ""), verifier)


def verify_bundle(app, meta, config):
    if app.is_symlink() or not app.is_dir():
        raise ReleaseError("Expected a regular application bundle")
    plist = plistlib.loads((app / "Contents/Info.plist").read_bytes())
    expected = info_plist(meta, config)
    for key in expected:
        if plist.get(key) != expected[key]:
            raise ReleaseError("Bundle version or updater configuration does not match this release")
    executable = app / "Contents/MacOS/Albert"
    arch = run(["/usr/bin/lipo", "-archs", str(executable)], capture=True).decode().strip()
    if arch != "arm64":
        raise ReleaseError("Release archive must contain the arm64 application")
    assert_relative_links(app)
    run(["/usr/bin/codesign", "--verify", "--deep", "--strict", str(app)], capture=True)
    verify_identity(app, config["fingerprint"])
    load = run(["/usr/bin/otool", "-l", str(executable)], capture=True).decode()
    if "/.build/" in load or "@executable_path/../Frameworks" not in load:
        raise ReleaseError("Application is not relocatable")
    for relative in ("Contents/Frameworks/Sparkle.framework/Sparkle", "Contents/Frameworks/Sparkle.framework/Versions/B/Updater.app/Contents/MacOS/Updater"):
        if not (app / relative).is_file():
            raise ReleaseError("Bundle is missing Sparkle or its installer helper")


def package(app, output):
    meta = metadata()
    config = release_configuration(meta)
    private_key = os.environ.get("SPARKLE_PRIVATE_KEY", "")
    if not private_key:
        raise ReleaseError("SPARKLE_PRIVATE_KEY is required")
    verify_bundle(app, meta, config)
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ReleaseError("Release output must be empty; refusing to replace an existing archive")
    # Build into a sibling staging directory; failed signing leaves no publishable files.
    with tempfile.TemporaryDirectory(prefix="albert-release-", dir=output.parent) as stage_dir:
        stage = Path(stage_dir)
        archive = stage / archive_name(meta)
        run(["/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", str(app), str(archive)])
        with tempfile.TemporaryDirectory(prefix="albert-relocated-") as relocated:
            run(["/usr/bin/ditto", "-x", "-k", str(archive), relocated])
            verify_bundle(Path(relocated) / app.name, meta, config)
        signature = sign_archive(archive, private_key)
        verify_signature(archive, config["public_key"], signature)
        feed = stage / "appcast.xml"
        feed.write_bytes(appcast(meta, config, signature, archive.stat().st_size))
        verify_appcast(feed, archive, meta, config)
        (stage / "checksums.txt").write_text("".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in (archive, feed)))
        for path in stage.iterdir():
            shutil.move(str(path), output / path.name)
    print(output)


def check_history(meta, feeds):
    for feed in feeds:
        xml = Path(feed).read_bytes()
        if b"<!DOCTYPE" in xml.upper() or b"<!ENTITY" in xml.upper():
            raise ReleaseError("Release history appcast has a DTD")
        root = ET.fromstring(xml)
        builds = root.findall("./channel/item/{" + SPARKLE_NS + "}version")
        if not builds:
            raise ReleaseError("Existing macOS release has no build number")
        for build in builds:
            if not build.text or not re.fullmatch(r"[1-9][0-9]*", build.text) or int(build.text) >= meta["build"]:
                raise ReleaseError("New build must be greater than every previous macOS release build")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command")
    builder = commands.add_parser("build"); builder.add_argument("configuration", nargs="?", default="release")
    packager = commands.add_parser("package"); packager.add_argument("--app", type=Path, default=ROOT / "build" / APP_NAME); packager.add_argument("--output", type=Path, default=ROOT / "build/release")
    commands.add_parser("check-config")
    history = commands.add_parser("check-history"); history.add_argument("feeds", nargs="*", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "build": build(args.configuration)
        elif args.command in {None, "package"}: package(getattr(args, "app", ROOT / "build" / APP_NAME), getattr(args, "output", ROOT / "build/release"))
        elif args.command == "check-history": check_history(metadata(), args.feeds)
        elif args.command == "check-config":
            meta = metadata(); config = release_configuration(meta)
            print(json.dumps({"version": meta["version"], "build": meta["build"], "tag": config["tag"], "archive": archive_name(meta)}))
    except (ReleaseError, OSError, ValueError) as error:
        print(f"Release failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
