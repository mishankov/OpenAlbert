import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import local


class LocalRunTests(unittest.TestCase):
    def make_run(self, directory, command="check"):
        return local.Run(command, Path(directory) / "report")

    def test_test_environment_excludes_live_and_signing_settings(self):
        with mock.patch.dict(os.environ, {"LLM_API_KEY": "private", "OPENALBERT_LIVE_RETRIEVAL_EVAL": "1",
                                        "SPARKLE_PRIVATE_KEY": "private", "ALBERT_DESKTOP_BACKEND": "host", "CI": "true"}):
            env = local.test_environment()
        for name in ("LLM_API_KEY", "OPENALBERT_LIVE_RETRIEVAL_EVAL", "SPARKLE_PRIVATE_KEY", "ALBERT_DESKTOP_BACKEND", "CI"):
            self.assertNotIn(name, env)

    def test_failed_command_keeps_output_and_failure_in_report(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            run = self.make_run(directory)
            with self.assertRaisesRegex(RuntimeError, "exit 7"):
                run.command("broken", [sys.executable, "-c", "print('diagnostic'); raise SystemExit(7)"])
            report = json.loads((run.directory / "result.json").read_text())
            self.assertEqual(report["steps"][0]["status"], "failed")
            self.assertIn("diagnostic", (run.directory / report["steps"][0]["log"]).read_text())

    def test_timeout_stops_child_and_records_failure(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            run = self.make_run(directory)
            with self.assertRaises(subprocess.TimeoutExpired):
                run.command("deadline", [sys.executable, "-c", "import time; time.sleep(60)"], timeout=.1)
            self.assertEqual(run.data["steps"][0]["status"], "failed")

    def test_go_console_keeps_failure_diagnostics_and_all_events_on_disk(self):
        events = [
            {"Action": "output", "Package": "p", "Test": "Good", "Output": "verbose success\n"},
            {"Action": "pass", "Package": "p", "Test": "Good"},
            {"Action": "output", "Package": "p", "Test": "Bad", "Output": "expected A, got B\n"},
            {"Action": "fail", "Package": "p", "Test": "Bad"},
        ]
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()) as console:
            display = local.GoEvents(Path(directory))
            for event in events:
                display.line(json.dumps(event) + "\n")
            display.close()
            saved = [json.loads(line) for line in (Path(directory) / "go-tests.jsonl").read_text().splitlines()]
            self.assertEqual(saved, events)
            self.assertNotIn("verbose success", console.getvalue())
            self.assertIn("expected A, got B", console.getvalue())
            self.assertIn("FAIL p/Bad", console.getvalue())

    def test_go_cleanup_runs_even_when_startup_or_tests_fail(self):
        for failing in ("go-services", "go-race-integration"):
            with self.subTest(failing=failing), tempfile.TemporaryDirectory() as directory:
                run = self.make_run(directory)
                calls = []

                def command(name, args, **kwargs):
                    calls.append((name, args))
                    if name == failing:
                        raise RuntimeError("synthetic failure")

                with mock.patch.object(run, "command", side_effect=command), self.assertRaises(RuntimeError):
                    run.go()
                self.assertEqual(calls[-1][0], "go-cleanup")
                self.assertIn("--volumes", calls[-1][1])
                self.assertIn(run.repository, calls[-1][1])

    def test_publication_never_pushes_after_a_failed_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory, "publish")
            calls = []

            def images(arch, deployment=False):
                calls.append(arch)
                if arch == "arm64":
                    raise RuntimeError("smoke failed")

            with mock.patch.dict(os.environ, {"OPENALBERT_IMAGE_PREFIX": "ghcr.io/example/openalbert"}), \
                    mock.patch.object(run, "clean_checkout"), mock.patch.object(run, "check"), \
                    mock.patch.object(run, "tests"), mock.patch.object(run, "images", side_effect=images), \
                    mock.patch.object(run, "command") as command:
                with self.assertRaisesRegex(RuntimeError, "smoke failed"):
                    run.publish()
                command.assert_not_called()
            self.assertEqual(calls, ["amd64", "arm64"])

    def test_computer_checks_are_opt_in_and_clean_up_after_failure(self):
        self.assertNotIn("computer", local.COMPONENTS)
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory)
            calls = []

            def command(name, args, **kwargs):
                calls.append((name, args))
                if name == "computer-lifecycle":
                    raise RuntimeError("synthetic update failure")

            with mock.patch.object(run, "command", side_effect=command), self.assertRaises(RuntimeError):
                run.computer()
            self.assertEqual(calls[-1][0], "computer-test-cleanup")
            self.assertIn(run.repository + ":computer-test", calls[-1][1])
            self.assertNotIn("computer-ssh", [name for name, _ in calls])
            with mock.patch.object(run, "go"), mock.patch.object(run, "backup"), \
                    mock.patch.object(run, "web"), mock.patch.object(run, "macos"), \
                    mock.patch.object(run, "computer") as computer:
                run.tests()
                computer.assert_not_called()

    def test_publication_rechecks_sources_before_first_upload(self):
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory, "publish")
            with mock.patch.dict(os.environ, {"OPENALBERT_IMAGE_PREFIX": "ghcr.io/example/openalbert"}), \
                    mock.patch.object(run, "clean_checkout", side_effect=[None, RuntimeError("sources changed")]), \
                    mock.patch.object(run, "check"), mock.patch.object(run, "tests"), mock.patch.object(run, "images"), \
                    mock.patch.object(run, "command") as command:
                with self.assertRaisesRegex(RuntimeError, "sources changed"):
                    run.publish()
                command.assert_not_called()


spec = importlib.util.spec_from_file_location("macos_release", Path(__file__).with_name("macos_release.py"))
macos_release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(macos_release)


class MacReleaseTests(unittest.TestCase):
    def test_pagination_handles_adjacent_json_arrays(self):
        with mock.patch.object(macos_release, "gh", return_value=b' [ {"id": 1} ]\n [ {"id": 2} ]\n'):
            self.assertEqual(macos_release.pages("endpoint"), [{"id": 1}, {"id": 2}])

    def test_existing_tag_is_rejected_without_downloading_or_uploading(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(macos_release, "pages", side_effect=[[{"name": "macos-v1.0.0"}], []]), \
                mock.patch.object(macos_release, "gh") as gh:
            with self.assertRaisesRegex(macos_release.ReleaseError, "already exists"):
                macos_release.history({"repository": "example/app", "tag": "macos-v1.0.0"}, Path(directory))
            gh.assert_not_called()

    def test_ambiguous_previous_release_metadata_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(macos_release, "pages", side_effect=[[], [{"tag_name": "macos-v0.1.0", "assets": []}]]):
            with self.assertRaisesRegex(macos_release.ReleaseError, "ambiguous"):
                macos_release.history({"repository": "example/app", "tag": "macos-v1.0.0"}, Path(directory))


if __name__ == "__main__":
    unittest.main()
