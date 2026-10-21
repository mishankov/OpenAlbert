import http from "node:http";
import { readFile } from "node:fs/promises";
const web = new URL("../web/", import.meta.url);
http
  .createServer(async (req, res) => {
    const pathname = new URL(req.url, "http://localhost").pathname;
    const file = {
      "/inspect/": "index.html",
      "/inspect/app.js": "app.js",
      "/inspect/style.css": "style.css",
    }[pathname];
    if (!file) {
      res.writeHead(404).end();
      return;
    }
    res.setHeader(
      "Content-Type",
      file.endsWith(".js")
        ? "text/javascript"
        : file.endsWith(".css")
          ? "text/css"
          : "text/html",
    );
    res.end(await readFile(new URL(file, web)));
  })
  .listen(4181, "127.0.0.1");
