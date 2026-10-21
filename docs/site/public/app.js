import { keyboardAction } from './keyboard.js';

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const escape = (text = '') => String(text).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const paths = {
  book: '<path d="M4 4h6a3 3 0 0 1 3 3v14a4 4 0 0 0-3-2H4z"/><path d="M13 7a3 3 0 0 1 3-3h5v15h-5a4 4 0 0 0-3 2"/>',
  layers: '<path d="m12 3 9 5-9 5-9-5zM3 12l9 5 9-5M3 16l9 5 9-5"/>',
  cycle: '<path d="M20 8a8 8 0 0 0-14-3L3 8m0-5v5h5M4 16a8 8 0 0 0 14 3l3-3m0 5v-5h-5"/>',
  check: '<rect x="4" y="4" width="16" height="16" rx="4"/><path d="m8 12 3 3 5-6"/>',
  memory: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v7c0 4 16 4 16 0V5M4 12v7c0 4 16 4 16 0v-7"/>',
  spark: '<path d="m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5z"/>',
  tool: '<path d="M14 6a5 5 0 0 0-6 6L3 17a3 3 0 0 0 4 4l5-5a5 5 0 0 0 6-6l-3 3-4-4z"/>',
  pulse: '<path d="M2 12h5l3-8 4 16 3-8h5"/>',
};
const icon = (name) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths[name] || paths.book}</svg>`;
const mac = /Mac|iPhone|iPad/.test(navigator.platform);
$('[data-search-key]').textContent = mac ? '⌘ K' : 'Ctrl K';
let pages = [];
let currentPage;
let observer;
let results = [];
let selectedResult = 0;
let zoom = 1;
let diagramAspect = 1;
let composing = false;
const openers = new WeakMap();

function showDialog(dialog, opener = document.activeElement) {
  openers.set(dialog, opener);
  dialog.showModal();
}

function parseRoute() {
  try { return decodeURIComponent(location.hash).split('/').slice(1); }
  catch { return ['overview']; }
}

function renderPage() {
  const [requested = 'overview', anchor] = parseRoute();
  currentPage = pages.find((page) => page.id === requested) || pages[0];
  const page = currentPage;
  document.title = `${page.label} — Jarvis`;
  $('#breadcrumb-current').textContent = page.label;
  $('#page-number').textContent = String(page.index).padStart(2, '0');
  $('#source-link').href = `docs/${page.file}`;
  const article = $('#article');
  article.classList.toggle('overview', page.id === 'overview');
  article.innerHTML = page.html;
  if (page.id === 'overview') {
    const heading = [...article.querySelectorAll('h2')].find((h) => h.textContent === 'Навигация');
    if (heading) {
      const figure = document.createElement('figure');
      figure.className = 'diagram overview-diagram';
      figure.innerHTML = `<button class="diagram-open" data-diagram="docs/diagrams/components.svg" data-aspect="${page.diagrams?.['components.svg'] || 2}" data-title="Карта системы Jarvis" aria-label="Увеличить схему: карта системы Jarvis" aria-keyshortcuts="Enter" title="Открыть схему · Enter"><img src="docs/diagrams/components.svg" alt="Карта компонентов Jarvis"><span class="diagram-expand" aria-hidden="true">↗</span></button><figcaption>Карта системы<span>Нажми, чтобы увеличить</span></figcaption>`;
      heading.before(figure);
      const list = heading.nextElementSibling;
      if (list?.tagName === 'UL') {
        list.classList.add('section-grid');
        [...list.children].forEach((item) => {
          const link = item.querySelector('a');
          const destination = pages.find((p) => link?.getAttribute('href') === `#/${p.id}`);
          if (!destination) return;
          const description = item.textContent.slice(link.textContent.length).replace(/^\s*—\s*/, '');
          item.innerHTML = `<a class="section-card" href="#/${destination.id}"><span class="section-card-top">${icon(destination.icon)}<span class="section-number">0${destination.index}</span></span><strong>${escape(destination.label)}<span aria-hidden="true">↗</span></strong><span>${escape(description)}</span></a>`;
        });
      }
    }
  }
  article.querySelectorAll('p').forEach((p) => {
    if (p.textContent.trim() === 'Исходник схемы') p.className = 'diagram-source';
  });
  article.querySelectorAll('table').forEach((table) => {
    const wrapper = document.createElement('div');
    wrapper.className = 'table-wrap';
    wrapper.tabIndex = 0;
    wrapper.setAttribute('role', 'region');
    wrapper.setAttribute('aria-label', 'Таблица инструментов; при необходимости прокручивается по горизонтали');
    table.before(wrapper);
    wrapper.append(table);
  });
  $$('.page-nav a').forEach((a) => {
    if (a.getAttribute('href') === `#/${page.id}`) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  $('#toc-links').innerHTML = page.headings.map((h) => `<a href="#/${page.id}/${h.id}" data-section="${h.id}">${escape(h.title)}</a>`).join('');
  $('#compact-toc-links').innerHTML = $('#toc-links').innerHTML;
  const previous = pages[page.index - 1];
  const next = pages[page.index + 1];
  $('#page-pagination').innerHTML = `${previous ? `<a href="#/${previous.id}" class="previous-page"><span>← Предыдущий раздел</span><strong>${escape(previous.label)}</strong></a>` : '<span></span>'}${next ? `<a href="#/${next.id}" class="next-page"><span>Следующий раздел →</span><strong>${escape(next.label)}</strong></a>` : '<span></span>'}`;
  observer?.disconnect();
  observer = new IntersectionObserver((entries) => {
    for (const entry of entries) if (entry.isIntersecting) {
      $$('#toc-links a').forEach((a) => a.classList.toggle('active', a.dataset.section === entry.target.id));
    }
  }, { rootMargin: '-10% 0px -70% 0px' });
  article.querySelectorAll('h2').forEach((h) => observer.observe(h));
  const focusBeforeScroll = document.activeElement;
  requestAnimationFrame(() => {
    const target = anchor ? document.getElementById(anchor) : article.querySelector('h1');
    if (anchor && target) target.scrollIntoView();
    else window.scrollTo(0, 0);
    if (!$('dialog[open]') && document.activeElement === focusBeforeScroll) target?.focus({ preventScroll: true });
  });
}

function openSearch() {
  if ($('dialog[open]')) return;
  renderSearch();
  showDialog($('#search-dialog'));
  $('#search-input').focus();
}

function snippet(text, query) {
  const position = text.toLocaleLowerCase('ru').indexOf(query.toLocaleLowerCase('ru'));
  const start = Math.max(0, position - 45);
  let value = text.slice(start, start + 155);
  const prefix = start ? '…' : '';
  const suffix = start + 155 < text.length ? '…' : '';
  if (!query || position < 0) return escape(prefix + value + suffix);
  const at = position - start;
  return `${prefix}${escape(value.slice(0, at))}<mark>${escape(value.slice(at, at + query.length))}</mark>${escape(value.slice(at + query.length))}${suffix}`;
}

function renderSearch() {
  const query = $('#search-input').value.trim();
  const words = query.toLocaleLowerCase('ru').split(/\s+/).filter(Boolean);
  if (!words.length) results = pages.map((page) => ({ page, title: page.title, anchor: '', text: '', score: 0 }));
  else results = pages.flatMap((page) => page.sections.map((section) => ({ page, ...section })))
    .filter((item) => words.every((word) => `${item.title} ${item.text}`.toLocaleLowerCase('ru').includes(word)))
    .map((item) => ({ ...item, score: words.reduce((sum, word) => sum + (item.title.toLocaleLowerCase('ru').includes(word) ? 10 : 1), 0) }))
    .sort((a, b) => b.score - a.score).slice(0, 15);
  selectedResult = 0;
  const container = $('#search-results');
  if (!results.length) {
    container.innerHTML = '<div class="search-empty"><strong>Ничего не найдено</strong><p>Попробуй другое слово: например, «память», «SSH» или «задача».</p></div>';
    $('#search-input').removeAttribute('aria-activedescendant');
    return;
  }
  container.innerHTML = `<div class="search-results-label">${query ? `Найдено: ${results.length}` : 'Разделы документации'}</div><div role="listbox" id="search-options">${results.map((item, index) => `<a id="search-option-${index}" role="option" aria-selected="${index === 0}" tabindex="-1" class="search-result${index === 0 ? ' selected' : ''}" data-result="${index}" href="#/${item.page.id}${item.anchor ? `/${item.anchor}` : ''}">${icon(item.page.icon)}<span><small>${escape(item.page.label)}</small><strong>${escape(item.title)}</strong>${query ? `<span class="search-snippet">${snippet(item.text, query)}</span>` : ''}</span><span class="result-arrow" aria-hidden="true">↵</span></a>`).join('')}</div>`;
  $('#search-input').setAttribute('aria-activedescendant', 'search-option-0');
}

function moveSearch(direction) {
  if (!results.length) return;
  selectedResult = (selectedResult + direction + results.length) % results.length;
  $$('[data-result]').forEach((a, index) => {
    a.classList.toggle('selected', index === selectedResult);
    a.setAttribute('aria-selected', String(index === selectedResult));
  });
  const selected = $(`#search-option-${selectedResult}`);
  selected?.scrollIntoView({ block: 'nearest' });
  $('#search-input').setAttribute('aria-activedescendant', `search-option-${selectedResult}`);
}

function openResult(index = selectedResult) {
  const item = results[index];
  if (!item) return;
  $('#search-dialog').close();
  const hash = `#/${item.page.id}${item.anchor ? `/${item.anchor}` : ''}`;
  if (location.hash === hash) renderPage();
  else location.hash = hash;
}

function diagramSpace() {
  const stage = $('#diagram-stage');
  const style = getComputedStyle(stage);
  const width = Math.max(1, stage.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight));
  const height = Math.max(1, stage.clientHeight - parseFloat(style.paddingTop) - parseFloat(style.paddingBottom));
  return { width, height, fit: Math.min(width, height * diagramAspect) };
}

