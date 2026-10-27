# Инструменты проекта

- `local.py` — общий runner проверок и публикации; каталог команд — корневой Taskfile.
- `macos_release.py` — история и публикация релизов macOS.
- `checks/` — проверки Compose и контракта Go ↔ Swift.
- `smoke/` — проверки отдельных образов и полного развёртывания.
- `testing/go.Dockerfile` — Go integration-тесты с Chromium и FFmpeg.

Запускай локальные проверки из корня через `task check`, `task test:go`,
`task test:computer`, `task test:backup`, `task test:web`, `task test:macos`, `task images` или `task verify`.
См. [полное руководство](../docs/local-checks.md).
