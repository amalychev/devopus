# ADR-0003: Go + Protobuf (Buf) + ConnectRPC

**Статус:** Accepted

## Решение
Go для агента, Core, gateway и Tunnel; TypeScript + React для панели. Единый контракт — Protobuf в репозитории схем (Buf), `buf breaking` в CI. API — ConnectRPC (совместим с gRPC и JSON); типизированные клиенты для Go и TS из одной схемы.

## Последствия
- Общий код протокола между агентом и сервером.
- ⚠ В схеме архитектуры исходного документа API gateway подписан как «REST/GraphQL» — расходится с решением ConnectRPC; принимаем ConnectRPC (JSON-режим покрывает REST-потребности).
- Транспорт агента: WebSocket + Protobuf **или** ConnectRPC bidi streaming — выбрать в начале этапа 2 (критерий: прохождение корпоративных прокси).
