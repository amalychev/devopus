// The demo's front: a page that calls /api from the browser, and /ssr that calls the
// environment's public URL from inside the container (DNS and the local CA at work).
const http = require("node:http");

const page = `<!doctype html><meta charset=utf-8><title>Devopus demo</title>
<style>body{font:16px/1.5 system-ui;max-width:40rem;margin:3rem auto;padding:0 1rem}</style>
<h1>Devopus demo</h1>
<p>Three modules on one host: this page (Node), <a href="/api/items">/api</a> (Python) and
<a href="/admin/">/admin</a> (PHP). Server-side call through the proxy: <a href="/ssr">/ssr</a>.</p>
<ul id=items><li>loading…</li></ul>
<script>
fetch("/api/items").then(r => r.json()).then(items => {
  document.getElementById("items").innerHTML = items.map(i => "<li>" + i.name + "</li>").join("");
});
</script>`;

http.createServer(async (req, res) => {
  if (req.url === "/ssr") {
    try {
      const r = await fetch(process.env.PUBLIC_URL + "/api/items");
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ via: process.env.PUBLIC_URL, status: r.status, items: await r.json() }));
    } catch (e) {
      res.writeHead(502, { "content-type": "text/plain" });
      res.end("server-side fetch failed: " + (e.cause || e));
    }
    return;
  }
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(page);
}).listen(3000, () => console.log("web on :3000"));
