# Log gateway

`cmd/log-gateway/` запускает HTTP-сервер; `internal/logpipe/` содержит
нормализацию Docker Splunk events и тесты. Dockerfile собирает отдельный образ.
Go-модуль находится в корне репозитория.

Адрес назначения VictoriaLogs и порты задаются при совместном развёртывании.
См. [логирование](../../docs/observability.md).