function sizeDiagramDialog() {
  const dialog = $('#diagram-dialog');
  const stage = $('#diagram-stage');
  const css = getComputedStyle(stage);
  const paddingX = parseFloat(css.paddingLeft) + parseFloat(css.paddingRight);
  const paddingY = parseFloat(css.paddingTop) + parseFloat(css.paddingBottom);
  const chrome = $('.diagram-dialog-header').getBoundingClientRect().height + $('.diagram-toolbar').getBoundingClientRect().height;
  const availableHeight = innerHeight - (innerWidth <= 620 ? 16 : 48);
  const contentHeight = Math.max(1, dialog.clientWidth - paddingX) / diagramAspect;
  dialog.style.height = `${Math.min(1100, availableHeight, contentHeight + paddingY + chrome + 2)}px`;
}

function updateZoom(value) {
  zoom = Math.max(0.25, Math.min(6, value));
  const image = $('#diagram-image');
  const width = diagramSpace().fit * zoom;
  image.style.width = `${width}px`;
  image.style.height = `${width / diagramAspect}px`;
  $('#zoom-level').textContent = `${Math.round(zoom * 100)}%`;
  $('[data-action="zoom-out"]').disabled = zoom <= 0.25;
  $('[data-action="zoom-in"]').disabled = zoom >= 6;
}

