"""Migrations of the demo: idempotent, like a framework's migrate."""
import os

import psycopg

with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
    conn.execute("CREATE TABLE IF NOT EXISTS items (id serial PRIMARY KEY, name text NOT NULL UNIQUE)")
    for name in ("Clone of every repository", "Hooks: install, then migrate", "One host, paths per module"):
        conn.execute("INSERT INTO items (name) VALUES (%s) ON CONFLICT DO NOTHING", (name,))
print("migrated")
