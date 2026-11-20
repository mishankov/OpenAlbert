---
name: manage-mstack-skills
description: Use when the user asks to list, install, update, or remove mstack skills in a project, or to install or upgrade the mstack CLI.
---

# Manage mstack skills

Install the CLI if it's missing during a requested skill operation. Use the
same command when asked to upgrade it. The installer requires Go 1.23 or later.

```sh
go install github.com/mishankov/mstack/cmd/mstack@latest
```

Verify the version with `mstack --version`. If the binary is missing from
`PATH`, use it from `GOBIN` or the `bin` directory in the first `GOPATH` entry.

Run skill commands from the intended project's Git checkout.

- Use `mstack list` to see available and installed skills.
- Use `mstack install <skill>...` for the requested skills. The CLI installs
  their required dependencies. Use `mstack install --all` for the full catalog.
- Use `mstack update --check` to check for updates without applying them.
- Use `mstack update <skill>...` to update named skills, or `mstack update` to
  update all managed skills still available upstream. Bulk updates report and
  skip retired skills.
- Use `mstack remove <skill>...` to remove named managed skills when requested.
  Before removing a skill that other installed skills use, tell the user which
  workflows depend on it.

If the CLI reports a collision or local edits, preserve the affected files and
report their paths. After a successful write, summarize what changed.
