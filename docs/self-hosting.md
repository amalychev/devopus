# Self-hosted сервер воркспейса

Сервер воркспейса — «пульт» с воркспейсами, участниками, проектами, версиями настроек и шифртекстами секретов ([ADR-0016](../adr/0016-workspace-server.md)) — можно держать у себя. Это та же программа `devopus`, что и в облаке. Аккаунты остаются в Auth Devopus: люди входят как обычно, а данные воркспейса лежат на вашем сервере.

## Что нужно

| Что | Зачем |
|---|---|
| Ubuntu 24.04+ (amd64 или arm64), root | Сервер ставится службой systemd |
| Docker Engine | PostgreSQL работает в контейнере с томом |
| Домен с A/AAAA-записью на этот сервер | HTTPS с сертификатами Let's Encrypt |
| Открытые порты 80 и 443 | 443 — сервер, 80 — проверки Let's Encrypt и перенаправление на https |
| Доступ из сервера к Auth Devopus | Ключи для проверки токенов (JWKS) и вход через веб |

## Установка

```bash
curl -fsSL https://raw.githubusercontent.com/amalychev/devopus/main/install.sh | DEVOPUS_NO_SETUP=1 sh
sudo devopus server install --domain ws.acme.com \
  --mail smtp://user:password@smtp.acme.com:587 --mail-from devopus@acme.com
sudo devopus server install --domain ws.acme.com --dry-run   # сначала посмотреть, что будет сделано
```

`install` делает следующее:

1. Создаёт системного пользователя `devopus` и папки `/var/lib/devopus-server` (сертификаты) и `/var/backups/devopus-server`.
2. Запускает PostgreSQL в контейнере `devopus-server-postgres` с томом `devopus-server-pgdata`, только на `127.0.0.1:5439`. Пароль — случайный, в `/etc/devopus/postgres.env` (`0600`), в аргументы команд он не попадает.
3. Пишет настройки в `/etc/devopus/server.env` (`0600`, читает root) и службу `/etc/systemd/system/devopus-server.service`: `devopus server run` от пользователя `devopus`, порты 80/443 без root (`CAP_NET_BIND_SERVICE`), файловая система только на чтение, кроме папки данных.
4. Включает и запускает службу, ждёт ответа `https://<домен>/healthz`: первый запрос получает сертификат.

Повторный `install` обновляет настройки (домен, почту, адрес Auth); пароль базы и данные остаются.

Дальше на своей машине:

```bash
devopus register --plan self-hosted          # или devopus login
devopus workspace create acme --server https://ws.acme.com
```

Веб-интерфейс воркспейса — `https://ws.acme.com`.

## Обслуживание

| Команда | Что |
|---|---|
| `devopus server status` | Служба, база, ответ сервера |
| `sudo devopus server backup` | Дамп базы в `/var/backups/devopus-server/devopus-server-<время>.dump` (`0600`) |
| `sudo devopus server restore <файл>` | Загрузить дамп; сервер на это время останавливается |
| `sudo devopus server upgrade [--version v1.2.3]` | Бэкап, загрузка релиза с проверкой контрольной суммы, замена бинарника, перезапуск, проверка; схема базы обновляется при старте |
| `sudo devopus server uninstall [--purge]` | Убрать службу; `--purge` — и базу со всеми данными |
| `journalctl -u devopus-server` | Лог сервера |

Бэкап содержит воркспейсы, участников, проекты со всеми версиями и шифртексты секретов. Секреты в нём остаются зашифрованными: ключи воркспейсов — на устройствах участников ([ADR-0009](../adr/0009-secrets-key-distribution.md)). Копируйте бэкапы с сервера.

## Безопасность

- **Токены привязаны к серверу.** Access-токен содержит адрес сервера (`aud`), и другой сервер его не примет. Self-hosted сервер не может предъявить токены своих пользователей облачному серверу, а облачный — вашему.
- **Секреты — E2E.** Сервер хранит только шифртексты: значения расшифровывают агенты участников.
- **Пароли и коды** хранит Auth Devopus; сервер воркспейса видит только email и id пользователя из токена.
- **Без интернета** (Auth внутри инсталляции) — этап Enterprise, пока не поддерживается ([roadmap](../roadmap.md)).

## Проверено

Логика установки, переустановки с сохранением данных, бэкапа, восстановления и удаления проверена тестом на настоящих Docker и PostgreSQL (`internal/selfhost`); скачивание релиза — с проверкой контрольной суммы. На настоящей Ubuntu с systemd и публичным доменом установка **ещё не проверялась**.
