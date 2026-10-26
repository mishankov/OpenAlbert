# Computer

Ubuntu-среда для удалённых команд: Dockerfile, конфигурация SSH,
инициализация ключей и Python-исполнитель `runner.py`.

Backend подключается по SSH через собственный адаптер в
`apps/backend/internal/computer`. Общий smoke-тест расположен в
`tools/smoke/computer-smoke.py`.

См. [разработку и проверку Computer box](../../docs/development.md).
