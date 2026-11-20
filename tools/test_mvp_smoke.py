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
import urllib.parse


spec = importlib.util.spec_from_file_location("mvp_smoke", Path(__file__).parent / "smoke/mvp-smoke.py")
mvp_smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mvp_smoke)


class DevelopmentSmokeTests(unittest.TestCase):
    def test_running_infrastructure_credentials_are_shared_without_external_secrets(self):
        secret = "external-secret-must-not-propagate"
        postgres_user = "custom@user"
        postgres_password = "custom:$postgres/password"
        minio_user = "custom$minio"
        minio_password = "custom-$minio-secret"
        service_env = {
            "postgres": [f"POSTGRES_USER={postgres_user}", f"POSTGRES_PASSWORD={postgres_password}"],
            "minio": [f"MINIO_ROOT_USER={minio_user}", f"MINIO_ROOT_PASSWORD={minio_password}"],
            "temporal": ["DEFAULT_NAMESPACE=custom-namespace", "TEMPORAL_ADDRESS=temporal:7233"],
        }
        for values in service_env.values():
            values.extend([f"LLM_API_KEY={secret}", f"TELEGRAM_BOT_TOKEN={secret}",
                           f"YANDEX_SEARCH_URL=https://{secret}.example", f"UNRELATED_SETTING={secret}"])
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
            if command[:2] == ["docker", "inspect"]:
                return json.dumps(service_env[command[-1].removesuffix("-container")])
            if "ps" in command:
                service = command[-1]
                return "" if service == "openalbert" else service + "-container"
            if "--env-file" in command:
                self.assertEqual(Path(command[command.index("--env-file") + 1]).name, ".env.example")
            if "minio-init" in command or "up" in command:
                files = [Path(command[i + 1]) for i, value in enumerate(command) if value == "-f"]
                self.assertIn("path: /dev/null", files[0].read_text())
                overrides.append([json.loads(path.read_text()) for path in files[1:]])
            if "up" in command:
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
                                                 "TELEGRAM_BASE_URL": secret, "S3_SECRET_KEY": secret}, clear=True), \
                    mock.patch.object(Path, "read_text", guarded_read), \
                    mock.patch.object(mvp_smoke, "run", side_effect=fake_run), \
                    mock.patch.object(mvp_smoke.subprocess, "run") as cleanup, \
                    mock.patch.object(mvp_smoke.http.server, "ThreadingHTTPServer"), \
                    mock.patch.object(mvp_smoke.threading, "Thread"), \
                    contextlib.redirect_stdout(io.StringIO()), \
                    self.assertRaisesRegex(RuntimeError, "stop fake smoke"):
                mvp_smoke.smoke(args, settings)
            self.assertNotIn(secret, str(cleanup.call_args_list))

        initial, startup = overrides
        self.assertNotIn(secret, json.dumps(overrides))
        self.assertEqual(startup[0], initial[0])
        self.assertEqual(len(initial), 1, "bucket initialization must use running MinIO credentials")
        infrastructure = initial[0]["services"]
        expected_s3 = {"S3_ACCESS_KEY": minio_user.replace("$", "$$"),
                       "S3_SECRET_KEY": minio_password.replace("$", "$$")}
        self.assertEqual(infrastructure["minio-init"]["environment"], expected_s3)
        app_env = infrastructure["openalbert"]["environment"]
        self.assertEqual({key: app_env[key] for key in expected_s3}, expected_s3)
        self.assertEqual(app_env["TEMPORAL_NAMESPACE"], "custom-namespace")
        self.assertEqual(app_env["TEMPORAL_ADDRESS"], "temporal:7233")
        app = startup[-1]["services"]["openalbert"]
        url = urllib.parse.urlsplit(app["environment"]["DATABASE_URL"])
        self.assertEqual(urllib.parse.unquote(url.username), postgres_user)
        self.assertEqual(urllib.parse.unquote(url.password), postgres_password)
        self.assertTrue(url.path.startswith("/mvp_smoke_"))
        self.assertTrue(app["environment"]["S3_BUCKET"].startswith("mvp-smoke-"))
        self.assertEqual(app["volumes"][0]["source"], "smoke_knowledge")
        create = next(command for command, _ in commands if any("CREATE DATABASE" in part for part in command))
        self.assertEqual(create[create.index("-U") + 1], postgres_user)
        startup_env = next(env for command, env in commands if "up" in command)
        self.assertEqual(startup_env["LLM_API_KEY"], "smoke-key")
        self.assertEqual(startup_env["TELEGRAM_BOT_TOKEN"], "smoke-token")
        self.assertEqual(startup_env["YANDEX_API_KEY"], "smoke-key")
        self.assertEqual(startup_env["TELEGRAM_BASE_URL"], "http://host.docker.internal:18089")
        self.assertEqual(startup_env["YANDEX_SEARCH_URL"], "http://host.docker.internal:18089/search")

    def test_missing_or_ambiguous_running_service_fails_before_inspection(self):
        for containers in ("", "first\nsecond"):
            with self.subTest(containers=containers), mock.patch.object(mvp_smoke, "run", return_value=containers) as run:
                with self.assertRaisesRegex(SystemExit, "one running postgres"):
                    mvp_smoke.running_environment(["docker", "compose"], {}, "postgres", {"POSTGRES_PASSWORD"})
                run.assert_called_once_with(["docker", "compose", "ps", "-q", "postgres"], {})

    def test_missing_required_credentials_fail_without_reporting_values(self):
        with mock.patch.object(mvp_smoke, "run", side_effect=["container", '["UNRELATED_KEY=secret"]']):
            with self.assertRaisesRegex(SystemExit, "missing required infrastructure settings") as error:
                mvp_smoke.running_environment(["docker", "compose"], {}, "postgres", {"POSTGRES_PASSWORD"})
        self.assertNotIn("secret", str(error.exception))


if __name__ == "__main__":
    unittest.main()
