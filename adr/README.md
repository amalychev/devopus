# Architecture Decision Records

Формат: контекст → решение → последствия. Статусы: **Accepted** (зафиксировано в исходных документах), **Proposed** (есть рекомендация, нужна проверка прототипом), **Open** (решение не принято).

| # | Решение | Статус | Когда закрыть |
|---|---|---|---|
| [0001](0001-desired-state-reconciliation.md) | Агент приводит систему к желаемому состоянию, а не исполняет команды | Accepted | — |
| [0002](0002-modular-monolith-three-deploys.md) | Модульный монолит + три деплоя | Accepted | — |
| [0003](0003-go-protobuf-connectrpc.md) | Go + Protobuf (Buf) + ConnectRPC | Accepted | — |
| [0004](0004-envoy-single-proxy.md) | Один Envoy на устройство как единственный TLS-терминатор | Accepted | — |
| [0005](0005-isolation-modes.md) | Simple Isolation в MVP, Contextual — этап 3 | Accepted | — |
| [0006](0006-docker-network-topology.md) | Топология docker-сетей | Proposed | Этап 0 (P-2) |
| [0007](0007-db-cloning.md) | Клонирование БД (Postgres / MySQL) | Proposed | Этап 0 (P-3, P-4) |
| [0008](0008-config-location.md) | Где хранить `workspace.yaml` | Open | Этап 0 |
| [0009](0009-secrets-key-distribution.md) | Доставка ключа команды на новое устройство | Proposed | До этапа 2 |
| [0010](0010-agent-license.md) | Лицензия агента | Open | До публичного релиза агента |
| [0011](0011-privileged-helper.md) | Privileged helper, никогда не слушать 0.0.0.0 | Accepted | — |
