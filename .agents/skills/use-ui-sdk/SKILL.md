---
name: use-ui-sdk
description: Use when implementing or changing web UI components, screens, forms, or interactions, or when the user requests UI SDK.
---

# Use UI SDK for web UI

Use [UI SDK](https://github.com/vercel-labs/ui) for interactive components and
behaviors. Keep the project's visual design and the requested scope.

## 1. Find the integration

Check the app's framework, package manager, existing components, and installed
`@ui-sdk/*` packages. Reuse existing UI SDK integration. When adding it, install
`@ui-sdk/core` and the matching adapter with the project's package manager:
`@ui-sdk/react`, `@ui-sdk/vue`, `@ui-sdk/svelte`, `@ui-sdk/solid`, or `@ui-sdk/web`.
Keep versions compatible with the app. If the framework is unsupported or a
project constraint prevents adoption, report the concrete limitation; keep the
work scoped rather than migrating the app.

For a plain HTML page without a build step, follow
[the Web Components guide](https://ui-sdk.dev/web.md) for its script and stylesheet
setup instead of adding a package toolchain.

## 2. Load the implementation guidance

Read `node_modules/@ui-sdk/core/skills/ui-sdk/SKILL.md` and follow its relevant
references. This is the SDK's own skill, matched to the installed version.
Read `node_modules/@ui-sdk/<framework>/docs/llms.txt`, then only the component
and behavior pages needed for the task. `<framework>` is `react`, `vue`, `svelte`,
`solid`, or `web`. For a page without a build step, use the Web Components guide.

If the packaged skill or docs are unavailable, consult the
[official skill](https://github.com/vercel-labs/ui/blob/main/packages/core/skills/ui-sdk/SKILL.md)
and [documentation](https://ui-sdk.dev/docs), checking compatibility with the
installed version before using an API.

Use shipped components for supported interactions. Compose core behaviors when
a required component is missing. Keep existing working components outside the
requested change intact.

## 3. Verify the interaction

Give each primary command a discoverable platform-appropriate shortcut. Check
existing bindings and report unavoidable conflicts. Buttons and shortcuts must
share an action and respect disabled and busy states, dialogs, and unsaved drafts.
Handle IME composition, key repeat, and non-English keyboard layouts. Expose
accessibility shortcut metadata where applicable.

Run the project's relevant checks and verify the main keyboard flow with its UI
test harness, including focus, dismissal, accessible names, and the changed
interaction. Report what was verified and any checks that could not run.
