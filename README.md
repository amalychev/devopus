# Devopus — централизованное управление окружением разработки команды

> Название продукта — **Devopus** (решено 14.10.2026; домен ещё не выбран). Источники: «Платформа локальных dev-окружений: архитектура и онбординг» и «Технические проблемы платформы локальных dev-окружений и их решения» (оба от 14.10.2026).

**Одной фразой:** админ и тимлиды управляют серверной средой команды — проекты, приложения, сервисы и их версии, — а у каждого разработчика она сама появляется на ПК, с отдельным окружением на каждую ветку и каждого AI-агента.

- **Воркспейс** — компания админа (Google); у админа их может быть несколько. Настройки и веб-панель — на сервере Devopus, self-hosted — позже.
- **Проект** — продукт (Google Drive); **приложения** — его репозитории (frontend, backend, admin); **сервисы** — php, nodejs, mysql, redis, typesense, queue, cronjob и т. д.: живут в проекте и подключаются к одному или нескольким приложениям.
- Приложения разворачивают **агенты** на ПК разработчиков (позже — и на серверах админа для staging и production).
- **Роли:** админ, тимлид проекта, разработчик с правами по выдаче. Без входа (`devopus login`, email + одноразовый код) ничего не работает.
- **Всё через команду `devopus`**, а затем то же в веб-интерфейсе. Новичок: принял инвайт → вошёл → поставил Devopus → проекты развернулись сами.

Модель — [vision](docs/product/vision.md), [роли](docs/product/roles.md), [roadmap](roadmap.md). Сейчас готов локальный движок (v0.1.0); аккаунты, воркспейсы и сервер — этапы 2–4.

> **Где остановились и что дальше — [STATUS.md](STATUS.md).**

## Карта документации

| Раздел | Файл | Что внутри |
|---|---|---|
| Продукт | [docs/product/vision.md](docs/product/vision.md) | Идея и модель: воркспейс-сервер, проекты, сервисы; ЦА, конкуренты, тарифы, метрики |
| | [docs/product/roles.md](docs/product/roles.md) | Роли (админ, тимлид, разработчик), права, инвайты |
| | [docs/product/glossary.md](docs/product/glossary.md) | Термины — единственный источник значений слов |
| | [docs/product/user-flows.md](docs/product/user-flows.md) | Регистрация, воркспейс, проекты и сервисы, инвайт, онбординг, раскатка изменений, переопределения, фичи, staging |
| Архитектура | [docs/architecture/overview.md](docs/architecture/overview.md) | Компоненты, деплои, потоки, модель данных |
| | [docs/architecture/auth.md](docs/architecture/auth.md) | Auth: регистрация и вход по email + коду, токены, сессии |
| | [docs/architecture/agent.md](docs/architecture/agent.md) | Устройство локального агента, reconcile-цикл |
| | [docs/architecture/routing.md](docs/architecture/routing.md) | Три слоя маршрутизации, Simple / Contextual Isolation |
| | [docs/architecture/data.md](docs/architecture/data.md) | Клонирование БД, золотая копия, общие и собственные данные |
| | [docs/architecture/local-networking.md](docs/architecture/local-networking.md) | Порты 80/443, DNS `*.localhost`, HTTPS/CA, IPv6 |
| | [docs/architecture/docker-isolation.md](docs/architecture/docker-isolation.md) | Неймспейсы, сети, volumes, runtime'ы |
| | [docs/architecture/windows.md](docs/architecture/windows.md) | Двухкомпонентный агент, WSL2 |
| | [docs/architecture/security.md](docs/architecture/security.md) | Модель угроз, E2E-секреты, приватность, compliance |
| | [docs/architecture/tech-stack.md](docs/architecture/tech-stack.md) | Технологии с обоснованием |
| Спецификации | [specs/workspace-yaml.md](specs/workspace-yaml.md) | Формат конфига проекта v1 |
| | [specs/agent-protocol.md](specs/agent-protocol.md) | Протокол агент ↔ сервер |
| | [specs/cli.md](specs/cli.md) | Команда `devopus` — единая точка управления: агент, панель, проекты, фичи |
| | [specs/mvp-prd.md](specs/mvp-prd.md) | PRD этапа 1 (MVP): user stories и критерии приёмки |
| | [specs/nfr.md](specs/nfr.md) | Нефункциональные требования и как их мерить |
| План | [roadmap.md](roadmap.md) | Этапы 0–4, критерии перехода, P0/P1/P2 |
| | [docs/stage-0-plan.md](docs/stage-0-plan.md) | Валидация: интервью + технические прототипы |
| | [docs/stage-0/](docs/stage-0/README.md) | **Итоговый отчёт этапа 0**, итоги интервью, комплект для пилотов |
| | [docs/refactoring-plan.md](docs/refactoring-plan.md) | **План рефакторинга** v0.1.0 под новую модель: шаги R0–R11, раскладка кода |
| | [docs/stage-1/plan.md](docs/stage-1/plan.md) | **Этап 1 (MVP):** вехи M1–M9, раскладка кода, журнал |
| Код | `cmd/`, `internal/`, `schema/` | Агент и CLI на Go (см. «Разработка» ниже) |
| | [docs/development.md](docs/development.md) | **Как развернуть и запустить Devopus:** сборка, агент, демо, тесты, релиз, неполадки |
| Прототипы | [prototypes/](prototypes/) | P-1…P-8: код и отчёты технических прототипов этапа 0 |
| Решения | [adr/](adr/README.md) | Architecture Decision Records |
| Риски | [risks-and-open-questions.md](risks-and-open-questions.md) | Риски, открытые вопросы, найденные противоречия в источниках |

