"""Validate both deployment modes without reading a real .env or starting services."""
import copy
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]
OWN_IMAGES = {"openalbert", "stt", "log-gateway", "minio", "minio-init", "backup"}
TEST_ENV = {
    "OPENALBERT_IMAGE_PREFIX": "ghcr.io/example/openalbert",
    "OPENALBERT_IMAGE_TAG": "sha-" + "1" * 40,
    "POSTGRES_PASSWORD": "ci-only-password",
    "S3_ACCESS_KEY": "openalbert-ci",
    "S3_SECRET_KEY": "ci-only-secret",
    "TELEGRAM_BOT_TOKEN": "123:ci-only-token",
    "TELEGRAM_ALLOWED_USER_IDS": "123",
    "LLM_API_KEY": "ci-only-key",
    "LLM_MODEL": "ci-model",
}
INLINE_CONFIGS = {
    "postgres": ("postgres-init", "services/postgres/init.sql", "/docker-entrypoint-initdb.d/01-databases.sql"),
    "temporal": ("temporal-dynamicconfig", "services/temporal/dynamicconfig.yaml", "/etc/temporal/config/dynamicconfig/development-sql.yaml"),
}


def render(filename=None, overrides=None, env_file=".env.example", cwd=ROOT):
    # Use only the example and deterministic overrides, not developer credentials.
    env = {key: value for key, value in os.environ.items() if key in {"PATH", "HOME", "DOCKER_CONFIG"}}
    env.update(TEST_ENV)
    env.update(overrides or {})
    command = ["docker", "compose", "--project-directory", str(cwd), "--env-file", env_file]
    if filename is not None:
        for path in filename if isinstance(filename, list) else [filename]:
            command += ["-f", path]
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
    for name, original in dev["services"].items():
        expected = copy.deepcopy(original)
        actual = copy.deepcopy(release["services"][name])
        assert "build" not in actual, f"{name}: release must not build on the host"
        if name in OWN_IMAGES:
            assert "build" in expected, f"{name}: local development must build from source"
            build = expected["build"]
            assert Path(build["context"]) == ROOT, f"{name}: build context moved away from the repository root"
            assert (ROOT / build["dockerfile"]).is_file(), f"{name}: Dockerfile is missing"
            image = f'{TEST_ENV["OPENALBERT_IMAGE_PREFIX"]}/{name}:{TEST_ENV["OPENALBERT_IMAGE_TAG"]}'
            assert actual.pop("image") == image, f"{name}: unexpected published image"
            expected.pop("build")
        else:
            assert actual["image"] == expected["image"], f"{name}: upstream versions differ"
        if name in INLINE_CONFIGS:
            key, path, target = INLINE_CONFIGS[name]
            assert release["configs"][key]["content"] == (ROOT / path).read_text(), f"{key}: inline config drifted"
            expected["volumes"] = [v for v in expected.get("volumes", []) if v["target"] != target]
            if not expected["volumes"]:
                expected.pop("volumes")
            assert actual.pop("configs") == [{"source": key, "target": target}], f"{key}: unexpected config mount"
        assert actual == expected, f"{name}: development/release runtime settings drifted"
        for volume in actual.get("volumes", []):
            if name == "backup":
                assert volume["type"] == "bind", "backup: requires explicit host binds"
                continue
            assert volume["type"] == "bind" and "/container_data/" in volume["source"], f"{name}: release requires checkout files"
            assert Path(volume["source"]).is_relative_to(ROOT / "container_data"), f"{name}: persistent data moved away from the repository root"
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
    tunnel = config(["deploy/compose.yaml", "deploy/networking/compose.wireguard.yaml"])
    app_ports = tunnel["services"]["openalbert"]["ports"]
    assert {(p["host_ip"], str(p["published"]), p["target"]) for p in app_ports} == {
        ("127.0.0.1", "8080", 8080), ("10.77.0.2", "8080", 8080),
    }, "Desktop tunnel override must preserve loopback and bind only the dedicated WireGuard IP"
    assert "backup" not in current["services"], "Backups must be opt-in"
    with_backup = check_pair({"COMPOSE_PROFILES": "backup,local-stt"})
    backup = with_backup["services"]["backup"]
    assert backup["logging"]["driver"] == "local", "backup must survive the log gateway pause"
    binds = {v["target"]: v for v in backup["volumes"]}
    assert binds["/installation"]["read_only"] and binds["/etc/openalbert-backup"]["read_only"]
    assert not binds["/installation/container_data/backup-state"].get("read_only", False)
    for name, service in with_backup["services"].items():
        if name != "backup":
            assert not any(v["target"] == "/var/run/docker.sock" for v in service.get("volumes", [])), "Docker socket leaked to an application service"
    assert current["name"] == "openalbert", "Unexpected Compose project name"
    assert "stt" in current["services"], "Example settings must enable bundled speech recognition"
    for stt_url in ["", "http://speech.example:8080/v1"]:
        for browser in ["true", "false"]:
            check_pair({"STT_BASE_URL": stt_url, "BROWSER_ENABLED": browser})
        external = check_pair({"STT_BASE_URL": stt_url, "COMPOSE_PROFILES": ""})
        assert "stt" not in external["services"], "Disabled bundled speech recognition must not start"
        dependency = external["services"]["openalbert"].get("depends_on", {}).get("stt")
        assert dependency is None or dependency.get("required") is False, "External speech recognition still requires the bundled service"
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
    for logs_url in ["", "http://logs.example:9428"]:
        inspector_url = "https://inspect.example/inspect/"
        release = check_pair({"VICTORIALOGS_URL": logs_url, "INSPECTOR_PUBLIC_URL": inspector_url})
        app = release["services"]["openalbert"]["environment"]
        assert app["VICTORIALOGS_URL"] == logs_url, "Explicit logs endpoint/disable setting was lost"
        assert app["INSPECTOR_PUBLIC_URL"] == inspector_url, "Public inspector links were lost"
    for variable in TEST_ENV:
        result = render("deploy/compose.yaml", overrides={variable: ""}, env_file=".env.release.example")
        assert result.returncode and variable in result.stderr, f"{variable}: release should reject an empty required value"
    # This is the advertised deployment contract: compose + .env, no source tree.
    with tempfile.TemporaryDirectory(prefix="openalbert-compose-") as directory:
        destination = Path(directory)
        (destination / "compose.yaml").write_text((ROOT / "deploy/compose.yaml").read_text())
        (destination / ".env").write_text((ROOT / ".env.release.example").read_text())
        config(env_file=".env", cwd=destination)
    print("Development/release Compose parity, required settings and standalone deployment: OK")


if __name__ == "__main__":
    main()
