# Queue worker. Restores the context stored in the message so its own outgoing calls
# carry the feature baggage, exactly like a request handler would.
import json, os

import redis
import requests
from opentelemetry import context, propagate

INSTANCE = os.environ["INSTANCE"]
QUEUE = os.environ["QUEUE"]
r = redis.Redis(host="redis", decode_responses=True)

while True:
    _, raw = r.brpop(QUEUE)
    msg = json.loads(raw)
    token = context.attach(propagate.extract(msg["headers"]))
    try:
        payments = requests.get("http://payments/payments/charge", timeout=5).json()
    finally:
        context.detach(token)
    r.set(f"result:{msg['id']}", json.dumps({"worker": INSTANCE, "queue": QUEUE, "payments": payments}), ex=600)