function openDiagram(button) {
  if ($('dialog[open]')) return;
  const image = $('#diagram-image');
  $('#diagram-title').textContent = button.dataset.title;
  image.alt = button.dataset.title;
  diagramAspect = Number(button.dataset.aspect) || 1;
  zoom = 1;
  image.onload = () => updateZoom(zoom);
  image.src = button.dataset.diagram;
  showDialog($('#diagram-dialog'), button);
  sizeDiagramDialog();
  $('#diagram-stage').scrollTo(0, 0);
  updateZoom(1);
}

const actions = {
  search: openSearch,
  menu: () => { if (!$('dialog[open]')) showDialog($('#menu-dialog')); },
  toc: () => { if (!$('dialog[open]')) showDialog($('#toc-dialog')); },
  'search-focus': () => $('#search-input').focus(),
  blocked: () => {},
  'dialog-close': () => $('dialog[open]')?.close(),
  'zoom-in': () => { if (!$('[data-action="zoom-in"]').disabled) updateZoom(zoom + 0.25); },
  'zoom-out': () => { if (!$('[data-action="zoom-out"]').disabled) updateZoom(zoom - 0.25); },
  'zoom-reset': () => updateZoom(1),
  'zoom-width': () => { const { width, fit } = diagramSpace(); updateZoom(width / fit); },
  'search-next': () => moveSearch(1),
  'search-previous': () => moveSearch(-1),
  'search-open': () => openResult(),
};

