# Спецификация: протокол агент ↔ сервер

**Статус:** черновик · **Этап:** 2 (в MVP облачной связи нет, но локальный API агента использует те же сообщения DesiredState/StatusReport)

## Транспорт

- Одно **исходящее** WebSocket-соединение агента с Agent gateway (`wss://agents.<домен>/v1/connect`), проходит NAT и корпоративные прокси (поддержка `HTTPS_PROXY`).
- Сообщения — Protobuf, схемы в общем репозитории (Buf). Альтернатива — ConnectRPC bidi streaming (решить в ADR при старте этапа 2).
- Аутентификация — токен устройства, полученный через Device Flow, + подпись challenge ключом устройства.
- Логи и терминал мультиплексируются в том же соединении (`StreamOpen`/`StreamData`).

## Принцип

Сервер передаёт **желаемое состояние**, а не команды. Потеря сообщения не ломает систему: следующий `DesiredState` полностью описывает, что должно быть на устройстве.

## Сообщения

| Сообщение | Направление | Содержимое |
|---|---|---|
| `Hello` | агент → сервер | `device_id`, версия агента, версия протокола, `last_applied_revision`, возможности (runtime, ОС, reflink, ресурсы) |
| `HelloAck` | сервер → агент | Принят / требуется обновление (`min_agent_version`, URL подписанного релиза) / отозван |
| `DesiredState` | сервер → агент | `revision` (монотонная), полный список окружений устройства с версиями конфига, зашифрованные секреты, подпись |
| `StatusReport` | агент → сервер | Фактическое состояние каждого окружения (`phase`, прогресс шагов, ошибки, URL, ресурсы, `schema_lag`), `applied_revision` |
| `Action` | сервер → агент | Разовое действие из белого списка: `restart_service`, `take_snapshot`, `refresh_golden_copy`, `run_doctor`, `wake`, `sleep` |
| `ActionResult` | агент → сервер | Результат действия по `action_id` |
| `ConfirmRequest` | агент → сервер | Запрос подтверждения от пользователя в панели (например, удаление worktree с незапушенными изменениями) — дублируется локальным уведомлением |
| `ConfirmResponse` | сервер → агент | Решение пользователя |
| `StreamOpen` / `StreamData` / `StreamClose` | оба | Логи и терминал в браузер, мультиплексирование по `stream_id` |
| `Heartbeat` | оба | Каждые 15 с; 3 пропуска (≈45 с) → устройство офлайн |

### Набросок схемы (иллюстрация, не код реализации)

```proto
message DesiredState {
  uint64 revision = 1;
  repeated EnvironmentSpec environments = 2;
  bytes signature = 3;              // подпись сервера над (revision, environments)
}

message EnvironmentSpec {
  string id = 1;                    // стабильный ID
  string project_id = 2;
  string config_version_id = 3;
  bytes  config = 4;                // workspace.yaml (или ссылка + хэш)
  Kind   kind = 5;                  // BASE | FEATURE
  string slug = 6;
  string branch = 7;
  repeated string forked_modules = 8;
  IsolationMode isolation = 9;      // SIMPLE | CONTEXTUAL
  DesiredPhase phase = 10;          // RUNNING | SLEEPING | ABSENT
  repeated EncryptedSecret secrets = 11;
  repeated Override overrides = 12;
}

message EnvironmentStatus {
  string id = 1;
  Phase  phase = 2;                 // CREATING | RUNNING | SLEEPING | ERROR | DELETING
  repeated StepProgress steps = 3;
  string url = 4;
  string error = 5;
  Resources resources = 6;
  int32  schema_lag = 7;
  uint64 applied_revision = 8;
}
```

## Фазы окружения

| Фаза | Значение |
|---|---|
| `CREATING` | Клон, образы, хуки |
| `RUNNING` | Контейнеры запущены, health check пройден |
| `SLEEPING` | Контейнеры остановлены, данные и worktree на месте |
| `ERROR` | Сбой создания или упал контейнер; логи в панели; возможен повтор |
| `DELETING` | Удаление с проверкой коммитов |

## Переподключение

- Экспоненциальная задержка с jitter (например, 1 с → 60 с, full jitter), чтобы тысячи агентов не ударили по серверу одновременно.
- В `Hello` агент сообщает `last_applied_revision`; сервер отвечает актуальным `DesiredState`; агент доводит незаконченные операции.
- Без связи агент работает по кэшированному `DesiredState`.

## Версионирование

- Только обратно совместимые изменения: новые поля, без удаления и переиспользования номеров. Контроль — `buf breaking` в CI.
- Сервер поддерживает **две последние мажорные** версии агента. Слишком старому — `HelloAck{update_required}`; агент скачивает подписанный релиз и проверяет cosign-подпись.

## Масштабирование

- Агент может попасть на любой инстанс Agent gateway.
- Gateway подписывается на NATS-топик `device.<id>` при подключении устройства; Core публикует изменения желаемого состояния в этот топик.
- Онлайн-статус — Redis/Valkey с TTL, обновляемым heartbeat'ом.
- Цель: 10 000 одновременных агентов на инстанс.

## Безопасность протокола

- `Action` — только из белого списка; неизвестный тип отклоняется.
- `DesiredState` подписан; агент проверяет подпись ключом платформы (для self-hosted — ключом инсталляции).
- Действия, требующие локального подтверждения, не выполняются только по серверному `ConfirmResponse` — нужно подтверждение на устройстве (если так задано политикой).

## Открытые вопросы

- Подпись `DesiredState`: ключ платформы защищает от MITM, но не от взлома самой платформы (T1). Нужна ли подпись изменений конфига ключом **админа воркспейса** (клиентская подпись в браузере)?
- Размер `DesiredState` при большом числе окружений — нужен ли дельта-режим.
