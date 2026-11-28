# OpenAlbert

Персональный Telegram-бот на Go с долговременной памятью, фоновыми задачами,
браузерными инструментами, распознаванием и синтезом речи и подключением
к пользовательскому Linux-компьютеру по SSH с автоматической подготовкой исполнителя.
LLM подключается через внешний OpenAI-совместимый API или план ChatGPT.

Все операционные данные, артефакты, память и журнал событий хранятся встроенными средствами:
pure-Go SQLite (`DATA_DIR/openalbert.db`, режим WAL, `CGO_ENABLED=0`), локальное хранилище
файлов (`DATA_DIR/artifacts`), таблица логов `openalbert_logs` (хранение 30 дней, `logs_search`
работает из коробки) и встроенный Go-оркестратор. Внешние БД, брокеры очередей, MinIO
и серверы логов не требуются.

[Клиентский HTTP API](docs/client-api.md) предоставляет доступ к личному разговору,
поручениям, памяти и файлам. [Веб-инспектор](docs/inspector.md) помогает изучать
сессии и работу агента.

- [Локальная разработка и тесты](docs/development.md): `deploy/compose.local.yaml` собирает код из checkout; поддерживается запуск одного контейнера и голого статического бинарника `./openalbert`.
- [Запуск готовых образов и публикация](docs/deployment.md): `deploy/compose.yaml` получает четыре образа из GHCR (`openalbert`, `stt`, `tts`, `backup`).
- [Архитектура и документация](docs/README.md).

Проверки, тесты и сборки выполняются локально: `task check`, `task test`,
`task images` и полный прогон `task verify`. Логи и отчёты сохраняются в
`reports/local/`; `task reports` показывает последний результат.
[Команды и настройка](docs/local-checks.md). Публикация Linux AMD64/ARM64 образов
запускается отдельно через `task publish`. Фактическая скорость распознавания
речи на Raspberry Pi 5 с 64-битной ОС ещё требует замеров.

Рабочие `.env`, ключи, история, вложения и память находятся вне Git и образов.

## Структура репозитория

```text
apps/
  backend/          Основное Go-приложение: агент, встроенный оркестратор, SQLite, API, инспектор, память и задачи
services/
  backup/           Онлайн-резервное копирование SQLite и данных в S3 (`VACUUM INTO`, без Docker socket)
  stt/              Необязательный сервис распознавания речи (Nemotron, профиль `local-stt`)
  tts/              Необязательный сервис синтеза речи (Piper, профиль `local-tts`)
  computer/         Необязательный шаблон компьютера пользователя
shared/backup/      Библиотека резервного копирования для backend и backup
deploy/             Совместное развёртывание: Compose, Bake и сетевые шаблоны
tools/              Общие проверки, smoke-тесты и публикация
docs/               Документация, схемы и сайт документации
```

В папке компонента находятся его исходники, тесты, ресурсы и Dockerfile.
`deploy/` связывает компоненты в систему.
У каждого Go-компонента свой `go.mod` и, если есть внешние зависимости, свой `go.sum`.
Корневой `go.work` объединяет модули для локальной разработки. Образы и проверки
Go используют зависимости каждого модуля отдельно.

## Запуск

Команды Task выполняются из корня. Для локального запуска с встроенным STT:

```sh
docker compose --project-directory . -f deploy/compose.local.yaml up -d --build
```

`--project-directory .` сохраняет `.env`, контекст сборки и постоянные каталоги на хосте
в корне checkout: `./data` (SQLite `openalbert.db`, `artifacts/`, `knowledge/`, `chatgpt/`),
`./stt-models`, `./backup-state`, `./backup-work`, `./backup-config` и `./computer/client`.

- **Минимальный режим одного контейнера:** задай `COMPOSE_PROFILES=` и пустой `STT_BASE_URL=` в `.env` — запустится только `openalbert`.
- **Голый статический бинарник без Docker (`CGO_ENABLED=0`):** собери `CGO_ENABLED=0 go -C apps/backend build -o ../../openalbert ./cmd/openalbert` и запусти `./openalbert` с переменными `DATA_DIR`, `LLM_CONFIG_FILE`, `LLM_API_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USER_IDS` и `BROWSER_ENABLED=false` (если на хосте нет Chromium; подробности в [руководстве запуска](docs/development.md)).
- **Установка готовых образов без исходников:** скачай `deploy/compose.yaml` как `compose.yaml` в каталог установки — подробности в [руководстве публикации и установки](docs/deployment.md#установка-без-исходников).
