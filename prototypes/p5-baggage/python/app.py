# Base backend in Python (Flask). Knows nothing about features: it calls http://payments
# and enqueues jobs; OpenTelemetry auto-instrumentation carries the baggage along.
import http.client, json, os, uuid

import redis
import requests
from flask import Flask, jsonify, request
from opentelemetry import baggage, propagate

app = Flask(__name__)
queue = redis.Redis(host="redis", decode_responses=True)


def received():
    return request.headers.get("baggage", "")


@app.get("/api/python/checkout")
def checkout():
    r = requests.get("http://payments/payments/charge", timeout=5)
    return jsonify(service="backend-python", received_baggage=received(), payments=r.json())


@app.get("/api/python/checkout-raw")
def checkout_raw():
    # http.client is not instrumented: the feature context is lost here on purpose.
    conn = http.client.HTTPConnection("payments", 80, timeout=5)
    conn.request("GET", "/payments/charge")
    payments = json.loads(conn.getresponse().read())
    return jsonify(service="backend-python", received_baggage=received(), payments=payments)


def queue_for(feature):
    # Platform-provided routing table: a feature gets its own queue only if it runs its own
    # worker; otherwise the job goes to the base queue and keeps the baggage in the message.
    with open("/etc/docklet/forks.json") as f:
        forks = json.load(f)
    if feature and "worker" in forks.get(feature, []):
        return f"jobs:{feature}"
    return "jobs"


@app.get("/api/python/order")
def order():
    headers = {}
    propagate.inject(headers)  # traceparent + baggage of the current request
    job_id = uuid.uuid4().hex
    name = queue_for(baggage.get_baggage("feature"))
    queue.lpush(name, json.dumps({"id": job_id, "headers": headers}))
    return jsonify(service="backend-python", job=job_id, queue=name, received_baggage=received())


@app.get("/api/python/job/<job_id>")
def job(job_id):
    result = queue.get(f"result:{job_id}")
    return jsonify(json.loads(result) if result else {"pending": True})


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=80, threaded=True)
