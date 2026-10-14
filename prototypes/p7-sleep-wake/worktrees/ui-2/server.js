// Feature ui-2: frontend module in Node.
require("node:http").createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ env: "ui-2", path: req.url }));
}).listen(80);
