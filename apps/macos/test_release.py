"""Release gates use public RFC8032 fixture keys, never the user's signing keys."""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import plistlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import xml.etree.ElementTree as ET

import release as tooling

# RFC8032 test vector 1. This published test key must never sign a real release.
SEED = base64.b64encode(bytes.fromhex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")).decode()
PUBLIC = base64.b64encode(bytes.fromhex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")).decode()
META = {"version": "0.1.0", "build": 1}
ENV = {"ALBERT_UPDATE_FEED_URL": "https://github.com/mishankov/OpenAlbert/releases/latest/download/appcast.xml",
       "SPARKLE_PUBLIC_KEY": PUBLIC, "ALBERT_SIGNING_CERT_SHA1": "A" * 40}


class MetadataTests(unittest.TestCase):
    def test_application_allows_http_connections(self):
        self.assertEqual(tooling.info_plist(META)["NSAppTransportSecurity"],
                         {"NSAllowsArbitraryLoads": True})

    def test_microphone_permission_is_scoped_to_application(self):
        self.assertTrue(tooling.info_plist(META)["NSMicrophoneUsageDescription"])
        entitlements = plistlib.loads((tooling.ROOT / "Albert.entitlements").read_bytes())
        self.assertEqual(entitlements, {"com.apple.security.device.audio-input": True})
        app = Path("/fixture/Albert.app")
        helper = app / "Contents/Frameworks/Sparkle.framework/Versions/B/XPCServices/Downloader.xpc"
        with mock.patch("release.macho_targets", return_value=[helper, app]), mock.patch("release.run") as run:
            tooling.sign_bundle(app, env={})
        helper_args, app_args = [call.args[0] for call in run.call_args_list[:2]]
        self.assertIn("--preserve-metadata=entitlements", helper_args)
        self.assertNotIn("--entitlements", helper_args)
        self.assertIn("--entitlements", app_args)
        self.assertEqual(app_args[app_args.index("--entitlements") + 1], str(tooling.ROOT / "Albert.entitlements"))

    def test_central_version(self):
        current = tooling.metadata()
        self.assertGreater(current["build"], 0)
        self.assertIsNotNone(tooling.SEMVER.fullmatch(current["version"]))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "version.json"
            for value in ({"version": "1.2.3", "build": 7}, {"version": "1.2.3-beta.2+test", "build": 8}):
                path.write_text(json.dumps(value))
                self.assertEqual(tooling.metadata(path), value)
            for value in ({"version": "01.0.0", "build": 1}, {"version": "1.0.0-01", "build": 1},
                          {"version": "0.1.0", "build": True}, {"version": "0.1.0", "build": 0},
                          {"version": "0.1.0", "build": "1"}, {"version": "0.1.0", "build": 2**31}, [], {}):
                path.write_text(json.dumps(value))
                with self.subTest(value=value), self.assertRaises(tooling.ReleaseError):
                    tooling.metadata(path)

    def test_update_configuration_is_opt_in_and_validated(self):
        self.assertIsNone(tooling.update_configuration({}))
        for env in ({"SPARKLE_PUBLIC_KEY": PUBLIC}, {"ALBERT_UPDATE_FEED_URL": ENV["ALBERT_UPDATE_FEED_URL"]},
                    dict(ENV, SPARKLE_PUBLIC_KEY="wrong"), dict(ENV, ALBERT_UPDATE_FEED_URL="http://example.com/feed"),
                    dict(ENV, ALBERT_UPDATE_FEED_URL="https://user:password@example.com/feed"),
                    dict(ENV, ALBERT_UPDATE_FEED_URL="https://example.com/feed?token=secret")):
            with self.subTest(env=env), self.assertRaises(tooling.ReleaseError):
                tooling.update_configuration(env)
        self.assertEqual(tooling.update_configuration(ENV)["public_key"], PUBLIC)
        for key in ("SUFeedURL", "SUPublicEDKey"):
            self.assertNotIn(key, tooling.info_plist(META))
        info = tooling.info_plist(META, tooling.update_configuration(ENV))
        self.assertEqual(info["CFBundleVersion"], "1")
        self.assertFalse(info["SUAutomaticallyUpdate"])
        self.assertFalse(info["SUAllowsAutomaticUpdates"])
        self.assertTrue(info["SUVerifyUpdateBeforeExtraction"])
        self.assertEqual(plistlib.loads(plistlib.dumps(info)), info)

    def test_release_identity_feed_and_tag_are_pinned(self):
        config = tooling.release_configuration(META, ENV)
        self.assertEqual(config["tag"], "macos-v0.1.0")
        self.assertEqual(tooling.archive_url(META, config), "https://github.com/mishankov/OpenAlbert/releases/download/macos-v0.1.0/Albert-0.1.0-arm64.zip")
        for overrides in ({"ALBERT_SIGNING_CERT_SHA1": ""}, {"ALBERT_SIGNING_CERT_SHA1": "-"},
                          {"RELEASE_TAG": "v0.1.0"}, {"RELEASE_TAG": "macos-v0.2.0"},
                          {"RELEASE_REPOSITORY": "https://github.com/wrong"}, {"RELEASE_REPOSITORY": "another/repo"}):
            with self.subTest(overrides=overrides), self.assertRaises(tooling.ReleaseError):
                tooling.release_configuration(META, dict(ENV, **overrides))

    def test_stable_feed_rejects_prereleases_and_build_metadata(self):
        for version in ("0.2.0-beta.1", "0.2.0+build.2", "0.2.0-nightly"):
            with self.subTest(version=version), self.assertRaises(tooling.ReleaseError):
                tooling.release_configuration(dict(META, version=version), ENV)

    def test_secrets_never_reach_public_build_environment(self):
        env = dict(ENV, SPARKLE_PRIVATE_KEY="test-secret", ALBERT_SIGNING_P12="certificate-secret", ALBERT_SIGNING_PASSWORD="password-secret")
        clean = tooling.public_environment(env)
        self.assertTrue(set(tooling.SECRETS).isdisjoint(clean))
        self.assertEqual(clean["SPARKLE_PUBLIC_KEY"], PUBLIC)
        with mock.patch("release.subprocess.run") as run:
            run.return_value.returncode = 1
            run.return_value.stderr = b"secret text"
            with self.assertRaisesRegex(tooling.ReleaseError, "failed") as error:
                tooling.run(["sign_update", "--ed-key-file", "-"], input=b"secret", env=env, capture=True)
            self.assertNotIn("secret", str(error.exception))
            self.assertTrue(set(tooling.SECRETS).isdisjoint(run.call_args.kwargs["env"]))

    def test_bundle_links_must_be_relative_and_internal(self):
        with tempfile.TemporaryDirectory() as directory:
            app = Path(directory) / "Test.app"; app.mkdir()
            (app / "target").write_text("target")
            (app / "relative").symlink_to("target")
            tooling.assert_relative_links(app)
            (app / "external").symlink_to("../outside")
            with self.assertRaises(tooling.ReleaseError): tooling.assert_relative_links(app)

    def test_build_numbers_cannot_be_reused(self):
        config = tooling.release_configuration(META, ENV)
        signature = base64.b64encode(bytes(64)).decode()
        with tempfile.TemporaryDirectory() as directory:
            feed = Path(directory) / "appcast.xml"
            feed.write_bytes(tooling.appcast(META, config, signature, 123))
            with self.assertRaises(tooling.ReleaseError): tooling.check_history(META, [feed])
            with self.assertRaises(tooling.ReleaseError): tooling.check_history(dict(META, build=0), [feed])
            tooling.check_history(dict(META, build=2), [feed])


@unittest.skipUnless(sys.platform == "darwin", "macOS CryptoKit and Sparkle command line tools required")
class ArchiveAuthenticationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.signer = tooling.ROOT / ".build/artifacts/sparkle/Sparkle/bin/sign_update"
        if not cls.signer.is_file():
            raise unittest.SkipTest("Resolve/build the pinned Sparkle package before archive signature tests")
        cls.verifier = tooling.signature_verifier()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.archive = self.directory / tooling.archive_name(META)
        self.archive.write_bytes(b"PK\x03\x04Test archive bytes: fixture only")
        self.config = tooling.release_configuration(META, ENV)
        self.signature = tooling.sign_archive(self.archive, SEED, self.signer)
        self.feed = self.directory / "appcast.xml"
        self.feed.write_bytes(tooling.appcast(META, self.config, self.signature, self.archive.stat().st_size))

    def test_actual_sparkle_signature_matches_embedded_key_and_archive(self):
        tooling.verify_signature(self.archive, PUBLIC, self.signature, self.verifier)
        tooling.verify_appcast(self.feed, self.archive, META, self.config, self.verifier)
        altered_key = base64.b64encode(bytes(32)).decode()
        with self.assertRaises(tooling.ReleaseError): tooling.verify_signature(self.archive, altered_key, self.signature, self.verifier)

    def test_modified_archive_is_rejected_even_when_size_is_unchanged(self):
        data = self.archive.read_bytes()
        self.archive.write_bytes(data[:-1] + bytes([data[-1] ^ 1]))
        with self.assertRaises(tooling.ReleaseError): tooling.verify_appcast(self.feed, self.archive, META, self.config, self.verifier)

    def test_appcast_metadata_tampering_and_ambiguity_are_rejected(self):
        original = self.feed.read_bytes()
        for name, transform in {
            "wrong build": lambda root: setattr(root.find("./channel/item/{" + tooling.SPARKLE_NS + "}version"), "text", "999"),
            "wrong version": lambda root: setattr(root.find("./channel/item/{" + tooling.SPARKLE_NS + "}shortVersionString"), "text", "9.0.0"),
            "wrong arch": lambda root: setattr(root.find("./channel/item/{" + tooling.SPARKLE_NS + "}hardwareRequirements"), "text", "x86_64"),
            "wrong URL": lambda root: root.find("./channel/item/enclosure").set("url", "https://evil.example/zip"),
            "wrong size": lambda root: root.find("./channel/item/enclosure").set("length", "99999"),
            "wrong signature": lambda root: root.find("./channel/item/enclosure").set("{" + tooling.SPARKLE_NS + "}edSignature", base64.b64encode(bytes(64)).decode()),
            "duplicate enclosure": lambda root: root.find("./channel/item").append(copy.deepcopy(root.find("./channel/item/enclosure"))),
            "duplicate build": lambda root: root.find("./channel/item").append(copy.deepcopy(root.find("./channel/item/{" + tooling.SPARKLE_NS + "}version"))),
        }.items():
            with self.subTest(name=name):
                root = ET.fromstring(original); transform(root); self.feed.write_bytes(ET.tostring(root))
                with self.assertRaises(tooling.ReleaseError): tooling.verify_appcast(self.feed, self.archive, META, self.config, self.verifier)
        self.feed.write_bytes(b'<!DOCTYPE rss [<!ENTITY x "value">]>' + original)
        with self.assertRaises(tooling.ReleaseError): tooling.verify_appcast(self.feed, self.archive, META, self.config, self.verifier)

    def test_invalid_or_missing_private_key_never_uses_keychain(self):
        for private in ("", "not-base64", base64.b64encode(bytes(64)).decode()):
            with self.subTest(private=private), mock.patch("release.subprocess.run") as run:
                with self.assertRaises(tooling.ReleaseError): tooling.sign_archive(self.archive, private, self.signer)
                run.assert_not_called()

    def test_ad_hoc_app_is_rejected_by_stable_identity_gate(self):
        app = tooling.ROOT / "build" / tooling.APP_NAME
        if not app.exists(): self.skipTest("Build the local app before checking the ad-hoc identity gate")
        with self.assertRaises(tooling.ReleaseError): tooling.verify_identity(app, "0" * 40)

class SigningHelperTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location("albert_release_signing", tooling.ROOT / "release-signing.py")
        cls.signing = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.signing)

    def test_certificate_backup_must_match_pin_validity_and_code_signing_usage(self):
        text = f"sha1 Fingerprint={':'.join(['AA'] * 20)}\nnotBefore=Jan  1 00:00:00 2020 GMT\nnotAfter=Jan  1 00:00:00 2099 GMT\n    X509v3 Extended Key Usage:\n        Code Signing\n"
        env = dict(ENV, ALBERT_SIGNING_PASSWORD="fixture-password")
        with mock.patch.object(self.signing, "quiet", side_effect=[b"public certificate", text.encode()]) as quiet:
            self.signing.certificate(env, Path("fixture.p12"))
            self.assertEqual(quiet.call_args_list[0].kwargs["env"]["ALBERT_SIGNING_PASSWORD"], "fixture-password")
            self.assertNotIn("SPARKLE_PRIVATE_KEY", quiet.call_args_list[0].kwargs["env"])
        for altered in (text.replace("AA:", "BB:", 1), text.replace("2099", "2021"), text.replace("2020", "2098"), text.replace("Code Signing", "TLS Web Server Authentication")):
            with self.subTest(altered=altered), mock.patch.object(self.signing, "quiet", side_effect=[b"public certificate", altered.encode()]):
                with self.assertRaises(tooling.ReleaseError): self.signing.certificate(env, Path("fixture.p12"))

    @unittest.skipUnless(sys.platform == "darwin", "macOS system LibreSSL required")
    def test_real_system_openssl_parses_disposable_code_signing_pkcs12(self):
        # Only this temporary test identity is generated. No keychain is created
        # or read, and no user certificate/key participates in this test.
        with tempfile.TemporaryDirectory(prefix="albert-signing-test-") as directory:
            directory = Path(directory)
            key, pem, p12 = (directory / name for name in ("test.key", "test.pem", "test.p12"))
            config = directory / "request.cnf"
            password_env = dict(tooling.public_environment(), ALBERT_TEST_P12_PASSWORD="disposable-test-password")
            def command(args):
                return subprocess.run(["/usr/bin/openssl", *map(str, args)], check=True, capture_output=True, env=password_env).stdout
            for usage in ("codeSigning", "serverAuth"):
                # The subject deliberately contains Code Signing. It must not
                # let a certificate with only TLS EKU pass the signing gate.
                config.write_text("[req]\ndistinguished_name=dn\nx509_extensions=extensions\nprompt=no\n[dn]\nCN=Albert Code Signing Test Fixture\n[extensions]\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical," + usage + "\n")
                args = ["req", "-new", "-x509", "-sha256", "-days", "1", "-config", config, "-out", pem]
                args += ["-key", key] if key.exists() else ["-newkey", "rsa:2048", "-nodes", "-keyout", key]
                command(args)
                command(["pkcs12", "-export", "-inkey", key, "-in", pem, "-out", p12,
                         "-passout", "env:ALBERT_TEST_P12_PASSWORD", "-keypbe", "PBE-SHA1-3DES",
                         "-certpbe", "PBE-SHA1-3DES", "-macalg", "sha1"])
                text = command(["x509", "-in", pem, "-noout", "-fingerprint", "-sha1"]).decode()
                fingerprint = text.split("=", 1)[1].strip().replace(":", "")
                env = dict(ENV, ALBERT_SIGNING_CERT_SHA1=fingerprint, ALBERT_SIGNING_PASSWORD=password_env["ALBERT_TEST_P12_PASSWORD"])
                if usage == "codeSigning":
                    self.signing.certificate(env, p12)
                    with self.assertRaises(tooling.ReleaseError): self.signing.certificate(dict(env, ALBERT_SIGNING_CERT_SHA1="0" * 40), p12)
                else:
                    with self.assertRaises(tooling.ReleaseError): self.signing.certificate(env, p12)

    def test_missing_secrets_fail_before_accessing_any_keychain(self):
        import io
        with mock.patch.dict(os.environ, ENV, clear=True), mock.patch.object(self.signing, "quiet") as quiet, mock.patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(self.signing.main(), 1)
            quiet.assert_not_called()

    def test_bundle_gate_checks_updater_flags_versions_and_architecture(self):
        config = tooling.release_configuration(META, ENV)
        with tempfile.TemporaryDirectory() as directory:
            app = Path(directory) / "Test.app"
            info = app / "Contents/Info.plist"
            info.parent.mkdir(parents=True)
            for relative in ("Contents/MacOS/Albert", "Contents/Frameworks/Sparkle.framework/Sparkle", "Contents/Frameworks/Sparkle.framework/Versions/B/Updater.app/Contents/MacOS/Updater"):
                path = app / relative; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(b"fixture")
            expected = tooling.info_plist(META, config)
            def command(args, **kwargs):
                if args[0] == "/usr/bin/lipo": return b"arm64\n"
                if args[0] == "/usr/bin/otool": return b"@executable_path/../Frameworks"
                return b""
            info.write_bytes(plistlib.dumps(expected))
            with mock.patch("release.run", side_effect=command), mock.patch("release.verify_identity"):
                tooling.verify_bundle(app, META, config)
                for field, bad in (("CFBundleVersion", "2"), ("CFBundleShortVersionString", "0.2.0"), ("SUPublicEDKey", "bad"), ("SUAutomaticallyUpdate", True), ("SUAllowsAutomaticUpdates", True)):
                    with self.subTest(field=field):
                        info.write_bytes(plistlib.dumps(dict(expected, **{field: bad})))
                        with self.assertRaises(tooling.ReleaseError): tooling.verify_bundle(app, META, config)
                info.write_bytes(plistlib.dumps(expected))
            with mock.patch("release.run", return_value=b"x86_64\n"), mock.patch("release.verify_identity"):
                with self.assertRaises(tooling.ReleaseError): tooling.verify_bundle(app, META, config)


if __name__ == "__main__":
    unittest.main()
