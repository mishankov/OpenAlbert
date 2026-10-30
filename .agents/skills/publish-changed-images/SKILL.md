---
name: publish-changed-images
description: Build, verify, and publish only Docker images affected since each component's last published revision. Use for selective image releases or requests to avoid rebuilding unchanged services. Includes an executable OpenAlbert workflow; adapt other repositories to their own build graph.
---

Publish the smallest correct set of changed images. Compare against the registry's published state, not just the last commit or PR. Preserve the user's registry, architecture, and release scope.

## Select images

Read repository instructions and its build/publication configuration. Trace Dockerfile inputs, shared libraries, embedded assets, dependency locks, build arguments, and runtime files copied into each target. A shared dependency can affect several images. Documentation outside image inputs does not require a rebuild.

Inspect each published component's revision label on every required platform. Compare its build inputs with the requested clean revision. Components may have different published revisions after earlier selective releases. Missing or ambiguous provenance needs investigation or a conservative rebuild, never a claim that the component is unchanged. A source-only comparison does not detect refreshed mutable base images; include affected targets when the user requests dependency or base-image refreshes.

State the chosen components and why. Build only those Bake targets, for the project's required architectures. Leave unchanged remote images and tags untouched unless the user requests a complete release tag set. If nothing changed, report that without publishing.

## Verify and publish

Keep repository verification requirements. Reuse passing checks only when their reports prove the exact clean revision, configuration, and required coverage. A cancelled overall run can provide evidence for individual completed stages; failed or interrupted stages must finish successfully.

Run selected image smoke tests and required integration/deployment checks with unchanged companions pinned by digest. Build and test all selected architectures before uploading. Keep desktop checks in the managed VM when the repository requires it.

Publish only with authorization from the current request/session. A request to build and publish already grants that authorization; do not ask again. Skill creation or a planning-only request does not authorize a push. Do not deploy unless requested.

Use the existing tagging conventions. Give selected images their immutable revision tags and requested moving tags. Reusing an unchanged manifest is a tag operation, not a rebuild; do not do it when the user asks to leave unchanged images alone. Never relabel an old binary as newly built. Explain any shared deployment-tag limitation.

Before pushing, ensure the verified checkout still matches the planned clean revision and branch. Stop on failed checks, unavailable credentials, unexpected source changes, or registry errors. Preserve reports and report partially published components accurately. Retry a demonstrated transient infrastructure failure once; do not repeatedly rerun tests to obtain a pass.

Inspect the remote manifests after publication: required architectures, revision labels, immutable/moving tag equality, and unchanged component manifests. Finish with published component names, tags, verification result, and any deployment limitation.

## OpenAlbert

Read [references/openalbert.md](references/openalbert.md) when working in OpenAlbert. The bundled [scripts/openalbert_publish.py](scripts/openalbert_publish.py) plans by default and publishes only with `--execute`. It uses the repository's existing verification, smoke, and publishing helpers through Task, and leaves source files unchanged.

For another repository, derive its build graph and commands instead of using the OpenAlbert helper.
