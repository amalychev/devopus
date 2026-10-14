import os
import psycopg

with psycopg.connect(os.environ["DATABASE_URL"], connect_timeout=3) as conn:
    conn.execute("CREATE TABLE IF NOT EXISTS charges (id serial PRIMARY KEY, amount int NOT NULL, env text NOT NULL)")
