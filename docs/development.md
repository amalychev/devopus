# Как развернуть и запустить Devopus

Инструкция для тех, кто работает над самим Devopus: собрать из исходников, поднять агента и демо-проект, вести разработку, гонять тесты и выпускать релиз. Как поставить готовый Devopus пользователю — раздел «Установка» в [README](../README.md#установка).

**Тестовый сервер:** https://devopus.webapace.ink — Auth и сервер воркспейса из последнего коммита `main`, обновляются сами через пару минут после `git push`. Как им пользоваться и как развернуть его заново — [test-server.md](test-server.md).

## 1. Что нужно

| Что | Зачем | Проверено |
|---|---|---|
| macOS 26 (Apple Silicon или Intel) | Основная платформа MVP | ✅ |
| Linux | Собирается, пакеты ставятся; агент на Linux-хосте **не проверен** | ⚠️ |
| Go 1.27+ | Сборка и тесты | ✅ |
| Docker-runtime: Docker Desktop, OrbStack или Colima | Агент управляет контейнерами через Docker API | ✅ все три |
| git | Агент делает clone и worktree | ✅ |
| Node 24 + npm | Только при правках панели (`web/`) или `proto/` | — |
| GoReleaser v2 | Только для сборки релиза (`brew install goreleaser`) | — |

Node для обычной сборки не нужен: собранная панель лежит в `internal/panel/dist` и встраивается в бинарник.

## 2. Быстрый старт

```bash
git clone git@github.com:amalychev/devopus.git
cd devopus
go build -o devopus ./cmd/devopus

# Auth и его Postgres (с этапа 2 все команды, кроме входа, требуют входа) — §3а
docker run -d --name devopus-auth-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=auth \
  -p 127.0.0.1:5433:5432 -v devopus-auth-pg:/var/lib/postgresql/data postgres:17-alpine
DEVOPUS_AUTH_DATABASE_URL='postgres://postgres:dev@127.0.0.1:5433/auth?sslmode=disable' \
  go run ./cmd/devopus-auth run     # в отдельном терминале; коды из писем — в его выводе
./devopus register                # email, код из вывода devopus-auth, план Free
# сервер воркспейса — в третьем терминале (§3а, «Сервер воркспейса»)

./devopus doctor          # runtime, порты 80/443, *.localhost, CA
./devopus doctor --fix    # доверить локальный CA (macOS спросит пароль) — один раз

./devopus workspace demo                 # демо-воркспейс: проект, пароль базы — E2E-секретом
./devopus up demo --dir examples/demo    # первый раз скачивает образы; дальше ~10 с
./devopus open                           # https://demo.localhost
./devopus panel open                     # локальная веб-панель в браузере
```

Удобнее положить бинарник в `PATH`: `go build -o ~/.local/bin/devopus ./cmd/devopus`, дальше просто `devopus …`. Все команды по группам и как начать — `devopus --help`; у каждой группы свой `--help` (`devopus agent --help`).

Что есть в демо — [examples/demo/README.md](../examples/demo/README.md). Попробовать фичу:

```bash
devopus feature create PAY-1 --apps api   # https://pay-1.demo.localhost, свой worktree и копия базы
devopus feature list
devopus feature rm PAY-1
devopus down demo --volumes           # убрать демо вместе с данными
```

## 3. Агент и панель

Всё управляется командой `devopus` ([ADR-0012](../adr/0012-devopus-cli-control-point.md)). На машине два независимых компонента с одинаковым набором команд:

| | Агент — `devopus agent …` | Панель — `devopus panel …` |
|---|---|---|
| Что делает | Поднимает окружения: приводит Docker к состоянию проектов | Веб-интерфейс к агенту на `127.0.0.1` |
| Нужен ли другой | Работает без панели | Без агента показывает «агент не отвечает» и ждёт его |
| Службой (старт при входе) | `devopus agent install` | `devopus panel install` |
| Запуск в фоне | `devopus agent start`, или любая команда, которой нужен агент (`up`, `feature`…) | `devopus panel start`, или `devopus panel open` |
| Остановить / перезапустить | `devopus agent stop` / `restart` | `devopus panel stop` / `restart` |
| Состояние | `devopus agent status` | `devopus panel status` |
| Лог | `devopus agent logs --follow` (`~/.devopus/agent.log`) | `devopus panel logs --follow` (`~/.devopus/panel/panel.log`) |
| На переднем плане (отладка) | `devopus agent run` | `devopus panel run` |
| Удалить | `devopus agent uninstall [--purge]` | `devopus panel uninstall` |

Общая картина — `devopus status` (агент, runtime, прокси, CA, панель, окружения) и `devopus doctor`.

**После пересборки перезапустите компоненты вручную.** Сборки без версии (`dev`) друг друга не перезапускают; автоматический перезапуск после обновления работает только для релизных версий.

```bash
go build -o ~/.local/bin/devopus ./cmd/devopus && devopus agent restart && devopus panel restart
```

Остановка агента не останавливает окружения: контейнеры работают дальше, а новый агент подхватывает их с того же места.

`devopus agent install` и `devopus panel install` запускайте только с бинарника, который лежит на постоянном месте: служба запоминает путь к нему. Из `go run` команда откажет.

### Второй экземпляр для экспериментов

`DEVOPUS_HOME` даёт полностью отдельные агента и панель: своё состояние, свои контейнеры (метка экземпляра), свои службы. Основной агент и его окружения не затрагиваются.

```bash
export DEVOPUS_HOME=/tmp/dvp-test     # путь короткий: сокет агента ограничен 104 байтами
./devopus up
./devopus agent uninstall --purge --yes   # убрать всё за собой, панель тоже
```

Если основной прокси занимает 80/443 и 18080/18443, прокси второго экземпляра не стартует: окружения будут в ошибке на шаге `proxy`.

### Порты 80/443

Если 80/443 заняты (nginx, Laravel Herd, Valet), Devopus берёт 18080/18443 и адреса становятся вида `https://demo.localhost:18443`. Кто занял порты, показывают `devopus status` и `devopus doctor`. Чтобы Devopus занял 80/443 и отдавал чужие хосты вашему dev-серверу:

```bash
# сначала перенесите dev-сервер на 8080/8443
devopus proxy coexist --http 8080 --https 8443
devopus proxy coexist --off           # вернуть как было
```

### Где что лежит

`~/.devopus` (или `$DEVOPUS_HOME`):

| Путь | Что |
|---|---|
| `state.json` | Желаемое состояние: проекты, окружения, фичи |
| `session.json`, `session.lock` | Вход на этом устройстве: пользователь, сессия, её сроки, где лежит refresh-токен; блокировка обновления токенов |
| `workspace.json` | Текущий воркспейс: id, имя, адрес его сервера |
| `refresh-token`, `device-id` | Refresh-токен, только если нет системного хранилища ключей (файл `0600`); случайный id устройства для лимитов Auth |
| `agent.sock`, `agent.lock`, `agent.log` | Локальный API, блокировка «один агент на каталог», лог |
| `ca/`, `certs/` | Локальный CA (`ca/root.pem` — то, чему доверяет система) и сертификаты окружений |
| `proxy/` | Ключи mTLS к Envoy и порт xDS |
| `src/` | Клоны модулей из других репозиториев и worktree фич |
| `envs/` | Отметки и логи хуков и сборок по окружениям |
| `lifecycle.json` | Активность фич, сон и пробуждение |
| `panel/` | Локальная панель: `port`, `token` (вход по ссылке `devopus panel open`), `panel.lock`, `panel.log` |

Контейнеры, тома и сети Devopus помечены `com.devopus.*`: `docker ps --filter label=com.devopus.managed=true`.

## 3а. Вход и Auth

Сервис Auth ([ADR-0014](../adr/0014-auth-service.md), [auth](architecture/auth.md)) — отдельный бинарник `cmd/devopus-auth`. Пока у продукта нет домена и хостинга, он работает на Mac разработчика: `127.0.0.1:7480`, письма с кодом пишутся в его лог.

| Что | Как |
|---|---|
| Запустить | `DEVOPUS_AUTH_DATABASE_URL=postgres://… go run ./cmd/devopus-auth run` — миграции применяются сами; `devopus-auth migrate` — только миграции |
| Настройки | Флаги или переменные: `--listen` (`DEVOPUS_AUTH_LISTEN`), `--issuer` — публичный адрес (`DEVOPUS_AUTH_ISSUER`), `--mail log\|smtp://user:pass@host:587` и `--mail-from` (`DEVOPUS_AUTH_MAIL`, `DEVOPUS_AUTH_MAIL_FROM`), `--trust-proxy` |
| Куда идёт CLI | `http://127.0.0.1:7480` по умолчанию; другой Auth — `DEVOPUS_AUTH_URL` или `--auth-url` у `register`/`login`. Адрес запоминается в сессии |
| Войти | `devopus register` (новый аккаунт), `devopus login` (браузер), `--email` (код в терминале), `--device` (код на другом устройстве); `devopus whoami`, `devopus auth sessions`, `devopus logout` |
| Где сессия | Refresh-токен — в Keychain (элемент `devopus`, аккаунт — каталог данных) или в Secret Service; без них — файл `refresh-token`. Выбрать явно: `DEVOPUS_CREDENTIAL_STORE=keychain\|secret-service\|file` |

CLI и агент на машине делят одну сессию: агент сам обновляет токены и показывает аккаунт в `devopus status`. У второго экземпляра (`DEVOPUS_HOME`) — свой вход.

### Сервер воркспейса

`devopus server run` — «пульт» с воркспейсами, участниками, ролями, правами, инвайтами и проектами ([ADR-0016](../adr/0016-workspace-server.md)). В разработке — на `127.0.0.1:7481` с отдельной базой рядом с базой Auth:

```bash
docker exec devopus-auth-pg psql -U postgres -c 'CREATE DATABASE server'
DEVOPUS_SERVER_DATABASE_URL='postgres://postgres:dev@127.0.0.1:5433/server?sslmode=disable' \
  devopus server run            # письма с инвайтами — в его выводе
devopus workspace create acme
devopus project create shop
devopus invite create lead@acme.dev --role teamlead --project shop
```

Демо-воркспейс одной командой — воркспейс `demo` с проектом из [examples/demo](../examples/demo):

```bash
devopus workspace demo
devopus project show demo
devopus service set demo php 8.4          # новая версия настроек
devopus project config history demo
devopus project config rollback demo 1    # назад, тоже новой версией
```

Агент берёт проекты **текущего воркспейса** устройства (`devopus workspace use`) с его сервера и поднимает их сам; `devopus status` показывает связь (`Link`). У демо приложения из папок (`path`) — агенту нужна папка с ними на этой машине:

```bash
devopus up demo --dir examples/demo   # один раз: папка запоминается
devopus down demo                     # остановить здесь; сервер его не перезапустит до 'devopus up'
```

Личные переопределения — `devopus.yaml` в папке проекта (той, что задана `--dir`, иначе `~/.devopus/projects/<проект>`), в `.gitignore`. Что применилось — `devopus project config show <проект> --effective`; расхождения команды — `devopus drift`.

Секреты — E2E ([ADR-0009](../adr/0009-secrets-key-distribution.md)): `devopus secrets set demo DB_PASSWORD` шифрует значение на этой машине. Первый секрет создаёт ключ воркспейса и один раз показывает код восстановления. Новому участнику ключ доставляет агент админа или тимлида, пока тот в сети; без него — `devopus secrets recover` с кодом. Ключ устройства хранится рядом с refresh-токеном (`device-key`, если нет Keychain).

Проект с сервера и проект папки (`workspace.yaml`) с одним именем не уживаются: агент оставляет проект папки и пишет об этом в лог. Освободить имя — `devopus down --volumes --project <имя>` в папке.

Веб-интерфейс воркспейса — тот же адрес в браузере (`http://127.0.0.1:7481`): вход кодом из письма (код — в выводе `devopus-auth`), участники, инвайты, права. Правки интерфейса — `cd web && npm run dev:workspace` (идёт к работающему серверу) и `npm run build` перед коммитом ([web/README.md](../web/README.md)).

Токены сервер проверяет по ключам Auth (`--auth-url`, по умолчанию `DEVOPUS_AUTH_URL`). CLI ходит на `DEVOPUS_SERVER_URL` (по умолчанию `http://127.0.0.1:7481`); текущий воркспейс — `~/.devopus/workspace.json`, сменить — `devopus workspace use <имя>`. Несколько участников на одной машине — разные `DEVOPUS_HOME`.

## 4. Разработка

### Тесты

```bash
go test ./...          # всё; тесты с Docker пропускаются, если runtime не отвечает
go test -race ./...    # перед коммитом
go vet ./...
```

Сквозные тесты (`internal/agent`) поднимают настоящие окружения в отдельном каталоге данных и убирают их за собой; идут несколько минут. На другом runtime:

```bash
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock go test ./internal/agent/...
DOCKER_HOST=unix://$HOME/.orbstack/run/docker.sock  go test ./internal/agent/...
```

`colima start` и `orb start` переключают Docker-контекст на себя — потом верните его: `docker context use desktop-linux`.

Тесты Auth, сервера воркспейса, `credentials` и CLI работают с настоящим PostgreSQL: `internal/pgtest` поднимает контейнер `postgres:17-alpine` на тестовый бинарник и создаёт базу на каждый тест; без Docker они пропускаются. Свой сервер вместо контейнера: `DEVOPUS_TEST_DATABASE_URL=postgres://…` (так их гоняет CI). Тест с настоящим Keychain — `DEVOPUS_TEST_KEYCHAIN=1 go test ./internal/credentials -run Keychain`.

Для замера NFR-2 (база 1 ГБ): `DEVOPUS_TEST_PG_SCALE=68 go test ./internal/agent -run TestFeatureData`.

### Протокол и панель

| После правок | Команда |
|---|---|
| `proto/` | `go tool -modfile=tools/go.mod buf generate`, затем `cd web && npm run gen` |
| `internal/auth/store/…`, `internal/server/store/…` (миграции, `queries.sql`) | `go tool -modfile=tools/go.mod sqlc generate -f internal/<auth\|server>/store/sqlc.yaml` (CI проверяет `sqlc diff`) |
| `web/` | `cd web && npm ci && npm run build` — и закоммитить `internal/panel/dist` вместе с исходниками (CI проверяет, что сборка совпадает) |
| Панель с горячей перезагрузкой | `cd web && npm run dev` — API идёт через работающую панель (`devopus panel start`) |

Подробнее о панели — [web/README.md](../web/README.md). Раскладка кода по пакетам — [план этапа 1](stage-1/plan.md#раскладка-репозитория).

### CI

`.github/workflows/ci.yml` на каждый push: `go vet` и `go test -race` на Ubuntu и macOS (тесты с Docker в CI выключены), тесты Auth и входа с сервисом Postgres, `buf lint` и совпадение `gen/` и кода sqlc, линт, типы и сборка панели, `goreleaser check`.

## 5. Релиз

Один раз (пока не сделано, см. [STATUS.md](../STATUS.md)):

1. Репозиторий `amalychev/devopus` публичный: `curl` и Homebrew скачивают релизы без авторизации.
2. Публичный репозиторий `amalychev/homebrew-tap` — туда CI кладёт cask.
3. Секрет `HOMEBREW_TAP_TOKEN` в `amalychev/devopus` — токен с правом записи в `amalychev/homebrew-tap`.

Проверить сборку локально, ничего не публикуя:

```bash
HOMEBREW_TAP_TOKEN=x goreleaser release --snapshot --clean
ls dist/   # архивы darwin/linux × amd64/arm64, checksums.txt, .deb, .rpm, homebrew/Casks/devopus.rb

# установка из локальной сборки так же, как через curl
DEVOPUS_BASE_URL=file://$PWD/dist DEVOPUS_BIN_DIR=/tmp/dvp-bin DEVOPUS_NO_SETUP=1 sh install.sh
```

Выпустить:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` соберёт всё, создаст **черновик** релиза на GitHub и обновит cask в tap. Проверьте черновик и опубликуйте его — после этого работают:

```bash
brew install amalychev/tap/devopus
curl -fsSL https://raw.githubusercontent.com/amalychev/devopus/main/install.sh | sh
```

Версия попадает в бинарник из тега (`devopus version`).

## 6. Неполадки

| Симптом | Что делать |
|---|---|
| `sign in first` (код 5) | `devopus login` или `devopus register`; Auth должен работать (§3а) |
| `Auth is not reachable` | `devopus-auth run` не запущен или `DEVOPUS_AUTH_URL` указывает не туда |
| Браузер ругается на сертификат | `devopus doctor --fix`. В связке ключей мог остаться корень от прежних названий (Docklet, Devcorb) — удалите его в «Связке ключей» |
| Адреса на `:18443` | 80/443 кто-то занял — `devopus status`; см. «Порты 80/443» |
| `the agent did not answer` | `devopus agent logs`. `bind: invalid argument` — слишком длинный `$DEVOPUS_HOME` |
| Агент или панель работают со старым кодом | `devopus agent restart`, `devopus panel restart` после пересборки |
| Окружение в `error` | `devopus status` показывает шаг и причину; логи — `devopus logs <модуль>`, логи хуков — в панели на вкладке «Logs» |
| `*.localhost` не резолвится | VPN или свой резолвер перехватывает `.localhost`; на macOS до 26 не поддерживается |
| Docker недоступен | Запустите Docker Desktop / `orb start` / `colima start`; `devopus doctor` покажет, к какому сокету идёт агент |
| Убрать всё с машины | `devopus agent uninstall --purge` — окружения с данными, службы агента и панели, CA, `~/.devopus` (worktree с несохранённой работой остаются) |
