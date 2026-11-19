# Backend

Основное Go-приложение OpenAlbert: агент, Temporal Workflows и Activities,
Telegram, Desktop API, встроенный веб-инспектор, память и хранилища.

- `cmd/openalbert/` — точка входа и сборка зависимостей.
- `internal/` — реализация приложения, тесты и встроенные ресурсы.
- `Dockerfile` — образ приложения с Chromium.

Go-модуль общий для репозитория. Из его корня:

```sh
go run ./apps/backend/cmd/openalbert
task check
task test:go
```

Запуск требует настроенных подключений. См. [разработку](../../docs/development.md)
и [структуру репозитория](../../README.md#структура-репозитория).

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
