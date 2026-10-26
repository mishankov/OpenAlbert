# PostgreSQL

`init.sql` создаёт отдельные базы Temporal при первой инициализации PostgreSQL.
Схема самого приложения принадлежит backend:
`apps/backend/internal/storage/schema.sql`.

Локальный Compose подключает `init.sql` только для чтения. Release Compose
содержит тот же SQL inline для установки без checkout; `task check` сверяет их.
Версия образа, доступы и постоянный том задаются в `deploy/compose*.yaml`.
