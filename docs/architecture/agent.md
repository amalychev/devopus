# Локальный агент

Один бинарник на Go под macOS, Linux (и Windows через WSL2 с этапа 3). Работает фоновой службой (launchd / systemd / Windows Service через `kardianos/service`). Open-source.

## Состав

| Подсистема | Ответственность |
|---|---|
| **Reconciler** | Сравнивает DesiredState с фактическим и выполняет шаги до сходимости |
| **Runtime adapter** | Слой над Docker Engine API (официальный Go-клиент) + `compose-go`; адаптеры Docker Desktop, OrbStack, Colima, Podman |
| **Git** | Системный `git` через exec: clone, worktree add/remove, проверка грязного состояния |
| **Proxy control-plane** | `go-control-plane`: отдаёт Envoy маршруты (xDS) и сертификаты (SDS) без перезапуска |
| **CA** | Локальный CA с Name Constraints, выпуск сертификатов на каждый хост, установка доверия |
| **DNS** | Локальный DNS для зоны `.localhost` на хосте; CoreDNS-контейнер для docker-сетей |
| **Data** | Золотые копии, клонирование БД, снапшоты |
| **Lifecycle** | Детектор активности (запросы через Envoy + fs-события worktree), усыпление, пробуждение, бюджет ресурсов |
| **Preflight / Doctor** | Проверки окружения ОС до старта и при смене сети |
| **Cloud link** | Исходящий WebSocket к Agent gateway (этап 2+); в MVP — выключен |
| **Local API / UI** | Локальная панель для одного разработчика (MVP); CLI говорит с агентом через сокет |
| **MCP server** | Официальный Go SDK; инструменты для AI-агентов (этап 3) |
| **Updater** | Автообновление подписанных релизов (cosign) |

## Разделение привилегий

Агент работает **без root**. Всё, что требует прав, вынесено в минимальный privileged helper:

| ОС | Helper | Задачи |
|---|---|---|
| macOS | launchd-демон (root) | Открывает `127.0.0.1:80/443` и `[::1]:80/443`, передаёт сокеты через socket activation; `/etc/resolver/localhost`; установка CA |
| Linux | — (setcap на Envoy) / one-shot sudo | `cap_net_bind_service`; DNS через systemd-resolved; CA |
| Windows | Windows Service | NRPT, DNS на :53, hosts (fallback), CA, резервирование портов, Defender-исключения |

Helper принимает только фиксированный набор типизированных запросов от агента по локальному сокету.

## Reconcile-цикл

```
loop:
  desired := cache.Load()  // локальный файл (MVP) или последний DesiredState из облака
  actual  := observe()     // Docker labels com.docklet.env=<id>, git worktree list, Envoy config
  plan    := diff(desired, actual)
  for step in plan (с учётом зависимостей):
      execute(step)        // идемпотентно
      report(progress)
  gc()                     // ресурсы с метками без записи в desired
```

Требования:
- **Идемпотентность каждого шага.** Повторный запуск после краша приводит к тому же результату.
- **Наблюдение по меткам.** Всё, что создаёт агент, помечено `com.docklet.env=<id>`, `com.docklet.project=<id>`; GC работает только по меткам и никогда не трогает чужие ресурсы.
- **Оффлайн.** Без связи работает по кэшированному состоянию; все запущенные окружения и создание фич из локального кэша доступны.
- **Безопасность удаления.** Перед удалением worktree — проверка `git status` и непушнутых коммитов; при наличии — запрос подтверждения (локально или в панели).

## Шаги создания окружения фичи

1. Валидация конфига, вычисление набора форков и зависимостей по графу `uses`.
2. `git fetch` + `git worktree add <path>/<env-id>/<module> -b feature/<slug>` для каждого форка.
3. Сеть: подключение к сети проекта с алиасами (см. [docker-isolation.md](docker-isolation.md)).
4. Данные: клон БД из золотой копии для каждого `owned` ресурса.
5. Генерация `.env`: логические имена, ссылки `${{module.url}}` (Simple Isolation), секреты.
6. Образы: pull готовых или build; хуки `install` → `migrate` (только на клоне).
7. Старт контейнеров, `workers`, `cron` (только у форкнутых модулей).
8. Сертификат на хост (SDS) → маршрут (xDS) → health check.
9. `StatusReport: running` + URL.

## Детектор активности

Фича считается активной, если за `sleep_after`:
- через Envoy прошёл хотя бы один запрос на её хост (access log / stats по cluster), **или**
- в любом её worktree было fs-событие (исключая `.git`, `node_modules`, `vendor`).

## Preflight / `docklet doctor`

| Проверка | Действие при провале |
|---|---|
| Docker runtime установлен и запущен | Понятная ошибка + кнопка/команда запуска |
| Порты 80/443 свободны на IPv4 и IPv6 | Показать владельца, предложить режим сосуществования или fallback |
| `*.localhost` резолвится системным резолвером | Настроить DNS (с подтверждением прав) |
| CA установлен во все хранилища | Предложить установку; иначе HTTP-режим |
| Пул адресов Docker не пересекается с маршрутами (VPN) | Предложить `default-address-pools` 10.213.0.0/16 |
| Свободная RAM / диск под бюджет | Предупреждение |
| Файловая система поддерживает reflink | Информировать о скорости клонирования |

Doctor перезапускается при смене сети (VPN может переписать NRPT и `/etc/resolver`).
