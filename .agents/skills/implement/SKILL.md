---
name: implement
description: Use whenever implementing a change.
---

# Implement

## Check for skill updates

The lead agent runs this step once per project per task, before editing files
or delegating implementation. From the target project's Git root, skip the
check if `.agents/mstack.lock.json` is absent. Otherwise run
`mstack update --check` with a short execution timeout.

- If the check succeeds with no updates or dependencies to install, continue.
- If the check succeeds with changes to apply, summarize the skills to update
  and any new dependencies. Use approval the user already gave for this task
  or explicit permission for automatic mstack skill updates in the project's
  `AGENTS.md`. When neither applies and the run is attended, ask once whether
  to update now or continue with the installed skills. With approval, run
  `mstack update` with a short timeout that allows graceful cancellation, then
  report the result. Without approval, report the deferred changes and continue
  with the installed skills.
- If mstack is unavailable or either command returns a nonzero exit or times
  out, report the issue and any conflicting paths. Preserve local edits and
  continue with the installed skills. If the check fails, report any changes
  it listed as deferred.

## Implement the change

Use subagents to parallelize independent work.
