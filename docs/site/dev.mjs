import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { watch } from 'node:fs';
import { resolve, extname, sep } from 'node:path';
import { build, output, root } from './build.mjs';

await build();
const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json; charset=utf-8', '.svg': 'image/svg+xml', '.md': 'text/plain; charset=utf-8', '.mmd': 'text/plain; charset=utf-8' };
const port = Number(process.env.PORT || 4173);
let building = Promise.resolve();
let timer;
watch(root, { recursive: true }, (_, file) => {
    if (!file) return;
    const sourceChanged = (file.endsWith('.md') && !file.includes(sep))
      || file.startsWith(`diagrams${sep}`)
      || file.startsWith(`site${sep}public${sep}`)
      || file === `site${sep}README.md`;
    if (!sourceChanged) return;
    clearTimeout(timer);
    timer = setTimeout(() => {
      building = building.then(() => build()).then(() => console.log('Documentation rebuilt.')).catch((error) => console.error(error));
    }, 100);
});

const server = createServer(async (request, response) => {
  try {
    await building;
    const url = new URL(request.url, 'http://localhost');
    const path = resolve(output, `.${decodeURIComponent(url.pathname === '/' ? '/index.html' : url.pathname)}`);
    if (!path.startsWith(output + sep)) { response.writeHead(403).end(); return; }
    const info = await stat(path);
    const file = info.isDirectory() ? resolve(path, 'index.html') : path;
    response.writeHead(200, { 'Content-Type': types[extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
    response.end(await readFile(file));
  } catch (error) {
    response.writeHead(error.code === 'ENOENT' ? 404 : 500).end('Not found');
  }
});
server.listen(port, '127.0.0.1', () => console.log(`Jarvis documentation: http://localhost:${port}`));
