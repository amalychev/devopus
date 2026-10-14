# Спецификация: CLI агента (MVP)

**Статус:** черновик; реализованы `init`, `validate`, `export compose`, `schema`, `version` (M1), `agent run`, `status`, `up`, `down` (M2; `up` пока создаёт только сеть окружения). CLI — тонкий клиент к локальному API агента (unix-сокет / named pipe). Все команды поддерживают `--json`. Файл конфига — `-f/--file`, по умолчанию `workspace.yaml` в текущей или ближайшей родительской папке.

## Установка и служба

| Команда | Описание |
|---|---|
| `docklet install` | Регистрирует агента как службу, ставит privileged helper, запускает preflight. Каждое системное изменение — с подтверждением |
| `docklet uninstall [--purge]` | Удаляет службу, helper, DNS-настройки, CA из всех хранилищ; `--purge` — ещё и данные окружений (с подтверждением) |
| `docklet doctor [--fix]` | Preflight-проверки (Docker, порты 80/443 v4/v6, DNS, CA, пулы адресов, reflink, ресурсы) с владельцами конфликтов и предложениями исправлений |
| `docklet status` | Агент, runtime, режим портов (default / сосуществование / fallback), окружения |
| `docklet ui` | Открыть локальную панель |
| `docklet agent run` | Агент на переднем плане (до `install` CLI запускает его в фоне сам) |
| `docklet version` | Версия |

## Проект

| Команда | Описание |
|---|---|
| `docklet init [--from-compose <file>]… [--project <name>] [--no-pin] [--force]` | Создать `workspace.yaml` в текущей папке (она — корень репозитория). Без `--from-compose` берёт `compose.yaml`/`docker-compose.yml` из папки, если есть, иначе пишет шаблон. Несколько `--from-compose` сливаются как `docker compose -f`. Плавающие теги закрепляются по digest из реестра (`--no-pin` — пропустить). Секреты — в `.env.secrets`. Отчёт с уровнем A/B/C/N; код выхода 2 при уровне C |
| `docklet validate` | Проверка по JSON Schema и правилам + антипаттерны |
| `docklet export compose [-o file]` | Экспорт в `docker-compose.yml` (по умолчанию в stdout); откажет, если конфиг с ошибками |
| `docklet schema` | Напечатать JSON Schema `workspace.yaml` |
| `docklet up [-d] [--timeout]` | Поднять базовое окружение (clone репозиториев, данные, хуки); показывает прогресс шагов. `-d` — не ждать |
| `docklet down [--volumes] [--project]` | Остановить окружения проекта; `--volumes` — удалить проект со всеми ресурсами и данными |
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
