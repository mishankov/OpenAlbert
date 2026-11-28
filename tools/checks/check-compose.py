"""Validate both deployment modes without reading a real .env or starting services."""
import copy
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]
OWN_IMAGES = {"openalbert", "stt", "tts", "backup"}
REMOVED_SERVICES = {"postgres", "temporal", "temporal-ui", "minio", "minio-init", "victorialogs", "log-gateway"}
REMOVED_ENV = {
    "DATABASE_URL", "POSTGRES_PASSWORD", "POSTGRES_PORT",
    "TEMPORAL_ADDRESS", "TEMPORAL_NAMESPACE", "TEMPORAL_TASK_QUEUE",
    "TEMPORAL_UI_HOST_PORT", "TEMPORAL_LOG_LEVEL",
    "S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BUCKET", "S3_SECURE",
    "VICTORIALOGS_URL", "DOCKER_LOGS_GATEWAY_URL",
}
TEST_ENV = {
    "OPENALBERT_IMAGE_PREFIX": "ghcr.io/example/openalbert",
    "OPENALBERT_IMAGE_TAG": "sha-" + "1" * 40,
    "TELEGRAM_BOT_TOKEN": "123:ci-only-token",
    "TELEGRAM_ALLOWED_USER_IDS": "123",
}


def render(filename=None, overrides=None, env_file=".env.example", cwd=ROOT, model_env_file="/dev/null"):
    # Neither Compose's interpolation env nor service env_file may read private files.
    env = {key: value for key, value in os.environ.items() if key in {"PATH", "HOME", "DOCKER_CONFIG"}}
    env.update(TEST_ENV)
    env.update(overrides or {})
    with tempfile.TemporaryDirectory(prefix="openalbert-compose-settings-") as directory:
        command = ["docker", "compose", "--project-directory", str(cwd), "--env-file", env_file]
        files = filename if isinstance(filename, list) else [filename or "compose.yaml"]
        for index, original in enumerate(files):
            path = Path(original)
            if not path.is_absolute():
                path = cwd / path
            source = path.read_text()
            if "path: ./models.env" in source:
                copied = Path(directory) / f"compose-{index}.yaml"
                copied.write_text(source.replace("path: ./models.env", "path: " + json.dumps(model_env_file)))
                path = copied
            command += ["-f", str(path)]
        return subprocess.run(
            command + ["config", "--format", "json"],
            cwd=cwd, env=env, capture_output=True, text=True, check=False,
        )


def config(filename=None, **kwargs):
    result = render(filename, **kwargs)
    if result.returncode:
        raise AssertionError(result.stderr)
    return json.loads(result.stdout)


def check_pair(overrides):
    dev = config("deploy/compose.local.yaml", overrides=overrides)
    release = config("deploy/compose.yaml", overrides=overrides)
    assert dev["services"].keys() == release["services"].keys(), "Compose service sets differ"
    assert not (REMOVED_SERVICES & release["services"].keys()), "Removed infrastructure service still present"
    assert not release.get("configs"), "Release Compose must not define removed inline configs"
    assert not dev.get("configs"), "Local Compose must not define removed inline configs"
    for name, original in dev["services"].items():
        expected = copy.deepcopy(original)
        actual = copy.deepcopy(release["services"][name])
        assert name in OWN_IMAGES, f"{name}: unexpected service outside the 4-image catalogue"
        assert "build" not in actual, f"{name}: release must not build on the host"
        assert "build" in expected, f"{name}: local development must build from source"
        build = expected["build"]
        assert Path(build["context"]) == ROOT, f"{name}: build context moved away from the repository root"
        assert (ROOT / build["dockerfile"]).is_file(), f"{name}: Dockerfile is missing"
        image = f'{TEST_ENV["OPENALBERT_IMAGE_PREFIX"]}/{name}:{TEST_ENV["OPENALBERT_IMAGE_TAG"]}'
        assert actual.pop("image") == image, f"{name}: unexpected published image"
        expected.pop("build")
        assert actual == expected, f"{name}: development/release runtime settings drifted"
        driver = actual.get("logging", {}).get("driver")
        assert driver in {"local", "json-file"}, f"{name}: unexpected logging driver {driver!r}"
        leaked_env = REMOVED_ENV & actual.get("environment", {}).keys()
        assert not leaked_env, f"{name}: removed environment variables present: {sorted(leaked_env)}"
        for volume in actual.get("volumes", []):
            assert volume["target"] != "/var/run/docker.sock", f"{name}: Docker socket must not be mounted"
            if volume["type"] == "tmpfs":
                continue
            assert volume["type"] == "bind", f"{name}: expected bind volume for {volume['target']}"
            assert "/container_data/" not in volume["source"], f"{name}: stale container_data path in {volume['source']}"
            if name == "openalbert" and volume["target"] == "/etc/openalbert/models.toml":
                assert volume["read_only"]
                assert not volume.get("bind", {}).get("create_host_path", False)
                assert Path(volume["source"]).name == "models.toml"
                continue
            assert Path(volume["source"]).is_relative_to(ROOT), f"{name}: bind moved away from project root"
    return release


