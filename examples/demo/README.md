# Демо-проект

Три приложения на одном хосте, база и перехватчик почты. Это проект демо-воркспейса: на нём разрабатывают и отлаживают сам Devopus, на нём же идут сквозные тесты движка.

| Что | Где | Рантайм |
|---|---|---|
| `web` — страница и серверный запрос к `/api` через прокси (`/ssr`) | `https://demo.localhost/` | Node 22 |
| `api` — JSON из Postgres; хуки `install` (venv в томе) и `migrate` | `https://demo.localhost/api` | Python 3.12 |
| `admin` — страница, которая ходит в `api` по логическому имени | `https://demo.localhost/admin/` | PHP 8.3 |
| `db` — Postgres с healthcheck и томом | `db:5432` внутри окружения | — |
| `mail` — Mailpit, веб-интерфейс через `expose` | `https://mail.demo.localhost` | — |

```bash
devopus workspace demo                  # воркспейс demo с этим проектом; пароль базы — E2E-секретом
devopus up demo --dir examples/demo     # код приложений — в этой папке; первый раз скачивает образы
devopus open
devopus logs api
devopus exec api -- /venv/bin/python -c "import psycopg; print(psycopg.__version__)"
devopus down demo --volumes             # удалить вместе с базой
```

`workspace.yaml` здесь — описание проекта в формате этапа 1: из него `devopus workspace demo` делает проект на сервере. Работать с ним напрямую больше нельзя ([R10](../../docs/refactoring-plan.md)). Если 80/443 заняты, адреса будут на `:18443` (`devopus status` покажет, кто занял порты).
