# Модели, провайдеры и роли

[К содержанию](README.md)

OpenAlbert выбирает роль по операции приложения: `chat` — интерактивный агентский цикл Telegram и личного HTTP API; `background` — запланированные задачи и инициатива; `analysis` — итоговый разбор закрытой сессии, краткое содержание и извлечение памяти без tools; необязательная роль `coding` задаёт модель по умолчанию для ACP кодинг-агента (если не задана, используется `chat`).

## Формат `models.toml`

Скопируй `deploy/models.example.toml` в `models.toml` рядом с `.env`. Маршруты в `[routes]` указывают пару `<provider>/<model_id>`. Провайдеры объявляются в `[providers.<name>]`, а точечные переопределения параметров конкретной модели — в `[models."<provider>/<model_id>"]`.

```toml
[routes]
chat = "chatgpt/gpt-5.4"
background = "proxy/deepseek-chat"
analysis = "chatgpt/gpt-5.4-mini"
coding = "chatgpt/gpt-5.4"

[providers.proxy]
adapter = "openai_chat_completions"
base_url = "https://proxy.example.com/v1"
api_key_env = "LLM_API_KEY"
discover_models = true
include_models = ["gpt-5*", "claude-*", "deepseek-*"]
exclude_models = ["*embedding*"]
default_capabilities = ["tools", "vision", "structured_output"]
default_context_window_tokens = 128000
default_max_output_tokens = 8192
default_timeout = "120s"

[providers.proxy.headers_env]
"X-LLM-Proxy-Key" = "OPENALBERT_PROXY_KEY"

[providers.chatgpt]
adapter = "openai_chatgpt"
discover_models = true
default_capabilities = ["tools", "vision", "structured_output"]
default_context_window_tokens = 128000
default_max_output_tokens = 8192
default_timeout = "120s"

[models."proxy/deepseek-reasoner"]
display_name = "DeepSeek Reasoner"
context_window_tokens = 128000
max_output_tokens = 8192
supported_reasoning_efforts = ["low", "medium", "high"]

[models."proxy/deepseek-reasoner".options]
thinking = "enabled"
include_reasoning_content = true

[models."proxy/deepseek-reasoner".pricing]
input_per_million = 0.55
cached_input_per_million = 0.14
output_per_million = 2.19
```

## Обнаружение моделей (`/models`) и выбор в T3 Code

Когда у провайдера задано `discover_models = true`, сервер опрашивает:

- `GET {base_url}/models` для адаптера `openai_chat_completions` (с кэшированием на 5 минут и фильтрацией по `include_models` / `exclude_models`);
- каталог подписки ChatGPT (`Plan.Models`) для адаптера `openai_chatgpt`.

Если ответ `/models` не содержит лимитов контекста или возможностей, применяются значения `default_*` провайдера, а поверх них — явные переопределения из `[models."<provider>/<model>"]`.

Авторизованный клиент (`openalbert-acp`) получает итоговый список `tools`-моделей через `GET /api/v1/llm/models` и запускает потоковые запросы к выбранной модели через `POST /api/v1/llm/complete` (SSE).

## Прокси, заголовки и секреты

- `api_key_env` и `headers_env` ссылаются на имена переменных окружения (значения задаются в `.env` или `models.env`), а не на сами ключи.
- При наличии `headers_env` требуется явный `base_url`. Служебные HTTP-заголовки (`Authorization`, `Host`, и т. п.) зарезервированы.
- Для `openai_chatgpt` запрещены `api_key_env`, `default_options`/`options` и `default_pricing`/`pricing`: все подписочные модели используют единый вход `/chatgpt login`.

## Валидация и лимиты

- Таймаут (`default_timeout` / `timeout`) должен быть положительным и не больше `150s`.
- Роли `chat`, `background` и `coding` требуют возможность `tools`; роль `analysis` — `structured_output`.
- `models.toml` загружается и проверяется при старте приложения без вывода секретов в сообщениях об ошибках.
