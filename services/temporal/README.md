# Temporal

`dynamicconfig.yaml` содержит динамическую конфигурацию сервера Temporal.
Локальный Compose подключает файл только для чтения; release Compose содержит
его inline. `task check` проверяет совпадение.

Образы Temporal и Temporal UI, подключение к PostgreSQL, namespace и порты
задаются в `deploy/compose*.yaml`. Workflows и Activities приложения принадлежат
`apps/backend/internal/orchestration` и `apps/backend/internal/agent`.
