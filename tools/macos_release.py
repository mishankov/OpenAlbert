#!/usr/bin/env python3
"""Local macOS release history validation and explicit GitHub draft upload."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "apps/macos"))
from release import ROOT, ReleaseError, check_history, metadata, release_configuration


def gh(*args):
    return subprocess.check_output(["gh", *args])


def pages(endpoint):
    # gh --paginate prints adjacent JSON arrays, one for each response page.
    source = gh("api", "--paginate", endpoint).decode()
    decoder = json.JSONDecoder()
    values = []
    while source.strip():
        page, end = decoder.raw_decode(source.lstrip())
        values.extend(page)
        source = source.lstrip()[end:]
    return values


def history(configuration, directory):
    repository, tag = configuration["repository"], configuration["tag"]
    tags = pages(f"repos/{repository}/tags?per_page=100")
    releases = pages(f"repos/{repository}/releases?per_page=100")
    if any(item["name"] == tag for item in tags) or any(item["tag_name"] == tag for item in releases):
        raise ReleaseError("This release tag already exists; tags and release archives cannot be reused")
    directory.mkdir(parents=True, exist_ok=True)
    feeds = []
    for release in releases:
        if not release["tag_name"].startswith("macos-v"):
            continue
        assets = [asset for asset in release["assets"] if asset["name"] == "appcast.xml"]
        if len(assets) != 1:
            raise ReleaseError("A previous macOS release has missing or ambiguous appcast metadata")
        asset_id = assets[0]["id"]
        path = directory / f"{asset_id}.xml"
        path.write_bytes(gh("api", "-H", "Accept: application/octet-stream", f"repos/{repository}/releases/assets/{asset_id}"))
        feeds.append(path)
    check_history(metadata(), feeds)
    print(f"Release history verified: {len(feeds)} previous appcasts")


def draft(configuration, artifacts):
    repository, tag = configuration["repository"], configuration["tag"]
    # Verify the packaging output itself; the actual archive name comes from its
    # authenticated appcast, and is checked by release.py during packaging.
    files = sorted(artifacts.iterdir())
    if not {"appcast.xml", "checksums.txt"}.issubset({path.name for path in files}) or len([p for p in files if p.suffix == ".zip"]) != 1:
        raise ReleaseError("Release artifacts must contain appcast.xml, checksums.txt and exactly one ZIP")
    if any(not path.is_file() or path.is_symlink() for path in files):
        raise ReleaseError("Release artifacts must be regular files")
    if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT.parents[1]).strip():
        raise ReleaseError("Draft upload requires a clean checkout")
    sha = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT.parents[1], text=True).strip()
    # The API fails atomically if another operator reserved the tag after history
    # validation. Never delete or retarget a tag on an upload failure.
    gh("api", "--method", "POST", f"repos/{repository}/git/refs", "-f", "ref=refs/tags/" + tag, "-f", "sha=" + sha)
    subprocess.run(["gh", "release", "create", tag, *map(str, files), "--repo", repository,
                    "--verify-tag", "--draft", "--title", "Альберт " + tag, "--generate-notes"], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["history", "draft"])
    parser.add_argument("--output", type=Path)
    parser.add_argument("--artifacts", type=Path)
    args = parser.parse_args()
    try:
        configuration = release_configuration(metadata(), os.environ)
        if args.command == "history":
            if args.output is None:
                parser.error("history requires --output")
            history(configuration, args.output)
        else:
            if args.artifacts is None:
                parser.error("draft requires --artifacts")
            # Recheck history immediately before reserving the immutable tag.
            import tempfile
            with tempfile.TemporaryDirectory(prefix="albert-release-history-") as temporary:
                history(configuration, Path(temporary))
            draft(configuration, args.artifacts)
    except (ReleaseError, OSError, subprocess.SubprocessError, ValueError) as error:
        print(f"macOS release failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
