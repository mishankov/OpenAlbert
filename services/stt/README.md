# Распознавание речи

- `cmd/stt/` — точка входа HTTP-прокси.
- `internal/sttserver/` — приём аудио, декодирование через FFmpeg и вызов NeMo.
- `entrypoint.py` — подготовка модели и запуск процессов.
- `Dockerfile` — сборка прокси и NeMo-Speech.cpp.

Go-сервис использует корневой модуль и не зависит от внутренних пакетов backend.
HTTP-адаптер клиента находится в `apps/backend/internal/integrations/stt.go`.
См. [голосовые сообщения](../../docs/speech.md).
