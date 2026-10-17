# Тестовый сервер проекта

У проекта есть тестовый сервер — **https://devopus.webapace.ink** (Hetzner). На нём работает всё, что уже готово, из последнего коммита `main`:

| Адрес | Что |
|---|---|
| https://devopus.webapace.ink | Сервер воркспейса: веб-интерфейс воркспейса (вход, участники, проекты) и API для CLI и агентов |
| https://devopus-auth.webapace.ink | Auth: аккаунты, вход по коду из письма, токены |
| https://devopus.webapace.ink/dl/ | CLI `devopus` того же коммита для macOS и Linux; `version.txt` — какой коммит развёрнут |

**Автодеплой:** каждую минуту сервер проверяет `main`. Если там новый коммит, сервер собирает его, перезапускает Auth и сервер воркспейса и проверяет `/healthz`. Если новая версия не поднялась, возвращается прежняя, а этот коммит больше не пробуется. После `git push` новая версия работает на сервере через 2–4 минуты.

Агентов и окружений проектов на сервере нет: окружения разворачивают агенты на машинах тестировщиков ([ADR-0016](../adr/0016-workspace-server.md)).

## Как тестировать со своей машины

```bash
# CLI развёрнутого коммита (на Intel-маке — devopus_darwin_amd64, на Linux — devopus_linux_amd64)
curl -fsSLo devopus https://devopus.webapace.ink/dl/devopus_darwin_arm64 && chmod +x devopus
curl -fsS https://devopus.webapace.ink/dl/version.txt

export DEVOPUS_AUTH_URL=https://devopus-auth.webapace.ink
export DEVOPUS_SERVER_URL=https://devopus.webapace.ink
./devopus register --email you@example.com      # код из письма; ниже — где он, если почта не настроена
./devopus workspace demo                         # воркспейс с демо-проектом
./devopus up                                     # агент разворачивает проект на этой машине
```

Веб: https://devopus.webapace.ink — тот же аккаунт.

Агент и CLI с тестового сервера используют домашнюю папку `~/.devopus` и порты 80/443 так же, как обычная установка. Если на машине уже работает другой агент Devopus, тестируйте с отдельной папкой (`DEVOPUS_HOME=~/.devopus-test`) или на другой машине.

**Коды входа без почты.** Пока SMTP не настроен (`MAIL=log`), письма с кодами и инвайтами пишутся в журнал сервера:

```bash
ssh root@<IP сервера> 'journalctl -u devopus-auth -n 50 --no-pager | grep -i code'
ssh root@<IP сервера> 'journalctl -u devopus-server -n 50 --no-pager | grep -i invite'
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
dig +short devopus-auth.webapace.ink A
grep -rhoE 'ssl_certificate(_key)? [^;]+' /etc/nginx/ | sort -u   # как HTTPS сделан у других сайтов
docker version --format '{{.Server.Version}}'   # Docker есть
nginx -v; ls /etc/nginx/sites-enabled /etc/nginx/conf.d   # nginx держит 80/443; как подключены сайты
ss -ltnH '( sport = :7480 or sport = :7481 or sport = :5440 )'   # пусто: порты свободны (иначе — AUTH_PORT, SERVER_PORT, PG_PORT)
```

**DNS** настраивает владелец домена в Cloudflare: записи `devopus` и `devopus-auth` указывают на этот сервер, прокси Cloudflare включён (оранжевое облако), как у других сайтов. Поэтому `dig` покажет адреса Cloudflare (`104.x`, `172.64–71.x`) — это нормально. Если какая-то из записей не находится, **остановитесь и попросите владельца** её добавить.

Имя Auth — на один уровень под зоной (`devopus-auth.webapace.ink`, а не `auth.devopus.webapace.ink`): бесплатный сертификат Cloudflare покрывает `*.webapace.ink`, но не `*.devopus.webapace.ink`. Поток настроек агентов прокси не обрывает: сервер шлёт в него сигнал жизни раз в 30 секунд.

**HTTPS между Cloudflare и сервером — так же, как у других сайтов.** Посмотрите, что нашла команда `grep ssl_certificate` выше, и выберите режим:

| Что у других сайтов | Режим установки |
|---|---|
| Сертификат, покрывающий `*.webapace.ink` (обычно Cloudflare Origin Certificate) | `TLS=cert CERT=<путь к .pem> KEY=<путь к ключу>` — nginx слушает 443 с этим сертификатом |
| Сертификатов нет, сайты слушают только 80 (в Cloudflare режим SSL «Flexible») | `TLS=none` — только 80 |
| Свои сертификаты Let's Encrypt на каждое имя через webroot (`authenticator = webroot` в `/etc/letsencrypt/renewal/*.conf`) — **так на тестовом сервере** | в два прохода, см. ниже |

Если неясно, какой режим подходит, — остановитесь и спросите владельца, не выдумывайте.

**Let's Encrypt через webroot (как у других сайтов тестового сервера).** Сертификат нужен до того, как nginx будет слушать 443, а выпустить его можно, только когда оба имени уже отдают `/.well-known/acme-challenge/` на 80. Поэтому:

```bash
WR=/var/www/certbot     # webroot из /etc/letsencrypt/renewal/*.conf других сайтов
sudo SRC=$SRC TLS=none WEBROOT=$WR bash deploy/test-server/setup.sh          # 1: только 80
sudo certbot certonly --webroot -w $WR --cert-name devopus-test \
  -d devopus.webapace.ink -d devopus-auth.webapace.ink                        # 2: сертификат на оба имени
sudo SRC=$SRC TLS=cert WEBROOT=$WR \
  CERT=/etc/letsencrypt/live/devopus-test/fullchain.pem \
  KEY=/etc/letsencrypt/live/devopus-test/privkey.pem bash deploy/test-server/setup.sh   # 3: 80 и 443
```

`WEBROOT` нужен и в третьем проходе: через него certbot продлевает сертификат. Чтобы nginx подхватил продлённый сертификат, проверьте, как это сделано у других сайтов (`deploy_hook`/`renew_hook` в их `renewal/*.conf` или общий хук в `/etc/letsencrypt/renewal-hooks/deploy/`); если общего хука нет — добавьте `renew_hook = systemctl reload nginx` в `renewal/devopus-test.conf`.

**Старые сайты с теми же именами.** Если в nginx уже есть `server_name devopus.webapace.ink` или `devopus-auth.webapace.ink` (например, заглушка), отключите её до установки: `mv <файл> <файл>.disabled` — иначе nginx выберет один из двух сайтов. Удалять не нужно.

### 2. Запустить установку

```bash
cd $SRC
sudo SRC=$SRC TLS=cert CERT=/etc/ssl/... KEY=/etc/ssl/... bash deploy/test-server/setup.sh   # или TLS=none
# с почтой для кодов входа и инвайтов:
#   MAIL=smtp://user:password@smtp.example.com:587 MAIL_FROM=devopus@webapace.ink
# другие порты, если заняты:            AUTH_PORT=… SERVER_PORT=… PG_PORT=…
# без прокси Cloudflare можно TLS=certbot ACME_EMAIL=<email>: Let's Encrypt через nginx
```

Порты по умолчанию — `7480` (Auth), `7481` (сервер воркспейса), `5440` (PostgreSQL), все на `127.0.0.1`. На этом сервере заняты, например, 3187, 3719, 8082–8084, 15678 и 18789–18790; скрипт сам проверяет, что выбранные порты свободны, и останавливается, если нет.

Скрипт:

1. Проверяет Docker и nginx, ставит git и curl (certbot — только для `TLS=certbot`).
2. Создаёт системного пользователя `devopus`.
3. Поднимает PostgreSQL в контейнере `devopus-test-postgres` (том `devopus-test-pgdata`, только `127.0.0.1:5440`) с базами `devopus_auth` и `devopus_server`. Пароль — случайный, в `/etc/devopus-test/postgres.env`.
4. Пишет настройки в `/etc/devopus-test/{auth,server,deploy}.env` (`0600`).
5. Ставит службы systemd: `devopus-auth` (`127.0.0.1:7480`), `devopus-server` (`127.0.0.1:7481`), `devopus-deploy.service` и `devopus-deploy.timer` (раз в минуту).
6. Делает первый деплой: собирает в контейнере `golang:<версия из go.mod>` (первая сборка — несколько минут, потом кеш в томах `devopus-test-gomod`, `devopus-test-gocache`), ставит бинарники в `/usr/local/bin`, запускает службы.
7. Добавляет сайт nginx (`/etc/nginx/sites-available/devopus-test` или `conf.d/devopus-test.conf`): оба имени, проксирование без буферизации и с долгим таймаутом для потока настроек агентов, `/dl/` — сборки CLI. С `TLS=cert` сайт слушает и 443 с указанным сертификатом. Затем `nginx -t` и перезагрузка nginx; с `TLS=certbot` ещё `certbot --nginx` для обоих имён (тогда существующий файл сайта не переписывается — в нём правки certbot; переписать — `REWRITE_NGINX=1`).
8. Печатает ответы `/healthz`.

Чужие сайты nginx скрипт не трогает. Если `nginx -t` падает, проблема в чужой конфигурации — остановитесь и сообщите, ничего в ней не исправляя.

### 3. Проверить

```bash
curl -fsS https://devopus.webapace.ink/healthz        # ok
curl -fsS https://devopus-auth.webapace.ink/healthz   # ok
curl -fsS https://devopus.webapace.ink/dl/version.txt # версия и коммит = git -C $SRC rev-parse origin/main
systemctl list-timers devopus-deploy.timer
systemctl status devopus-auth devopus-server nginx --no-pager | grep -E 'Active|●'
```

Затем пройдите сценарий со своей стороны: скачайте CLI с `/dl/` и выполните `register` (код — в `journalctl -u devopus-auth`) и `workspace create test`. Откройте https://devopus.webapace.ink, войдите тем же email и убедитесь, что воркспейс виден.

Проверить автодеплой: попросите владельца сделать коммит в `main` (или посмотрите на следующий) и через 2–4 минуты сравните `version.txt` с `origin/main`. Что делал деплой — `journalctl -u devopus-deploy -n 100`.

### Если что-то не так

| Симптом | Что смотреть |
|---|---|
| Cloudflare отвечает 521/522/525/526 | 521/522 — nginx недоступен на 80/443 (файрвол Hetzner, `ufw status`); 525/526 — режим SSL в Cloudflare не совпадает с `TLS`: «Full (strict)» требует `TLS=cert` с сертификатом, которому Cloudflare доверяет (Origin Certificate) |
| certbot не получает сертификат (`TLS=certbot`) | `/var/log/letsencrypt/letsencrypt.log`; включён прокси Cloudflare или порт 80 закрыт |
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
