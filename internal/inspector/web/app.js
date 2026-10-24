const $ = (s, root = document) => root.querySelector(s);
const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const pretty = (value) => {
  if (typeof value === "string") {
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return value;
    }
  }
  return JSON.stringify(value, null, 2) ?? "";
};
const json = (value) => `<pre class="raw">${esc(pretty(value))}</pre>`;
const date = (value) =>
  value && !String(value).startsWith("0001-")
    ? new Date(value).toLocaleString("ru-RU", {
        day: "numeric",
        month: "short",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      })
    : "Время не сохранено";
const shortDate = (value) =>
  value
    ? new Date(value).toLocaleString("ru-RU", {
        day: "2-digit",
        month: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      })
    : "";
const duration = (tr) =>
  tr.ended_at && tr.started_at
    ? `${Math.max(0, (new Date(tr.ended_at) - new Date(tr.started_at)) / 1000).toFixed(1)} с`
    : "выполняется";
const labels = {
  queued: "В очереди",
  no_speech: "Речь не распознана",
  unavailable: "Недоступно",
  message: "Сообщение",
  task: "Задача",
  initiative: "Инициатива",
  active: "Активна",
  closed: "Закрыта",
  running: "Не завершён",
  completed: "Завершён",
  failed: "Ошибка",
  sent: "Доставлено",
  pending: "В очереди",
  sending: "Отправляется",
  unknown: "Доставка неизвестна",
  cancelled: "Отменён",
  no_reply: "Без сообщения",
  reply_queued: "Ответ в очереди",
};
const label = (value) => labels[value] || value;
const pill = (value) =>
  `<span class="state ${esc(value)}">${esc(label(value))}</span>`;
const state = {
  sessions: [],
  next: 0,
  id: "",
  data: null,
  tab: "timeline",
  busy: false,
  listToken: 0,
  detailToken: 0,
  drawerOpener: null,
  drawerToken: 0,
  view: "sessions",
  filePrefix: "",
  files: [],
  fileNext: "",
  fileToken: 0,
  fileBusy: false,
};
const main = $("#main");
const artifactURL = (artifact) =>
  `/api/inspect/artifact?${new URLSearchParams({ conversation_id: artifact.conversation_id, id: artifact.id })}`;
async function api(path) {
  const response = await fetch("/api/inspect/" + path, { cache: "no-store" });
  if (!response.ok)
    throw new Error(
      response.status === 404
        ? "Запись или файл не найдены."
        : `Не удалось получить данные (${response.status}). Повторите обновление.`,
    );
  return response.json();
}
function announce(text) {
  $("#announce").textContent = text;
}
function sessionName(s) {
  return (
    s.conversation.title ||
    (s.conversation.private ? "Личный разговор" : "Групповой разговор")
  );
}
function setBusy(value) {
  state.busy = value;
  $("#refresh").disabled = value || state.fileBusy;
  $("#refresh").setAttribute("aria-busy", String(value || state.fileBusy));
}
function listRender() {
  const focusedHref = $("#sessions").contains(document.activeElement)
    ? document.activeElement.getAttribute("href")
    : null;
  $("#sessions").innerHTML = state.sessions.length
    ? state.sessions
        .map(
          (s) => `
    <a class="session-link ${s.id === state.id ? "selected" : ""}" ${s.id === state.id ? 'aria-current="page"' : ""} href="#/session/${encodeURIComponent(s.id)}">
      <div class="session-line"><span class="session-title">${esc(sessionName(s))}</span><time>${esc(shortDate(s.created_at))}</time></div>
      <div class="session-preview">${esc(s.preview || s.summary || "Без текстовых сообщений")}</div>
      <div class="session-line session-meta"><span>${esc(label(s.trigger))} · ${esc(label(s.status))}</span><span>${s.message_count} зап. · ${s.tool_count} tools</span></div>
    </a>`,
        )
        .join("")
    : '<div class="list-empty">Сессий с такими условиями нет.<br>Попробуйте другой запрос или источник.</div>';
  if (focusedHref)
    [...$("#sessions").querySelectorAll("a")]
      .find((a) => a.getAttribute("href") === focusedHref)
      ?.focus({ preventScroll: true });
  $("#list-count").textContent =
    state.sessions.length + (state.next ? "+" : "");
  $("#more-sessions").classList.toggle("hidden", !state.next);
}

