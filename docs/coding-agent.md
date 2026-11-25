# Кодинг-агент (`openalbert` CLI, ACP и LAN T3 Bridge)

[К содержанию](README.md)

OpenAlbert можно использовать как локального кодинг-агента в T3 Code и других клиентах с поддержкой **Agent Client Protocol (ACP)** через единый клиентский CLI `openalbert`, а также как встроенный **100%-LAN T3 Bridge** для управления потоками T3 Code с Raspberry Pi без облачных релеев и без расхода LLM-токенов.

## Архитектура

Исполнение разделено между локальной машиной и сервером OpenAlbert:

- **`openalbert` (`apps/cli`)** — единый консольный Go-бинарник без GUI. Подкоманда `openalbert acp` запускается клиентом (например, T3 Code) по stdio (JSON-RPC 2.0) и выполняет:
  - локальный агентский цикл с потоковой передачей текста и рассуждений (`agent_message_chunk`, `agent_thought_chunk`);
  - строгий **Codemode** на общем движке `shared/codemode` (QuickJS-NG в WebAssembly через `wazero`) в режиме живого stateful-исполнителя (`RunLive`);
  - локальные инструменты разработки: `fs_read`, `fs_write`, `fs_edit`, `fs_glob`, `fs_grep`, `bash`;
  - обнаружение и вызов навыков (`skill_invoke` и slash-команды ACP `available_commands_update`) из репозитория (`.agents/skills`, `.claude/skills`), домашнего каталога (`~/.agents/skills`, `~/.claude/skills`) и сервера OpenAlbert;
  - клиент локальных MCP-серверов, переданных клиентом ACP в `session/new` (`mcpServers`), с пробросом всех их инструментов в пространство `tools.*` внутри Codemode;
  - локальное сохранение сессий (`~/.local/state/openalbert/acp/sessions/`) для мгновенного `session/load`;
  - опциональный **LAN T3 Bridge** (`bridge = true` или флаг `--bridge`): удерживает синглтон-блокировку `~/.local/state/openalbert/bridge.lock`, подключается к серверу OpenAlbert по LAN (`/api/v1/t3/*`), держит живой SSE-канал команд для мгновенного `thread_launch` / `steer` / `respond` / `control`, отслеживает активные потоки без лишних запросов в простое и перехватывает служебный тик `__openalbert_bridge_tick__` без единого вызова LLM.
- **Сервер OpenAlbert (`apps/backend`)** обслуживает защищённые Bearer-токеном маршруты `/api/v1/*`:
  - `GET /api/v1/llm/models` — каталог моделей по настроенным провайдерам (`models.toml` + кэшированный `/models`);
  - `POST /api/v1/llm/complete` — потоковый SSE-шлюз к LLM с сохранением API-ключей, подписки ChatGPT, трейсов моделей и оценок стоимости на сервере;
  - `GET /api/v1/agent/context` и `POST /api/v1/agent/tools/{name}` — чтение `SOUL.md`, `USER.md`, навыков сервера и выполнение серверных инструментов владельца (`memory_*`, `task_*`, `web_*`, `browser_*`, `logs_search`, `computer_*`, `conversation_*`, `artifact_*`, `soul_*`, `group_*`, `backup_status`) без Telegram-доставки;
  - `POST /api/v1/coding/sessions`, `/sync` и `/close` — синхронизация сообщений, вложенных вызовов Codemode и локальных/MCP-трейсов в Инспектор OpenAlbert под отдельной областью `coding:<session_id>` (источник `Кодинг`).

## Анализ и память

Сессии кодинга изолированы от `personal:<owner_id>` и не отправляют сообщения в Telegram, а также исключены из фоновой инициативы (`InitiativeWorkflow`). При закрытии сессии (`POST /api/v1/coding/sessions/{id}/close`) она попадает в стандартную очередь `AnalysisWorkflow`: извлечённые устойчивые предпочтения и факты сохраняются в личной памяти владельца (`personal:<owner_id>`).

## Установка и настройка `~/.config/openalbert/config.toml`

Установите `openalbert` напрямую через `go install` (требуется Go 1.26+):

```sh
go install github.com/mishankov/OpenAlbert/apps/cli/cmd/openalbert@latest
```

Или соберите локально из клона репозитория:

```sh
go build -o ~/.local/bin/openalbert ./apps/cli/cmd/openalbert
```

Убедитесь, что `$(go env GOPATH)/bin` (или `~/.local/bin`) добавлен в `PATH`.

Создайте `~/.config/openalbert/config.toml` (права `0600`; для обратной совместимости автоматически поддерживается fallback на `~/.config/openalbert/acp.toml`):

```toml
server_url = "http://192.168.1.31:8080"
token_env = "OPENALBERT_API_TOKEN"   # или token_file = "~/.config/openalbert/token"
default_model = ""                   # необязательно; по умолчанию routes.coding / routes.chat на сервере
sync_inspector = true                # по умолчанию true

# Необязательные настройки LAN T3 Bridge:
bridge = false                       # true или флаг --bridge для включения T3 Bridge
host_label = ""                      # по умолчанию hostname машины
bridge_tick_prompt = "__openalbert_bridge_tick__"
```

Также поддерживаются переменные окружения `OPENALBERT_CONFIG` (и устаревшая `OPENALBERT_ACP_CONFIG`), `OPENALBERT_SERVER_URL`, `OPENALBERT_API_TOKEN` и `OPENALBERT_BRIDGE`.

Для подключения в T3 Code добавьте ACP-провайдер с командой `openalbert acp` (или `openalbert acp --bridge` для включения моста).
