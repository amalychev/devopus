# Локальная панель

Веб-панель агента для одного разработчика (веха M8): окружения проектов, прогресс по шагам, модули и ресурсы, логи, данные, создание фичи, sleep/wake/удаление. React + TypeScript, Vite, Tailwind CSS v4, shadcn/ui (Base UI), клиент — Connect-ES к локальному API агента (`proto/devopus/agent/v1`).

Агент раздаёт собранную панель сам: `npm run build` кладёт её в `internal/agent/ui/dist`, оттуда она встраивается в бинарник (`go:embed`). Сборка закоммичена, чтобы `go build` не требовал Node; после правок в `web/` — пересобрать и закоммитить `dist` вместе с исходниками.

| Команда | Что |
|---|---|
| `npm run dev` | Vite с горячей перезагрузкой; API проксируется в работающий агент (порт и токен — из `~/.devopus/panel-port` и `panel-token`, или из `$DEVOPUS_HOME`) |
| `npm run build` | Проверка типов и сборка в `internal/agent/ui/dist` |
| `npm run gen` | TypeScript из `proto/` (`buf.gen.web.yaml` → `src/gen`) |
| `npm run lint` | ESLint |

Открыть панель — `devopus ui`: ссылка с токеном ставит cookie (`HttpOnly`, `SameSite=Strict`), без неё API отвечает 401. Агент слушает панель только на `127.0.0.1` и отклоняет запросы с чужим `Host` (защита от DNS rebinding).
