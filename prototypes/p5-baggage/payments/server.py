# Payments stub: answers which instance served the call and what baggage it received.
import json, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

INSTANCE = os.environ["INSTANCE"]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"service": "payments", "instance": INSTANCE,
                           "baggage": self.headers.get("baggage", "")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("0.0.0.0", 80), Handler).serve_forever()
