// Base backend in Node. Plain http server; calls payments with http.get and with fetch.
const http = require("node:http");

function viaHttp() {
  return new Promise((resolve, reject) => {
    http.get("http://payments/payments/charge", (res) => {
      let body = "";
      res.on("data", (c) => (body += c));
      res.on("end", () => resolve(JSON.parse(body)));
    }).on("error", reject);
  });
}

async function viaFetch() {
  const res = await fetch("http://payments/payments/charge");
  return res.json();
}

http.createServer(async (req, res) => {
  const routes = { "/api/node/checkout": viaHttp, "/api/node/checkout-fetch": viaFetch };
  const call = routes[req.url];
  if (!call) {
    res.writeHead(404).end();
    return;
  }
  try {
    const payments = await call();
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ service: "backend-node", received_baggage: req.headers.baggage || "", payments }));
  } catch (e) {
    res.writeHead(502).end(String(e));
  }
}).listen(80);
