# Инструменты проекта

- `local.py` — общий runner проверок и публикации; каталог команд — корневой Taskfile.
- `checks/` — проверки Compose.
- `smoke/` — проверки отдельных образов и полного развёртывания.
- `testing/go.Dockerfile` — Go integration-тесты с Chromium и FFmpeg.

Запускай локальные проверки из корня через `task check`, `task test:go`,
`task test:computer`, `task test:backup`, `task test:web`, `task images` или `task verify`.
См. [полное руководство](../docs/local-checks.md).
