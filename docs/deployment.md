# Готовые образы и публикация

[К содержанию](README.md)

Есть два независимых способа запуска:

- `compose.local.yaml` — локальная разработка: собирает шесть собственных образов из исходников.
- `compose.yaml` — установка готовых образов: не содержит `build` и не требует checkout.

Обычный `docker compose up -d` использует готовые образы. Для локальной разработки
всегда указывай `-f compose.local.yaml`, например `docker compose -f compose.local.yaml up -d --build`.

Оба варианта используют одинаковые сервисы, настройки, каталоги данных и локальные порты.
Их соответствие проверяет `python3 deploy/check-compose.py`. Не запускай оба варианта
одновременно с одним Telegram-токеном или общими каталогами данных.

## Какие образы собираем

- **`openalbert`** — Go-приложение, инспектор и Chromium из Alpine.
- **`computer`** — Ubuntu 24.04, SSH, Python, Git и пользователь `albert` с `sudo` без пароля.
  Этот же образ запускает одноразовый `computer-init` для создания ключей.
- **`stt`** — NeMo-Speech.cpp, наш Go-адаптер, FFmpeg и bootstrap. Веса скачиваются
  при запуске и хранятся отдельно в `container_data/stt`; в образ не включаются.
- **`log-gateway`** — наш приёмник и маршрутизатор Docker-логов.
- **`minio`** — неизменённые исходники MinIO, закреплённые на коммите
  `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` (`RELEASE.2025-10-15T17-29-55Z`).
- **`minio-init`** — клиент `mc` из коммита
  `7394ce0dd2a80935aded936b09fa12cbb3cb8096` (`RELEASE.2025-08-13T08-35-41Z`)
  и наш скрипт создания бакета. Скрипт включён в образ и допускает повторный запуск.

Собственные сборки MinIO и `mc` нужны, поскольку прежние публичные образы не
получаются из реестров. Они собираются из исходников без зависимости от этих образов.
Закреплённые исходники доступны по ссылкам в `deploy/minio.Dockerfile`; лицензии
AGPL и credits клиента включены в соответствующие образы. Это сборки сторонних
компонентов, их обновление нужно сопровождать отдельно. Версия сервера новее
прежнего `RELEASE.2025-04-22T22-12-26Z`: перед переносом существующих данных сделай
резервную копию и проверь обновление на копии хранилища.

**Стандартными оставляем** `postgres:17.9-alpine`, `temporalio/auto-setup:1.29.1`,
`temporalio/ui:2.54.1` и `victoriametrics/victoria-logs:v1.53.0`.
Собственного кода в них нет; SQL и dynamic config передаются средствами Compose,
поэтому отдельные сборки этих сервисов не нужны. Все четыре образа имеют ARM64-варианты.
`auto-setup` соответствует текущей установке с одним сервером Temporal; отказоустойчивый
кластер и отдельный процесс миграций эта конфигурация не настраивает.

## Что делает GitHub Actions

Workflow `.github/workflows/ci.yml` работает на pull request, push в `master` и при
ручном запуске. Публикация разрешена только для запуска на ветке `master`, кроме PR.

Проверки выполняются до сборки образов:

1. Проверка workflow через actionlint и обоих Compose: соответствие сервисов,
   обязательные переменные, отсутствие исходников на сервере, отключённый/внешний STT.
2. `gofmt`, `go vet`, `go test -race` с временными PostgreSQL и VictoriaLogs,
   настоящим Chromium и FFmpeg. Проверяется и реальный контракт поиска логов.
   Telegram, платные LLM и скачивание весов для тестов не нужны.
3. `npm ci`, тесты и сборка документации, Playwright-тесты документации и инспектора,
   включая существующие проверки сочетаний клавиш, IME и русской раскладки.

Затем каждый из шести образов собирается отдельно на нативных Linux AMD64 и ARM64
runners. Образы проходят smoke-проверки; для `minio-init` дополнительно запускаются
временный MinIO, повторное создание бакета и реальный S3-контракт OpenAlbert.
Для `computer` проверяется реальный SSH, sudo, обмен файлами, отмена, дедупликация
команд и сохранение состояния после пересоздания контейнера.
Проверка STT запускает native CLI без скачивания модели; качество распознавания
и расход памяти требуют отдельного эксперимента с весами. Кэши сборки разделены
по компоненту и архитектуре.

После сборок CI поднимает опубликованный вариант Compose с пустыми каталогами
данных и подставными Telegram/LLM API. Проверяются инициализация PostgreSQL и
Temporal, фоновая задача, загрузка вложения в Ubuntu, обработка с sudo и сохранение
результата в S3, Markdown-память, инспектор и
путь Docker → log-gateway → VictoriaLogs. После перезапуска приложения проверяются
сохранённый Telegram offset и отсутствие повторной обработки и отправки ответов.
Общие SHA- и `master`-теги публикуются только после успешной проверки этого запуска.

