# OpenAlbert selective publication

Registry prefix: `ghcr.io/mishankov/openalbert`. Required platforms: `linux/amd64` and `linux/arm64`. Publish a clean `master` checkout, normally the merged revision. Respect current AGENTS.md and inspect `tools/local.py`, `deploy/docker-bake.hcl`, and the Dockerfiles before using the helper if the repository has changed.

The helper's current source-input map is:

| Target | Inputs |
| --- | --- |
| openalbert | apps/backend/, shared/backup/, shared/codemode/ |
| backup | services/backup/, shared/backup/ |
| stt | services/stt/ |
| tts | services/tts/ |
| log-gateway | services/log-gateway/ |
| minio | services/minio/Dockerfile |
| minio-init | services/minio/Dockerfile, services/minio/init.sh |

`.dockerignore` or Bake changes conservatively affect every target. Each Go image builds with its own module files and `GOWORK=off`. The backend and backup CLI also copy the shared backup library, so changes to that library select both images. Upgrade a dependency in each module that should pick up the new version. Confirm newly introduced shared files/imports before relying on this map. MinIO builds pinned upstream source. Ordinary documentation changes do not enter these container targets.

Plan without building or pushing:

```sh
python3 <skill-dir>/scripts/openalbert_publish.py --repo <clean-master-checkout> --prefix ghcr.io/mishankov/openalbert
```

The planner inspects each component's `master` image, both platform revision labels, and platform digests. It compares every distinct published revision to local HEAD. An explicit registry `not found` for `tts:master` selects TTS for first publication on both platforms. Other missing images, missing platforms, and registry access or network failures halt the plan. Unknown or unavailable revision labels conservatively select that image for rebuild.

When building and publishing is authorized, execute through Task using a temporary Taskfile outside tracked sources (for example, in ignored `reports/local/`). Set its task `dir` to the clean checkout and command to:

```sh
python3 <skill-dir>/scripts/openalbert_publish.py --repo <clean-master-checkout> --prefix ghcr.io/mishankov/openalbert --execute
```

The helper reuses `tools/local.py`'s clean-master gate, check/test suites, reports, image smoke scripts, deployment/restart checks, and publication loop. It narrows only the build and upload target set. Container-release verification includes Go race/integration, backup restore and web keyboard flows. It pulls unchanged deployment companions by platform digest. Existing Docker credentials are used; no application .env is loaded.

Selected images receive `sha-<full revision>-amd64`, `sha-<full revision>-arm64`, `sha-<full revision>`, and `master`. Unselected images retain their existing manifests, tags, and revision labels.

`deploy/compose.yaml` uses one `OPENALBERT_IMAGE_TAG` for all components. After a selective release, the new SHA tag exists only on changed images. The moving `master` set still works, but do not tell users to set the global deployment tag to that SHA. A fully pinned release needs per-component references/overrides or an explicitly requested tag-only promotion of unchanged manifests. Never imply that selective publication deployed the app.

The helper runs the container-release verification described above; for a session that already has exact-revision passing reports, an agent may prepare a temporary Task adapter using the same `Run` helpers to resume only unfinished stages. Record the report provenance and retain all checks required by the repository. Do not edit tracked publication tooling simply to bypass its target selection or validation gates.