document.addEventListener('click', (event) => {
  const button = event.target.closest('button');
  if (button?.disabled) return;
  if (button?.dataset.action) actions[button.dataset.action]?.();
  if (button?.dataset.close) document.getElementById(button.dataset.close).close();
  if (button?.dataset.diagram) openDiagram(button);
  const result = event.target.closest('[data-result]');
  if (result) { event.preventDefault(); openResult(Number(result.dataset.result)); }
  if (event.target.closest('#menu-dialog a')) $('#menu-dialog').close();
  if (event.target.closest('#toc-dialog a')) $('#toc-dialog').close();
  if (event.target.closest('.skip-link')) { event.preventDefault(); $('#content').focus(); }
});
document.addEventListener('compositionstart', () => { composing = true; });
document.addEventListener('compositionend', () => { composing = false; });
document.addEventListener('keydown', (event) => {
  if (composing) return;
  const editable = Boolean(event.target.closest('input, textarea, [contenteditable="true"]'));
  const state = { dialog: $('dialog[open]')?.id, editable };
  if (event.repeat) {
    const candidate = keyboardAction({ code: event.code, keyCode: event.keyCode, isComposing: event.isComposing, metaKey: event.metaKey, ctrlKey: event.ctrlKey, shiftKey: event.shiftKey, altKey: event.altKey, repeat: false }, state);
    if (candidate) event.preventDefault();
    return;
  }
  const action = keyboardAction(event, state);
  if (action && actions[action]) { event.preventDefault(); actions[action](); }
});
$$('dialog').forEach((dialog) => {
  dialog.addEventListener('close', () => {
    const opener = openers.get(dialog);
    if (opener?.isConnected) opener.focus({ preventScroll: true });
  });
  dialog.addEventListener('cancel', (event) => { if (composing) event.preventDefault(); });
  dialog.addEventListener('click', (event) => {
    const rect = dialog.getBoundingClientRect();
    if (event.target === dialog && (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom)) dialog.close();
  });
});
$('#search-input').addEventListener('input', renderSearch);
window.addEventListener('hashchange', () => { if (pages.length) renderPage(); });
new ResizeObserver(() => {
  if ($('#diagram-dialog').open) { sizeDiagramDialog(); updateZoom(zoom); }
}).observe($('#diagram-stage'));

try {
  const response = await fetch('pages.json');
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  pages = await response.json();
  if (!Array.isArray(pages) || !pages.length) throw new Error('Empty documentation');
  $('#search-scope').textContent = `По всем разделам (${pages.length})`;
  $$('.page-nav').forEach((nav) => {
    nav.innerHTML = pages.map((page) => `<a href="#/${page.id}">${icon(page.icon)}<span>${escape(page.label)}</span><span class="nav-active-dot" aria-hidden="true"></span></a>`).join('');
  });
  $('#search-input').setAttribute('role', 'combobox');
  $('#search-input').setAttribute('aria-expanded', 'true');
  $('#search-input').setAttribute('aria-autocomplete', 'list');
  renderPage();
} catch (error) {
  console.error(error);
  $('#article').innerHTML = '<div class="load-error" role="alert"><h1>Не удалось загрузить документацию</h1><p>Обнови страницу, чтобы попробовать ещё раз.</p><button class="retry-button" onclick="location.reload()" title="Обновить страницу · ⌘R / Ctrl+R" aria-keyshortcuts="Meta+R Control+R">Попробовать снова <kbd>⌘ / Ctrl R</kbd></button></div>';
  $('[data-action="search"]').disabled = true;
}
