# ADR-0007: Клонирование БД

**Статус:** Accepted: Postgres — по итогам P-3, MySQL — по итогам P-4 (14.10.2026)

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

## Результат прототипа P-4 (14.10.2026)
- **Решение для MySQL:** отдельный инстанс на фичу. Золотой инстанс обновляется через `CLONE LOCAL` (запись в main блокируется ≤ 40 мс), клон — копия его каталога (`cp --reflink` где возможно) и запуск `mysqld`: 1,9 с на ext4, 1,2 с и 131 МБ диска с reflink при базе 1,1 ГБ.
- Transportable tablespaces отклонены: 5,8–11,3 с, линейно по числу таблиц, блокировка записи в main до 0,9 с; `IMPORT` переписывает страницы, и reflink не экономит место.
- `mysqldump` — только запасной путь (26 с на 1 ГБ).
- Цена — RAM: инстанс фичи с бережливыми настройками около 120 МБ (по умолчанию 423 МБ).
- Следствие для модели данных: агент поддерживает два вида клона — «база в общем инстансе» (Postgres) и «свой контейнер БД» (MySQL).
- Не проверены: MariaDB (нет clone plugin), MySQL 5.7, базы больше 1 ГБ. Подробно — [prototypes/p4-mysql-clone/README.md](../prototypes/p4-mysql-clone/README.md).
