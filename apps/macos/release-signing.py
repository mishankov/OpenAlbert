#!/usr/bin/env python3
"""Run release build/package in an isolated, temporary signing keychain.

Expects an explicitly configured persistent certificate backup in CI. Never
creates an identity, reads login.keychain, or falls back to ad-hoc release signing.
"""
import base64
import binascii
import datetime
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import tempfile

from release import ROOT, ReleaseError, metadata, public_environment, release_configuration

OPENSSL = "/usr/bin/openssl"


def quiet(args, *, env=None, input=None):
    result = subprocess.run([str(v) for v in args], env=env or public_environment(), input=input, capture_output=True)
    if result.returncode:
        raise ReleaseError(f"{Path(args[0]).name} failed during isolated signing (exit {result.returncode})")
    return result.stdout


def certificate(env, p12):
    password_env = public_environment(env)
    password_env["ALBERT_SIGNING_PASSWORD"] = env["ALBERT_SIGNING_PASSWORD"]
    pem = quiet([OPENSSL, "pkcs12", "-in", p12, "-passin", "env:ALBERT_SIGNING_PASSWORD", "-clcerts", "-nokeys"], env=password_env)
    details = quiet([OPENSSL, "x509", "-noout", "-fingerprint", "-sha1", "-dates", "-text"], input=pem).decode()
    fingerprint = re.search(r"(?:SHA1|sha1) Fingerprint=([A-Fa-f0-9:]+)", details)
    if not fingerprint or fingerprint[1].replace(":", "").upper() != env["ALBERT_SIGNING_CERT_SHA1"].upper():
        raise ReleaseError("Certificate does not match ALBERT_SIGNING_CERT_SHA1; identity rotation refused")
    dates = []
    for prefix in ("notBefore=", "notAfter="):
        line = next((line[len(prefix):] for line in details.splitlines() if line.startswith(prefix)), "")
        try:
            dates.append(datetime.datetime.strptime(line, "%b %d %H:%M:%S %Y %Z").replace(tzinfo=datetime.timezone.utc))
        except ValueError:
            raise ReleaseError("Invalid certificate validity dates") from None
    usages = []
    lines = details.splitlines()
    for index, line in enumerate(lines):
        if line.strip().startswith("X509v3 Extended Key Usage:"):
            indent = len(line) - len(line.lstrip())
            for value in lines[index + 1:]:
                if not value.strip() or len(value) - len(value.lstrip()) <= indent:
                    break
                usages.extend(part.strip() for part in value.strip().split(","))
    if not dates[0] <= datetime.datetime.now(datetime.timezone.utc) < dates[1] or "Code Signing" not in usages:
        raise ReleaseError("Certificate must be currently valid for code signing")


def main():
    env = dict(os.environ)
    try:
        release_configuration(metadata(), env)
        if not env.get("ALBERT_SIGNING_P12") or not env.get("ALBERT_SIGNING_PASSWORD") or not env.get("SPARKLE_PRIVATE_KEY"):
            raise ReleaseError("ALBERT_SIGNING_P12, ALBERT_SIGNING_PASSWORD and SPARKLE_PRIVATE_KEY are required")
        try:
            backup = base64.b64decode(env["ALBERT_SIGNING_P12"], validate=True)
        except (ValueError, binascii.Error):
            raise ReleaseError("ALBERT_SIGNING_P12 must be a base64 PKCS#12 backup") from None
        if not backup or len(backup) > 131072:
            raise ReleaseError("Invalid PKCS#12 backup size")
        with tempfile.TemporaryDirectory(prefix="albert-signing-") as scratch:
            scratch = Path(scratch)
            p12, keychain = scratch / "identity.p12", scratch / "signing.keychain-db"
            p12.write_bytes(backup); p12.chmod(0o600)
            certificate(env, p12)
            password = secrets.token_urlsafe(32)
            created = False
            try:
                quiet(["/usr/bin/security", "create-keychain", "-p", password, keychain]); created = True
                quiet(["/usr/bin/security", "set-keychain-settings", "-lut", "21600", keychain])
                quiet(["/usr/bin/security", "unlock-keychain", "-p", password, keychain])
                quiet(["/usr/bin/security", "import", p12, "-k", keychain, "-P", env["ALBERT_SIGNING_PASSWORD"], "-T", "/usr/bin/codesign"])
                quiet(["/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", password, keychain])
                p12.unlink()
                build_env = public_environment(env)
                build_env["ALBERT_SIGNING_KEYCHAIN"] = str(keychain)
                build_env["ALBERT_BUILD_ARCH"] = "arm64"
                subprocess.run([str(ROOT / "build-app.sh"), "release"], env=build_env, check=True)
                # The update key is passed only to the packaging process; its own
                # children scrub it and Sparkle receives it exclusively via stdin.
                package_env = dict(build_env, SPARKLE_PRIVATE_KEY=env["SPARKLE_PRIVATE_KEY"])
                subprocess.run([sys.executable, str(ROOT / "release.py"), "package"], env=package_env, check=True)
            finally:
                if created:
                    subprocess.run(["/usr/bin/security", "delete-keychain", str(keychain)], env=public_environment(env), capture_output=True)
    except (ReleaseError, OSError, subprocess.CalledProcessError) as error:
        # CalledProcessError.command never contains credentials for build/package.
        print(f"Release signing failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
