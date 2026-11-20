import http from "node:http";
import { readFile } from "node:fs/promises";
const web = new URL("../web/", import.meta.url);
http
  .createServer(async (req, res) => {
    const pathname = new URL(req.url, "http://localhost").pathname;
    const file = {
      "/inspect/": "index.html",
      "/inspect/app.js": "app.js",
      "/inspect/format-javascript.js": "format-javascript.js",
      "/inspect/prettier/standalone.mjs": "prettier/standalone.mjs",
      "/inspect/prettier/babel.mjs": "prettier/babel.mjs",
      "/inspect/prettier/estree.mjs": "prettier/estree.mjs",
      "/inspect/prism-javascript.js": "prism-javascript.js",
      "/inspect/style.css": "style.css",
    }[pathname];
    if (!file) {
      res.writeHead(404).end();
      return;
    }
    res.setHeader(
      "Content-Type",
      /\.m?js$/.test(file)
        ? "text/javascript"
        : file.endsWith(".css")
          ? "text/css"
          : "text/html",
    );
    res.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'");
    res.end(await readFile(new URL(file, web)));
  })
  .listen(Number(process.env.INSPECTOR_TEST_PORT || 4181), "127.0.0.1");
