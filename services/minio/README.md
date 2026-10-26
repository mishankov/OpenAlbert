# MinIO

`Dockerfile` собирает закреплённые upstream-версии MinIO и клиента `mc`.
Цели `minio` и `minio-init` создают два образа; `init.sh` повторно безопасно
создаёт бакет и настраивает доступ.

Порты, credentials, бакет и постоянный том задаются в `deploy/compose*.yaml`.
См. [сборку и публикацию образов](../../docs/deployment.md).
