# Спецификация: CLI агента (MVP)

**Статус:** черновик; реализованы `init`, `validate`, `export compose`, `schema`, `version` (M1), `agent run`, `status`, `up`, `down` (M2), `doctor [--fix]`, `routes`, `open`, `proxy restart`, `proxy coexist` (M3; `up` создаёт сеть, сертификат, DNS и маршрут окружения, модули — с M4). CLI — тонкий клиент к локальному API агента (unix-сокет / named pipe). Все команды поддерживают `--json`. Файл конфига — `-f/--file`, по умолчанию `workspace.yaml` в текущей или ближайшей родительской папке.

## Установка и служба

| Команда | Описание |
|---|---|
| `devcorb install` | Регистрирует агента как службу, ставит privileged helper, запускает preflight. Каждое системное изменение — с подтверждением |
| `devcorb uninstall [--purge]` | Удаляет службу, helper, DNS-настройки, CA из всех хранилищ; `--purge` — ещё и данные окружений (с подтверждением) |
| `devcorb doctor [--fix] [--yes]` | Preflight-проверки (Docker, порты 80/443 v4/v6, DNS, CA, пулы адресов, reflink, ресурсы) с владельцами конфликтов и предложениями исправлений. `--fix` делает то, что Devcorb умеет сам (M3: CA в связку ключей macOS), спрашивая перед каждым; без терминала — код 3, если нет `--yes`. Код 4, если есть ошибки. *(M3: runtime, агент, порты, DNS, CA, пул адресов; reflink и ресурсы — с M6)* |
| `devcorb status` | Агент, runtime, прокси (режим портов default / coexist / fallback, владельцы занятых портов, связь с Envoy), CA и доверие к нему, окружения с URL |
| `devcorb proxy restart` | Пересоздать прокси: он заново проверит 80/443 (URL меняются, если меняются порты) |
| `devcorb proxy coexist --http <port> [--https <port>]` / `--off` | Режим сосуществования: dev-сервер (Herd, Valet, nginx) перенесён на другие порты, Devcorb занимает 80/443 и отдаёт ему все незнакомые хосты (HTTPS — без расшифровки) |
| `devcorb ui` | Открыть локальную панель |
| `devcorb agent run` | Агент на переднем плане (до `install` CLI запускает его в фоне сам) |
| `devcorb version` | Версия |

## Проект

| Команда | Описание |
|---|---|
| `devcorb init [--from-compose <file>]… [--project <name>] [--no-pin] [--force]` | Создать `workspace.yaml` в текущей папке (она — корень репозитория). Без `--from-compose` берёт `compose.yaml`/`docker-compose.yml` из папки, если есть, иначе пишет шаблон. Несколько `--from-compose` сливаются как `docker compose -f`. Плавающие теги закрепляются по digest из реестра (`--no-pin` — пропустить). Секреты — в `.env.secrets`. Отчёт с уровнем A/B/C/N; код выхода 2 при уровне C |
| `devcorb validate` | Проверка по JSON Schema и правилам + антипаттерны |
| `devcorb export compose [-o file]` | Экспорт в `docker-compose.yml` (по умолчанию в stdout); откажет, если конфиг с ошибками |
| `devcorb schema` | Напечатать JSON Schema `workspace.yaml` |
| `devcorb up [-d] [--timeout]` | Поднять базовое окружение (clone репозиториев, данные, хуки); показывает прогресс шагов. `-d` — не ждать |
| `devcorb down [--volumes] [--project]` | Остановить окружения проекта; `--volumes` — удалить проект со всеми ресурсами и данными |
| `devcorb open [env]` | Открыть URL в браузере; без аргумента — базовое окружение проекта из `workspace.yaml` |

## Фичи

| Команда | Описание |
|---|---|
| `devcorb feature create <name> --modules a,b [--branch …] [--from-task URL]` | Создать фичу; печатает подсказку про общие ресурсы по графу `uses` |
| `devcorb feature add <name> <module>` | Добавить модуль в фичу |
| `devcorb feature list` | Фичи, фазы, URL, ресурсы, общие/собственные данные |
| `devcorb feature sleep <name>`, `devcorb feature wake <name>` | Ручное управление |
| `devcorb feature rm <name> [--force]` | Удалить; без `--force` откажет при незакоммиченных/незапушенных изменениях |
| `devcorb feature path <name> [module]` | Путь к worktree (для IDE и AI-агентов) |

## Данные

| Команда | Описание |
|---|---|
| `devcorb db golden refresh [service]` | Пересоздать золотую копию |
| `devcorb db reset <feature> [service]` | Пересоздать клон из золотой копии |
| `devcorb db lag` | Отставание схем клонов от золотой копии |
| `devcorb snapshot build --config workspace.yaml` | (для CI клиента) собрать обезличенный снапшот |

## Отладка

| Команда | Описание |
|---|---|
| `devcorb logs <env> [module] [-f]` | Логи |
| `devcorb exec <env> <module> -- <cmd>` | Выполнить команду в контейнере модуля |
| `devcorb routes [env]` | Текущая таблица маршрутов Envoy: URL, путь, окружение, цель |

## Коды выхода

`0` успех · `1` ошибка · `2` неверные аргументы/конфиг · `3` требуется подтверждение (в неинтерактивном режиме) · `4` preflight не пройден.

## Этап 2+

`devcorb login` (device flow), `devcorb link <project>`, `devcorb secrets …`, `devcorb drift`.

## Этап 3

`devcorb mcp` — запуск MCP-сервера (stdio) с инструментами: `list_projects`, `create_feature`, `feature_status`, `feature_url`, `feature_logs`, `delete_feature`.
