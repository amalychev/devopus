# Спецификация: `workspace.yaml` v1

**Статус:** черновик · **Версия формата:** 1

Весь проект описывается одним файлом `workspace.yaml`. Он лежит в отдельном репозитории или в основном (открытый вопрос, [ADR-0008](../adr/0008-config-location.md)) и двусторонне синхронизируется с панелью. Формат валидируется JSON Schema — IDE и панель подсказывают поля.

**Обязательные функции v1:** импорт существующего `docker-compose.yml` и экспорт обратно в него.

## Пример

```yaml
version: 1
project: shop
domain: shop.localhost

services:
  mysql:
    image: mysql:8.4
    data:
      seed: snapshot://shop-anonymized/latest   # обезличенный снапшот
  redis:
    image: redis:7
  mail:
    image: axllent/mailpit
    expose: mail                                # mail.shop.localhost
  s3:
    image: minio/minio

modules:
  backend:
    repo: git@github.com:acme/shop-api.git
    runtime: php:8.3
    routes: ["/api"]
    uses: [mysql, redis, mail, s3, payments]
    env:
      CACHE_DRIVER: redis
      QUEUE_CONNECTION: redis
      MAIL_HOST: mail
      PAYMENTS_URL: http://payments            # имя не меняется в фичах
      STRIPE_KEY: ${{ secrets.STRIPE_TEST_KEY }}
    hooks:
      install: composer install
      migrate: php artisan migrate --force
    cron:
      - schedule: "* * * * *"
        command: php artisan schedule:run
    workers:
      - command: php artisan queue:work

  frontend:
    repo: git@github.com:acme/shop-web.git
    runtime: node:22
    routes: ["/"]
    hooks:
      install: npm ci
    command: npm run dev

  payments:
    repo: git@github.com:acme/shop-payments.git
    runtime: python:3.12
    routes: ["/payments"]
    uses: [mysql]
    hooks:
      install: pip install -r requirements.txt
    command: uvicorn app:main --host 0.0.0.0

features:
  database: clone          # clone | shared
  sleep_after: 2h
  cleanup: on_merge
  isolation: simple        # simple | contextual (contextual — этап 3)

overrides:
  allowed: [xdebug, ports]
```

## Корневые поля

| Поле | Тип | Обяз. | Описание |
|---|---|---|---|
| `version` | int | да | Версия формата. Сейчас `1` |
| `project` | string `^[a-z][a-z0-9-]{1,30}$` | да | Имя проекта; используется в именах Docker-ресурсов |
| `domain` | hostname | да | Домен базового окружения. Должен оканчиваться на `.localhost` (или домен платформы) |
| `services` | map<name, Service> | нет | Инфраструктурные зависимости без кода |
| `modules` | map<name, Module> | да (≥1) | Модули проекта |
| `features` | FeaturePolicy | нет | Политика фич |
| `overrides` | OverridePolicy | нет | Что разработчик может менять лично |
| `data` | DataPolicy | нет | *Предложение:* правила маскирования для снапшотов (см. ниже) |

Имена в `services` и `modules` образуют одно пространство имён и являются **логическими DNS-именами** внутри окружения (`http://payments`, `mysql:3306`). Должны быть уникальны и соответствовать `^[a-z][a-z0-9-]{0,62}$` — ограничению DNS-метки. Изначальный лимит в 31 символ оказался надуманным: у Sentry имена до 37 символов ([P-8](../prototypes/p8-compose-import/README.md)).

## Service

