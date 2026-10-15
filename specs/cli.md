# Спецификация: CLI агента (MVP)

**Статус:** черновик; реализованы `init`, `validate`, `export compose`, `schema`, `version` (M1), `agent run`, `status`, `up`, `down` (M2), `doctor [--fix]`, `routes`, `open`, `proxy restart`, `proxy coexist` (M3), `logs`, `exec` (M4; `up` поднимает сервисы и модули, `status` показывает их). CLI — тонкий клиент к локальному API агента (unix-сокет / named pipe). Все команды поддерживают `--json`. Файл конфига — `-f/--file`, по умолчанию `workspace.yaml` в текущей или ближайшей родительской папке.

## Установка и служба

| Команда | Описание |
|---|---|
| `devopus install` | Регистрирует агента как службу, ставит privileged helper, запускает preflight. Каждое системное изменение — с подтверждением |
| `devopus uninstall [--purge]` | Удаляет службу, helper, DNS-настройки, CA из всех хранилищ; `--purge` — ещё и данные окружений (с подтверждением) |
| `devopus doctor [--fix] [--yes]` | Preflight-проверки (Docker, порты 80/443 v4/v6, DNS, CA, пулы адресов, reflink, ресурсы) с владельцами конфликтов и предложениями исправлений. `--fix` делает то, что Devopus умеет сам (M3: CA в связку ключей macOS), спрашивая перед каждым; без терминала — код 3, если нет `--yes`. Код 4, если есть ошибки. *(M3: runtime, агент, порты, DNS, CA, пул адресов; reflink и ресурсы — с M6)* |
| `devopus status` | Агент, runtime, прокси (режим портов default / coexist / fallback, владельцы занятых портов, связь с Envoy), CA и доверие к нему, окружения с URL |
| `devopus proxy restart` | Пересоздать прокси: он заново проверит 80/443 (URL меняются, если меняются порты) |
| `devopus proxy coexist --http <port> [--https <port>]` / `--off` | Режим сосуществования: dev-сервер (Herd, Valet, nginx) перенесён на другие порты, Devopus занимает 80/443 и отдаёт ему все незнакомые хосты (HTTPS — без расшифровки) |
| `devopus ui` | Открыть локальную панель |
| `devopus agent run` | Агент на переднем плане (до `install` CLI запускает его в фоне сам) |
| `devopus version` | Версия |

## Проект

| Команда | Описание |
|---|---|
| `devopus init [--from-compose <file>]… [--project <name>] [--no-pin] [--force]` | Создать `workspace.yaml` в текущей папке (она — корень репозитория). Без `--from-compose` берёт `compose.yaml`/`docker-compose.yml` из папки, если есть, иначе пишет шаблон. Несколько `--from-compose` сливаются как `docker compose -f`. Плавающие теги закрепляются по digest из реестра (`--no-pin` — пропустить). Секреты — в `.env.secrets`. Отчёт с уровнем A/B/C/N; код выхода 2 при уровне C |
| `devopus validate` | Проверка по JSON Schema и правилам + антипаттерны |
| `devopus export compose [-o file]` | Экспорт в `docker-compose.yml` (по умолчанию в stdout); откажет, если конфиг с ошибками |
| `devopus schema` | Напечатать JSON Schema `workspace.yaml` |
| `devopus up [-d] [--timeout]` | Поднять базовое окружение (clone репозиториев, образы, сервисы, хуки, модули, маршруты); показывает прогресс шагов и в конце — адреса модулей и сервисов. `-d` — не ждать. Повторный `up` без изменений ничего не перезапускает |
| `devopus down [--volumes] [--project]` | Остановить окружения проекта; `--volumes` — удалить проект со всеми ресурсами и данными |
| `devopus open [env]` | Открыть URL в браузере; без аргумента — базовое окружение проекта из `workspace.yaml` |

## Фичи

| Команда | Описание |
|---|---|
| `devopus feature create <name> --modules a,b [--branch …] [--from-task URL]` | Создать фичу; печатает подсказку про общие ресурсы по графу `uses` |
| `devopus feature add <name> <module>` | Добавить модуль в фичу |
| `devopus feature list` | Фичи, фазы, URL, ресурсы, общие/собственные данные |
| `devopus feature sleep <name>`, `devopus feature wake <name>` | Ручное управление |
| `devopus feature rm <name> [--force]` | Удалить; без `--force` откажет при незакоммиченных/незапушенных изменениях |
| `devopus feature path <name> [module]` | Путь к worktree (для IDE и AI-агентов) |

## Данные

| Команда | Описание |
|---|---|
| `devopus db golden refresh [service]` | Пересоздать золотую копию |
| `devopus db reset <feature> [service]` | Пересоздать клон из золотой копии |
| `devopus db lag` | Отставание схем клонов от золотой копии |
| `devopus snapshot build --config workspace.yaml` | (для CI клиента) собрать обезличенный снапшот |

## Отладка

| Команда | Описание |
|---|---|
| `devopus logs [env] <module\|service> [--follow] [-n N]` | Вывод модуля, воркера или сервиса. С одним аргументом — базовое окружение проекта из текущей папки. `-f` занят под `--file`, поэтому только `--follow`. Логи хуков и сборок — в `~/.devopus/envs/<env>/logs/` |
| `devopus exec [env] <module\|service> [-- <cmd>…]` | Выполнить команду в контейнере (по умолчанию `sh`); с терминалом — интерактивно |
| `devopus routes [env]` | Текущая таблица маршрутов Envoy: URL, путь, окружение, цель |

## Коды выхода

`0` успех · `1` ошибка · `2` неверные аргументы/конфиг · `3` требуется подтверждение (в неинтерактивном режиме) · `4` preflight не пройден.

## Этап 2+

`devopus login` (device flow), `devopus link <project>`, `devopus secrets …`, `devopus drift`.

## Этап 3

`devopus mcp` — запуск MCP-сервера (stdio) с инструментами: `list_projects`, `create_feature`, `feature_status`, `feature_url`, `feature_logs`, `delete_feature`.
