# Технологический стек

Основа — **Go** для всего серверного и локального кода, **TypeScript + React** для панели, **один контракт на Protobuf** между всеми частями. Конкретные версии фиксируются на старте разработки (этап 1).

## Локальный агент

| Задача | Технология | Зачем |
|---|---|---|
| Язык | Go | Один бинарник под macOS, Windows, Linux; нативные SDK Docker и Envoy |
| Контейнеры | Docker Engine API (Go-клиент), `compose-go` | Без вызова CLI; импорт compose-файлов |
| Runtime'ы | Docker Desktop, OrbStack, Colima, Podman | Обход лицензии Docker Desktop, быстрые ФС на Mac |
| Git и worktree | Системный `git` через exec | Совместимость с SSH-ключами и настройками разработчика |
| Связь с облаком | WebSocket + Protobuf (или ConnectRPC streaming) | Исходящее соединение проходит NAT и корпоративные прокси |
| Фоновая служба | launchd / systemd / Windows Service (`kardianos/service`) | Автостарт |
| AI-агенты | MCP-сервер (официальный Go SDK) | Claude Code, Codex создают фичи и получают URL |
| Дистрибуция | GoReleaser, Homebrew, winget, .deb/.rpm, автообновление с подписью | Установка одной командой |

## Локальная инфраструктура

| Задача | Технология | Зачем |
|---|---|---|
| Прокси | Envoy + `go-control-plane` (xDS/SDS от агента) | Маршруты по host, path, baggage без перезапуска |
| Домены | `*.localhost` + локальный DNS; CoreDNS в docker-сетях | Без правки hosts |
| HTTPS | Локальный CA (smallstep/truststore, как mkcert), Name Constraints | Замок, Secure-cookies, Service Worker |
| Контекст фичи | W3C Baggage + OpenTelemetry auto-instrumentation | Передача без правок кода |
| Почта | Mailpit | Письма ловятся локально |
| S3 | MinIO | Паритет с продом |
| Кэш/очереди/сессии | Redis или Valkey | Одинаковые драйверы у всех |
| БД | MySQL, PostgreSQL в версиях из конфига | Паритет с продом |
| Клонирование БД | PG `TEMPLATE` + `FILE_COPY`/`clone`, CoW-снапшоты volume'ов | Копия за секунды |
| Cron | Контейнер-шедулер или планировщик агента (`docker exec`) | Только у форков |

## Бэкенд платформы

| Задача | Технология |
|---|---|
| Язык | Go: модульный монолит + Agent gateway + Tunnel |
| API | ConnectRPC (Protobuf, совместим с gRPC и JSON), Buf; `buf breaking` в CI |
| Шина | NATS JetStream |
| БД | PostgreSQL, sqlc + pgx, миграции Atlas или goose |
| Фоновые задачи | River |
| Туннели | frp или свой на QUIC; wildcard-TLS через Let's Encrypt DNS-01 (certmagic) |
| Конфиг | YAML + JSON Schema |

## Веб-панель

| Задача | Технология |
|---|---|
| Фреймворк | React + TypeScript, Vite |
| Роутинг и данные | TanStack Router + TanStack Query, клиент ConnectRPC |
| UI | Tailwind CSS + shadcn/ui |
| Реальное время | WebSocket через Agent gateway |
| Терминал / редактор | xterm.js, Monaco Editor (YAML + JSON Schema) |
| Мобильный доступ | Адаптивная вёрстка + PWA |

## Данные платформы

PostgreSQL (схема на модуль) · Redis/Valkey (онлайн-статусы с TTL) · Loki → ClickHouse (логи) · S3 + OCI-реестр (снапшоты и образы).

## Интеграции и безопасность

OIDC (GitHub, Google), SAML · OAuth Device Flow · X25519 (age/libsodium) · Sigstore/cosign · GitHub App, GitLab, Bitbucket вебхуки · Jira, Linear, GitHub Issues · Stripe.

## Инфраструктура и наблюдаемость

| Задача | Технология |
|---|---|
| Хостинг | На старте контейнеры на нескольких VM (Kamal); при росте — managed Kubernetes |
| IaC | OpenTofu / Terraform |
| CI/CD | GitHub Actions, GHCR |
| Observability | OpenTelemetry Collector → Prometheus, Tempo, Loki, Grafana |
| Ошибки | Sentry |
| Тесты | Go test + testcontainers-go; Playwright для панели; матрица runtime'ов (Docker Desktop, OrbStack, Colima, Podman) × ОС |

## Структура репозиториев (предложение)

| Репозиторий | Содержимое | Лицензия |
|---|---|---|
| `devopus` (monorepo) или раздельно | `agent/`, `proto/` (Buf), `schema/` (JSON Schema `workspace.yaml`), `docs/` | Open-source (см. ADR-0010) |
| `devopus-cloud` | Core, Agent gateway, Tunnel, панель | Проприетарная |
