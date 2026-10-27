# Развёртывание

Этот каталог связывает приложения и сервисы в систему:

- `compose.local.yaml` — сборка и запуск из checkout.
- `compose.yaml` — запуск готовых образов без исходников.
- `compose.test.yaml` — одноразовая среда Go integration-тестов.
- `docker-bake.hcl` — общий каталог сборок образов.
- `networking/` — примеры WireGuard и Caddy для Desktop API.

Из корня репозитория:

```sh
docker compose --project-directory . -f deploy/compose.local.yaml up -d --build
```

Все пути Compose отсчитываются от корня checkout через `--project-directory .`.
Это сохраняет `.env` и `container_data/` на прежнем месте. Task задаёт этот
параметр автоматически для тестового Compose.

При установке без исходников скачай `compose.yaml` из этого каталога в каталог
установки рядом с `.env`. После этого работают обычные `docker compose up -d`
и другие команды без дополнительных параметров.

Исходники, Dockerfiles и конфигурация отдельных компонентов находятся
в `apps/` и `services/`; проверки — в `tools/`.

Компьютер не входит в этот стек или каталог образов. Его создаёт и обслуживает
пользователь отдельно; см. [создание и перенос](../docs/computer.md).
