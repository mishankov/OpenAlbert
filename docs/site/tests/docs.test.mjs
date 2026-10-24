import test from 'node:test';
import assert from 'node:assert/strict';
import { access, readFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build, compilePage } from '../build.mjs';
import { keyboardAction } from '../public/keyboard.js';

const event = (values = {}) => ({ code: 'KeyK', key: 'k', metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, repeat: false, isComposing: false, ...values });

test('сборка сохраняет все документы, ссылки и 14 схем', async (t) => {
  const output = await mkdtemp(join(tmpdir(), 'openalbert-docs-test-'));
  t.after(() => rm(output, { recursive: true, force: true }));
  const pages = await build(output);
  assert.equal(pages.length, 14);
  assert.ok(pages.find((page) => page.id === 'deployment')?.html.includes('compose.yaml'));
  assert.ok(pages.find((page) => page.id === 'speech')?.html.includes('STT_BASE_URL'));
  const ids = new Set(pages.map((page) => page.id));
  const diagrams = new Set();
  for (const page of pages) {
    assert.ok(page.html.includes('<h1'));
    assert.equal(new Set(page.headings.map((heading) => heading.id)).size, page.headings.length);
    assert.ok(!page.html.includes('<p><figure'), 'figure не должна находиться внутри paragraph');
    for (const [, target] of page.html.matchAll(/href="#\/([^\/"]+)/g)) assert.ok(ids.has(target));
    for (const [, src] of page.html.matchAll(/src="(docs\/diagrams\/[^\"]+)"/g)) {
      diagrams.add(src);
      await access(join(output, src));
    }
  }
  assert.equal(diagrams.size, 14);
  assert.ok((await readFile(join(output, 'docs/diagrams/index.html'), 'utf8')).includes('components.mmd'));
  await access(join(output, 'docs/site/README.md'));
  for (const path of ['docs/node_modules', 'docs/package.json', 'docs/site/build.mjs']) {
    await assert.rejects(access(join(output, path)), { code: 'ENOENT' });
  }
});

test('компиляция обрабатывает русские заголовки, повторения и ссылки', () => {
  const page = compilePage('# Пример\n\n## Память и задачи\n\n[Задачи](tasks.md)\n\n## Память и задачи\n\nТекст', ['sample', 'sample.md', 'Пример', 'book']);
  assert.deepEqual(page.headings.map((h) => h.id), ['память-и-задачи', 'память-и-задачи-2']);
  assert.match(page.html, /href="#\/tasks"/);
});

test('поиск открывается на macOS, Windows и русской раскладке', () => {
  assert.equal(keyboardAction(event({ metaKey: true })), 'search');
  assert.equal(keyboardAction(event({ ctrlKey: true })), 'search');
  assert.equal(keyboardAction(event({ metaKey: true, key: 'л' })), 'search');
  assert.equal(keyboardAction(event()), null);
});

test('IME, удержание клавиши и другие сочетания не запускают действие', () => {
  for (const values of [{ isComposing: true }, { keyCode: 229 }, { repeat: true }, { altKey: true }, { shiftKey: true }]) {
    assert.equal(keyboardAction(event({ metaKey: true, ...values })), null);
  }
});

test('сочетания не обходят открытый диалог и оставляют Enter кнопкам', () => {
  assert.equal(keyboardAction(event({ metaKey: true }), { dialog: 'diagram-dialog' }), 'blocked');
  assert.equal(keyboardAction(event({ metaKey: true }), { dialog: 'search-dialog' }), 'search-focus');
  assert.equal(keyboardAction(event({ code: 'Escape' }), { dialog: 'search-dialog', editable: true }), 'dialog-close');
  assert.equal(keyboardAction(event({ code: 'Enter' }), { dialog: 'search-dialog', editable: false }), null);
  assert.equal(keyboardAction(event({ code: 'Enter' }), { dialog: 'search-dialog', editable: true }), 'search-open');
  assert.equal(keyboardAction(event({ code: 'ArrowDown' }), { dialog: 'search-dialog', editable: true }), 'search-next');
  assert.equal(keyboardAction(event({ code: 'Equal' }), { dialog: 'diagram-dialog' }), 'zoom-in');
  assert.equal(keyboardAction(event({ code: 'Digit0' }), { dialog: 'diagram-dialog' }), 'zoom-reset');
  assert.equal(keyboardAction(event({ code: 'Minus' }), { dialog: 'diagram-dialog', editable: true }), null);
});