def check_user_computer():
    # The copied template builds only on the user's host and has its own lifecycle.
    with tempfile.TemporaryDirectory(prefix="openalbert-user-computer-") as directory:
        destination = Path(directory) / "computer"
        shutil.copytree(ROOT / "services/computer", destination)
        computer = config("compose.yaml", env_file="/dev/null", cwd=destination)
        assert set(computer["services"]) == {"computer", "computer-init"}
        assert computer["name"] != "openalbert"
        service = computer["services"]["computer"]
        assert Path(service["build"]["context"]) == destination
        assert service["image"] == "openalbert-computer:local"
        assert service["pull_policy"] == "never" and not service.get("ports")
        for name in ("computer", "computer-init"):
            for volume in computer["services"][name]["volumes"]:
                assert Path(volume["source"]).is_relative_to(destination)
        for address in ("", "computer:22"):
            for base in ("deploy/compose.yaml", "deploy/compose.local.yaml"):
                attached = config([base, str(destination / "app.compose.yaml")], overrides={
                    "COMPUTER_DIRECTORY": str(destination), "COMPUTER_SSH_ADDRESS": address,
                })
                app = attached["services"]["openalbert"]
                assert not {"computer", "computer-init"} & attached["services"].keys()
                assert app["environment"]["COMPUTER_SSH_ADDRESS"] == address
                assert set(app["networks"]) == {"default", "user-computer"}
                assert attached["networks"]["user-computer"]["external"]
                assert attached["networks"]["user-computer"]["name"] == computer["networks"]["default"]["name"]
                keys = [v for v in app["volumes"] if v["target"] == "/data/computer"]
                assert len(keys) == 1 and keys[0]["read_only"]
                assert Path(keys[0]["source"]) == destination / "client"