async function loadList(more = false, preserve = false) {
  const token = ++state.listToken;
  $("#sessions").setAttribute("aria-busy", "true");
  $("#list-error").classList.add("hidden");
  $("#more-sessions").disabled = true;
  try {
    const params = new URLSearchParams({
      q: $("#search").value,
      status: $("#status").value,
      trigger: $("#trigger").value,
      before: more ? state.next : 0,
    });
    const result = await api("sessions?" + params);
    if (token !== state.listToken) return;
    const query = JSON.stringify([
      params.get("q"),
      params.get("status"),
      params.get("trigger"),
    ]);
    const keep =
      preserve && state.listQuery === query && state.sessions.length > 50;
    if (keep) {
      const seen = new Set(result.sessions.map((s) => s.id));
      state.sessions = [
        ...result.sessions,
        ...state.sessions.filter((s) => !seen.has(s.id)),
      ];
    } else {
      state.sessions = more
        ? [...state.sessions, ...result.sessions]
        : result.sessions;
      state.next = result.next_before;
    }
    state.listQuery = query;
    listRender();
  } catch (error) {
    if (token === state.listToken) {
      $("#list-error").textContent = error.message;
      $("#list-error").classList.remove("hidden");
    }
  } finally {
    if (token === state.listToken) {
      $("#sessions").setAttribute("aria-busy", "false");
      $("#more-sessions").disabled = false;
    }
  }
}
const tabs = [
  ["timeline", "Хронология"],
  ["models", "Модель"],
  ["audio", "Аудио"],
  ["delivery", "Доставка"],
  ["turns", "Запуски"],
  ["artifacts", "Артефакты"],
];
function tabCount(name, d) {
  return {
    timeline: d.session.message_count,
    models: d.traces.filter((t) => t.kind === "model" || t.kind === "analysis")
      .length,
    audio: (d.audio || []).length,
    delivery: d.outgoing.length,
    turns: d.turns.length,
    artifacts: d.artifacts.length,
  }[name];
}
function traceForMessage(row) {
  const matches = (t) =>
    t.kind === "model" && row.key === `${t.turn_id}/assistant/${t.step}`;
  return (
    state.data.traces.findLast((t) => matches(t) && t.status === "completed") ||
    state.data.traces.findLast(matches)
  );
}

const timeOnly = (value) =>
  value && !String(value).startsWith("0001-")
    ? new Date(value).toLocaleTimeString("ru-RU", {
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      })
    : "—";
const excerpt = (text, limit = 100) =>
  String(text ?? "")
    .replace(/\s+/g, " ")
    .slice(0, limit);
const traceButton = (id, text = "Контекст") =>
  `<button class="link-button" data-trace="${esc(id)}" title="${esc(text)} · Enter" aria-keyshortcuts="Enter">${esc(text)}</button>`;
const recordButton = (id, text = "JSON") =>
  `<button class="link-button" data-record="${esc(id)}" title="Исходная запись · Enter" aria-keyshortcuts="Enter">${esc(text)}</button>`;
