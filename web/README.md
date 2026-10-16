# Локальная панель

Веб-панель агента для одного разработчика (веха M8): окружения проектов, прогресс по шагам, модули и ресурсы, логи, данные, создание фичи, sleep/wake/удаление. React + TypeScript, Vite, Tailwind CSS v4, shadcn/ui (Base UI), клиент — Connect-ES к локальному API агента (`proto/devopus/agent/v1`).

Панель — отдельный процесс `devopus panel run` ([ADR-0012](../adr/0012-devopus-cli-control-point.md)): он отдаёт собранное приложение и пересылает его запросы агенту по unix-сокету. `npm run build` кладёт сборку в `internal/panel/dist`, оттуда она встраивается в бинарник (`go:embed`). Сборка закоммичена, чтобы `go build` не требовал Node; после правок в `web/` — пересобрать и закоммитить `dist` вместе с исходниками.

| Команда | Что |
|---|---|
| `npm run dev` | Vite с горячей перезагрузкой; API идёт через работающую панель (`devopus panel start`; порт и токен — из `~/.devopus/panel/port` и `token`, или из `$DEVOPUS_HOME/panel`) |
| `npm run build` | Проверка типов и сборка в `internal/panel/dist` |
| `npm run gen` | TypeScript из `proto/` (`buf.gen.web.yaml` → `src/gen`) |
| `npm run lint` | ESLint |

Открыть панель — `devopus panel open`: ссылка с токеном ставит cookie (`HttpOnly`, `SameSite=Strict`), без неё API отвечает 401. Панель слушает только `127.0.0.1` и отклоняет запросы с чужим `Host` (защита от DNS rebinding).
