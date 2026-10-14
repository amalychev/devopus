# ADR-0007: Клонирование БД

**Статус:** Proposed (Postgres — принято; MySQL — закрыть прототипом P-4)

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
