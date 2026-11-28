# Backend

Основное Go-приложение OpenAlbert: агент, встроенный оркестратор,
SQLite-хранилище, локальное хранилище артефактов, встроенный журнал событий,
Telegram, Desktop API, встроенный веб-инспектор и файловая память.

- `cmd/openalbert/` — точка входа и сборка зависимостей.
- `internal/` — реализация приложения, тесты и встроенные ресурсы.
- `Dockerfile` — автономный образ приложения с Chromium.

У backend свой Go-модуль. Из корня репозитория:

```sh
go run ./apps/backend/cmd/openalbert
task check
task test:go
```

Для минимального запуска бинарника достаточно `.env` (или переменных окружения
`LLM_CONFIG_FILE`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USER_IDS`) и `models.toml`.
Все локальные данные сохраняются в `DATA_DIR` (по умолчанию `./data`).
См. [разработку](../../docs/development.md) и [структуру репозитория](../../README.md#структура-репозитория).

### Telegram widget fallback

`TELEGRAM_WIDGETS=auto` (default) tries widgets and caches confirmed Bot API
unavailability (`INPUT_MEDIA_WIDGET_NOT_ALLOWED` or a missing API method) for one
hour per running bot transport. The cache is in memory and resets on restart.
`on` probes on every new send, bypassing this cache; `off` sends the accessible
text caption and schedule buttons directly. All modes preserve private-chat
checks, validation, delivery deduplication, and rate-limit retries.

A delivered text fallback returns `fallback=true`, `fallback_reason`
(`api_not_supported`, `api_rejected`, `validation`, or `disabled`) and, for API
rejections, `api_error` with HTTP status, Telegram error code and bounded
description. A 429 remains a retry rather than a text fallback; timeouts and
5xx outcomes are never replaced automatically because they may have delivered.
`telegram.widget.send` logs API attempts, rejection diagnostics and raw/compressed
HTML byte counts without logging HTML or state. A cached rejection retains the
original diagnostic. Ordinary bots may be excluded from Telegram's staged widget
rollout even in private chats; changing HTML does not resolve that restriction.
