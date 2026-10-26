# UI keyboard shortcuts

- Always include a keyboard shortcut when adding a primary UI command such as Create, Save, Edit, or Delete. Prefer familiar platform conventions (for example, ⌘N for Create/New on macOS). Check existing bindings and report any unavoidable conflict instead of silently omitting the shortcut.
- Make shortcuts discoverable on buttons, in tooltips, or in menus, and expose accessibility shortcut metadata where applicable.
- Buttons and shortcuts must use the same action, respect disabled/busy states and dialogs, preserve unsaved drafts, and handle IME composition, key repeat, and non-English keyboard layouts.
- Before finishing UI work, explicitly review shortcuts and verify the main keyboard flow with the project's appropriate test harness.

# Isolated macOS development and tests

- Local desktop tests, keyboard/focus checks, native smoke and screenshot capture run in the managed Tart macOS VM. Use `python3 apps/macos/desktop_vm.py setup` once, then `doctor`, `test`, `smoke` or `screenshots`. Use `run` to open the development app inside the guest.
- Compile current sources and test binaries on the host, then execute prepared outputs in the guest. Do not invoke SwiftPM, swiftc or asset compilation inside the guest. Pure Go/Core tests and tooling checks may run on the host.
- Never fall back to the user's desktop when the VM is unavailable. `ALBERT_DESKTOP_BACKEND=host` is an explicit foreground diagnostic override and requires the user's instruction to use their desktop. Run local checks through Task; there are no hosted CI workflows.
- Keep host clipboard, microphone/audio, USB devices, personal profiles, `.env`, Git configuration and signing credentials out of the guest. Use synthetic data and disposable profiles. Microphone tests use injected devices; no real microphone recording without the user's explicit action.
- Preserve the shared VM lock across worktrees. Do not take over or stop a VM started outside this runner, and do not modify Polka's VM. Stop the owned guest on success, error, timeout and cancellation; retain diagnostics. Report incomplete or failed guest checks accurately.
