# UI keyboard shortcuts

- Always include a keyboard shortcut when adding a primary UI command such as Create, Save, Edit, or Delete. Prefer familiar platform conventions (for example, ⌘N for Create/New on macOS). Check existing bindings and report any unavoidable conflict instead of silently omitting the shortcut.
- Make shortcuts discoverable on buttons, in tooltips, or in menus, and expose accessibility shortcut metadata where applicable.
- Buttons and shortcuts must use the same action, respect disabled/busy states and dialogs, preserve unsaved drafts, and handle IME composition, key repeat, and non-English keyboard layouts.
- Before finishing UI work, explicitly review shortcuts and verify the main keyboard flow with the project's appropriate test harness.
