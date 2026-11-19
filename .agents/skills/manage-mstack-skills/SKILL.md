---
name: manage-mstack-skills
description: Use when the user asks to list, install, or update mstack skills in a project, or to install or upgrade the mstack CLI.
---

# Manage mstack skills

Install or upgrade the CLI when requested using Git credentials with read
access to the private `mstack` repository.

```sh
GOPRIVATE=github.com/mishankov/mstack \
  go install github.com/mishankov/mstack/cmd/mstack@latest
```

Verify the version on `PATH` with `mstack --version`. If the binary is missing
during a skill operation, report the setup command so the user can install it.

Run skill commands from the intended project's Git checkout.

- Use `mstack list` to see available and installed skills.
- Use `mstack install <skill>...` for the requested skills. The CLI installs
  their required dependencies. Use `mstack install --all` for the full catalog.
- Use `mstack update --check` to check for updates without applying them.
- Use `mstack update <skill>...` to update named skills, or `mstack update` to
  update every managed skill in the project.

If the CLI reports a collision or local edits, preserve the affected files and
report their paths. After a successful write, summarize what changed.
