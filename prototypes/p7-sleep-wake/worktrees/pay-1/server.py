# Feature pay-1: payments module that needs its own Postgres.
import json, os, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg

DB = os.environ["DATABASE_URL"]


def connect():
    # The DB container starts together with this one; give it a moment after a wake.
    for _ in range(100):
        try:
            return psycopg.connect(DB, connect_timeout=1)
        except psycopg.OperationalError:
            time.sleep(0.1)
    raise RuntimeError("database is not reachable")


class Handler(BaseHTTPRequestHandler):
    def reply(self, body, code=200):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.startswith("/charge"):
            with connect() as conn:
                conn.execute("CREATE TABLE IF NOT EXISTS charges (id serial PRIMARY KEY, at timestamptz DEFAULT now())")
                cid = conn.execute("INSERT INTO charges DEFAULT VALUES RETURNING id").fetchone()[0]
            return self.reply({"env": "pay-1", "charge": cid})
        self.reply({"env": "pay-1"})

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.reply({"env": "pay-1", "echo": body.decode()})

    def log_message(self, *a):
        pass


ThreadingHTTPServer(("0.0.0.0", 80), Handler).serve_forever()
