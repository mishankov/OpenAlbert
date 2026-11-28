import contextlib
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
        with mock.patch.dict(os.environ, {"LLM_API_KEY": "private", "LLM_CONFIG_FILE": "/private/models.toml",
                                        "LLM_CONFIG_HOST_FILE": "/private/models.toml", "OPENALBERT_LIVE_RETRIEVAL_EVAL": "1",
                                        "SIGNING_PRIVATE_KEY": "private", "APPLICATION_TOKEN": "private", "CI": "true",
                                        "OPENALBERT_TEST_BACKUP_MINIO_IMAGE": "local/minio:test"}):
            env = local.test_environment()
            self.assertEqual(env["OPENALBERT_TEST_BACKUP_MINIO_IMAGE"], "local/minio:test")
        for name in ("LLM_API_KEY", "LLM_CONFIG_FILE", "LLM_CONFIG_HOST_FILE", "OPENALBERT_LIVE_RETRIEVAL_EVAL", "SIGNING_PRIVATE_KEY", "APPLICATION_TOKEN", "CI"):
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
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory)
            calls = []

            def command(name, args, **kwargs):
                calls.append((name, args))
                if name == "go-race-integration":
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
                    mock.patch.object(run, "web"), \
                    mock.patch.object(run, "computer") as computer:
                run.tests()
                computer.assert_not_called()

    def test_image_publication_runs_server_checks_before_upload(self):
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory, "publish")
            calls = []

            def images(arch, deployment=False):
                self.assertTrue(deployment)
                calls.append("images-" + arch)
                run.data["images"][arch] = {"openalbert": "synthetic:" + arch}

            with mock.patch.dict(os.environ, {"OPENALBERT_IMAGE_PREFIX": "registry.example/openalbert"}), \
                    mock.patch.object(local, "COMPONENTS", ("openalbert",)), \
                    mock.patch.object(run, "clean_checkout"), mock.patch.object(run, "check"), \
                    mock.patch.object(run, "go", side_effect=lambda: calls.append("go")), \
                    mock.patch.object(run, "backup", side_effect=lambda: calls.append("backup")), \
                    mock.patch.object(run, "web", side_effect=lambda: calls.append("web")), \
                    mock.patch.object(run, "images", side_effect=images), \
                    mock.patch.object(run, "command", side_effect=lambda *a, **kw: calls.append("upload")):
                run.publish()
            self.assertEqual(calls[:5], ["go", "backup", "web", "images-amd64", "images-arm64"])
            self.assertEqual(calls[5:], ["upload"] * 5)

    def test_full_test_suite_runs_only_go_backup_and_web(self):
        with tempfile.TemporaryDirectory() as directory:
            run = self.make_run(directory, "test")
            stages = mock.Mock()
            with mock.patch.object(run, "go", stages.go), \
                    mock.patch.object(run, "backup", stages.backup), \
                    mock.patch.object(run, "web", stages.web), \
                    mock.patch.object(run, "command") as command, \
                    mock.patch.object(local.subprocess, "Popen") as process:
                run.tests()
                self.assertEqual(stages.mock_calls, [mock.call.go(), mock.call.backup(), mock.call.web()])
                command.assert_not_called()
                process.assert_not_called()

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


if __name__ == "__main__":
    unittest.main()
