# Тестовый сервер проекта

У проекта есть тестовый сервер — **https://devopus.webapace.ink** (Hetzner). На нём работает всё, что уже готово, из последнего коммита `main`:

| Адрес | Что |
|---|---|
| https://devopus.webapace.ink | Сервер воркспейса: веб-интерфейс воркспейса (вход, участники, проекты) и API для CLI и агентов |
| https://auth.devopus.webapace.ink | Auth: аккаунты, вход по коду из письма, токены |
| https://devopus.webapace.ink/dl/ | CLI `devopus` того же коммита для macOS и Linux; `version.txt` — какой коммит развёрнут |

**Автодеплой:** каждую минуту сервер проверяет `main`. Если там новый коммит, сервер собирает его, перезапускает Auth и сервер воркспейса и проверяет `/healthz`. Если новая версия не поднялась, возвращается прежняя, а этот коммит больше не пробуется. После `git push` новая версия работает на сервере через 2–4 минуты.

Агентов и окружений проектов на сервере нет: окружения разворачивают агенты на машинах тестировщиков ([ADR-0016](../adr/0016-workspace-server.md)).

## Как тестировать со своей машины

```bash
# CLI развёрнутого коммита (на Intel-маке — devopus_darwin_amd64, на Linux — devopus_linux_amd64)
curl -fsSLo devopus https://devopus.webapace.ink/dl/devopus_darwin_arm64 && chmod +x devopus
curl -fsS https://devopus.webapace.ink/dl/version.txt

export DEVOPUS_AUTH_URL=https://auth.devopus.webapace.ink
export DEVOPUS_SERVER_URL=https://devopus.webapace.ink
./devopus register --email you@example.com      # код из письма; ниже — где он, если почта не настроена
./devopus workspace demo                         # воркспейс с демо-проектом
./devopus up                                     # агент разворачивает проект на этой машине
```

Веб: https://devopus.webapace.ink — тот же аккаунт.

Агент и CLI с тестового сервера используют домашнюю папку `~/.devopus` и порты 80/443 так же, как обычная установка. Если на машине уже работает другой агент Devopus, тестируйте с отдельной папкой (`DEVOPUS_HOME=~/.devopus-test`) или на другой машине.

**Коды входа без почты.** Пока SMTP не настроен (`MAIL=log`), письма с кодами и инвайтами пишутся в журнал сервера:

```bash
ssh root@devopus.webapace.ink 'journalctl -u devopus-auth -n 50 --no-pager | grep -i code'
ssh root@devopus.webapace.ink 'journalctl -u devopus-server -n 50 --no-pager | grep -i invite'
```

## Развёртывание с нуля — инструкция для ИИ-агента на сервере

Задача: на сервере Hetzner развернуть Auth и сервер воркспейса из клона этого репозитория, с HTTPS и автодеплоем `main`. На сервере уже работают другие сайты: порты 80/443 держит nginx, через него идут все сайты. Наши службы слушают только `127.0.0.1:<порт>`, nginx проксирует на них. Go на сервер не ставится: сборка идёт в контейнере `golang` (Docker на сервере есть). Всё делает один идемпотентный скрипт [deploy/test-server/setup.sh](../deploy/test-server/setup.sh); повторный запуск безопасен: база и её пароль сохраняются.

### 1. Проверить, что готово

```bash
cat /etc/os-release | head -3        # Ubuntu 24.04+ (подойдёт и Debian 12+)
SRC=/path/to/devopus                 # где лежит клон; git -C $SRC remote -v — github.com/amalychev/devopus
git -C $SRC status --short           # клон без локальных правок: автодеплой делает checkout -f
sudo -u $(stat -c %U $SRC) git -C $SRC fetch origin main   # владелец клона может забирать обновления
curl -4 -s ifconfig.me               # IP сервера
dig +short devopus.webapace.ink A
dig +short auth.devopus.webapace.ink A
docker version --format '{{.Server.Version}}'   # Docker есть
nginx -v; ls /etc/nginx/sites-enabled /etc/nginx/conf.d   # nginx держит 80/443; как подключены сайты
ss -ltnH '( sport = :7480 or sport = :7481 or sport = :5440 )'   # пусто: порты свободны (иначе — AUTH_PORT, SERVER_PORT, PG_PORT)
```

**DNS — сделать это может только владелец домена.** Обе записи, `devopus.webapace.ink` и `auth.devopus.webapace.ink`, должны указывать на IP сервера. В Cloudflare у них должен быть режим **DNS only** (серое облако): сертификаты Let's Encrypt выдаёт certbot через nginx по HTTP-проверке на самом сервере. Кроме того, прокси Cloudflare обрывает поток настроек агентов (`Watch`) после 100 секунд тишины.

Если `dig` показывает адреса Cloudflare (`104.x`, `172.64–71.x`) или запись `auth.` не находится — **остановитесь и попросите владельца** поправить DNS. Не обходите это.

### 2. Запустить установку

```bash
cd $SRC
sudo DOMAIN=devopus.webapace.ink SRC=$SRC ACME_EMAIL=<email владельца> bash deploy/test-server/setup.sh
# с почтой для кодов входа и инвайтов:
#   MAIL=smtp://user:password@smtp.example.com:587 MAIL_FROM=devopus@webapace.ink
# другие порты, если заняты:            AUTH_PORT=… SERVER_PORT=… PG_PORT=…
# HTTPS уже настроен для этих имён иначе: TLS=none (certbot не запускается)
```

