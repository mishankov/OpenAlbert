---
name: capture-issue
description: Use when capturing a new request, deferring an improvement discovered during implementation, or preserving unfinished work in the backlog.
---

# Capture an issue

## 1. Find the backlog

Use the tracker or backlog established by the project's instructions and conversation. Resolve a material ambiguity about where the item belongs before writing. Keep one canonical record.

## 2. Check for existing work

Search for the same problem or outcome, including linked records if work moved between trackers. Extend a matching item with new information; if it already covers the request, leave it unchanged and report **Already tracked**. Link a distinct follow-up or regression to its related item. Preserve existing identities and references.

## 3. Write the item

Include a clear title, the problem, and the desired outcome. Add the supplied example or evidence. For a bug, include the known reproduction and expected versus observed behavior.

Add acceptance points, dependencies, and priority when already established. Preserve the user's scope and mark material unknowns as questions. Do not invent an implementation plan or delay capture to resolve every detail.

## 4. Save and verify

Create or update the item, then read it back. Use tracker-generated IDs where available. Before assigning an ID in a file backlog, check its current shared version and known concurrent additions; resolve collisions without reusing another item's identity.

For a local edit, verify the repository, path, and branch. Follow the session's Git scope. If the entry exists only in a worktree, state that integration or push remains pending.

## 5. Return the result

Report **Created**, **Updated**, **Already tracked**, or **Blocked**, with the issue URL or repository/path and entry ID when available. Include any pending integration or concrete blocker. Claim success only after verification.

Stop after capture unless the request also includes further work; then continue within its established scope.
