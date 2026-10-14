# Спецификация: CLI агента (MVP)

**Статус:** черновик. CLI — тонкий клиент к локальному API агента (unix-сокет / named pipe). Все команды поддерживают `--json`.

## Установка и служба

| Команда | Описание |
|---|---|
| `docklet install` | Регистрирует агента как службу, ставит privileged helper, запускает preflight. Каждое системное изменение — с подтверждением |
| `docklet uninstall [--purge]` | Удаляет службу, helper, DNS-настройки, CA из всех хранилищ; `--purge` — ещё и данные окружений (с подтверждением) |
| `docklet doctor [--fix]` | Preflight-проверки (Docker, порты 80/443 v4/v6, DNS, CA, пулы адресов, reflink, ресурсы) с владельцами конфликтов и предложениями исправлений |
| `docklet status` | Агент, runtime, режим портов (default / сосуществование / fallback), окружения |
| `docklet ui` | Открыть локальную панель |

## Проект

| Команда | Описание |
|---|---|
| `docklet init [--from-compose <file>]` | Создать `workspace.yaml`; импорт compose с отчётом |
| `docklet validate` | Проверка по JSON Schema и правилам + антипаттерны |
| `docklet export compose [-o file]` | Экспорт в `docker-compose.yml` |
| `docklet up` | Поднять базовое окружение (clone репозиториев, данные, хуки) |
| `docklet down [--volumes]` | Остановить базовое окружение |
| `docklet open [env]` | Открыть URL в браузере |

## Фичи

| Команда | Описание |
|---|---|
| `docklet feature create <name> --modules a,b [--branch …] [--from-task URL]` | Создать фичу; печатает подсказку про общие ресурсы по графу `uses` |
| `docklet feature add <name> <module>` | Добавить модуль в фичу |
| `docklet feature list` | Фичи, фазы, URL, ресурсы, общие/собственные данные |
| `docklet feature sleep <name>`, `docklet feature wake <name>` | Ручное управление |
| `docklet feature rm <name> [--force]` | Удалить; без `--force` откажет при незакоммиченных/незапушенных изменениях |
| `docklet feature path <name> [module]` | Путь к worktree (для IDE и AI-агентов) |

## Данные

| Команда | Описание |
|---|---|
| `docklet db golden refresh [service]` | Пересоздать золотую копию |
| `docklet db reset <feature> [service]` | Пересоздать клон из золотой копии |
| `docklet db lag` | Отставание схем клонов от золотой копии |
| `docklet snapshot build --config workspace.yaml` | (для CI клиента) собрать обезличенный снапшот |

## Отладка

| Команда | Описание |
|---|---|
| `docklet logs <env> [module] [-f]` | Логи |
| `docklet exec <env> <module> -- <cmd>` | Выполнить команду в контейнере модуля |
| `docklet routes [env]` | Текущая таблица маршрутов Envoy |

## Коды выхода

`0` успех · `1` ошибка · `2` неверные аргументы/конфиг · `3` требуется подтверждение (в неинтерактивном режиме) · `4` preflight не пройден.

## Этап 2+

`docklet login` (device flow), `docklet link <project>`, `docklet secrets …`, `docklet drift`.

## Этап 3

`docklet mcp` — запуск MCP-сервера (stdio) с инструментами: `list_projects`, `create_feature`, `feature_status`, `feature_url`, `feature_logs`, `delete_feature`.
