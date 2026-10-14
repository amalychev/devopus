
### Реальные продукты

| Проект | Уровень | Модули / сервисы | Маршруты (догадка) | Секреты → `secrets` | Код → worktree | Что смотреть руками | Экспорт обратно |
|---|---|---|---|---|---|---|---|
| example-voting-app | B — валиден, нужен обзор | 3 / 2 | result → /result; vote → / | 1 | 2 | в профилях: 1; bind файлов репозитория: 2; нет в формате v1: entrypoint (1), healthcheck у модуля (1), networks (свои сети) (5) | ok |
| mastodon-dev | B — валиден, нужен обзор | 1 / 4 | app → / | 1 | 1 | нет в формате v1: networks (свои сети) (5), ulimits (1) | ok |
| sentry-self-hosted | B — валиден, нужен обзор | 8 / 20 | nginx → nginx.<host> (сервис) | 8 | 8 | в профилях: 25; bind файлов репозитория: 30; закреплено по digest: 1; нет в формате v1: entrypoint (8), healthcheck у модуля (6), ulimits (4) | ok |
| immich-dev | B — валиден, нужен обзор | 4 / 2 | immich-web → / | 0 | 5 | в профилях: 1; bind вне репозитория: 3; нет в формате v1: healthcheck у модуля (3), tmpfs (3) | ok |
| posthog-dev | B — валиден, нужен обзор | 1 / 22 | kafka-ui → kafka-ui.<host> (сервис); proxy → proxy.<host> (сервис); temporal-ui → temporal-ui.<host> (сервис); webhook-tester → webhook-tester.<host> (сервис) | 3 | 0 | в профилях: 24; bind вне репозитория: 2; bind файлов репозитория: 14; закрепить позже (429 реестра): 2; переименовано: 1; нет в формате v1: entrypoint (4), healthcheck у модуля (1), networks (свои сети) (2) | ok |

Итого: A — 0, B — 5, C — 0, не целевых — 0.

### Типовые стеки (Docker awesome-compose)

| Проект | Уровень | Модули / сервисы | Маршруты (догадка) | Секреты → `secrets` | Код → worktree | Что смотреть руками | Экспорт обратно |
|---|---|---|---|---|---|---|---|
| react-express-mongodb | B — валиден, нужен обзор | 2 / 1 | frontend → / | 0 | 2 | нет в формате v1: networks (свои сети) (3) | ok |
| nginx-golang-mysql | B — валиден, нужен обзор | 1 / 2 | proxy → proxy.<host> (сервис) | 1 | 0 | bind файлов репозитория: 1; закрепить позже (429 реестра): 1; нет в формате v1: secrets (файлы) (2) | ok |
| django | A — без правок | 1 / 0 | web → / | 0 | 0 | — | ok |
| flask-redis | B — валиден, нужен обзор | 1 / 1 | web → / | 0 | 1 | закреплено по digest: 1 | ok |
| spring-postgres | B — валиден, нужен обзор | 1 / 1 | backend → / | 1 | 0 | закрепить позже (429 реестра): 1; нет в формате v1: networks (свои сети) (2), secrets (файлы) (1) | ok |
| react-java-mysql | B — валиден, нужен обзор | 2 / 1 | frontend → / | 1 | 1 | нет в формате v1: networks (свои сети) (3), secrets (файлы) (2) | ok |
| nextcloud-redis-mariadb | N — не целевой: нет кода для разработки | 0 / 3 | nc → nc.<host> (сервис) | 3 | 0 | нет в формате v1: networks (свои сети) (3) | ok |
| wordpress-mysql | N — не целевой: нет кода для разработки | 0 / 2 | wordpress → wordpress.<host> (сервис) | 3 | 0 | закрепить позже (429 реестра): 1 | ok |
| angular | A — без правок | 1 / 0 | web → / | 0 | 1 | — | ok |
| fastapi | A — без правок | 1 / 0 | api → / | 0 | 0 | — | ok |
| nginx-flask-mysql | B — валиден, нужен обзор | 2 / 1 | backend → /backend; proxy → / | 1 | 0 | нет в формате v1: networks (свои сети) (3), secrets (файлы) (2) | ok |
| react-rust-postgres | B — валиден, нужен обзор | 2 / 1 | frontend → / | 2 | 2 | нет в формате v1: networks (свои сети) (3) | ok |
| vuejs | A — без правок | 1 / 0 | web → / | 0 | 1 | — | ok |
| traefik-golang | B — валиден, нужен обзор | 1 / 1 | frontend → frontend.<host> (сервис) | 0 | 0 | bind вне репозитория: 1 | ok |
| elasticsearch-logstash-kibana | N — не целевой: нет кода для разработки | 0 / 3 | logstash → logstash.<host> (сервис) | 0 | 0 | bind файлов репозитория: 2; нет в формате v1: networks (свои сети) (3) | ok |

