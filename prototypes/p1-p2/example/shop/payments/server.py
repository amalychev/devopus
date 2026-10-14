import json, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import psycopg

ENV = os.environ.get("DOCKLET_ENV_ID", "?")
DB = os.environ["DATABASE_URL"]


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        url = urlparse(self.path)
        q = parse_qs(url.query)
        try:
            with psycopg.connect(DB, connect_timeout=3) as conn:
                if url.path == "/payments/charge":
                    amount = int(q.get("amount", ["1"])[0])
                    row = conn.execute(
                        "INSERT INTO charges (amount, env) VALUES (%s, %s) RETURNING id", (amount, ENV)
                    ).fetchone()
                    return self.reply(200, {"module": "payments", "env": ENV, "id": row[0], "amount": amount})
                if url.path == "/payments/list":
                    rows = conn.execute("SELECT id, amount, env FROM charges ORDER BY id").fetchall()
                    return self.reply(200, {"module": "payments", "env": ENV, "charges": [list(r) for r in rows]})
                if url.path in ("/payments", "/payments/"):
                    return self.reply(200, {"module": "payments", "env": ENV})
        except psycopg.Error as e:
            return self.reply(503, {"error": str(e)})
        self.reply(404, {"error": "not found", "path": url.path})


ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", 80))), Handler).serve_forever()
