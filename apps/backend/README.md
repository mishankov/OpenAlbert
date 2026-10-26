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