function messageText(text, key) {
  if (text.length < 1200) return `<div class="prose">${esc(text)}</div>`;
  return `<details class="long-text" data-open-key="text-${esc(key)}"><summary><span class="prose text-preview">${esc(text)}</span><span class="when-closed">Развернуть ${text.length.toLocaleString("ru-RU")} символов</span><span class="when-open">Свернуть</span></summary><div class="prose">${esc(text)}</div></details>`;
}
function messageRow(row) {
  const m = row.message,
    trace = traceForMessage(row);
  const role = m.role === "user" ? "USER" : m.role === "tool" ? "RESULT" : "AI";
  const name =
    m.author_name ||
    (m.role === "user"
      ? "Пользователь"
      : m.role === "tool"
        ? "Результат без связанного вызова"
        : "Модель");
  return `<article class="event" id="event-${row.seq}"><div class="event-meta"><time title="${esc(date(row.recorded_at))}">${esc(timeOnly(row.recorded_at))}</time><span class="role role-${esc(m.role)}">${role} <span class="seq">#${row.seq}</span></span></div><div class="event-content"><div class="event-line"><span class="event-name">${esc(name)}</span><span class="row-actions">${trace ? traceButton(trace.id) : ""}${recordButton(row.seq)}</span></div>${m.content ? messageText(m.role === "tool" ? pretty(m.content) : m.content, row.seq) : ""}${m.reasoning ? `<details class="reasoning" data-open-key="reasoning-${row.seq}"><summary>Reasoning · ${m.reasoning.length.toLocaleString("ru-RU")} символов</summary><div class="prose">${esc(m.reasoning)}</div></details>` : ""}${m.images?.length ? `<span class="muted">Изображения: ${m.images.length}</span>` : ""}${!m.content && !m.reasoning && !m.images?.length ? '<span class="muted">Завершение без текста</span>' : ""}</div></article>`;
}
function toolRow(row, call) {
  const key = row.key.match(/^(.*)\/assistant\/(\d+)$/);
  const result = state.data.messages.find(
    (r) => r.key === row.key.replace("/assistant/", "/tool/") + "/" + call.id,
  );
  const attempts = state.data.traces.filter(
    (t) =>
      t.kind === "tool" &&
      t.call_id === call.id &&
      key &&
      t.turn_id === key[1] &&
      t.step === Number(key[2]),
  );
  let status = result ? "completed" : attempts.at(-1)?.status || "pending";
  if (result) {
    try {
      if (JSON.parse(result.message.content)?.error) status = "failed";
    } catch {}
  }
  const model = traceForMessage(row);
  return `<details class="tool-event tool-call" id="event-call-${row.seq}-${esc(call.id)}" data-open-key="call-${row.seq}-${esc(call.id)}"><summary><span class="event-meta"><time title="${esc(date(row.recorded_at))}">${esc(timeOnly(row.recorded_at))}</time><span class="role role-tool">TOOL <span class="seq">#${row.seq}</span></span></span><span class="tool-summary"><span class="tool-name">${esc(call.name)}</span><span class="argument-preview">${esc(excerpt(JSON.stringify(call.arguments), 150))}</span><span class="tool-state">${result ? pill(status) : '<span class="muted">Нет результата</span>'}</span><span class="expand-mark" aria-hidden="true">▸</span></span></summary><div class="tool-expanded"><div class="tool-payloads"><section><h4>Аргументы</h4>${json(call.arguments)}</section><section><h4>Результат ${result ? `<span class="muted">#${result.seq}</span>` : ""}</h4>${result ? json(result.message.content) : '<p class="muted">Результата нет в загруженной истории.</p>'}</section></div><div class="tool-footer"><span class="mono muted" title="${esc(call.id)}">${esc(call.id)}</span><span class="row-actions">${model ? traceButton(model.id) : ""}${recordButton(row.seq, "Запись вызова")}${result ? recordButton(result.seq, "Запись результата") : ""}${attempts.map((t) => traceButton(t.id, `Попытка ${t.attempt} · ${duration(t)}`)).join("")}</span></div></div></details>`;
}
function timeline(d) {
  const paired = new Set();
  for (const row of d.messages)
    for (const call of row.message.tool_calls || []) {
      const result = d.messages.find(
        (r) =>
          r.key === row.key.replace("/assistant/", "/tool/") + "/" + call.id,
      );
      if (result) paired.add(result.seq);
    }
  const events = d.messages
    .map((row) => {
      if (paired.has(row.seq)) return "";
      const calls = row.message.tool_calls || [];
      return (
        (row.message.content ||
        row.message.reasoning ||
        row.message.images?.length ||
        !calls.length
          ? messageRow(row)
          : "") + calls.map((call) => toolRow(row, call)).join("")
      );
    })
    .join("");
  const unfinished = d.traces.filter((t) => t.status === "running");
  const failures = d.traces.filter(
    (t) =>
      t.kind === "tool" &&
      t.status === "failed" &&
      !d.messages.some((m) => m.message.tool_call_id === t.call_id),
  );
  return `${unfinished.map((t) => `<div class="activity-line">Нет сохранённого завершения: <strong>${esc(t.name || "модель")}</strong> · ${esc(date(t.started_at))} ${traceButton(t.id, "Подробности")}</div>`).join("")}<div class="timeline">${events || '<div class="empty-panel">Сообщений пока нет.</div>'}${failures.map((t) => `<div class="error">${esc(t.name)}: ${esc(t.error)} ${traceButton(t.id, `Попытка ${t.attempt}`)}</div>`).join("")}</div>${d.next_after ? `<div class="pagination"><span>${d.messages.length} из ${d.session.message_count} записей</span><button id="more-messages" class="button">Загрузить следующие события</button></div>` : ""}`;
}
function models(d) {
  const rows = d.traces.filter(
    (t) => t.kind === "model" || t.kind === "analysis",
  );
  if (!rows.length)
    return '<div class="empty-panel">Вызовов модели пока нет.</div>';
  return `<div class="table-scroll"><table class="data-table"><thead><tr><th>Шаг</th><th>Модель</th><th>Начало</th><th>Время</th><th>Попытка</th><th>Состояние</th><th></th></tr></thead><tbody>${rows.map((t) => `<tr><td class="mono">${t.step + 1}</td><td><span title="${esc(t.turn_id)}">${esc(t.historical ? "Не сохранена" : t.name || "Модель")}${t.kind === "analysis" ? " · анализ" : ""}</span></td><td class="nowrap muted">${t.historical ? "—" : esc(shortDate(t.started_at))}</td><td class="mono nowrap">${t.historical ? "—" : esc(duration(t))}</td><td class="mono">${t.historical ? "—" : t.attempt}</td><td>${pill(t.status)}</td><td>${traceButton(t.id, t.historical ? "Ответ" : "Контекст")}</td></tr>`).join("")}</tbody></table></div>`;
}
function deliveries(d) {
  if (!d.outgoing.length)
    return '<div class="empty-panel">Сообщения не отправлялись.</div>';
  return `<div class="delivery-list">${d.outgoing.map((o) => `<article class="event"><div class="event-meta"><time title="${esc(date(o.enqueued_at))}">${esc(timeOnly(o.enqueued_at))}</time><span class="role">TELEGRAM</span></div><div class="event-content"><div class="event-line"><strong>${esc(o.conversation.title || o.conversation.id)}</strong><span>${pill(o.state)} <span class="muted">· ${o.attempts} попыток</span></span>${recordButton("outgoing:" + o.id)}</div>${messageText(o.text, o.id)}</div></article>`).join("")}</div>`;
}
function turns(d) {
  return `<div class="runs">${d.turns.map((t, i) => `<details class="run-row" data-open-key="turn-${i}"><summary><span>Запуск ${i + 1}</span><time class="muted" title="${esc(date(t.Input?.StartedAt))}">${esc(timeOnly(t.Input?.StartedAt))}</time>${pill(t.Stats?.FinishReason || "completed")}<span class="run-numbers">${t.Stats?.ModelCalls || 0} LLM · ${t.Stats?.ToolCalls || 0} tools · ${t.Stats?.ToolErrors || 0} ошибок</span><span class="mono">${((t.Stats?.InputTokens || 0) + (t.Stats?.OutputTokens || 0)).toLocaleString("ru-RU")} ток.</span></summary><div class="run-body"><div class="event-line"><span class="mono muted">${esc(t.Input?.Key)}</span>${recordButton("turn:" + i)}</div><p class="muted">Токены: ${t.Stats?.InputTokens || 0} вход / ${t.Stats?.OutputTokens || 0} выход. Отправка: ${t.Sent ? "запрошена" : "не запрошена"}.</p>${t.LastText ? `<h4>Финальный текст модели</h4>${messageText(t.LastText, "turn-" + i)}` : ""}</div></details>`).join("")}${!d.turns.length ? '<div class="empty-panel">Завершённых запусков пока нет.</div>' : ""}${d.analysis || d.session.summary ? `<details class="analysis-row" data-open-key="analysis"><summary>Анализ сессии <span class="muted">${d.session.analyzed ? "завершён" : "не завершён"}</span></summary><div class="run-body">${d.session.summary ? `<div class="prose">${esc(d.session.summary)}</div>` : ""}${d.analysis ? `<p>${recordButton("analysis", "Данные анализа и кандидаты в память")}</p>` : ""}</div></details>` : ""}</div>`;
}
function artifacts(d) {
  if (!d.artifacts.length)
    return '<div class="empty-panel">Артефактов нет.</div>';
  return `<ul class="file-list">${d.artifacts.map((a) => `<li><a href="${esc(artifactURL(a))}" target="_blank" rel="noopener noreferrer">${["image/png", "image/jpeg", "image/webp"].includes(a.mime) ? `<img src="${esc(artifactURL(a))}" alt="" loading="lazy">` : '<span class="file-icon">FILE</span>'}<span class="file-name">${esc(a.name)}<span class="mono muted">${esc(a.id)}</span></span><span class="muted">${esc(a.mime)}</span></a></li>`).join("")}</ul>`;
}

