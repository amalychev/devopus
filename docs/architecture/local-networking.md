# Локальная сеть: порты, DNS, HTTPS

Занятый порт 80 и `*.localhost` на Windows — решаемые неудобства, но без их решения продукт не открывается в браузере. Приоритеты: порты и DNS — **P0**, HTTPS — **P1**.

## 1. Порты 80/443 (P0)

По умолчанию платформа занимает `127.0.0.1:80/443` (и `[::1]`) через привилегированный helper; при конфликте явно переходит на высокий порт. **Чужие службы агент никогда не останавливает без подтверждения.**

### Кто обычно занимает порт

| ОС | Владельцы | Как найти |
|---|---|---|
| macOS | Laravel Herd, Valet, MAMP, brew-nginx, Apache | `lsof -nP -iTCP:80 -sTCP:LISTEN` |
| Linux | системный nginx/apache, чужой compose с `ports: 80:80` | `ss -ltnp 'sport = :80'` |
| Windows | http.sys (PID 4 «System»): IIS, World Wide Web Publishing, SSRS | `Get-NetTCPConnection -LocalPort 80 -State Listen`, `netsh http show servicestate` |

### Три режима

| Режим | Когда | Поведение |
|---|---|---|
| **Default** | 80/443 свободны | Envoy слушает их. При ошибке панель показывает, *кто* держит порт |
| **Сосуществование** | Порт держит известный dev-сервер (Herd, Valet, nginx) | Агент предлагает перенести его на 8080; Envoy встаёт на 80 и проксирует туда все незнакомые Host |
| **Fallback** | Иначе | Фиксированный высокий порт: `https://pay-123.shop.localhost:18443`. Режим явный и виден в панели |

**Цена fallback:** порт входит в OAuth redirect URI; cookies порт игнорируют (окружения на разных портах видят одни cookies); часть фреймворков генерирует абсолютные URL без порта.

### Зарезервированные порты Windows

Симптом: bind → «access forbidden», хотя порт никто не слушает. Причина: Hyper-V/WinNAT резервируют случайные диапазоны.
Решение: агент проверяет `netsh interface ipv4 show excludedportrange protocol=tcp`; при пересечении предлагает `net stop winnat` → `netsh int ipv4 add excludedportrange …` → `net start winnat`.

### Привилегированные порты

- **macOS:** с Mojave порт < 1024 без root открывается на `0.0.0.0`, но не на `127.0.0.1`. Слушать `0.0.0.0` **запрещено** — окружения с данными станут доступны всей локальной сети. Решение: launchd-демон с root открывает `127.0.0.1:80/443` и передаёт сокет агенту через socket activation.
- **Linux** (по убыванию предпочтительности):
  1. `setcap cap_net_bind_service=+ep` на бинарник прокси;
  2. публикация порта через rootful dockerd, если Envoy — контейнер;
  3. `sysctl net.ipv4.ip_unprivileged_port_start=80` — глобально, только с согласия.
  Rootless Docker и Podman без одного из этих шагов порт 80 не откроют.

### IPv6

Симптом: браузер открывает чужой сервер. Причина: браузер идёт на `[::1]:80`, а платформа слушает только `127.0.0.1:80`; если `::1:80` держит другая программа — запросы молча уходят к ней.
Решение: Envoy слушает оба адреса; агент проверяет конфликт на обоих.

## 2. Резолвинг `*.localhost` (P0)

На поведение браузеров полагаться нельзя. Агент проверяет резолвинг при установке и настраивает локальный DNS для зоны `.localhost`.

**Проблема:** Chrome/Edge/Firefox резолвят `*.localhost` сами; Postman, Node, Python, PHP, Java, эмуляторы используют `getaddrinfo`, который на Windows поддомены localhost не резолвит. `hosts` не поддерживает wildcard.

| ОС | Механизм | Права | Комментарий |
|---|---|---|---|
| Windows | Локальный DNS на `127.0.0.1:53` + правило NRPT для `.localhost` | Админ, один раз | Основной. Остальной DNS не трогается |
| Windows | Записи в `hosts` при создании/удалении фичи | Служба с админ-правами | Fallback. Defender иногда помечает правки; файл разрастается |
| macOS | Локальный DNS + `/etc/resolver/localhost` | root, один раз | Штатный механизм для зоны |
| Linux | systemd-resolved уже резолвит `*.localhost` в loopback | Не нужны | Без systemd-resolved — локальный DNS + NSS |

```powershell
Add-DnsClientNrptRule -Namespace ".localhost" -NameServers "127.0.0.1"
```

NRPT задаёт только IP, порт всегда 53 → DNS-служба обязана слушать **именно 53**.

**Риск:** VPN-клиенты переписывают NRPT и `/etc/resolver`. Агент перепроверяет резолвинг при смене сети и предупреждает в панели.

### Публичный wildcard-домен как fallback

`*.dev.<продукт>.com → 127.0.0.1` работает без настройки (как `lvh.me`). Ограничение: домашние роутеры (Fritz!Box, dnsmasq `stop-dns-rebind`) и корпоративные DNS вырезают приватные IP (защита от DNS rebinding). Только fallback.

**Не подходит:** маршрутизация по пути на одном хосте (`127.0.0.1:18443/pay-123/`) — пути `/api`, `/auth`, `/payments` заняты модулями, префикс ломает относительные пути, SPA-роутинг и cookies.

### `*.localhost` внутри контейнера

Симптом: бэкенд не может обратиться к `https://pay-123.shop.localhost` (SSR, OAuth callback, генерация PDF по URL). Причина: внутри контейнера `*.localhost` — loopback самого контейнера.

Решение: CoreDNS-контейнер резолвит `*.localhost` в адрес Envoy в docker-сети, остальное — во встроенный DNS Docker (он wildcard не поддерживает). Подключается через `dns:` в сети фичи.

Для процессов на хосте вне Docker — `host.docker.internal`; на Linux явно: `extra_hosts: host.docker.internal:host-gateway`.

## 3. HTTPS и локальный CA (P1)

### Сертификаты

Wildcard покрывает один уровень: `*.shop.localhost` покрывает `pay-123.shop.localhost`, но не `shop.localhost` и не `mail.pay-123.shop.localhost`.

**Решение:** Envoy — единственный TLS-терминатор. Агент выпускает сертификат с SAN на каждый хост при создании окружения и отдаёт Envoy через SDS без перезапуска. Обновление заранее, до истечения.

### Хранилища доверия

| Где | Как |
|---|---|
| Системное (macOS, Windows, Linux) | `truststore` (smallstep) |
| Firefox | NSS или `security.enterprise_roots.enabled` |
| Node.js | `NODE_EXTRA_CA_CERTS` |
| Python | `REQUESTS_CA_BUNDLE` или пакет `truststore` |
| Java | Импорт в keystore JDK |
| WSL2 | Отдельное Linux-хранилище внутри WSL2 |
| Контейнеры | Монтирование CA + `update-ca-certificates` при старте |

Установка — с согласия; удаление — при деинсталляции.

### Безопасность ключа CA

Украденный ключ CA позволяет подделать любой сайт. Поэтому CA выпускается с **Name Constraints** только на `.localhost` и домены платформы; ключ доступен только пользователю агента.

### Запрет на установку CA → HTTP-режим

MDM или антивирус может запретить собственный CA. Браузеры считают `http://localhost` и `*.localhost` безопасным контекстом: Service Worker и `crypto.subtle` работают, Chrome принимает `Secure`-cookies. Панель предупреждает, какие функции недоступны.
