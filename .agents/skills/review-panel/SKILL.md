---
name: review-panel
description: Use when the user says "review panel".
---

# Review panel

Use the latest OpenAI GPT Astra and Google Gemini 4 Neon, both at high reasoning
effort. Honor explicit reviewer and effort overrides. Resolve the full reviewer
set before starting, and report any unavailable required model or effort.

Use [review-loop](../review-loop/SKILL.md) to resolve and start delegates. Start
one read-only delegate per reviewer in parallel and give them the same brief.
For a code review, also follow its scoping, checks, round budget, finding
severity, and approval rules. Before round 1, fix known issues and address
outstanding in-scope PR comments. When the user is exploring a replacement,
skip fixes to code being replaced.

- In every code review round, have each delegate independently apply both
  checks from the shared brief and return a verdict, findings, and a score out of 10.
- Verify findings as they arrive. Track findings by reviewer and ID, and
  resolve duplicates together. Wait for all required reviewers before applying
  fixes or deciding the round's outcome.
- If the user marks a reviewer as non-blocking, use available feedback without
  counting it toward approval or recording a score. Cancel an outstanding
  non-blocking review before editing the checkout it reads or ending the task.

For feedback without a code diff, give each delegate the original request
and the proposal. Have each independently check the proposal against the
request and applicable documented repository rules. Summarize their
findings, disagreements, and next steps.

## Report results

For code reviews, count each pass by the reviewer set as one round. Use the
section below in place of the single-reviewer PR line. If the work ends in a
PR, put it in the description before the model and harness line. For local
work, put it in the final reply. Report the actual verdict. Record `no score`
for a missing score. Scores are informational.

```markdown
## Review panel results

Rounds: 2
Verdict: approved

| Delegate | Round 1 | Round 2 |
| --- | --- | --- |
| GPT Astra | 8/10 | 10/10 |
| Gemini 4 Neon | 7/10 | 9/10 |
```