function audio(d) {
  if (!d.audio?.length)
    return '<div class="empty-panel">Голосовых сообщений пока нет.</div>';
  return `<div class="runs">${d.audio.map((r) => {
    const artifact = d.artifacts.find((a) => a.id === r.artifact_id);
    const traces = d.traces.filter((t) => t.kind === "transcription" && t.call_id === r.artifact_id);
    const author = d.messages.find((m) => m.message.source_message_id === r.incoming_id)?.message;
    const wait = r.started_at && !r.started_at.startsWith("0001-") ? Math.max(0, (new Date(r.started_at) - new Date(r.queued_at)) / 1000).toFixed(1) : null;
    return `<article class="audio-result"><div class="event-line"><strong>${esc(artifact?.name || "Аудио")}</strong>${pill(r.status)}${r.cached ? '<span class="muted">Из кэша · нового вызова STT не было</span>' : ""}</div><p class="muted">${esc(r.author_name || author?.author_name || r.incoming_id)} · ${esc(r.model)} · ${esc(date(r.queued_at))}</p>${artifact ? `<audio controls preload="none" aria-label="Исходное аудио" aria-keyshortcuts="Space" title="Воспроизведение и пауза · Space при фокусе на плеере" src="${esc(artifactURL(artifact))}"></audio><p><a href="${esc(artifactURL(artifact))}" target="_blank" rel="noopener noreferrer">Открыть исходное аудио</a></p>` : '<p class="muted">Исходный файл недоступен.</p>'}<p class="muted">${wait !== null ? `Ожидание: ${wait} с · ` : ""}${r.duration_seconds ? `Запись: ${r.duration_seconds.toFixed(1)} с · ` : ""}${r.ended_at && r.started_at && !r.started_at.startsWith("0001-") ? `Обработка: ${esc(duration(r))} · ` : ""}${r.cached ? "Время исходного распознавания: " : ""}Декодирование: ${r.decode_ms || 0} мс · STT: ${r.inference_ms || 0} мс</p>${r.text ? `<div class="prose">${esc(r.text)}</div>` : ""}${r.error_code ? `<p class="error">${esc(r.error_code)}</p>` : ""}${traces.map((t) => `<p>${traceButton(t.id, `Попытка ${t.attempt}`)} · ${esc(label(t.status))} · ${esc(duration(t))}</p>`).join("")}</article>`;
  }).join("")}</div>`;
}

function render() {
  const d = state.data;
  if (!d) return;
  const openKeys = [
    ...main.querySelectorAll("details[open][data-open-key]"),
  ].map((x) => x.dataset.openKey);
  const active = main.contains(document.activeElement)
    ? document.activeElement
    : null;
  const focused = active
    ? {
        id: active.id,
        trace: active.dataset.trace,
        record: active.dataset.record,
      }
    : null;
  const s = d.session;
  const tokens = d.turns.reduce(
    (n, t) => n + (t.Stats?.InputTokens || 0) + (t.Stats?.OutputTokens || 0),
    0,
  );
  main.innerHTML = `<div class="session-toolbar"><div id="detail-error" role="alert" class="error hidden"></div><div class="session-heading"><a class="back-link" href="#/">← Сессии</a><h2>${esc(sessionName(s))}</h2>${pill(s.status)}<span class="heading-source">${esc(label(s.trigger))} · ${esc(shortDate(s.created_at))}</span></div><div class="session-meta-line"><span class="mono" title="ID сессии">${esc(s.id)}</span><span>${esc(s.conversation_id)}</span><span>${s.tool_count} tools · ${tokens.toLocaleString("ru-RU")} ток.</span></div><nav class="tabs" role="tablist" aria-label="Разделы сессии">${tabs.map(([key, title]) => `<button class="tab" id="tab-${key}" role="tab" data-tab="${key}" aria-controls="panel" aria-selected="${state.tab === key}" tabindex="${state.tab === key ? 0 : -1}">${title} <small>${tabCount(key, d)}</small></button>`).join("")}</nav></div>${d.traces.some((t) => t.historical) ? '<div class="history-note">В старых записях контекст и время отдельных вызовов не сохранены.</div>' : ""}<section id="panel" role="tabpanel" aria-labelledby="tab-${state.tab}">${{ timeline, models, audio, delivery: deliveries, turns, artifacts }[state.tab](d)}</section>`;
  for (const el of main.querySelectorAll("details[data-open-key]")) {
    if (openKeys.includes(el.dataset.openKey)) el.open = true;
    const summary = el.querySelector(":scope > summary");
    if (summary && !summary.id) summary.id = "disclosure-" + el.dataset.openKey;
  }
  if (focused) {
    const el = focused.id
      ? document.getElementById(focused.id)
      : [...main.querySelectorAll("[data-trace],[data-record]")].find((x) =>
          focused.trace
            ? x.dataset.trace === focused.trace
            : x.dataset.record === focused.record,
        );
    el?.focus({ preventScroll: true });
  }
  document.title = `${sessionName(s)} · ${label(s.trigger)} · Inspector`;
}

