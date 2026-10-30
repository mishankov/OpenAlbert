# OpenAlbert selective publication

Registry prefix: `ghcr.io/mishankov/openalbert`. Required platforms: `linux/amd64` and `linux/arm64`. Publish a clean `master` checkout, normally the merged revision. Respect current AGENTS.md and inspect `tools/local.py`, `deploy/docker-bake.hcl`, and the Dockerfiles before using the helper if the repository has changed.

The helper's current source-input map is:

| Target | Inputs |
| --- | --- |
| openalbert | apps/backend/, services/backup/, go.mod, go.sum |
| backup | services/backup/, go.mod, go.sum |
| stt | services/stt/, go.mod, go.sum |
| log-gateway | services/log-gateway/, go.mod, go.sum |
| minio | services/minio/Dockerfile |
| minio-init | services/minio/Dockerfile, services/minio/init.sh |

`.dockerignore` or Bake changes conservatively affect every target. Confirm newly introduced shared files/imports before relying on this map. MinIO builds pinned upstream source and does not consume the repository's go.mod. The backend imports backup code, so backup changes can require both images. Native app and ordinary documentation changes do not enter these container targets.

Plan without building or pushing:

```sh
python3 <skill-dir>/scripts/openalbert_publish.py --repo <clean-master-checkout> --prefix ghcr.io/mishankov/openalbert
```

The planner inspects each component's `master` image, both platform revision labels, and platform digests. It compares every distinct published revision to local HEAD. It requires an existing published two-platform release; investigate a missing image, missing platform, or registry failure rather than using the helper for an initial release. Unknown/unavailable revision labels conservatively select that image for rebuild.

When building and publishing is authorized, execute through Task using a temporary Taskfile outside tracked sources (for example, in ignored `reports/local/`). Set its task `dir` to the clean checkout and command to:

```sh
python3 <skill-dir>/scripts/openalbert_publish.py --repo <clean-master-checkout> --prefix ghcr.io/mishankov/openalbert --execute
```

The helper reuses `tools/local.py`'s clean-master gate, check/test suites, reports, image smoke scripts, deployment/restart checks, and publication loop. It narrows only the build and upload target set. Verification includes Go race/integration, backup restore, web keyboard flows, Go/Swift contract, and native tests/smoke in Tart. It compiles native artifacts on the host and runs desktop tests only in the guest. It pulls unchanged deployment companions by platform digest. Existing Docker credentials are used; no application .env is loaded.

Selected images receive `sha-<full revision>-amd64`, `sha-<full revision>-arm64`, `sha-<full revision>`, and `master`. Unselected images retain their existing manifests, tags, and revision labels.

`deploy/compose.yaml` uses one `OPENALBERT_IMAGE_TAG` for all six components. After a selective release, the new SHA tag exists only on changed images. The moving `master` set still works, but do not tell users to set the global deployment tag to that SHA. A fully pinned release needs per-component references/overrides or an explicitly requested tag-only promotion of unchanged manifests. Never imply that selective publication deployed the app.

The helper runs full verification; for a session that already has exact-revision passing reports, an agent may prepare a temporary Task adapter using the same `Run` helpers to resume only unfinished stages. Record the report provenance and retain all checks required by the repository. Do not edit tracked publication tooling simply to bypass its target selection or validation gates.
