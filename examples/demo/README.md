# Демо-проект

Три модуля на одном хосте, база и перехватчик почты — критерий вехи M4.

| Что | Где | Рантайм |
|---|---|---|
| `web` — страница и серверный запрос к `/api` через прокси (`/ssr`) | `https://demo.localhost/` | Node 22 |
| `api` — JSON из Postgres; хуки `install` (venv в томе) и `migrate` | `https://demo.localhost/api` | Python 3.12 |
| `admin` — страница, которая ходит в `api` по логическому имени | `https://demo.localhost/admin/` | PHP 8.3 |
| `db` — Postgres с healthcheck и томом | `db:5432` внутри окружения | — |
| `mail` — Mailpit, веб-интерфейс через `expose` | `https://mail.demo.localhost` | — |

```bash
cd examples/demo
devcorb up              # первый раз скачивает образы node, python, php, postgres, mailpit
devcorb open
devcorb logs api
devcorb exec api -- /venv/bin/python -c "import psycopg; print(psycopg.__version__)"
devcorb down --volumes  # удалить вместе с базой
```

Если 80/443 заняты, адреса будут на `:18443` (`devcorb status` покажет, кто занял порты). Пароль базы — в `.env.secrets` (в git его не кладут; здесь он демо).
