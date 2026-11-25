# Кодинг-агент (ACP и `openalbert-acp`)

[К содержанию](README.md)

OpenAlbert можно использовать как локального кодинг-агента в T3 Code и других клиентах с поддержкой **Agent Client Protocol (ACP)**.

## Архитектура

Исполнение разделено между локальной машиной и сервером OpenAlbert:

- **`openalbert-acp` (`apps/acp`)** — консольный Go-бинарник без GUI. Запускается клиентом (например, T3 Code) по stdio (JSON-RPC 2.0) и выполняет:
  - локальный агентский цикл с потоковой передачей текста и рассуждений (`agent_message_chunk`, `agent_thought_chunk`);
  - строгий **Codemode** на общем движке `shared/codemode` (QuickJS-NG в WebAssembly через `wazero`) в режиме живого stateful-исполнителя (`RunLive`);
  - локальные инструменты разработки: `fs_read`, `fs_write`, `fs_edit`, `fs_glob`, `fs_grep`, `bash`;
  - обнаружение и вызов навыков (`skill_invoke` и slash-команды ACP `available_commands_update`) из репозитория (`.agents/skills`, `.claude/skills`), домашнего каталога (`~/.agents/skills`, `~/.claude/skills`) и сервера OpenAlbert;
  - клиент локальных MCP-серверов, переданных клиентом ACP в `session/new` (`mcpServers`), с пробросом всех их инструментов в пространство `tools.*` внутри Codemode;
  - локальное сохранение сессий (`~/.local/state/openalbert/acp/sessions/`) для мгновенного `session/load`.
- **Сервер OpenAlbert (`apps/backend`)** обслуживает защищённые Bearer-токеном маршруты `/api/v1/*`:
  - `GET /api/v1/llm/models` — каталог моделей по настроенным провайдерам (`models.toml` + кэшированный `/models`);
  - `POST /api/v1/llm/complete` — потоковый SSE-шлюз к LLM с сохранением API-ключей, подписки ChatGPT, трейсов моделей и оценок стоимости на сервере;
  - `GET /api/v1/agent/context` и `POST /api/v1/agent/tools/{name}` — чтение `SOUL.md`, `USER.md`, навыков сервера и выполнение серверных инструментов владельца (`memory_*`, `task_*`, `web_*`, `browser_*`, `logs_search`, `computer_*`, `conversation_*`, `artifact_*`, `soul_*`, `group_*`, `backup_status`) без Telegram-доставки;
  - `POST /api/v1/coding/sessions`, `/sync` и `/close` — синхронизация сообщений, вложенных вызовов Codemode и локальных/MCP-трейсов в Инспектор OpenAlbert под отдельной областью `coding:<session_id>` (источник `Кодинг`).

## Анализ и память

Сессии кодинга изолированы от `personal:<owner_id>` и не отправляют сообщения в Telegram, а также исключены из фоновой инициативы (`InitiativeWorkflow`). При закрытии сессии (`POST /api/v1/coding/sessions/{id}/close`) она попадает в стандартную очередь `AnalysisWorkflow`: извлечённые устойчивые предпочтения и факты сохраняются в личной памяти владельца (`personal:<owner_id>`).

## Установка и настройка `~/.config/openalbert/acp.toml`

Установите `openalbert-acp` напрямую через `go install` (требуется Go 1.26+):

```sh
go install github.com/mishankov/OpenAlbert/apps/acp/cmd/openalbert-acp@latest
```

Или соберите локально из клона репозитория:

```sh
go build -o ~/.local/bin/openalbert-acp ./apps/acp/cmd/openalbert-acp
```

Убедитесь, что `$(go env GOPATH)/bin` (или `~/.local/bin`) добавлен в `PATH`.

Создайте `~/.config/openalbert/acp.toml` (права `0600`):

```toml
server_url = "https://albert.example.com"
token_env = "OPENALBERT_API_TOKEN"   # или token_file = "~/.config/openalbert/token"
default_model = ""                   # необязательно; по умолчанию routes.coding / routes.chat на сервере
sync_inspector = true                # по умолчанию true
```

Также поддерживаются переменные окружения `OPENALBERT_ACP_CONFIG`, `OPENALBERT_SERVER_URL` и `OPENALBERT_API_TOKEN`.

Для подключения в T3 Code добавьте ACP-провайдер с командой `openalbert-acp`.
