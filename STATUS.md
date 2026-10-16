# Где остановились и что дальше

**Обновлено:** 16.10.2026

## Где мы

**Этап 1 (MVP): все 9 вех готовы**, осталось выпустить первый релиз и отдать пилотным командам. Готовы вехи M1 (`workspace.yaml` v1, импорт/экспорт compose) и M2 (агент: локальный API, желаемое состояние, reconcile-цикл, сеть окружения). Готова M3 (сеть и прокси: Envoy через xDS/SDS, CA, CoreDNS, порты 80/443). Готова **M4 (базовое окружение)**: сервисы, модули из папки, `repo` или Dockerfile, хуки `install` → `migrate` с повтором по изменению входов, маршруты по путям и `expose`, CA и DNS в контейнерах модулей, `logs`, `exec`; демо из трёх модулей ([examples/demo](examples/demo)) поднимается одной командой; проверено на Docker Desktop, Colima и OrbStack. Готова **M5 (фичи)**: `devopus feature create PAY-1 -m api` поднимает фичу на `https://pay-1.<домен>` с worktree на ветке `feature/PAY-1`, модулями, которые используют форк, и своими экземплярами сервисов; остальное фича берёт у main через прокси по тем же логическим именам; `feature add/list/rm/path`; изоляция двух фич и непрерывность main проверены интеграционным тестом на Docker Desktop, Colima и OrbStack. Готова **M6 (данные)**: фича получает свои Postgres и MySQL с данными main — копией золотой копии, снятой с работающего main без остановки; фича с базой 1 ГБ поднимается за 9,4 с (NFR-2: < 60 с); `db reset`, `db golden refresh`, `db lag`. Готова **M7 (жизненный цикл)**: фича засыпает без запросов и правок и просыпается первым запросом (20 параллельных — все доходят, около 1 с сверх старта приложения), `feature sleep/wake`, бюджет `max_running`, подсказка удалить фичу со слитой веткой. Готова **M8 (локальная панель)**: `devopus panel open` открывает локальную панель на `127.0.0.1` (вход по токену; с 16.10.2026 — отдельный компонент): проекты деревом, прогресс шагов в реальном времени, модули с CPU и памятью, логи модулей и хуков, данные с отставанием и сбросом, создание фичи с планом изоляции, sleep/wake/удаление; сценарий US-4 пройден из панели. Исходники — [web/](web/README.md), сборка встроена в бинарник. Готова **M9 (установка и дистрибуция)**: `devopus` — единая точка управления, агент и панель — независимые компоненты со своими командами ([ADR-0012](adr/0012-devopus-cli-control-point.md)); `devopus agent install` делает агента службой пользователя (launchd agent / systemd user unit), прогоняет `doctor` и предлагает доверить CA, `devopus panel install` — так же панель; `devopus agent uninstall [--purge]` возвращает всё обратно; CLI сам перезапускает агента после обновления; GoReleaser собирает архивы, `.deb`, `.rpm` и cask Homebrew, `install.sh` ставит через `curl … | sh`; CI — `.github/workflows/`. Не сделано: первый публичный релиз (нужен публичный репозиторий `devopus/devopus` и `devopus/homebrew-tap`), замер time-to-first-page на чистой машине, нотаризация macOS; телеметрия отложена до этапа 2. **Все 9 вех этапа 1 готовы** — дальше пилоты. План, журнал и открытые вопросы (M4: relay для curl внутри модулей, `cron`, Linux; M5: команды из worktree, gRPC к общим модулям; M6: сиды, MariaDB, диск без reflink; M7: `cleanup: after`, squash-merge, WebSocket к спящей фиче; M8: Playwright, `exec` в панели; M9: первый релиз, нотаризация, чистая машина, Linux) — [docs/stage-1/plan.md](docs/stage-1/plan.md).

**Этап 0 (валидация) — техническая часть закрыта, серия интервью закрыта 16.10.2026, новых не будет.**

Итоги — [docs/stage-0/findings.md](docs/stage-0/findings.md): тимлиды хотят, чтобы окружением всей команды управлял один человек и у всех оно было одинаковым; онбординг «вошёл → `brew`/`curl` → проект развернулся сам»; предложение управлять ещё staging и production с агентом на хостинге (кандидат в roadmap, риск R20). Вердикты качественные, без цифр; непроверенное (параллельные ветки, AI-агенты, цена, безопасность) — на пилотах.

Критерий перехода к этапу 1 — **5 команд подписали LOI**. Сейчас: 0 LOI.

