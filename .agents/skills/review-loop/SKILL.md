---
name: review-loop
description: Use when the user says "review loop".
---

# review-loop

Use a bounded review loop before or after filing a pull request. Default to
the latest OpenAI GPT Astra model with high reasoning effort. Honor explicit
model or effort overrides.

When another skill uses this procedure, apply its reviewer settings and
reporting instructions.

Make the smallest changes that resolve verified findings. Push back on scope
creep and findings that lack evidence.

## Budget

- Default: 3 rounds total. Round 1 is full. Later rounds check only
  changed code and open findings.
- Extend to 5 only for security, permissions, auth, data loss, or
  state/concurrency, while a blocker or should stays open with evidence. Or when
  the user asks.
- Stop early if a round returns only nits. Declined nits and comment,
  doc, or format-only fixes need no new round.
- Use one reviewer by default. Reviewers must not start subagents or reviewers.
- Keep a ledger: id, severity, outcome, usage if returned. Never guess it.

## 1. Scope

Run `git status --short`. Do not stash, discard, or include unrelated
changes. Limit review to the change paths if they separate cleanly.
Otherwise stop and ask.

Find the base after `git fetch origin`:

- PR: `gh pr view <n> --json baseRefName,headRefOid`. Check out the branch
  only if clean. Confirm `HEAD` is the PR head, or ahead only by this
  change. Base: `git merge-base HEAD origin/<baseRefName>`.
- Local: `git merge-base HEAD origin/<default-branch>`. No remote: a base
  the user names, or `HEAD` for uncommitted changes only.

The change is base to working tree in those paths, with untracked files.
Use one diff command all rounds. State intent and out-of-scope in 1-2
sentences. Run covering checks. Do not send code this change breaks. Note
missing or pre-failing checks in the brief.

## 2. Start review

In T3, call `orchestrator_capabilities`. Resolve the selected model and effort
for each reviewer from the capabilities and pass the effort in `target.options`.
Record provider, model, and effort only if exposed. Use a fresh `delegate_task`
and `clientRequestId` per reviewer per round (`role: "review"`, `mode: "async"`).
Save each `taskId`.
`childThreadId` is storage, not the next target. Do not use
`t3_thread_send`, `create_threads`, or `t3_thread_launch` for review.

If T3 tools are not listed, try one direct capability call. Outside T3, use
a native subagent that supports the selected model and effort and keep its ID.
In Claude Code, use a custom subagent only if it is set up to run those settings.
If no route supports the selected model and effort, report the blocker.

Do not edit scoped files while review runs. Wait for completion notices without
polling and keep to the configured reviewer set. Timeout, error, or no reply
leaves a required review incomplete. On failure, cancel the failed review,
verify it stopped, and restart it with the ledger.

Include the UI check below for user-facing UI changes. Fill in absolute paths
to the applicable references in the installed Impeccable skill.

Shared brief for every round (T3 children get no history):

```
Review only. Do not edit, commit, push, comment on PRs, or start subagents.
Repo: <abs path> Base: <sha> Intent: <intent> Out of scope: <scope> Checks: <results>
Request: <original request or accessible issue/spec, with later user decisions>
See: git diff <base> [-- <paths>]; git status --short (read untracked).
Read surrounding code and check for broken callers. In this round's scope:
- Compare behavior with the request. Identify missing, incorrect, or extra
  behavior and cite the requirement or scope decision behind each finding.
- Check the repository instructions and documented rules that apply to the
  changed paths. Cite the file and specific rule for each violation.
Skip style preferences unless they affect the intended UI or hide a bug. Hold
to the smallest change. Flag code, tests, or guards the intent does not need. Other
paths, rare cases, small perf, extra compat: nits.

UI check (if applicable): <screen or flow, user task, design conventions, runtime evidence>.
Apply the Impeccable audit checks read-only (reference/audit.md: <absolute path>).
Check accessibility, keyboard behavior, responsive layout, and theming as applicable.
For a new or substantially changed screen or flow, also apply the Impeccable
critique rubric (reference/critique-rubric.md: <absolute path>).
Verify the main keyboard flow with the project's harness. Check shortcut
discoverability when primary commands change. If references or runtime evidence
are missing, report the limitation and continue with available checks. Use the
same findings format and severity rules for verified, in-scope UI problems.
Treat visual preference as a nit.

blocker = intended case wrong. should = real bug callers will hit, in intent. nit = rest.
If you found only nits or no findings, return APPROVE.
Reply:
VERDICT: APPROVE or CHANGES
S1 [blocker|should|nit] <file>:<line> <claim>. Fix: <suggestion> (stable ids)
```

## 3. Answer findings

Check each finding and severity yourself. Another path, rare case, or
small cost is a nit, whatever label it came with.

- Agree: smallest fix. Prefer delete or narrow over new lock, cache,
  helper, or option. Fix the case that happened. Add one focused test
  that fails without the fix, if intent needs it.
- Disagree: say why, with evidence (file:line, test, output).
- Out of scope or nit: decline in one line. List as follow-up.

Update the ledger. Re-run checks and `git diff --shortstat <base>`. If
review grew the diff, cut first. Do not agree just to end the loop.

## 4. Next round

Start one only if budget remains and a blocker or should is open, behavior code
changed, or approval does not cover the current behavior diff. For each reviewer,
use a fresh `delegate_task` in T3, or follow up the saved native ID. Send the
shared brief, the ledger (`fixed:` / `disagree:` / `declined:` + evidence), the
same base diff, and the change since last round. Apply both the request and
repository-rule checks to the changes since the last round and open findings only.
Mark each old id `resolved` or `still open: <reason>`.

## 5. Stop and ship

Agreed: APPROVE from every required reviewer on the current behavior diff, no
blocker or should open, no concern of your own. Stuck: an item stays disputed
after two replies, or budget ends. Show both sides in 2-3 lines each. The user
decides.
Budget end is not approval.

Do not push during the loop. Push after agreement only if the user asked
to push, ship, or update the PR. If you push to a PR, add above the model
blurb:

`Reviewed with review-loop: <N> rounds with <model>. Verdict: approved`.

Add `on <effort>` only if verified.

Report: rounds, model, changes made after review, diff size before/after, fixed and
declined findings with reasons, usage if exposed (else say not), pushed or not.
