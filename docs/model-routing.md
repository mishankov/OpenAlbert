# Модели и роли

[К содержанию](README.md)

OpenAlbert выбирает роль по операции приложения. `chat` — интерактивный агентский цикл Telegram и macOS, включая группы; `background` — запланированные задачи и инициатива; `analysis` — итоговый разбор закрытой сессии, краткое содержание и извлечение памяти без tools. Задача «проанализировать логи» остаётся `background`: ключевые слова не меняют роль.

## TOML

Скопируй `deploy/models.example.toml` в `models.toml` рядом с `.env` и задай параметры своей модели. Приложению обязателен `LLM_CONFIG_FILE`; Compose выставляет `/etc/openalbert/models.toml` и подключает файл хоста из `LLM_CONFIG_HOST_FILE` (по умолчанию `./models.toml`) только для чтения. Вне Docker укажи `LLM_CONFIG_FILE=./models.toml`.

```toml
[routes]
chat = "everyday"
background = "everyday"
analysis = "economical"

[profiles.everyday]
adapter = "openai_chat_completions"
base_url = "https://api.example.com/v1"
model = "your-main-model"
api_key_env = "LLM_API_KEY"
capabilities = ["tools", "vision", "structured_output"]
context_window_tokens = 128000
max_output_tokens = 8192
timeout = "120s"

[profiles.economical]
adapter = "openai_chat_completions"
base_url = "https://api.example.com/v1"
model = "your-analysis-model"
api_key_env = "ANALYSIS_LLM_API_KEY"
capabilities = ["structured_output"]
context_window_tokens = 64000
max_output_tokens = 4096
timeout = "60s"
```

Имена профилей произвольные. Все три роли обязательны, каждая ссылается на один профиль. Несколько ролей могут использовать один профиль, в том числе одну модель с разными настройками. Лимиты и ID в примере иллюстративные: проверь их по контракту выбранного endpoint.

Значение `api_key_env` — имя переменной окружения процесса, не сам ключ. Compose передаёт `LLM_API_KEY` из `.env`. Для дополнительных имён создай рядом необязательный `models.env`:

```dotenv
ANALYSIS_LLM_API_KEY=your-analysis-api-key
```

Затем задай `chmod 600 models.env`. Оба Compose-файла читают этот файл через `env_file`; отдельный override не нужен. Помещай в него дополнительные переменные ключей, а `LLM_API_KEY` оставь в `.env`: явные значения `environment` имеют приоритет. Вне Docker загрузи эти переменные в окружение процесса перед запуском.

`models.toml` и `models.env` исключены из Git и Docker build context. Автоматический backup сохраняет TOML из bind-монтирования и, если существует, точные байты `models.env` внутри зашифрованного архива. Файлы должны находиться внутри каталога установки без символических ссылок. Внешние пути TOML нужно резервировать отдельно или перенести файл внутрь установки. Для необязательного `env_file` нужен Docker Compose 2.24.0 или новее.

## Возможности и настройки провайдера

- `tools` требуется профилям `chat` и `background`.
- `vision` требуется, когда запрос действительно содержит изображения. Возможность не выдаёт новые инструменты или права.
- `structured_output` требуется `analysis`; для этой роли адаптер запрашивает JSON-объект.

Адаптер проверяет совместимость запроса перед HTTP-вызовом. Настройки endpoint задаются явно, без распознавания модели по имени или провайдера по хосту:

```toml
[profiles.everyday.options]
reasoning_effort = "high"
thinking = "enabled"
include_reasoning_content = true
session_header = "x-opencode-session"
output_token_field = "max_tokens"
```

Таймаут профиля должен быть положительным и не больше `150s`: оставшееся время трёхминутной Temporal Activity нужно для сохранения результата.

Включай только поддерживаемые провайдером поля. `include_reasoning_content` возвращает сохранённое служебное reasoning в историю ассистента для DeepSeek thinking с tools. `session_header` задаёт поддерживаемый заголовок OpenCode Go; его значение — непрозрачный ID разговора. `output_token_field` выбирает `max_tokens` (по умолчанию) или `max_completion_tokens`. Эти настройки не обязательно нужны вместе.

Необязательная таблица `profiles.<имя>.pricing` принимает `input_per_million`, `cached_input_per_million`, `output_per_million`: неотрицательные цены в USD за миллион токенов. Если таблица задана, нужны все три значения. Без неё стоимость неизвестна; показанная оценка не заменяет биллинг провайдера.

## ChatGPT plan и API key

Существующий `OPENAI_AUTH_MODE` выбирает billing lane перед API-маршрутизатором. В `api_key` используются профили ролей из TOML. В `siwc` сохраняется модель ChatGPT, выбранная через `/chatgpt model`; API-ключи профилей могут быть пустыми. `auto` сохраняет существующий переход с ChatGPT plan на API при предусмотренных ошибках; для API lane нужны ключи профилей. Это не fallback между TOML-профилями.

`OPENAI_SIWC_MODEL` по-прежнему задаёт начальную модель ChatGPT. Если он пуст, используется model ID профиля `chat`. Существующие команды и Desktop API настройки ChatGPT продолжают работать. В API lane расчёт контекста анализа проходит через billing wrapper к выбранному профилю.

## Исполнение и ошибки

TOML загружается и проверяется при старте. Неизвестные поля, отсутствующие роли, неизвестные профили, неподдерживаемые возможности и некорректные лимиты приводят к ошибке запуска. Маршрута по умолчанию, fallback на другую модель, переопределения через `/model` или параметры задачи нет. Для применения изменений пересоздай приложение.

Каждый вызов учитывает окно контекста профиля и резерв ответа, включая историю, instructions, tools и изображения. Используется консервативная оценка, а не точный tokenizer провайдера. Удаляются только целые старые ходы; текущий запрос и его обмены с tools сохраняются. Если обязательная часть не помещается, вызов завершается понятной ошибкой. Отдельный байтовый лимит Temporal продолжает действовать.

Телеметрия и результат вызова показывают роль, профиль, фактическую модель, токены и при заданных ценах оценку стоимости. Возврат сохранённого checkpoint не означает новый платный запрос. Ошибки не переключают модель.
