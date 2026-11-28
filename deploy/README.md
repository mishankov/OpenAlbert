# Развёртывание

Этот каталог связывает приложения и сервисы в систему:

- `compose.local.yaml` — сборка и запуск из checkout.
- `compose.yaml` — запуск готовых образов без исходников.
- `compose.test.yaml` — одноразовая среда Go-тестов.
- `models.example.toml` — пример профилей LLM и привязок ролей.
- `backup.example.json` / `backup-s3.example.json` — примеры конфигурации резервного копирования.
- `docker-bake.hcl` — общий каталог сборок образов.
- `networking/` — примеры WireGuard и Caddy для клиентского HTTP API.

Из корня репозитория:

```sh
cp deploy/models.example.toml models.toml
# Задай модель и возможности в models.toml, ключ в .env.
docker compose --project-directory . -f deploy/compose.local.yaml up -d --build
```

Все пути Compose отсчитываются от корня checkout через `--project-directory .`.
Состояние OpenAlbert (SQLite-база `openalbert.db`, локальные артефакты `artifacts/`,
знания `knowledge/` и токены `chatgpt/`) хранится в едином каталоге `data/`.
Task задаёт `--project-directory .` автоматически для тестового Compose.

При установке без исходников скачай `compose.yaml` из этого каталога в каталог
установки рядом с `.env` и настроенным `models.toml`. Дополнительные ключи
профилей задаются в необязательном `models.env`, который также резервируется. После этого работают обычные `docker compose up -d`
и другие команды без дополнительных параметров. Для минимальной конфигурации без
локального STT убери `local-stt` из `COMPOSE_PROFILES` — запустится один контейнер `openalbert`.

Исходники, Dockerfiles и конфигурация отдельных компонентов находятся
в `apps/` и `services/`; проверки — в `tools/`.

Компьютер не входит в этот стек или каталог образов. Его создаёт и обслуживает
пользователь отдельно; см. [создание и перенос](../docs/computer.md).