В PR образы не публикуются, вход в GHCR не выполняется. Используется событие
`pull_request`, поэтому код из fork не получает токен для записи пакетов.
После успешных проверок и smoke-тестов на `master` загружаются образы отдельных
архитектур, затем создаются общие AMD64/ARM64-манифесты:

```text
ghcr.io/<owner>/<repository>/openalbert:sha-<полный SHA коммита>
ghcr.io/<owner>/<repository>/computer:sha-<полный SHA коммита>
ghcr.io/<owner>/<repository>/stt:sha-<полный SHA коммита>
ghcr.io/<owner>/<repository>/log-gateway:sha-<полный SHA коммита>
ghcr.io/<owner>/<repository>/minio:sha-<полный SHA коммита>
ghcr.io/<owner>/<repository>/minio-init:sha-<полный SHA коммита>
```

У каждого также есть обновляемый тег `master`. Для воспроизводимой установки
выбирай `sha-<полный SHA>` от полностью успешного workflow. Публикация нескольких
пакетов не атомарна; при сбое последнего этапа перезапусти неуспешные jobs и
дождись успеха всего workflow. На сервер ничего автоматически не развёртывается.

Namespace берётся из `github.repository` и переводится в нижний регистр, поэтому
workflow не привязан к конкретному владельцу. Репозиторий проекта —
[mishankov/OpenAlbert](https://github.com/mishankov/OpenAlbert), базовый путь образов —
`ghcr.io/mishankov/openalbert`. Пакеты появятся после первой успешной публикации workflow.

## Настройка GitHub

1. Опубликуй репозиторий с основной веткой `master`, включи Actions и разреши
   используемые actions в настройках организации, если там действует allowlist.
2. Разреши workflow создавать packages. Публикация использует встроенный
   `GITHUB_TOKEN` с `packages: write`; отдельный PAT для CI не нужен.
3. После первой публикации проверь видимость **каждого** GHCR package: публичный
   GitHub-репозиторий сам по себе не делает package публичным. Для анонимного
   `docker compose pull` пакеты должны быть public. Для закрытых пакетов нужен
   `docker login ghcr.io` на сервере с доступом на чтение.
4. В защите `master` включи обязательные проверки workflow. Нативные ARM64 jobs
   используют runner `ubuntu-24.04-arm`; он должен быть доступен для репозитория.
   При его недоступности потребуется ARM64 runner или настройка сборок через QEMU.

## Установка без исходников

Нужны Docker Engine, Docker Compose **2.23.1 или новее**, Linux AMD64 или ARM64,
свободные порты и постоянный диск. Inline configs требуют указанной версии Compose.
Для Raspberry Pi выбирай 64-битную ОС и SSD для данных; скорость полного стека,
включая STT и параллельные браузеры, на Pi пока не проверена.

Сохрани рядом `compose.yaml` и `.env.release.example` из выбранного коммита:

```sh
cp .env.release.example .env
mkdir -p container_data/knowledge
sudo chown 10001:10001 container_data/knowledge
```

В `.env` укажи `OPENALBERT_IMAGE_PREFIX=ghcr.io/<owner>/<repository>` в нижнем регистре
и `OPENALBERT_IMAGE_TAG=sha-<полный SHA коммита>` либо `master`. Заполни Telegram token,
разрешённые ID, настройки LLM и собственные PostgreSQL/S3 credentials.
`POSTGRES_PASSWORD` должен быть URL-safe: он подставляется в URL подключения.
Например, случайный пароль в hex не требует дополнительного URL-кодирования.
Пустые обязательные значения Compose отвергает до создания контейнеров.

```sh
docker compose config -q
docker compose pull
docker compose up -d
docker compose ps
```

Примеры настроек включают локальный STT через `COMPOSE_PROFILES=local-stt`.
Первый запуск скачивает около 742 МБ весов и прогревает модель. Бот может
запуститься раньше готовности STT; перед голосовыми сообщениями дождись `healthy`.
Для отключения распознавания задай `COMPOSE_PROFILES=` и `STT_BASE_URL=`.
Для внешнего STT оставь `COMPOSE_PROFILES=` и укажи его `STT_BASE_URL`, модель и ключ.
При изменении уже работающей установки останови локальный STT командой
`docker compose --profile local-stt stop stt`, затем выполни `docker compose up -d`.
Одна смена URL не отключает профиль; `--remove-orphans` не останавливает сервисы
из отключённых профилей.

Данные сохраняются в `container_data`: PostgreSQL, MinIO, VictoriaLogs, STT, Markdown-память и компьютер (ключи, домашний
каталог и рабочие файлы). `computer-init` автоматически подготавливает SSH; порт
компьютера доступен только внутри выделенной Compose-сети. Подробности и параметры —
[Ubuntu-компьютер](development.md#ubuntu-компьютер-альберта).
Инициализация баз Temporal выполняется только на пустом PostgreSQL data directory;
при переносе существующей установки нужны все три базы, вложения и память.
Смена `POSTGRES_PASSWORD` в `.env` не меняет пароль уже созданного пользователя БД.
Порты опубликованы только на `127.0.0.1`; удалённый доступ к инспектору и служебным
интерфейсам можно организовать SSH-туннелем. Telegram использует исходящий polling,
проброс входящего порта для бота не нужен.

Например, `ssh -N -L 8080:127.0.0.1:8080 user@server` открывает инспектор на
`http://localhost:8080/inspect/`. Если используешь другой локальный порт, задай
соответствующий `INSPECTOR_PUBLIC_URL`, чтобы ссылки бота вели на доступный адрес.

Для прямого доступа из доверенной LAN замени публикацию порта сервиса `openalbert`
на `"192.168.1.31:8080:8080"`, указав LAN-адрес своего сервера, и пересоздай
приложение: `docker compose up -d --no-deps --force-recreate openalbert`.
`INSPECTOR_PUBLIC_URL=http://192.168.1.31:8080/inspect/` задаёт только адрес ссылок.
Проверок Host/Origin и авторизации в приложении нет; доступ регулируется публикацией
порта в Compose. Подробности — [инспектор](inspector.md).

## Обновление установки

Сделай резервную копию данных, возьми Compose и пример настроек из нужного коммита,
задай общий SHA-тег и выполни:

```sh
docker compose pull
docker compose up -d
```

Не выполняй параллельно `up` из локального Compose. Возврат к старым образам
возможен только с учётом совместимости схемы БД и формата данных MinIO; для
обратного несовместимого изменения нужна соответствующая резервная копия.

## Резервная копия и восстановление

Для небольшой установки подходит холодная копия всего каталога: остановка
сервисов сохраняет PostgreSQL, Temporal и S3 в согласованном состоянии. Архив
содержит `.env` с ключами; храни его вне публичного репозитория и сервера приложения.
На Linux из каталога установки:

```sh
docker compose stop
sudo tar --numeric-owner -czf "../openalbert-backup-$(date +%Y%m%d-%H%M%S).tar.gz" .env compose.yaml container_data
docker compose up -d
```

Перед восстановлением останови прежнюю установку. Распакуй архив в пустой каталог
на Linux, сохраняя владельцев файлов, затем запусти ту же версию образов:

```sh
sudo tar --numeric-owner -xzf /path/to/openalbert-backup.tar.gz
docker compose config -q
docker compose pull
docker compose up -d
docker compose ps
```

Закрепляй SHA-тег перед созданием копии: `master` может указывать на более новые
образы. Периодическое создание копий, их хранение вне сервера и проверку
восстановления нужно настроить для конкретной установки.

## Локальные проверки CI

```sh
python3 deploy/check-compose.py
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
go vet ./...
go test -race ./...
npm ci --prefix docs
npm test --prefix docs
npm run build --prefix docs
```

Браузерные и интеграционные проверки требуют Chromium, FFmpeg и отдельных
временных PostgreSQL/S3, как в workflow. Инструкции разработки остаются в
[development.md](development.md).

Для сквозной проверки установки собери четыре образа и запусти тест:

```sh
docker build -t openalbert-ci:openalbert .
docker build -t openalbert-ci:log-gateway -f deploy/logging.Dockerfile .
docker build -t openalbert-ci:minio --target minio -f deploy/minio.Dockerfile .
docker build -t openalbert-ci:minio-init --target minio-init -f deploy/minio.Dockerfile .
python3 deploy/release-smoke.py
```

Для существующих локальных образов можно передать `--image компонент=тег`.
Проверка использует отдельный Compose project, временные данные и случайные
локальные порты; реальные ключи и `.env` проекта не читает. Веса STT не скачиваются.

Если локальные серверы документации или инспектора уже занимают тестовые порты,
задай `DOCS_TEST_PORT` и `INSPECTOR_TEST_PORT`. Это позволяет проверять текущий
checkout отдельными процессами, не используя сервер другого рабочего дерева.

Официальные справочники: [публикация в GHCR](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images),
[сборки нескольких архитектур](https://docs.docker.com/build/ci/github-actions/multi-platform/),
[inline configs Compose](https://docs.docker.com/reference/compose-file/configs/).
