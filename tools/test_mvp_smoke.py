import argparse
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("mvp_smoke", Path(__file__).parent / "smoke/mvp-smoke.py")
mvp_smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mvp_smoke)


class DevelopmentSmokeTests(unittest.TestCase):
    def test_isolated_smoke_ignores_real_secrets_and_uses_isolated_data_dir(self):
        secret = "external-secret-must-not-propagate"
        commands = []
        overrides = []
        compose_source = (mvp_smoke.ROOT / "deploy/compose.local.yaml").read_text()
        original_read = Path.read_text

        def guarded_read(path, *args, **kwargs):
            self.assertNotIn(path.name, (".env", "models.env"), "real application settings must stay unread")
            return original_read(path, *args, **kwargs)

        def fake_run(command, env, **kwargs):
            commands.append((list(command), dict(env)))
            self.assertEqual(env["COMPOSE_PROJECT_NAME"], "custom-project")
            self.assertNotIn(secret, json.dumps([command, env]))
            if "ps" in command:
                return ""
            if "--env-file" in command:
                self.assertEqual(Path(command[command.index("--env-file") + 1]).name, ".env.example")
            if "up" in command:
                files = [Path(command[i + 1]) for i, value in enumerate(command) if value == "-f"]
                self.assertIn("path: /dev/null", files[0].read_text())
                overrides.append([json.loads(path.read_text()) for path in files[1:]])
                raise RuntimeError("stop fake smoke after startup configuration")
            return ""

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "deploy").mkdir()
            (root / "deploy/compose.local.yaml").write_text(compose_source)
            (root / ".env").write_text("must remain unread: " + secret)
            (root / "models.env").write_text("must remain unread: " + secret)
            settings = root / "settings"
            settings.mkdir()
            args = argparse.Namespace(host_address="host.docker.internal", port=18089,
                                      without_search=False, with_browser=False)
            with mock.patch.object(mvp_smoke, "ROOT", root), \
                    mock.patch.dict(os.environ, {"COMPOSE_PROJECT_NAME": "custom-project", "LLM_API_KEY": secret,
                                                 "TELEGRAM_BASE_URL": secret, "EXA_API_KEY": secret}, clear=True), \
                    mock.patch.object(Path, "read_text", guarded_read), \
                    mock.patch.object(mvp_smoke, "run", side_effect=fake_run), \
                    mock.patch.object(mvp_smoke.subprocess, "run") as cleanup, \
                    mock.patch.object(mvp_smoke.http.server, "ThreadingHTTPServer"), \
                    mock.patch.object(mvp_smoke.threading, "Thread"), \
                    contextlib.redirect_stdout(io.StringIO()), \
                    self.assertRaisesRegex(RuntimeError, "stop fake smoke"):
                mvp_smoke.smoke(args, settings)
            self.assertNotIn(secret, str(cleanup.call_args_list))

        self.assertEqual(len(overrides), 1)
        self.assertNotIn(secret, json.dumps(overrides))
        app = overrides[0][-1]["services"]["openalbert"]
        self.assertEqual(app["volumes"][0]["target"], "/data")
        self.assertEqual(app["volumes"][0]["type"], "bind")
        startup_env = next(env for command, env in commands if "up" in command)
        self.assertEqual(startup_env["COMPOSE_PROFILES"], "")
        self.assertEqual(startup_env["STT_BASE_URL"], "")
        self.assertEqual(startup_env["LLM_API_KEY"], "smoke-key")
        self.assertEqual(startup_env["TELEGRAM_BOT_TOKEN"], "smoke-token")
        self.assertEqual(startup_env["EXA_API_KEY"], "smoke-exa-key")
        self.assertEqual(startup_env["TELEGRAM_BASE_URL"], "http://host.docker.internal:18089")
        self.assertEqual(startup_env["EXA_SEARCH_URL"], "http://host.docker.internal:18089/exa/search")
        self.assertEqual(startup_env["YANDEX_API_KEY"], "")

    def test_refuses_running_openalbert(self):
        compose_source = (mvp_smoke.ROOT / "deploy/compose.local.yaml").read_text()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "deploy").mkdir()
            (root / "deploy/compose.local.yaml").write_text(compose_source)
            settings = root / "settings"
            settings.mkdir()
            args = argparse.Namespace(host_address="host.docker.internal", port=18089,
                                      without_search=True, with_browser=False)
            with mock.patch.object(mvp_smoke, "ROOT", root), \
                    mock.patch.object(mvp_smoke, "run", return_value="running-container-id"), \
                    self.assertRaisesRegex(SystemExit, "Stop your OpenAlbert container"):
                mvp_smoke.smoke(args, settings)


if __name__ == "__main__":
    unittest.main()
