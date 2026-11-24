---
name: start-thread
description: Use automatically and without a user-facing announcement at the start of every new top-level thread, before other work, to check and update the mstack CLI and installed skills.
---

# Start thread

The lead agent runs this skill once per project at the start of every new
top-level thread, before other work. Delegated children and subagents skip it.
Do not re-run it on later turns in the same thread unless the user asks or work
moves to another Git checkout that has not been checked yet.

Run startup silently. Skip the skill announcement, progress commentary, and
success summary. Report startup failures and deferred changes as described
below, then continue with the user's task.

## Update mstack CLI and skills

Skip this step if the target project's Git root has no
`.agents/mstack.lock.json`, or if the user or the project's `AGENTS.md` directs
you to skip it (including the legacy instruction `Skip mstack skill update
checks during implementation.`).

- **Default (apply updates, including in unattended runs):**
  1. Upgrade the `mstack` CLI first with a short execution timeout (requires Go
     1.23 or later):

     ```sh
     go install github.com/mishankov/mstack/cmd/mstack@latest
     ```

     If `mstack` is missing from `PATH`, use the binary from `GOBIN` or the
     `bin` directory in the first `GOPATH` entry. If `go` is unavailable or
     `go install` fails or times out, report the issue briefly and fall back to
     any already-installed `mstack` binary.
  2. From the project's Git root, run `mstack update --check` with a short
     execution timeout.
  3. If the check succeeds and reports skill updates, renames, dependencies to
     install, or `Skill attributes need updating: .agents/.gitattributes`, run
     `mstack update` with a short timeout that allows graceful cancellation.
     The attributes message can appear alongside
     `Already current`. If any skill you are about to use was updated or
     renamed, re-read its `SKILL.md` before following it.
  4. If the check succeeds and reports no pending changes, continue.
- **When deferring updates** (if the user or the project's `AGENTS.md` directs
  you to defer updates, including the legacy instruction `Defer mstack skill
  updates during implementation.`): skip the CLI upgrade and `mstack update`.
  If `mstack` is available, run `mstack update --check` from the project's Git
  root with a short execution timeout, report any pending changes as deferred,
  and continue with the installed skills.
- **On failure:** if `mstack` is unavailable or either `mstack` command returns
  a nonzero exit or times out, report the issue and any conflicting paths.
  Preserve local edits and continue with the installed skills. If the check
  fails after listing changes, report those changes as deferred.
