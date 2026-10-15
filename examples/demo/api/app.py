"""The demo's API: items from Postgres under /api."""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.rstrip("/") in ("/api", "/api/items"):
            with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
                rows = conn.execute("SELECT id, name FROM items ORDER BY id").fetchall()
            body = json.dumps([{"id": i, "name": n} for i, n in rows]).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
        else:
            body = b'{"error": "not found"}'
            self.send_response(404)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


print("api on :8080", flush=True)
ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