## Главные принципы

0. **Сначала вход, потом всё остальное; права решает сервер воркспейса.** Источник правды о серверной среде — сервер воркспейса, а не файл в репозитории ([ADR-0013](adr/0013-config-on-workspace-server.md)).
1. **Изоляция прежде оптимизации.** Сначала доказываем, что две фичи не портят данные и состояние друг друга (Docker, git, БД), затем стабильную маршрутизацию, и только потом — экономию ресурсов через общие сервисы и W3C Baggage.
2. **Желаемое состояние, а не команды.** Сервер передаёт агенту, *что должно быть*, агент приводит Docker к этому состоянию. Потеря сообщения ничего не ломает.
3. **Код и данные не покидают машину.** Платформа не видит исходники и базы окружений; секреты зашифрованы ключом команды.
4. **Без lock-in.** Импорт и экспорт `docker-compose.yml`; агент open-source; self-hosted сервер воркспейса.
5. **Критерий, а не дата.** Переход между этапами roadmap — по критерию валидации.

## Статус

Этап 1 — **локальный движок** — готов и выпущен ([v0.1.0](https://github.com/amalychev/devopus/releases/tag/v0.1.0), [план](docs/stage-1/plan.md)): агент, окружения из описания стека, фичи с копиями данных, сон и пробуждение, локальная панель, установка. Пока он работает с `workspace.yaml` в репозитории — попробовать: [examples/demo](examples/demo). Дальше — **этап 2: аккаунты и воркспейсы** ([roadmap](roadmap.md)).

## Установка

```bash
brew install amalychev/tap/devopus                                            # macOS
curl -fsSL https://raw.githubusercontent.com/amalychev/devopus/main/install.sh | sh  # macOS и Linux
devopus agent install                  # агент — служба пользователя, проверки, доверие к CA
devopus panel open                     # локальная веб-панель (или devopus panel install — службой)
devopus agent uninstall [--purge]      # всё обратно; --purge — с данными окружений и панелью
devopus --help                         # все команды по группам
```

Пакеты `.deb` и `.rpm` — в [релизах на GitHub](https://github.com/amalychev/devopus/releases). Релизы публикуются из `amalychev/devopus`, cask Homebrew — в `amalychev/homebrew-tap`.

## Разработка

Полная инструкция — [docs/development.md](docs/development.md). Коротко: нужен Go 1.27+. Тесты, которым нужен Docker (сквозной тест агента, `docker compose config` для экспорта), без него пропускаются.

```bash
go test ./...                          # все тесты
go build -o devopus ./cmd/devopus      # бинарник

./devopus init                         # workspace.yaml из compose-файла в текущей папке
./devopus validate                     # проверка по схеме и правилам, с номерами строк
./devopus export compose -o out.yml    # обратно в docker-compose
./devopus schema                       # JSON Schema для редактора
./devopus up                           # поднять базовое окружение (стартует агента в фоне)
./devopus status                       # агент, runtime, прокси, окружения и их модули
./devopus open                         # открыть окружение в браузере
./devopus panel open                   # локальная панель: окружения, прогресс, логи, фичи
./devopus routes                       # таблица маршрутов прокси
./devopus logs api --follow            # вывод модуля или сервиса
./devopus exec api -- sh               # команда в контейнере модуля
./devopus feature create PAY-1 --apps api  # фича: worktree на feature/PAY-1, свой URL https://pay-1.<домен>
./devopus feature add PAY-1 web        # форкнуть ещё приложение
./devopus feature list                 # фичи проекта: фаза, URL, форки, что общее с main
./devopus feature path PAY-1           # папка кода фичи (для IDE и AI-агентов)
./devopus feature sleep PAY-1          # усыпить (сама засыпает после features.sleep_after); первый запрос будит
./devopus feature wake PAY-1           # разбудить заранее
./devopus feature rm PAY-1             # удалить; откажет при несохранённой работе без --force
./devopus logs PAY-1 api               # окружение — ID, слаг или имя фичи
./devopus db lag                       # копии баз фич и золотые копии main
./devopus db reset PAY-1               # заново скопировать данные main в фичу
./devopus db golden refresh            # снять золотые копии main с текущими данными
./devopus doctor --fix                 # проверки; доверие к локальному CA
./devopus down                         # остановить (данные остаются)
./devopus down --volumes               # удалить окружения проекта с данными
./devopus agent install                # агент службой (собранный бинарник — не из go run)
./devopus agent restart                # после пересборки; так же devopus panel restart

go tool -modfile=tools/go.mod buf generate   # после правок proto/
(cd web && npm ci && npm run gen && npm run build)  # после правок proto/ или web/: TypeScript и сборка панели
goreleaser release --snapshot --clean  # локальная сборка релиза в dist/: архивы, .deb, .rpm, cask
```

Данные агента — в `~/.devopus` (или `$DEVOPUS_HOME`).