Итого: A — 4, B — 8, C — 0, не целевых — 3.

### Подробности

**example-voting-app** (B — валиден, нужен обзор)
- маршруты: result → /result; vote → /
- в профилях: seed (seed)
- секреты: db.POSTGRES_PASSWORD
- bind файлов репозитория: healthchecks → /healthchecks; healthchecks → /healthchecks
- нет в формате v1: entrypoint: result; healthcheck у модуля: vote; networks (свои сети): db, redis, result, vote, worker
- экспорт обратно: ok

**mastodon-dev** (B — валиден, нужен обзор)
- маршруты: app → /
- секреты: db.POSTGRES_PASSWORD
- нет в формате v1: networks (свои сети): app, db, es, libretranslate, redis; ulimits: es
- экспорт обратно: ok

**sentry-self-hosted** (B — валиден, нужен обзор)
- маршруты: nginx → nginx.<host> (сервис)
- закреплено по digest: smtp: registry.gitlab.com/egos-tech/smtp → @sha256:56b49ce6860a…
- в профилях: ingest-feedback-events (feature-complete); ingest-monitors (feature-complete); ingest-occurrences (feature-complete); launchpad-taskworker (feature-complete); metrics-consumer (feature-complete); monitors-clock-tasks (feature-complete); monitors-clock-tick (feature-complete); post-process-forwarder-issue-platform (feature-complete); post-process-forwarder-transactions (feature-complete); process-spans (feature-complete); snuba-eap-items-consumer (feature-complete); snuba-issue-occurrence-consumer (feature-complete); snuba-metrics-consumer (feature-complete); snuba-outcomes-accepted-consumer (feature-complete); snuba-profiling-functions-consumer (feature-complete); snuba-profiling-profile-chunks-consumer (feature-complete); snuba-profiling-profiles-consumer (feature-complete); snuba-replays-consumer (feature-complete); snuba-subscription-consumer-metrics (feature-complete); snuba-subscription-consumer-transactions (feature-complete); snuba-transactions-consumer (feature-complete); transactions-consumer (feature-complete); uptime-checker (feature-complete); uptime-results (feature-complete); vroom (feature-complete)
- секреты: attachments-consumer.LAUNCHPAD_RPC_SHARED_SECRET; events-consumer.LAUNCHPAD_RPC_SHARED_SECRET; post-process-forwarder-errors.LAUNCHPAD_RPC_SHARED_SECRET; seaweedfs.AWS_SECRET_ACCESS_KEY; sentry-cleanup.LAUNCHPAD_RPC_SHARED_SECRET; taskscheduler.LAUNCHPAD_RPC_SHARED_SECRET; taskworker.LAUNCHPAD_RPC_SHARED_SECRET; web.LAUNCHPAD_RPC_SHARED_SECRET
- bind файлов репозитория: geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; nginx.conf → /etc/nginx/nginx.conf; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; redis.conf → /usr/local/etc/redis/redis.conf; relay → /work/.relay; geoip → /geoip; sentry → /etc/sentry; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; healthcheck → /healthcheck; symbolicator → /etc/symbolicator; symbolicator → /etc/symbolicator; taskbroker → /etc/taskbroker; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck; geoip → /geoip; certificates → /usr/local/share/ca-certificates; healthcheck → /healthcheck
- нет в формате v1: entrypoint: attachments-consumer, events-consumer, post-process-forwarder-errors, seaweedfs, sentry-cleanup, taskscheduler, taskworker, web; healthcheck у модуля: attachments-consumer, clickhouse, events-consumer, post-process-forwarder-errors, taskworker, web; ulimits: clickhouse, kafka, redis, web
- экспорт обратно: ok