Порты по умолчанию — `7480` (Auth), `7481` (сервер воркспейса), `5440` (PostgreSQL), все на `127.0.0.1`. На этом сервере заняты, например, 3187, 3719, 8082–8084, 15678 и 18789–18790; скрипт сам проверяет, что выбранные порты свободны, и останавливается, если нет.

Скрипт:

1. Проверяет Docker и nginx, ставит git, curl и certbot (если их нет).
2. Создаёт системного пользователя `devopus`.
3. Поднимает PostgreSQL в контейнере `devopus-test-postgres` (том `devopus-test-pgdata`, только `127.0.0.1:5440`) с базами `devopus_auth` и `devopus_server`. Пароль — случайный, в `/etc/devopus-test/postgres.env`.
4. Пишет настройки в `/etc/devopus-test/{auth,server,deploy}.env` (`0600`).
5. Ставит службы systemd: `devopus-auth` (`127.0.0.1:7480`), `devopus-server` (`127.0.0.1:7481`), `devopus-deploy.service` и `devopus-deploy.timer` (раз в минуту).
6. Делает первый деплой: собирает в контейнере `golang:<версия из go.mod>` (первая сборка — несколько минут, потом кеш в томах `devopus-test-gomod`, `devopus-test-gocache`), ставит бинарники в `/usr/local/bin`, запускает службы.
7. Добавляет сайт nginx (`/etc/nginx/sites-available/devopus-test` или `conf.d/devopus-test.conf`): оба имени, проксирование без буферизации и с долгим таймаутом для потока настроек агентов, `/dl/` — сборки CLI. Затем `nginx -t`, перезагрузка nginx и `certbot --nginx` для обоих имён: сертификаты и перенаправление на https. Существующий файл сайта не переписывается (там правки certbot); переписать — `REWRITE_NGINX=1`.
8. Печатает ответы `/healthz`.

Чужие сайты nginx скрипт не трогает. Если `nginx -t` падает, проблема в чужой конфигурации — остановитесь и сообщите, ничего в ней не исправляя.

### 3. Проверить

```bash
curl -fsS https://devopus.webapace.ink/healthz        # ok
curl -fsS https://auth.devopus.webapace.ink/healthz   # ok
curl -fsS https://devopus.webapace.ink/dl/version.txt # версия и коммит = git -C $SRC rev-parse origin/main
systemctl list-timers devopus-deploy.timer
systemctl status devopus-auth devopus-server nginx --no-pager | grep -E 'Active|●'
```

Затем пройдите сценарий со своей стороны: скачайте CLI с `/dl/` и выполните `register` (код — в `journalctl -u devopus-auth`) и `workspace create test`. Откройте https://devopus.webapace.ink, войдите тем же email и убедитесь, что воркспейс виден.

Проверить автодеплой: попросите владельца сделать коммит в `main` (или посмотрите на следующий) и через 2–4 минуты сравните `version.txt` с `origin/main`. Что делал деплой — `journalctl -u devopus-deploy -n 100`.

### Если что-то не так

| Симптом | Что смотреть |
|---|---|
| certbot не получает сертификат | `/var/log/letsencrypt/letsencrypt.log`; DNS указывает не на сервер, включён прокси Cloudflare или порт 80 закрыт файрволом (`ufw status`, firewall в Hetzner Cloud) |
| `/healthz` — 502 | Служба не работает: `journalctl -u devopus-server -n 50` (или `-u devopus-auth`) |
| Деплой не идёт | `journalctl -u devopus-deploy -n 100`; `cat /var/lib/devopus-test/failed` — коммит, который не поднялся и пропускается; после исправления в `main` деплой пойдёт сам. Принудительно: `sudo FORCE=1 bash -c 'set -a; . /etc/devopus-test/deploy.env; bash /usr/local/lib/devopus-test/deploy.sh'` |
| Сборка падает | `docker run` с `golang:<версия>` — нужен доступ к Docker Hub и proxy.golang.org; ошибка компиляции — в журнале деплоя, коммит помечен как `failed` |
| `git fetch` просит пароль | У владельца клона нет доступа к GitHub без пароля: deploy key или токен в `~/.git-credentials` этого пользователя |
| CLI или агент: `token is not for this service` | `DEVOPUS_SERVER_URL` должен быть ровно `https://devopus.webapace.ink` — он же `DEVOPUS_SERVER_PUBLIC_URL` в `/etc/devopus-test/server.env` |

### Обслуживание

```bash
journalctl -u devopus-auth -u devopus-server -f       # журналы
docker exec devopus-test-postgres pg_dumpall -U postgres > /root/devopus-test-$(date +%F).sql   # бэкап
sudo systemctl stop devopus-deploy.timer               # заморозить версию на время теста
sudo systemctl start devopus-deploy.timer
```

Сбросить данные (все аккаунты и воркспейсы): остановить службы, удалить контейнер и том `devopus-test-pgdata`, запустить `setup.sh` снова.

Это тестовый сервер, а не self-hosted установка для клиентов: у той свой путь — `devopus server install` ([self-hosting.md](self-hosting.md)) — с одним доменом, своей базой и облачным Auth. Здесь же Auth тоже свой, а сборка идёт из исходников каждого коммита.