Сделано:
- Документация, спецификации, roadmap, ADR — `docs/`, `specs/`, `adr/`, [roadmap.md](roadmap.md).
- Все 8 технических прототипов (P-1…P-8) с отчётами — `prototypes/`. Сводка — [docs/stage-0/report.md](docs/stage-0/report.md).
- Проверено на Docker Desktop, Colima и OrbStack (macOS 26, Apple Silicon).
- Выводы прототипов внесены в [PRD MVP](specs/mvp-prd.md#решения-из-прототипов-этапа-0) и [спецификацию `workspace.yaml`](specs/workspace-yaml.md).
- Итоги интервью — [docs/stage-0/findings.md](docs/stage-0/findings.md); комплект для пилотов — [docs/stage-0/](docs/stage-0/README.md).
- Решения: лицензия агента — Apache 2.0 (ADR-0010), название — Devopus.

## Что делать дальше

### Владелец продукта — блокирует переход к этапу 1

1. **Предложить пилот** подходящим командам ([pilot.md](docs/stage-0/pilot.md)) и **подписать 5 LOI** ([loi.md](docs/stage-0/loi.md)).
2. **Попросить у пилотных команд compose-файлы** (без секретов).
3. **Решить по staging/production** — брать ли направление ([roadmap](roadmap.md#кандидаты-по-итогам-интервью)).
4. ~~**Первый релиз**~~ ✅ [v0.1.0](https://github.com/amalychev/devopus/releases/tag/v0.1.0) опубликован 16.10.2026: `brew install amalychev/tap/devopus` и `curl -fsSL https://raw.githubusercontent.com/amalychev/devopus/main/install.sh | sh` работают (проверено на macOS и в Debian). Остаётся адрес maintainer'а для пакетов.

### Claude — по мере появления данных

| Когда | Что |
|---|---|
| Есть пилотные команды | Закрыть ADR-0008 «где хранить `workspace.yaml`» по их репозиториям |
| Есть пилотные команды | Решить по гибридному режиму (приложение на хосте) и по Windows (риск R15) — правки в roadmap и PRD |
| Пришли compose-файлы команд | Повторить прогон P-8 (`prototypes/p8-compose-import`) на их файлах |
| 5 LOI подписаны | Этап 0 закрыт формально (разработка этапа 1 уже идёт) |
| Пришли compose-файлы команд | Прогнать `devopus init` на них (как корпус P-8) |
| Есть чистая macOS (VM или новый пользователь) | Замерить time-to-first-page с `brew install amalychev/tap/devopus` |
| Есть Linux-машина | Проверить службу systemd, порты, CA, DNS и сквозные тесты на Linux; включить их в CI |

### Не блокирует, но нужно до этапа 1–2

- **Домен** продукта — до этапа 2 (превью-ссылки, wildcard-fallback).
- **Оставшиеся проверки:** Linux-хост (нужен доступ к машине; там xDS на `127.0.0.1` хоста из контейнера не виден — решить при проверке), старые версии macOS, Chrome, Safari и Firefox по отдельности (CA уже в связке ключей).

## Состояние окружения

- Docker-контекст: `desktop-linux`.
- Репозиторий: `origin` — git@github.com:amalychev/devopus.git (публичный с 16.10.2026, лицензия Apache 2.0); старый `amalychev/docklet` — remote `docklet`. Релизы и cask Homebrew — `amalychev/devopus` и `amalychev/homebrew-tap`.
- Colima и OrbStack установлены, VM остановлены (`colima start` / `orb start`, если понадобятся). `colima start` и `orb start` переключают Docker-контекст на себя — после них вернуть `docker context use desktop-linux`. Тесты на них запускать через `DOCKER_HOST` (`~/.colima/default/docker.sock`, `~/.orbstack/run/docker.sock`).
- На этой машине 80/443 занимает brew-nginx, поэтому прокси Devopus работает на 18080/18443 (fallback).
- Агента, панели и демо на машине сейчас нет: всё удалено 16.10.2026 для проверки с нуля (`devopus agent install` → `devopus panel open`). Панель — отдельный компонент (`devopus panel …`, [ADR-0012](adr/0012-devopus-cli-control-point.md)); после правок в `web/` — `npm run build` в `web/` и пересобрать бинарник: панель встроена в него.
- `~/.devopus` создан, агент может работать в фоне (`pkill -f 'devopus agent run'` — остановить). Демо (`examples/demo`) поднято этим агентом на `https://demo.localhost:18443`, фич нет. **CA `~/.devopus/ca/root.pem` не в связке ключей**: там корень от прежнего названия (Docklet). Для проверки в браузере — `devopus doctor --fix` (и удалить старый корень «Docklet» в «Связке ключей»).
- От названия Devcorb остались сеть `demo-env-rcyd8t` и тома `demo-rcyd8t-pgdata`, `demo-rcyd8t-venv` (контейнеры `devcorb-proxy` и DNS удалены 15.10.2026 — держали 18080/18443). Агент Devopus их не видит; удалить вручную, если не нужны.
- `~/.docklet` и бинарник `docklet` в старом scratchpad — остатки до переименования. Агент `docklet` (держал 18080/18443) остановлен 15.10.2026, его контейнеры удалены; `~/.docklet/state.json` не тронут.
- Опубликованный roadmap: https://claude.ai/artifact/XXRWjdCqmh1wb7WVczCjK7