**immich-dev** (B — валиден, нужен обзор)
- маршруты: immich-web → /
- в профилях: immich-app-base (_base)
- bind вне репозитория: /postgres → /var/lib/postgresql/data; /photos → /data; /etc/localtime → /etc/localtime
- нет в формате v1: healthcheck у модуля: immich-init, immich-machine-learning, immich-server; tmpfs: immich-init, immich-server, immich-web
- экспорт обратно: ok

**posthog-dev** (B — валиден, нужен обзор)
- маршруты: kafka-ui → kafka-ui.<host> (сервис); proxy → proxy.<host> (сервис); temporal-ui → temporal-ui.<host> (сервис); webhook-tester → webhook-tester.<host> (сервис)
- закрепить позже (реестр ответил 429): kafka-ui: provectuslabs/kafka-ui:latest; proxy: caddy
- в профилях: browserless (browserless); capture (capture); capture-ai (ingestion); duckgres (duckgres); dynamodb (dynamodb); ingestion-error-tracking (ingestion); ingestion-general (ingestion); ingestion-logs (ingestion); ingestion-sessionreplay (ingestion); ingestion-traces (ingestion); objectstorage-azure (batch-exports); opensearch (opensearch); opensearch-dashboards (opensearch); opensearch-init (opensearch); personhog-etcd-init (ingestion); personhog-identity (ingestion); personhog-leader (ingestion); personhog-replica (ingestion); personhog-router (ingestion); personhog-router-leader (ingestion); personhog-writer (ingestion); plugins (ingestion); recording-api (ingestion); usage-ingestion (ingestion)
- секреты: clickhouse.AWS_SECRET_ACCESS_KEY; db.POSTGRES_PASSWORD; feature-flags.INTERNAL_REQUEST_TOKEN
- bind файлов репозитория: posthog/idl → /idl; docker/clickhouse/docker-entrypoint-initdb.d → /docker-entrypoint-initdb.d; docker/clickhouse/config.xml → /etc/clickhouse-server/config.xml; docker/clickhouse/config.d/default.xml → /etc/clickhouse-server/config.d/default.xml; docker/clickhouse/config.d/dev-memory.xml → /etc/clickhouse-server/config.d/dev-memory.xml; docker/clickhouse/users-dev.xml → /etc/clickhouse-server/users.xml; docker/clickhouse/user_defined_function.xml → /etc/clickhouse-server/user_defined_function.xml; posthog/user_scripts → /var/lib/clickhouse/user_scripts; docker/postgres-init-scripts → /docker-entrypoint-initdb.d; products → /products; share → /share; docker/kafka/topics.txt → /topics.txt; otel-collector-config.dev.yaml → /etc/otel-collector-config.yaml; docker/temporal/dynamicconfig → /etc/temporal/config/dynamicconfig
- bind вне репозитория: /var/lib/docker/containers → /var/lib/docker/containers; /var/run/docker.sock → /var/run/docker.sock
- переименовано: kafka_ui → kafka-ui
- нет в формате v1: entrypoint: kafka-init, objectstorage, proxy, seaweedfs; healthcheck у модуля: feature-flags; networks (свои сети): jaeger, otel-collector
- экспорт обратно: ok