async function loadDetail(id, append = false) {
  const token = ++state.detailToken;
  const old = state.data?.session.id === id ? state.data : null;
  try {
    const after = append && old?.messages.length ? old.messages.at(-1).seq : 0;
    const data = await api(`sessions/${encodeURIComponent(id)}?after=${after}`);
    if (
      token !== state.detailToken ||
      id !== state.id ||
      state.view !== "sessions"
    )
      return;
    if (append && old) {
      const seen = new Set(old.messages.map((x) => x.seq));
      data.messages = [
        ...old.messages,
        ...data.messages.filter((x) => !seen.has(x.seq)),
      ];
    }
    state.data = data;
    render();
  } catch (error) {
    if (token !== state.detailToken || id !== state.id) return;
    const box = $("#detail-error");
    if (box) {
      box.textContent = error.message;
      box.classList.remove("hidden");
    } else
      main.innerHTML = `<a class="back-link" href="#/">← Все сессии</a><div class="error" role="alert">${esc(error.message)}</div><p>Нажмите «Обновить», чтобы повторить загрузку.</p>`;
  } finally {
    if (token === state.detailToken && $("#more-messages"))
      $("#more-messages").disabled = false;
  }
}
async function refresh() {
  if (
    state.busy ||
    state.fileBusy ||
    !$("#drawer").classList.contains("hidden")
  )
    return;
  setBusy(true);
  try {
    if (state.view === "files") await loadFiles();
    else
      await Promise.all([
        loadList(false, true),
        state.id ? loadDetail(state.id, true) : Promise.resolve(),
      ]);
    announce("Данные обновлены");
  } finally {
    setBusy(false);
  }
}
function route() {
  const view = location.hash === "#/files" ? "files" : "sessions";
  const changedView = view !== state.view;
  state.view = view;
  document.body.classList.toggle("has-files", view === "files");
  for (const name of ["sessions", "files"]) {
    const link = $("#nav-" + name);
    if (view === name) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (view === "files") {
    ++state.detailToken;
    closeDrawer();
    fileShell();
    loadFiles();
    return;
  }
  if (changedView) {
    ++state.fileToken;
    state.fileBusy = false;
    setBusy(state.busy);
  }
  let match = location.hash.match(/^#\/session\/(.+)$/);
  let id = "";
  if (match) {
    try {
      id = decodeURIComponent(match[1]);
    } catch {}
  }
  if (id === state.id && !changedView) return;
  state.id = id;
  state.data = null;
  state.tab = "timeline";
  closeDrawer();
  document.body.classList.toggle("has-session", !!id);
  listRender();
  if (id) {
    main.innerHTML =
      '<div class="loading-line" role="status">Загружаем историю сессии…</div>';
    main.scrollTop = 0;
    loadDetail(id);
  } else {
    main.innerHTML =
      '<div class="empty-panel">Выберите сессию в списке. <kbd>⌘K / Ctrl+K</kbd> — поиск.</div>';
    document.title = "Сессии · OpenAlbert Inspector";
  }
}

const fileContentURL = (key, download = false) =>
  "/api/inspect/file/content?" +
  new URLSearchParams({ key, ...(download ? { download: "1" } : {}) });
const fileSize = (bytes) =>
  bytes < 1024
    ? `${bytes} Б`
    : bytes < 1024 * 1024
      ? `${(bytes / 1024).toFixed(1)} КБ`
      : `${(bytes / 1024 / 1024).toFixed(1)} МБ`;
const fileDate = (value) =>
  new Date(value).toLocaleString("ru-RU", {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
function fileShell() {
  document.title = "Файлы · OpenAlbert Inspector";
  main.innerHTML = `<div class="session-toolbar files-toolbar"><div class="session-heading"><h2>Файлы MinIO</h2><span id="file-bucket" class="mono muted"></span><span class="files-order">По дате изменения ↓ · сначала новые</span></div><label for="file-prefix">Префикс ключа</label><div class="search-wrap"><input id="file-prefix" type="search" placeholder="Например, artifacts/" autocomplete="off" value="${esc(state.filePrefix)}" aria-keyshortcuts="Meta+K Control+K"><kbd>${navigator.platform?.includes("Mac") ? "⌘ K" : "Ctrl K"}</kbd></div></div><div id="files-error" role="alert" class="error hidden"></div><div id="files-list" aria-busy="true"><div class="loading-line" role="status">Загружаем файлы…</div></div><div class="pagination"><span id="files-count" role="status"></span><button id="more-files" class="button hidden" title="Загрузить ещё файлы · Enter" aria-keyshortcuts="Enter">Ещё файлы</button></div>`;
  if (state.files.length) renderFiles();
}
function renderFiles() {
  const focusedKey = document.activeElement?.dataset.file;
  $("#files-list").innerHTML = state.files.length
    ? `<ul class="file-list bucket-files">${state.files.map(fileCard).join("")}</ul>`
    : `<div class="empty-panel">${state.filePrefix ? "Файлов с таким префиксом нет. Измените или очистите фильтр." : "В бакете пока нет файлов."}</div>`;
  if (focusedKey)
    [...main.querySelectorAll("[data-file]")]
      .find((el) => el.dataset.file === focusedKey)
      ?.focus({ preventScroll: true });
  $("#files-count").textContent =
    `Файлы: ${state.files.length}${state.fileNext ? "+" : ""}`;
  $("#more-files").classList.toggle("hidden", !state.fileNext);
  for (const img of main.querySelectorAll(".file-thumbnail img")) {
    const update = () => {
      const loaded = img.naturalWidth > 0;
      img.parentElement.classList.toggle("is-loaded", loaded);
      img.parentElement.classList.toggle("is-failed", !loaded);
      $(".file-thumbnail-status", img.parentElement).textContent = loaded
        ? ""
        : "Не удалось загрузить изображение";
    };
    img.addEventListener("load", update, { once: true });
    img.addEventListener("error", update, { once: true });
    if (img.complete) update();
  }
}
function fileCard(f) {
  const isImage = [
    "image/png",
    "image/jpeg",
    "image/gif",
    "image/webp",
    "image/avif",
  ].includes((f.mime || "").split(";")[0].trim().toLowerCase());
  const type = f.name.match(/\.([a-z0-9]{1,8})$/i)?.[1].toUpperCase() || "ФАЙЛ";
  const preview = isImage
    ? `<span class="file-thumbnail"><img src="${esc(fileContentURL(f.key))}" alt="" loading="lazy" decoding="async"><span class="file-thumbnail-status">Загрузка…</span></span>`
    : `<span class="file-thumbnail file-document" aria-hidden="true"><span class="file-type">${esc(type)}</span></span>`;
  return `<li><button class="file-row" data-file="${esc(f.key)}" title="${isImage ? "Увеличить изображение" : "Просмотреть файл"} · Enter" aria-keyshortcuts="Enter">${preview}<span class="file-card-caption"><span class="file-name">${esc(f.name)}</span><span class="file-facts"><time title="Изменён ${esc(date(f.modified_at))}">${esc(fileDate(f.modified_at))}</time><span>${esc(fileSize(f.size))}</span></span><span class="file-key mono muted" title="${esc(f.key)}">${esc(f.key)}</span></span></button></li>`;
}
async function loadFiles(more = false) {
  if (state.view !== "files") return;
  clearTimeout(fileDebounce);
  const restoreMoreFocus = more && document.activeElement === $("#more-files");
  const token = ++state.fileToken;
  state.fileBusy = true;
  setBusy(state.busy);
  $("#files-list").setAttribute("aria-busy", "true");
  $("#more-files").disabled = true;
  $("#files-error").classList.add("hidden");
  try {
    const result = await api(
      "files?" +
        new URLSearchParams({
          prefix: state.filePrefix,
          after: more ? state.fileNext : "",
        }),
    );
    if (token !== state.fileToken || state.view !== "files") return;
    state.files = more ? [...state.files, ...result.files] : result.files;
    state.fileNext = result.next_after || "";
    $("#file-bucket").textContent = result.bucket;
    renderFiles();
  } catch (error) {
    if (token !== state.fileToken || state.view !== "files") return;
    $("#files-error").textContent = error.message;
    $("#files-error").classList.remove("hidden");
    if (!state.files.length)
      $("#files-list").innerHTML =
        '<div class="empty-panel">Не удалось загрузить файлы. Нажмите «Обновить» (Alt+R).</div>';
  } finally {
    if (token === state.fileToken && state.view === "files") {
      state.fileBusy = false;
      setBusy(state.busy);
      $("#files-list").setAttribute("aria-busy", "false");
      $("#more-files").disabled = false;
      if (restoreMoreFocus && document.activeElement === document.body) {
        const target = state.fileNext
          ? $("#more-files")
          : [...main.querySelectorAll("#files-list .file-row")].at(-1);
        target?.focus({ preventScroll: true });
      }
    }
  }
}
async function openFile(key, opener) {
  state.drawerFileKey = key;
  state.drawerTraceID = state.drawerRecordID = null;
  const token = showDrawer("Файл", opener);
  $("#drawer-content").innerHTML =
    '<div class="loading-line" role="status">Загружаем файл…</div>';
  try {
    const result = await api("file?" + new URLSearchParams({ key }));
    if (token !== state.drawerToken) return;
    const f = result.file;
    $("#drawer-title").textContent = f.name;
    $("#drawer-content").innerHTML =
      `<p class="mono">${esc(f.key)}</p><div class="trace-meta"><span>${esc(f.mime || "Тип неизвестен")}</span><span>${esc(fileSize(f.size))}</span><span>${esc(date(f.modified_at))}</span></div>${f.conversation_id ? `<p class="mono muted">Разговор: ${esc(f.conversation_id)} · Артефакт: ${esc(f.artifact_id)}</p>` : ""}<p><a id="download-file" class="button" href="${esc(fileContentURL(key, true))}" download title="Скачать файл (Alt+Shift+S)" aria-keyshortcuts="Alt+Shift+S">Скачать <kbd>Alt ⇧ S</kbd></a></p>${result.preview_kind === "image" ? `<img class="file-preview" src="${esc(fileContentURL(key))}" alt="${esc(f.name)}"><p id="image-error" class="error hidden" role="alert">Не удалось показать изображение. Скачайте файл или откройте его ещё раз.</p>` : result.preview_kind === "text" ? `${result.truncated ? '<p class="muted">Показаны первые 256 КБ. Полный текст доступен при скачивании.</p>' : ""}<pre class="raw file-text">${esc(result.text)}</pre>` : '<p class="muted">Предпросмотр этого формата недоступен. Скачайте файл, чтобы открыть его.</p>'}`;
    $(".file-preview", $("#drawer-content"))?.addEventListener("error", () => {
      if (token === state.drawerToken)
        $("#image-error").classList.remove("hidden");
    });
  } catch (error) {
    if (token === state.drawerToken)
      $("#drawer-content").innerHTML =
        `<div class="error" role="alert">${esc(error.message)}</div><p class="muted">Закройте подробности и откройте файл ещё раз.</p>`;
  }
}

function showDrawer(title, opener) {
  state.drawerOpener = opener;
  $(".workspace").inert = true;
  $(".topbar").inert = true;
  const token = ++state.drawerToken;
  $("#drawer").classList.remove("hidden");
  $("#drawer-backdrop").classList.remove("hidden");
  $("#drawer-title").textContent = title;
  $("#drawer-content").innerHTML = "";
  $("#close-drawer").focus();
  return token;
}
function openRecord(id, opener) {
  state.drawerFileKey = null;
  state.drawerTraceID = null;
  state.drawerRecordID = id;
  let title = "Исходная запись",
    record;
  if (id.startsWith("outgoing:")) {
    record = state.data.outgoing.find((o) => o.id === id.slice(9));
    title = "Доставка";
  } else if (id.startsWith("turn:")) {
    const index = Number(id.slice(5));
    record = state.data.turns[index];
    title = `Запуск ${index + 1}`;
  } else if (id === "analysis") {
    record = state.data.analysis;
    title = "Анализ сессии";
  } else {
    record = state.data.messages.find((m) => String(m.seq) === id);
    title = `Запись #${id}`;
  }
  showDrawer(title, opener);
  $("#drawer-content").innerHTML = json(record);
}
async function openTrace(id, opener) {
  state.drawerFileKey = null;
  state.drawerTraceID = id;
  state.drawerRecordID = null;
  const token = showDrawer("Вызов", opener);
  $("#drawer-content").innerHTML =
    '<div class="loading-line" role="status">Загрузка…</div>';
  try {
    const t = await api(
      "trace?" + new URLSearchParams({ session_id: state.id, id }),
    );
    if (token !== state.drawerToken) return;
    $("#drawer-title").textContent = t.name || "Ответ модели";
    const request = t.input || {},
      out = t.output || {},
      msg = out.Message;
    const isModel = t.kind === "model" || t.kind === "analysis";
    $("#drawer-content").innerHTML =
      `<div class="trace-meta">${pill(t.status)}<span>Шаг ${t.step + 1}</span>${t.historical ? "<span>Историческая запись</span>" : `<span>Попытка ${t.attempt}</span><span>${esc(duration(t))}</span><span>${esc(date(t.started_at))}</span>`}${isModel ? `<span>Токены: ${out.InputTokens || 0} / ${out.OutputTokens || 0}</span>` : ""}</div><p class="mono trace-id">${esc(t.turn_id)}</p>${t.historical ? '<p class="muted">Контекст и время не сохранены.</p>' : ""}${t.error ? `<div class="error">${esc(t.error)}</div>` : ""}${isModel ? `${msg?.content ? `<h3>Ответ</h3>${messageText(msg.content, "drawer-response")}` : ""}${msg?.reasoning || msg?.Reasoning ? `<details class="trace-section" open><summary>Reasoning</summary><div class="prose">${esc(msg.reasoning || msg.Reasoning)}</div></details>` : ""}<details class="trace-section"><summary>JSON ответа</summary>${json(out)}</details>${request.messages ? `<h3>Контекст <small>${request.messages.length}</small></h3><div class="context-list">${request.messages.map((m, i) => `<details class="context-message"><summary><span class="context-role">${i + 1}. ${esc(m.role)}</span><span class="context-preview">${esc(excerpt(m.content, 100))}</span><span class="muted">${m.content?.length || 0}</span></summary><div class="context-body"><div class="prose">${esc(m.content)}</div>${m.tool_calls?.length ? json(m.tool_calls) : ""}${m.images?.length ? json(m.images) : ""}</div></details>`).join("")}</div><details class="trace-section"><summary>Доступные инструменты <small>${request.tools?.length || 0}</small></summary>${(request.tools || []).map((tool) => `<details class="context-message"><summary><span class="mono">${esc(tool.name)}</span></summary><div class="context-body"><p class="prose">${esc(tool.description)}</p>${json(tool.parameters)}</div></details>`).join("")}</details>` : ""}` : `<h3>Аргументы</h3>${json(request)}<h3>Результат</h3>${json(out)}`}<details class="trace-section"><summary>Полная запись трассировки</summary>${json(t)}</details>`;
  } catch (error) {
    if (token === state.drawerToken)
      $("#drawer-content").innerHTML =
        `<div class="error" role="alert">${esc(error.message)}</div>`;
  }
}

function closeDrawer() {
  $(".workspace").inert = false;
  $(".topbar").inert = false;
  const wasOpen = !$("#drawer").classList.contains("hidden");
  ++state.drawerToken;
  $("#drawer").classList.add("hidden");
  $("#drawer-backdrop").classList.add("hidden");
  if (wasOpen) {
    const opener = state.drawerOpener?.isConnected
      ? state.drawerOpener
      : [
          ...main.querySelectorAll("[data-trace],[data-record],[data-file]"),
        ].find((el) =>
          state.drawerFileKey
            ? el.dataset.file === state.drawerFileKey
            : state.drawerRecordID
              ? el.dataset.record === state.drawerRecordID
              : el.dataset.trace === state.drawerTraceID,
        );
    opener?.focus();
  }
  state.drawerOpener = null;
}
$("#refresh").addEventListener("click", refresh);
$("#more-sessions").addEventListener("click", () => loadList(true));
let debounce;
$("#search").addEventListener("input", () => {
  clearTimeout(debounce);
  debounce = setTimeout(() => loadList(), 250);
});
$("#trigger").addEventListener("change", () => loadList());
$("#status").addEventListener("change", () => loadList());
main.addEventListener("click", (event) => {
  const tab = event.target.closest("[data-tab]");
  if (tab) {
    state.tab = tab.dataset.tab;
    render();
    return;
  }
  const record = event.target.closest("[data-record]");
  if (record) {
    openRecord(record.dataset.record, record);
    return;
  }
  const trace = event.target.closest("[data-trace]");
  if (trace) {
    openTrace(trace.dataset.trace, trace);
    return;
  }
  const file = event.target.closest("[data-file]");
  if (file) {
    openFile(file.dataset.file, file);
    return;
  }
  if (event.target.closest("#more-files")) {
    if (!state.fileBusy) loadFiles(true);
    return;
  }
  if (event.target.closest("#more-messages")) {
    event.target.disabled = true;
    loadDetail(state.id, true);
  }
});
let fileDebounce;
main.addEventListener("input", (event) => {
  if (event.target.id !== "file-prefix") return;
  state.filePrefix = event.target.value;
  clearTimeout(fileDebounce);
  ++state.fileToken;
  state.files = [];
  state.fileNext = "";
  $("#files-list").innerHTML =
    '<div class="loading-line" role="status">Загружаем файлы…</div>';
  $("#files-count").textContent = "";
  $("#more-files").classList.add("hidden");
  fileDebounce = setTimeout(() => loadFiles(), 250);
});
main.addEventListener("keydown", (event) => {
  if (
    event.isComposing ||
    event.repeat ||
    !event.target.matches("[role=tab]") ||
    !["ArrowRight", "ArrowLeft", "Home", "End"].includes(event.key)
  )
    return;
  event.preventDefault();
  let index = tabs.findIndex(([key]) => key === state.tab);
  index =
    event.key === "Home"
      ? 0
      : event.key === "End"
        ? tabs.length - 1
        : (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) %
          tabs.length;
  state.tab = tabs[index][0];
  render();
  $(`#tab-${state.tab}`).focus();
});
$("#close-drawer").addEventListener("click", closeDrawer);
$("#drawer-backdrop").addEventListener("click", closeDrawer);
document.addEventListener("keydown", (event) => {
  if (event.isComposing || event.keyCode === 229 || event.repeat) {
    if (
      (event.key === "Enter" || event.key === " ") &&
      event.target.closest("button,summary,#download-file")
    )
      event.preventDefault();
    return;
  }
  const drawerOpen = !$("#drawer").classList.contains("hidden");
  if (drawerOpen) {
    if (
      event.altKey &&
      event.shiftKey &&
      !event.metaKey &&
      !event.ctrlKey &&
      event.code === "KeyS"
    ) {
      const download = $("#download-file");
      if (download) {
        event.preventDefault();
        download.click();
      }
    }
    if (event.key === "Escape") {
      event.preventDefault();
      closeDrawer();
    }
    if (event.key === "Tab") {
      const focusable = [
        ...$("#drawer").querySelectorAll(
          'button,a,summary,input,select,[tabindex="0"]',
        ),
      ].filter((el) => el.getClientRects().length);
      const first = focusable[0],
        last = focusable.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus();
      }
    }
    return;
  }
  if (
    (event.metaKey || event.ctrlKey) &&
    !event.shiftKey &&
    !event.altKey &&
    event.code === "KeyK"
  ) {
    event.preventDefault();
    if (state.view === "files") {
      $("#file-prefix").focus();
      $("#file-prefix").select();
      return;
    }
    if (!$("#search").getClientRects().length) {
      location.hash = "#/";
      route();
    }
    $("#search").focus();
    $("#search").select();
  }
  if (
    event.altKey &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.shiftKey &&
    event.code === "KeyR"
  ) {
    event.preventDefault();
    if (!$("#refresh").disabled) refresh();
  }
});
if (!navigator.platform?.includes("Mac")) {
  $("#search-key").textContent = "Ctrl K";
  $("#refresh kbd").textContent = "Alt R";
} else $("#refresh kbd").textContent = "⌥ R";
window.addEventListener("hashchange", route);
loadList();
route();
setInterval(() => {
  if (
    state.view === "sessions" &&
    !document.hidden &&
    ![...main.querySelectorAll("audio")].some((a) => !a.paused) &&
    !state.busy &&
    $("#drawer").classList.contains("hidden")
  )
    refresh();
}, 10000);
