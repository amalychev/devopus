# Devcorb — платформа локальных dev-окружений

> Название продукта — **Devcorb** (решено 14.10.2026; домен ещё не выбран). Источники: «Платформа локальных dev-окружений: архитектура и онбординг» и «Технические проблемы платформы локальных dev-окружений и их решения» (оба от 14.10.2026).

**Одной фразой:** параллельные изолированные окружения на каждую ветку и каждого AI-агента, с данными, для всей команды — локально в Docker, с единой веб-панелью.

Разработчик ставит агента, вся настройка стека живёт в `workspace.yaml` и веб-панели (как в Railway). Каждая ветка получает свой worktree, свой стек и свой хост: `pay-123.shop.localhost`.

> **Где остановились и что дальше — [STATUS.md](STATUS.md).**

## Карта документации

| Раздел | Файл | Что внутри |
|---|---|---|
| Продукт | [docs/product/vision.md](docs/product/vision.md) | Позиционирование, ЦА, конкуренты, бизнес-модель, метрики |
| | [docs/product/glossary.md](docs/product/glossary.md) | Термины — единственный источник значений слов |
| | [docs/product/user-flows.md](docs/product/user-flows.md) | Онбординг, создание фичи, жизненный цикл, расхождения |
| Архитектура | [docs/architecture/overview.md](docs/architecture/overview.md) | Компоненты, деплои, потоки, модель данных |
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
| | [specs/cli.md](specs/cli.md) | CLI агента (MVP) |
| | [specs/mvp-prd.md](specs/mvp-prd.md) | PRD этапа 1 (MVP): user stories и критерии приёмки |
| | [specs/nfr.md](specs/nfr.md) | Нефункциональные требования и как их мерить |
| План | [roadmap.md](roadmap.md) | Этапы 0–4, критерии перехода, P0/P1/P2 |
| | [docs/stage-0-plan.md](docs/stage-0-plan.md) | Валидация: интервью + технические прототипы |
| | [docs/stage-0/](docs/stage-0/README.md) | **Итоговый отчёт этапа 0** и комплект для интервью и пилотов |
| | [docs/stage-1/plan.md](docs/stage-1/plan.md) | **Этап 1 (MVP):** вехи M1–M9, раскладка кода, журнал |
| Код | `cmd/`, `internal/`, `schema/` | Агент и CLI на Go (см. «Разработка» ниже) |
| Прототипы | [prototypes/](prototypes/) | P-1…P-8: код и отчёты технических прототипов этапа 0 |
| Решения | [adr/](adr/README.md) | Architecture Decision Records |
| Риски | [risks-and-open-questions.md](risks-and-open-questions.md) | Риски, открытые вопросы, найденные противоречия в источниках |

## Главные принципы

1. **Изоляция прежде оптимизации.** Сначала доказываем, что две фичи не портят данные и состояние друг друга (Docker, git, БД), затем стабильную маршрутизацию, и только потом — экономию ресурсов через общие сервисы и W3C Baggage.
2. **Желаемое состояние, а не команды.** Сервер передаёт агенту, *что должно быть*, агент приводит Docker к этому состоянию. Потеря сообщения ничего не ломает.
3. **Код и данные не покидают машину.** Платформа не видит исходники и базы окружений; секреты зашифрованы ключом команды.
4. **Без lock-in.** Конфиг — файл в git, двусторонне синхронизируется с панелью; импорт/экспорт `docker-compose.yml`; агент open-source.
5. **Критерий, а не дата.** Переход между этапами roadmap — по критерию валидации.

## Статус

Этап 1 (MVP) начат параллельно с продуктовой валидацией этапа 0 (20 интервью и 5 LOI ещё впереди — см. [итоговый отчёт этапа 0](docs/stage-0/report.md)). Готовы вехи M1 (`workspace.yaml` v1, импорт/экспорт compose) и M2 (каркас агента). План — [docs/stage-1/plan.md](docs/stage-1/plan.md).

## Разработка

Нужен Go 1.27+. Тесты, которым нужен Docker (сквозной тест агента, `docker compose config` для экспорта), без него пропускаются.

```bash
go test ./...                          # все тесты
go build -o devcorb ./cmd/devcorb      # бинарник

./devcorb init                         # workspace.yaml из compose-файла в текущей папке
./devcorb validate                     # проверка по схеме и правилам, с номерами строк
./devcorb export compose -o out.yml    # обратно в docker-compose
./devcorb schema                       # JSON Schema для редактора
./devcorb up                           # поднять базовое окружение (стартует агента в фоне)
./devcorb status                       # агент, runtime, окружения
./devcorb down --volumes               # удалить окружения проекта

go tool -modfile=tools/go.mod buf generate   # после правок proto/
```

Данные агента — в `~/.devcorb` (или `$DEVCORB_HOME`).
