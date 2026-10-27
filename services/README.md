# Сервисы

Здесь находятся компоненты, обслуживающие backend: собственные сервисы
и конфигурация сторонних систем. Каждая папка содержит файлы своего компонента.

| Компонент | Содержимое |
| --- | --- |
| [backup](backup/) | Планировщик S3-копий, шифрование, ротация, восстановление и чтение статуса backend |
| [computer](computer/) | Ubuntu-образ, SSH, исполнитель команд |
| [stt](stt/) | Go HTTP-прокси, NeMo-Speech.cpp, запуск распознавания |
| [log-gateway](log-gateway/) | Go-приёмник и нормализация Docker-логов |
| [postgres](postgres/) | Инициализация баз данных |
| [temporal](temporal/) | Dynamic config, Temporal и Temporal UI |
| [minio](minio/) | Dockerfile MinIO/mc и инициализация бакета |
| [victorialogs](victorialogs/) | Описание конфигурации хранилища логов |

Совместный запуск, версии готовых образов, подключения, порты и тома задаются
в [deploy](../deploy/). Для установки без checkout конфигурация PostgreSQL
и Temporal также включена в release Compose; `task check` проверяет её соответствие
исходным файлам сервисов.