def main():
    current = check_pair({})
    assert current["name"] == "openalbert", "Unexpected Compose project name"
    assert set(current["services"]) == {"openalbert", "stt"}, "Default .env.example must enable only openalbert and stt"
    app_binds = {v["target"]: v for v in current["services"]["openalbert"]["volumes"]}
    assert Path(app_binds["/data"]["source"]) == ROOT / "data" and not app_binds["/data"].get("read_only", False)
    assert Path(app_binds["/data/backup-state"]["source"]) == ROOT / "backup-state" and app_binds["/data/backup-state"]["read_only"]
    stt_binds = {v["target"]: v for v in current["services"]["stt"]["volumes"]}
    assert Path(stt_binds["/models"]["source"]) == ROOT / "stt-models"

    tunnel = config(["deploy/compose.yaml", "deploy/networking/compose.wireguard.yaml"])
    app_ports = tunnel["services"]["openalbert"]["ports"]
    assert {(p["host_ip"], str(p["published"]), p["target"]) for p in app_ports} == {
        ("127.0.0.1", "8080", 8080), ("10.77.0.2", "8080", 8080),
    }, "Desktop tunnel override must preserve loopback and bind only the dedicated WireGuard IP"

    assert "backup" not in current["services"], "Backups must be opt-in"
    with_backup = check_pair({"COMPOSE_PROFILES": "backup,local-stt"})
    backup = with_backup["services"]["backup"]
    binds = {v["target"]: v for v in backup["volumes"]}
    assert Path(binds["/installation"]["source"]) == ROOT and binds["/installation"]["read_only"]
    assert Path(binds["/installation/data"]["source"]) == ROOT / "data" and not binds["/installation/data"].get("read_only", False)
    assert Path(binds["/etc/openalbert-backup"]["source"]) == ROOT / "backup-config" and binds["/etc/openalbert-backup"]["read_only"]
    assert Path(binds["/installation/backup-state"]["source"]) == ROOT / "backup-state" and not binds["/installation/backup-state"].get("read_only", False)
    assert Path(binds["/var/lib/openalbert-backup"]["source"]) == ROOT / "backup-work" and not binds["/var/lib/openalbert-backup"].get("read_only", False)

    for stt_url in ["", "http://speech.example:8080/v1"]:
        for browser in ["true", "false"]:
            check_pair({"STT_BASE_URL": stt_url, "BROWSER_ENABLED": browser})
        minimal = check_pair({"STT_BASE_URL": stt_url, "COMPOSE_PROFILES": ""})
        assert set(minimal["services"]) == {"openalbert"}, "Minimal deployment must run only openalbert"
        deps = minimal["services"]["openalbert"].get("depends_on") or {}
        assert all(spec.get("required") is False for spec in deps.values()), "openalbert must have no required service dependencies"

    assert "tts" not in current["services"], "Bundled speech synthesis must be opt-in"
    assert current["services"]["openalbert"]["environment"]["TTS_BASE_URL"] == ""
    bundled = check_pair({"COMPOSE_PROFILES": "local-stt,local-tts", "TTS_BASE_URL": "http://tts:8080/v1",
                          "TTS_API_KEY": "tts-test-only"})
    assert bundled["services"]["tts"]["environment"]["TTS_API_KEY"] == "tts-test-only"
    assert not bundled["services"]["tts"].get("ports"), "Bundled TTS must remain internal"
    assert bundled["services"]["openalbert"]["environment"]["TTS_BASE_URL"] == "http://tts:8080/v1"
    external_tts = {"TTS_BASE_URL": "https://speech.example/v1", "TTS_MODEL": "external-model",
                    "TTS_VOICE": "external-voice", "TTS_API_KEY": "tts-test-only"}
    external = check_pair(external_tts)
    assert "tts" not in external["services"]
    app = external["services"]["openalbert"]["environment"]
    assert all(app[key] == value for key, value in external_tts.items()), "External synthesis settings were lost"

    assert not {"computer", "computer-init"} & current["services"].keys(), "Albert must not manage a user computer"
    assert set(current["services"]["openalbert"]["networks"]) == {"default"}
    assert current["services"]["openalbert"]["environment"]["COMPUTER_SSH_ADDRESS"] == ""

    bake = json.loads(subprocess.check_output(
        ["docker", "buildx", "bake", "-f", "deploy/docker-bake.hcl", "--print"], cwd=ROOT, text=True,
    ))
    assert set(bake["group"]["default"]["targets"]) == OWN_IMAGES, "Release build catalogue drifted"
    assert "computer" not in bake["target"], "User computers must not be built/published with Albert"
    check_user_computer()

    for address in ["", "remote.example:2222"]:
        configured = check_pair({"COMPUTER_SSH_ADDRESS": address})
        assert configured["services"]["openalbert"]["environment"]["COMPUTER_SSH_ADDRESS"] == address

    inspector_url = "https://inspect.example/inspect/"
    inspected = check_pair({"INSPECTOR_PUBLIC_URL": inspector_url})
    assert inspected["services"]["openalbert"]["environment"]["INSPECTOR_PUBLIC_URL"] == inspector_url

    for variable in TEST_ENV:
        result = render("deploy/compose.yaml", overrides={variable: ""}, env_file=".env.release.example")
        assert result.returncode and variable in result.stderr, f"{variable}: release should reject an empty required value"

    plan = check_pair({"OPENAI_AUTH_MODE": "siwc", "LLM_API_KEY": ""})
    assert plan["services"]["openalbert"]["environment"]["OPENAI_AUTH_MODE"] == "siwc"
    assert plan["services"]["openalbert"]["environment"]["LLM_API_KEY"] == ""
    disabled_consolidation = check_pair({"MEMORY_CONSOLIDATION_INTERVAL": "0"})
    assert disabled_consolidation["services"]["openalbert"]["environment"]["MEMORY_CONSOLIDATION_INTERVAL"] == "0"
    disabled_coding_reap = check_pair({"CODING_SESSION_STALE_TIMEOUT": "0"})
    assert disabled_coding_reap["services"]["openalbert"]["environment"]["CODING_SESSION_STALE_TIMEOUT"] == "0"

    exa_env = {
        "WEB_SEARCH_PROVIDER": "exa",
        "EXA_API_KEY": "exa-test-only",
        "EXA_SEARCH_URL": "https://exa.example/search",
        "EXA_MONTHLY_BUDGET_USD": "25",
    }
    exa_checked = check_pair(exa_env)
    exa_app = exa_checked["services"]["openalbert"]["environment"]
    assert all(exa_app[key] == value for key, value in exa_env.items()), "Exa search settings were lost"

    app = current["services"]["openalbert"]["environment"]
    assert app["DATA_DIR"] == "/data"
    assert app["BACKUP_STATE_DIR"] == "/data/backup-state"
    assert app["LLM_CONFIG_FILE"] == "/etc/openalbert/models.toml"
    assert "LLM_MODEL" not in app and "LLM_BASE_URL" not in app
    assert app["LLM_API_KEY"] == "", "Custom credential names must not require the example key"

    # Standalone deployment requires compose + .env + model config, no source tree.
    with tempfile.TemporaryDirectory(prefix="openalbert-compose-") as directory:
        destination = Path(directory)
        (destination / "compose.yaml").write_text((ROOT / "deploy/compose.yaml").read_text())
        (destination / ".env").write_text((ROOT / ".env.release.example").read_text())
        (destination / "models.toml").write_text((ROOT / "deploy/models.example.toml").read_text())
        credentials = destination / "models.env"
        credentials.write_text("ANALYSIS_LLM_API_KEY=synthetic-analysis-secret\n")
        standalone = config(env_file=".env", cwd=destination, model_env_file=str(credentials))
        assert standalone["services"]["openalbert"]["environment"]["ANALYSIS_LLM_API_KEY"] == "synthetic-analysis-secret"
        credentials.unlink()
        config(env_file=".env", cwd=destination, model_env_file=str(credentials))
    print("Development/release Compose parity, required settings and standalone deployment: OK")


if __name__ == "__main__":
    main()
