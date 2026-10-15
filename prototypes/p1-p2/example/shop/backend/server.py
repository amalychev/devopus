import json, os, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ENV = os.environ.get("DEVOPUS_ENV_ID", "?")
PAYMENTS = os.environ["PAYMENTS_URL"]


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/api/whoami":
            return self.reply(200, {"module": "backend", "env": ENV})
        if self.path.startswith("/api/checkout"):
            # Calls payments by its logical name; Simple Isolation makes it the env's own copy.
            amount = self.path.partition("amount=")[2] or "1"
            with urllib.request.urlopen(f"{PAYMENTS}/payments/charge?amount={amount}", timeout=5) as r:
                charge = json.load(r)
            return self.reply(200, {"module": "backend", "env": ENV, "charge": charge})
        self.reply(404, {"error": "not found", "path": self.path})


ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", 80))), Handler).serve_forever()
