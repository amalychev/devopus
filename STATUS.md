# Где остановились и что дальше

**Обновлено:** 15.10.2026

## Где мы

**Этап 1 (MVP) начат параллельно с продуктовой частью этапа 0.** Готовы вехи M1 (`workspace.yaml` v1, импорт/экспорт compose) и M2 (агент: локальный API, желаемое состояние, reconcile-цикл, сеть окружения). Готова M3 (сеть и прокси: Envoy через xDS/SDS, CA, CoreDNS, порты 80/443). Готова **M4 (базовое окружение)**: сервисы, модули из папки, `repo` или Dockerfile, хуки `install` → `migrate` с повтором по изменению входов, маршруты по путям и `expose`, CA и DNS в контейнерах модулей, `logs`, `exec`; демо из трёх модулей ([examples/demo](examples/demo)) поднимается одной командой; проверено на Docker Desktop, Colima и OrbStack. Готова **M5 (фичи)**: `devopus feature create PAY-1 -m api` поднимает фичу на `https://pay-1.<домен>` с worktree на ветке `feature/PAY-1`, модулями, которые используют форк, и своими экземплярами сервисов; остальное фича берёт у main через прокси по тем же логическим именам; `feature add/list/rm/path`; изоляция двух фич и непрерывность main проверены интеграционным тестом на Docker Desktop, Colima и OrbStack. Следующая — **M6: данные** (золотая копия, клон Postgres и MySQL вместо пустых экземпляров, `db lag/reset/golden refresh`). План, журнал и открытые вопросы (M4: relay для curl внутри модулей, `cron`, Linux; M5: команды из worktree, gRPC к общим модулям) — [docs/stage-1/plan.md](docs/stage-1/plan.md).

**Этап 0 (валидация) — техническая часть закрыта, продуктовая не начата.**

Критерий перехода к этапу 1 — **5 команд подписали LOI**. Сейчас: 0 интервью, 0 LOI.

Сделано:
- Документация, спецификации, roadmap, ADR — `docs/`, `specs/`, `adr/`, [roadmap.md](roadmap.md).
- Все 8 технических прототипов (P-1…P-8) с отчётами — `prototypes/`. Сводка — [docs/stage-0/report.md](docs/stage-0/report.md).
- Проверено на Docker Desktop, Colima и OrbStack (macOS 26, Apple Silicon).
- Выводы прототипов внесены в [PRD MVP](specs/mvp-prd.md#решения-из-прототипов-этапа-0) и [спецификацию `workspace.yaml`](specs/workspace-yaml.md).
- Комплект для интервью и пилотов — [docs/stage-0/](docs/stage-0/README.md).
- Решения: лицензия агента — Apache 2.0 (ADR-0010), название — Devopus.

## Что делать дальше

### Владелец продукта — блокирует переход к этапу 1

1. **Набрать кандидатов** по [screener.md](docs/stage-0/screener.md): 30–40 человек, квоты 8 продуктовых / 6 агентств / 6 AI-heavy.
2. **Разослать приглашения** по [outreach.md](docs/stage-0/outreach.md), вести воронку в [tracker.md](docs/stage-0/tracker.md).
3. **Провести 20 интервью** по [interview-guide.md](docs/stage-0/interview-guide.md). Карточку из [synthesis.md](docs/stage-0/synthesis.md) — в тот же день, в `docs/stage-0/interviews/NN-компания.md`.
4. **После 10 интервью — промежуточный синтез.** Если боль онбординга и параллельной работы обе ≤ 2 из 5 — остановиться и пересмотреть позиционирование.
5. **Предложить пилот** подходящим командам ([pilot.md](docs/stage-0/pilot.md)) и **подписать 5 LOI** ([loi.md](docs/stage-0/loi.md)).
6. **Попросить у пилотных команд compose-файлы** (без секретов).

### Claude — по мере появления данных

| Когда | Что |
|---|---|
| Появились карточки интервью | Свести `docs/stage-0/findings.md`: вердикт по H1–H7 с цифрами по правилам из [hypotheses.md](docs/stage-0/hypotheses.md) |
| По итогам интервью (вопрос Г5) | Закрыть ADR-0008 «где хранить `workspace.yaml`» |
| По итогам интервью (Г2, А7) | Решить по гибридному режиму (приложение на хосте) и по Windows (риск R15) — правки в roadmap и PRD |
| Пришли compose-файлы команд | Повторить прогон P-8 (`prototypes/p8-compose-import`) на их файлах |
| 5 LOI подписаны | Этап 0 закрыт формально (разработка этапа 1 уже идёт) |
| Пришли compose-файлы команд | Прогнать `devopus init` на них (как корпус P-8) |

### Не блокирует, но нужно до этапа 1–2

- **Домен** продукта — до этапа 2 (превью-ссылки, wildcard-fallback).
- **Оставшиеся проверки:** Linux-хост (нужен доступ к машине; там xDS на `127.0.0.1` хоста из контейнера не виден — решить при проверке), старые версии macOS, Chrome, Safari и Firefox по отдельности (CA уже в связке ключей).

## Состояние окружения

- Docker-контекст: `desktop-linux`.
- Репозиторий: https://github.com/amalychev/devopus (приватный).
- Colima и OrbStack установлены, VM остановлены (`colima start` / `orb start`, если понадобятся). `colima start` и `orb start` переключают Docker-контекст на себя — после них вернуть `docker context use desktop-linux`. Тесты на них запускать через `DOCKER_HOST` (`~/.colima/default/docker.sock`, `~/.orbstack/run/docker.sock`).
- На этой машине 80/443 занимает brew-nginx, поэтому прокси Devopus работает на 18080/18443 (fallback).
- `~/.devopus` создан, агент может работать в фоне (`pkill -f 'devopus agent run'` — остановить). Демо (`examples/demo`) поднято этим агентом на `https://demo.localhost:18443`, фич нет. **CA `~/.devopus/ca/root.pem` не в связке ключей**: там корень от прежнего названия (Docklet). Для проверки в браузере — `devopus doctor --fix` (и удалить старый корень «Docklet» в «Связке ключей»).
- От названия Devcorb остались сеть `demo-env-rcyd8t` и тома `demo-rcyd8t-pgdata`, `demo-rcyd8t-venv` (контейнеры `devcorb-proxy` и DNS удалены 15.10.2026 — держали 18080/18443). Агент Devopus их не видит; удалить вручную, если не нужны.
- `~/.docklet` и бинарник `docklet` в старом scratchpad — остатки до переименования. Агент `docklet` (держал 18080/18443) остановлен 15.10.2026, его контейнеры удалены; `~/.docklet/state.json` не тронут.
- Опубликованный roadmap: https://claude.ai/artifact/XXRWjdCqmh1wb7WVczCjK7