**react-express-mongodb** (B — валиден, нужен обзор)
- маршруты: frontend → /
- нет в формате v1: networks (свои сети): backend, frontend, mongo
- экспорт обратно: ok

**nginx-golang-mysql** (B — валиден, нужен обзор)
- маршруты: proxy → proxy.<host> (сервис)
- закрепить позже (реестр ответил 429): proxy: nginx
- секреты: db.MYSQL_ROOT_PASSWORD_FILE
- bind файлов репозитория: proxy/nginx.conf → /etc/nginx/conf.d/default.conf
- нет в формате v1: secrets (файлы): backend, db
- экспорт обратно: ok

**django** (A — без правок)
- маршруты: web → /
- экспорт обратно: ok

**flask-redis** (B — валиден, нужен обзор)
- маршруты: web → /
- закреплено по digest: redis: redislabs/redismod → @sha256:f8e639450a5c…
- экспорт обратно: ok

**spring-postgres** (B — валиден, нужен обзор)
- маршруты: backend → /
- закрепить позже (реестр ответил 429): db: postgres
- секреты: db.POSTGRES_PASSWORD_FILE
- нет в формате v1: networks (свои сети): backend, db; secrets (файлы): db
- экспорт обратно: ok

**react-java-mysql** (B — валиден, нужен обзор)
- маршруты: frontend → /
- секреты: db.MYSQL_ROOT_PASSWORD_FILE
- нет в формате v1: networks (свои сети): backend, db, frontend; secrets (файлы): backend, db
- экспорт обратно: ok

**nextcloud-redis-mariadb** (N — не целевой: нет кода для разработки)
- маршруты: nc → nc.<host> (сервис)
- секреты: db.MYSQL_ROOT_PASSWORD; db.MYSQL_PASSWORD; nc.MYSQL_PASSWORD
- нет в формате v1: networks (свои сети): db, nc, redis
- экспорт обратно: ok

**wordpress-mysql** (N — не целевой: нет кода для разработки)
- маршруты: wordpress → wordpress.<host> (сервис)
- закрепить позже (реестр ответил 429): wordpress: wordpress:latest
- секреты: db.MYSQL_PASSWORD; db.MYSQL_ROOT_PASSWORD; wordpress.WORDPRESS_DB_PASSWORD
- экспорт обратно: ok

**angular** (A — без правок)
- маршруты: web → /
- экспорт обратно: ok

**fastapi** (A — без правок)
- маршруты: api → /
- экспорт обратно: ok

**nginx-flask-mysql** (B — валиден, нужен обзор)
- маршруты: backend → /backend; proxy → /
- секреты: db.MYSQL_ROOT_PASSWORD_FILE
- нет в формате v1: networks (свои сети): backend, db, proxy; secrets (файлы): backend, db
- экспорт обратно: ok

**react-rust-postgres** (B — валиден, нужен обзор)
- маршруты: frontend → /
- секреты: backend.PG_PASSWORD; db.POSTGRES_PASSWORD
- нет в формате v1: networks (свои сети): backend, db, frontend
- экспорт обратно: ok

**vuejs** (A — без правок)
- маршруты: web → /
- экспорт обратно: ok

**traefik-golang** (B — валиден, нужен обзор)
- маршруты: frontend → frontend.<host> (сервис)
- bind вне репозитория: /var/run/docker.sock → /var/run/docker.sock
- экспорт обратно: ok

**elasticsearch-logstash-kibana** (N — не целевой: нет кода для разработки)
- маршруты: logstash → logstash.<host> (сервис)
- bind файлов репозитория: logstash/pipeline/logstash-nginx.config → /usr/share/logstash/pipeline/logstash-nginx.config; logstash/nginx.log → /home/nginx.log
- нет в формате v1: networks (свои сети): elasticsearch, kibana, logstash
- экспорт обратно: ok
