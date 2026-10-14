# ADR-0007: Клонирование БД

**Статус:** Postgres — Accepted по итогам P-3 (14.10.2026); MySQL — Proposed, закрыть прототипом P-4

## Postgres
Золотая копия после каждой миграции main (`pg_terminate_backend` на короткое время) → клоны фич через `CREATE DATABASE … TEMPLATE golden STRATEGY FILE_COPY` (PG 15+), с `file_copy_method = clone` на PG 18 и ФС с reflink (APFS, XFS, btrfs). Отдельная логическая БД на фичу в общем инстансе; отдельный инстанс — для проектов с жёстко заданным именем БД.

## MySQL — варианты
| Вариант | Скорость | Минусы |
|---|---|---|
| CoW-снапшот volume (остановленный mysqld или `FLUSH TABLES WITH READ LOCK`) | Секунды при reflink/ZFS/btrfs | Отдельный инстанс на фичу; ext4 в VM Docker Desktop — без CoW |
| Отдельный инстанс из снапшота | Секунды–минуты | RAM на фичу |
| `mysqldump` | Минуты | Только малые базы; fallback |

## Риск
Внутри VM Docker Desktop — ext4 без reflink; NFR «< 60 с при 1 ГБ» под угрозой. Прототип P-3 должен дать цифры; при провале — уточнить NFR по ФС или рекомендовать OrbStack.

## Результат прототипа P-3 (14.10.2026)
- Клон 1 ГБ: 0,42–0,55 с на ext4 (Docker Desktop), 33 мс на XFS с reflink; 5 ГБ: 2,5–3,0 с на ext4, 49 мс с reflink. NFR-2 по БД выполняется без reflink.
- Решение для Postgres: логическая БД на фичу в общем инстансе; `STRATEGY FILE_COPY`, на PG 18 — `file_copy_method = clone` всегда.
- Золотая копия: `ALLOW_CONNECTIONS false` → terminate → `CREATE DATABASE` → `ALLOW_CONNECTIONS true`; main недоступна около 0,6 с на 1 ГБ без reflink.
- Риск смещается со времени на место на диске: без reflink каждый клон — полная копия.
- Не проверены: APFS/OrbStack, Linux-хост, холодный кэш. Подробно — [prototypes/p3-db-clone/README.md](../prototypes/p3-db-clone/README.md).
