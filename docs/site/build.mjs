import { marked } from 'marked';
import { readFile, writeFile, mkdir, cp, readdir, rename } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve, join } from 'node:path';

export const root = fileURLToPath(new URL('../', import.meta.url));
export const output = join(root, 'site/dist');
export const navigation = [
  ['overview', 'README.md', 'Обзор', 'book'],
  ['architecture', 'architecture.md', 'Архитектура', 'layers'],
  ['sessions', 'sessions.md', 'Сессии и цикл', 'cycle'],
  ['tasks', 'tasks.md', 'Задачи', 'check'],
  ['memory', 'memory.md', 'Память', 'memory'],
  ['personality-and-skills', 'personality-and-skills.md', 'Личность и навыки', 'spark'],
  ['browser', 'browser.md', 'Браузер', 'layers'],
  ['tools', 'tools.md', 'Инструменты', 'tool'],
  ['background', 'background.md', 'Фоновая работа', 'pulse'],
  ['observability', 'observability.md', 'Логирование', 'pulse'],
  ['inspector', 'inspector.md', 'Инспектор сессий', 'layers'],
  ['development', 'development.md', 'Запуск MVP', 'tool'],
];

export const escapeHtml = (value = '') => String(value).replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
}[c]));
export const slugify = (text) => text.toLowerCase().normalize('NFKC').replace(/[^\p{L}\p{N}]+/gu, '-').replace(/^-|-$/g, '');
const plainText = (html) => html.replace(/<[^>]+>/g, ' ').replace(/&[^;]+;/g, ' ').replace(/\s+/g, ' ').trim();

export function compilePage(markdown, [id, file, label, icon], index = 0, diagrams = {}) {
  const source = markdown.replace(/^\[К содержанию\]\(README\.md\)\s*$/m, '');
  const headings = [];
  const occurrences = new Map();
  const renderer = new marked.Renderer();
  renderer.heading = ({ tokens, depth, text }) => {
    const title = plainText(thisParse(tokens));
    const base = slugify(title);
    const count = occurrences.get(base) || 0;
    occurrences.set(base, count + 1);
    const anchor = count ? `${base}-${count + 1}` : base;
    if (depth === 2) headings.push({ id: anchor, title });
    return `<h${depth} id="${anchor}" tabindex="-1">${thisParse(tokens)}</h${depth}>\n`;
  };
  renderer.link = ({ href, tokens, title }) => {
    const [path, anchor] = href.split('#');
    const page = navigation.find((p) => p[1] === path);
    let target = page ? `#/${page[0]}${anchor ? `/${anchor}` : ''}` : href;
    if (path.startsWith('diagrams')) target = `docs/${href}`;
    if (path === 'diagrams') target = 'docs/diagrams/';
    if (path === 'site/README.md') target = 'docs/site/README.md';
    const external = /^https?:/.test(target);
    return `<a href="${escapeHtml(target)}"${title ? ` title="${escapeHtml(title)}"` : ''}${external ? ' target="_blank" rel="noreferrer"' : ''}>${thisParse(tokens)}</a>`;
  };
  renderer.image = ({ href, text }) => {
    const src = `docs/${href}`;
    const alt = escapeHtml(text);
    const ratio = diagrams[href.split('/').pop()] || 1;
    return `<figure class="diagram"><button class="diagram-open" data-diagram="${escapeHtml(src)}" data-aspect="${ratio}" data-title="${alt}" aria-label="Увеличить схему: ${alt}" aria-keyshortcuts="Enter" title="Открыть схему · Enter"><img src="${escapeHtml(src)}" alt="${alt}" loading="lazy"><span class="diagram-expand" aria-hidden="true">↗</span></button><figcaption>${alt}<span>Нажми, чтобы увеличить</span></figcaption></figure>`;
  };
  const thisParse = (tokens) => marked.Parser.parseInline(tokens, { renderer });
  const html = marked.parse(source, { renderer }).replace(/<p>(<figure[\s\S]*?<\/figure>)<\/p>/g, '$1');
  const title = source.match(/^# (.+)$/m)?.[1] || label;
  const sections = source.split(/(?=^## )/m).map((part) => {
    const heading = part.match(/^## (.+)$/m)?.[1];
    return { title: heading || title, anchor: heading ? slugify(heading) : '', text: plainText(marked.parse(part)) };
  });
  return { id, file, label, icon, index, title, headings, sections, html, diagrams };
}

export async function build(destination = output) {
  const diagrams = {};
  for (const file of await readdir(join(root, 'diagrams'))) {
    if (!file.endsWith('.svg')) continue;
    const svg = await readFile(join(root, 'diagrams', file), 'utf8');
    const viewBox = svg.match(/\bviewBox="([^\"]+)"/)?.[1].split(/\s+/).map(Number);
    if (!viewBox || !(viewBox[2] > 0 && viewBox[3] > 0)) throw new Error(`Invalid SVG viewBox: ${file}`);
    diagrams[file] = viewBox[2] / viewBox[3];
  }
  const pages = await Promise.all(navigation.map(async (entry, index) =>
    compilePage(await readFile(join(root, entry[1]), 'utf8'), entry, index, diagrams)));
  await mkdir(destination, { recursive: true });
  await mkdir(join(destination, 'docs/site'), { recursive: true });
  const temporaryPages = join(destination, `.pages-${process.pid}.json`);
  await Promise.all([
    cp(join(root, 'site/public'), destination, { recursive: true }),
    cp(join(root, 'diagrams'), join(destination, 'docs/diagrams'), { recursive: true }),
    ...navigation.map(([, file]) => cp(join(root, file), join(destination, 'docs', file))),
    cp(join(root, 'site/README.md'), join(destination, 'docs/site/README.md')),
    writeFile(temporaryPages, JSON.stringify(pages)),
  ]);
  await rename(temporaryPages, join(destination, 'pages.json'));
  const diagramFiles = [...new Set(pages.flatMap((page) => [...page.html.matchAll(/src="docs\/diagrams\/([^\"]+\.svg)"/g)].map((match) => match[1])))];
  const diagramIndex = `<!doctype html><html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Исходники схем — Jarvis</title><style>body{font:16px/1.8 system-ui;max-width:760px;margin:50px auto;padding:0 24px;color:#33432e}a{color:#286447}li{margin:12px 0}small{margin-left:20px}</style><a href="../../#/overview">← К документации</a><h1>Исходники схем</h1><ul>${diagramFiles.map((file) => `<li><a href="${file}">${file}</a><small><a href="${file.replace('.svg', '.mmd')}">Mermaid</a></small></li>`).join('')}</ul></html>`;
  await writeFile(join(destination, 'docs/diagrams/index.html'), diagramIndex);
  return pages;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const pages = await build();
  console.log(`Built ${pages.length} documentation pages → site/dist`);
}