| Поле | Тип | Описание |
|---|---|---|
| `image` | string | OCI-образ с версией: тег вида версии (`postgres:17.2`) или digest. `latest` и плавающие теги (`redis:alpine`) валидатор не пропускает; импорт закрепляет их по текущему digest (P-8) |
| `expose` | string | Публикует веб-интерфейс сервиса как `<expose>.<domain>` (и `<expose>.<slug>.<domain>` в фичах) |
| `data.seed` | URI | Источник данных: `snapshot://<name>/<tag>`, `file://<path>`, `hook://<module>.<hook>` |
| `env` | map | Переменные окружения контейнера |
| `volumes` | list | Именованные тома (namespace'ятся по ID окружения) и bind-монтирования файлов репозитория (`./config/clickhouse.xml:/etc/clickhouse-server/config.d/x.xml`) — у Sentry их 30 (P-8). Bind вне репозитория — только с локальным подтверждением |
| `healthcheck` | object | Как в compose |
| `uses` | list<name> | Зависимости сервиса от других сервисов (Kafka → ZooKeeper); порядок старта (P-8) |
| `optional` | bool | Сервис не поднимается по умолчанию, только по требованию или в фиче, которой он нужен. Аналог compose `profiles`: у Sentry и PostHog по 24–25 таких сервисов (P-8) |
| `entrypoint`, `tmpfs`, `ulimits` | как в compose | Встречаются в 3 из 5 реальных продуктов (P-8) |
| `per_feature` | `clone \| shared \| instance` | *Предложение:* переопределяет `features.database` для конкретного сервиса |

## Module

| Поле | Тип | Описание |
|---|---|---|
| `repo` | git URL | Репозиторий модуля. Взаимоисключимо с `path` |
| `path` | string | Папка внутри основного репозитория (монорепо) |
| `branch` | string | Базовая ветка, по умолчанию `main` |
| `runtime` | `<lang>:<version>` | Рантайм: `php`, `node`, `python`, … Платформа предоставляет образы рантаймов (с `ext-opentelemetry` для PHP) |
| `image` / `build` | string / object | Альтернатива `runtime`: свой образ или сборка. `build` — объект `{context, dockerfile, target, args}`: строки «папка с Dockerfile» не хватило ни одному из 5 реальных продуктов (P-8) |
| `routes` | list<path-prefix> | Пути, которые Envoy направляет в модуль на хосте окружения. Пересечения префиксов между модулями — ошибка валидации (кроме вложенных, где побеждает самый длинный) |
| `port` | int | Порт внутри контейнера, по умолчанию автоопределение / 8080 |
| `uses` | list<name> | Зависимости (сервисы и модули). Граф без циклов для `hooks`; используется для подсказок при форке и порядка старта |
| `env` | map<string, string> | Переменные. Поддерживают выражения `${{ … }}` |
| `command` | string | Команда запуска основного процесса |
| `entrypoint` | string | Как в compose |
| `healthcheck` | object | Как в compose; агент использует его, чтобы понять, что модуль готов |
| `tmpfs`, `ulimits` | как в compose | |
| `optional` | bool | Как у сервиса |
| `hooks.install` | string | Установка зависимостей; запускается при создании worktree и при изменении lock-файлов |
| `hooks.migrate` | string | Миграции; в фиче — только на собственном клоне БД |
| `hooks.seed` | string | *Предложение:* сид-скрипт |
| `hooks.build` | string | *Предложение:* сборка (`npm run build`) |
| `cron` | list<{schedule, command}> | Только у базового окружения и форкнутых модулей |
| `workers` | list<{command, replicas?}> | Фоновые процессы; в фиче читают очереди с префиксом фичи |
| `volumes` | list | Тяжёлые папки (`node_modules`, `vendor`) в именованных томах для скорости на macOS/Windows |

## Выражения `${{ … }}`

| Выражение | Значение |
|---|---|
| `${{ secrets.NAME }}` | Секрет воркспейса или проекта; расшифровывается локально агентом |
| `${{ <module>.url }}` | Внутренний URL модуля в текущем окружении (Simple Isolation): в фиче указывает на форк, если он есть, иначе на базу |
| `${{ <service>.host }}`, `${{ <service>.port }}` | Хост/порт сервиса в текущем окружении |
| `${{ env.url }}` | Публичный URL окружения (`https://pay-123.shop.localhost`) |
| `${{ env.slug }}`, `${{ env.id }}` | Слаг и стабильный ID окружения |
| `${{ <service>.database }}` | Имя логической БД окружения (для `clone`) |

Изменение значения выражения (например, добавили модуль в фичу) → агент перезапускает затронутые контейнеры.

## FeaturePolicy (`features`)

| Поле | Тип | По умолчанию | Описание |
|---|---|---|---|
| `database` | `clone \| shared` | `clone` | Клонировать БД форкнутого модуля из золотой копии или использовать общую |
| `sleep_after` | duration | `2h` | Усыплять неактивные фичи |
| `cleanup` | `on_merge \| manual \| after:<duration>` | `on_merge` | Автоочистка |
| `isolation` | `simple \| contextual` | `simple` | Режим маршрутизации между модулями |
| `branch_prefix` | string | `feature/` | Префикс ветки |
| `max_running` | int | — | *Предложение:* дефолт бюджета; переопределяется на устройстве |

## OverridePolicy (`overrides`)

| Поле | Описание |
|---|---|
| `allowed` | Список разрешённых личных переопределений: `xdebug`, `ports`, `env:<NAME>`, `resources` |

Overrides хранятся в панели (`Override` = Member × Project) или в локальном `workspace.override.yaml` (в `.gitignore`); всегда видны в панели.

## DataPolicy (предложение)

```yaml
data:
  snapshots:
    shop-anonymized:
      source: mysql
      mask:
        users.email: email
        users.name: name
        users.phone: phone
        payments.card_last4: redact
```

Используется CLI в CI клиента для сборки обезличенного снапшота (`docklet snapshot build`). Правила `mask` доступны в Team. В Business политики маскирования воркспейса задаются в панели и накладываются поверх `mask`: проект может их только ужесточить. Сборка падает, если сканер ПДн находит немаскированные колонки (см. [data.md](../docs/architecture/data.md#снапшоты-team-и-business)).

## Правила валидации

1. `version == 1`.
2. Каждое имя в `uses` существует.
3. Нет циклов в графе `uses` среди модулей для порядка хуков (циклы рантайм-вызовов допустимы, но порядок старта тогда — параллельный).
4. Префиксы `routes` не пересекаются (кроме строгой вложенности).
5. Все образы с версией или digest (не `latest` и не плавающий тег вроде `alpine`).
6. Секреты указываются только через `${{ secrets.* }}`; литералы, похожие на ключи, — предупреждение.
7. `domain` в разрешённой зоне.
8. Детектор антипаттернов (предупреждения): `VITE_API_URL`/`NEXT_PUBLIC_API_URL` с абсолютным URL; `ports:` в модулях.

## Импорт из `docker-compose.yml`

| compose | workspace.yaml |
|---|---|
| сервис с `build:` | `modules.<name>` с `path`/`build` |
| сервис только с `image:` | `services.<name>` |
| `ports:` | отбрасываются (наружу только Envoy); первый HTTP-порт → `port`; предложить `routes` |
| `depends_on` | `uses` |
| `environment`, `env_file` | `env` (значения, похожие на секреты, → предложение вынести в `secrets`) |
| `volumes` (named) | `volumes` |
| `volumes` (bind на код) | заменяется worktree |
| `command` | `command` |

Импорт производит черновик и отчёт о том, что не удалось перенести.

Проверено прототипом [P-8](../prototypes/p8-compose-import/README.md) на 5 реальных продуктах (example-voting-app, Mastodon, Sentry self-hosted, Immich, PostHog) и 15 стеках Docker awesome-compose. Все 5 продуктов импортируются без ручных правок, все 20 проектов экспортируются обратно и проходят `docker compose config`.

Дополнительные правила импорта:

| compose | workspace.yaml |
|---|---|
| `profiles` | Сервисы профиля по умолчанию импортируются как обычные, остальные — с `optional: true` |
| `networks` (свои сети) | Сводятся к одной сети окружения; изоляцию даёт Docklet |
| `secrets`, `configs` (файлы) | Bind-монтирование файла репозитория или `${{ secrets.* }}` — предложение в отчёте |
| `extends`, якоря `x-`, `${…}`, `.env` | Разрешаются при загрузке через `compose-go`; переменные хоста без значения — в отчёт |
| `image` без версии | Закрепляется по текущему digest; при отказе реестра (429) — в отчёт с повтором позже |
| Проект без единого `build` | Не целевой: в нём нечего форкать (WordPress, Nextcloud, ELK) |
| Приложение запускается на хосте, compose держит только инфраструктуру (PostHog) | Гибридный режим не поддержан; открытый вопрос для интервью |


## Экспорт в `docker-compose.yml`

Генерирует compose для базового окружения (без Envoy-маршрутизации фич): сервисы, модули с `build`, `depends_on`, env с подставленными значениями (секреты — плейсхолдеры). Гарантия «выход без lock-in».
