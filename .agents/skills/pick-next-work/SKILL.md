---
name: pick-next-work
description: Use when reviewing what remains, comparing priorities, or choosing the next item to work on.
---

# Pick the next work

## 1. Read the backlog

Read the canonical backlog, established priorities, dependencies, and active work. Apply the user's constraints, including any named area or item. For a status request, summarize what remains and stop; distinguish recorded status from verified behavior. Do not change records or start implementation.

## 2. Check the likely candidates

Inspect enough current code, linked PRs, and delivery evidence to establish whether the shortlist is still useful and available. Distinguish unimplemented work from work already active, blocked, superseded, or awaiting review or delivery. Do not select already-owned work again unless continuation is requested.

Start with likely candidates; do not audit every issue against the whole codebase. Expand the check when evidence changes the recommendation. Flag stale records and incomplete access; do not silently treat unknown status as available work or rewrite the tracker during planning.

## 3. Choose a useful next step

Respect explicit priorities. Otherwise weigh current user pain, useful dependencies, a coherent user outcome, uncertainty, and the cost of finishing and verifying it. Explain the decisive tradeoff. Issue number and file order are not priority.

Recommend one item or a small related batch unless the user asks for a broader plan. Keep independent items identifiable. If implementation depends on an unresolved question, propose a bounded investigation with a question to answer and an exit condition.

## 4. Prepare the handoff

Name the item and why it should come next. Describe the intended slice, observable acceptance scenarios, material open decisions or blockers, and any established delivery endpoint. Reuse accepted decisions and adjust stale implementation assumptions to the current project. A few lines are enough; no new specification file is required.

## 5. Stop or continue as requested

For prioritization, return the recommendation and alternatives that would materially change the choice. If no item is ready, report the blocker and the next useful action.

For an explicit choose-and-implement request, select within the user's constraints and continue without asking for the same authorization again. When `full-send` or `full-stop` was requested, read that skill if available and pass it the selected item and handoff. If unavailable, report the missing skill instead of claiming to execute it. Leave implementation, review, and PR delivery to that workflow.

Update selection/status records only when included in the requested workflow. Never infer merge authority merely from selecting or implementing an item.
