import { tokenizeJavaScript } from "./prism-javascript.js";
import { formatJavaScript } from "./format-javascript.js";

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
function javascriptToken(token) {
  if (typeof token === "string") return esc(token);
  if (Array.isArray(token)) return token.map(javascriptToken).join("");
  return `<span class="token ${esc(token.type)}">${javascriptToken(token.content)}</span>`;
}
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
  coding: "Кодинг",
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
  showOtherTools: false,
  busy: false,
  refreshToken: 0,
  listToken: 0,
  detailToken: 0,
  routeTrace: "",
  drawerOpener: null,
  drawerToken: 0,
  view: "sessions",
  filePrefix: "",
  files: [],
  fileNext: "",
  fileToken: 0,
  fileBusy: false,
  memoryScope: "",
  memoryTab: "entries",
  memoryEntriesScope: null,
  memoryEntriesDirty: false,
  memoryQuery: "",
  memoryKind: "",
  memories: [],
  memoryNext: 0,
  memoryToken: 0,
  memoryBusy: false,
  memoryStats: null,
  memoryStatsScope: null,
  memoryStatsToken: 0,
  memoryStatsBusy: false,
  soulBusy: false,
  soulToken: 0,
  drawerConsolidation: false,
};
const main = $("#main");
const logsURL = new URL(location.href);
logsURL.port = "9428";
logsURL.pathname = "/select/vmui/";
logsURL.search = "";
logsURL.hash = "/?" + new URLSearchParams({
  // Legacy UI access logs have status/latency and an incorrectly inferred
  // ERROR level (from the JSON key "error"). Match HTTP facts before severity.
  query: 'NOT service:temporal-ui OR http_status:>=400 OR status:>=400 OR duration_ms:>=1000 OR latency:>=1000000000 OR http_error:true OR NOT error:"" OR (status:"" level:in(warn,error,fatal))',
  "g0.range_input": "1h",
  "g0.relative_time": "last_1_hour",
});
$("#nav-logs").href = logsURL.href;
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
  const anyBusy =
    value || state.fileBusy || state.memoryBusy || state.memoryStatsBusy || state.soulBusy;
  $("#refresh").disabled = anyBusy;
  $("#refresh").setAttribute("aria-busy", String(anyBusy));
  $("#nav-logs").setAttribute("aria-disabled", String(anyBusy));
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
function assistantTraceKey(row) {
  return row.message.parent_tool_call_id
    ? row.key.replace(/\/codemode\/[^/]+\/[^/]+(\/assistant\/\d+)$/, "$1")
    : row.key;
}
function traceForMessage(row) {
  const key = assistantTraceKey(row);
  const matches = (t) =>
    t.kind === "model" && key === `${t.turn_id}/assistant/${t.step}`;
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

function speechForMessage(row) {
  return row.message.role === "user" && row.message.source_message_id
    ? (state.data.audio || []).filter((r) => r.incoming_id === row.message.source_message_id)
    : [];
}
function sourcePlayer(r, prefix) {
  const artifact = state.data.artifacts.find((a) => a.id === r.artifact_id);
  if (!artifact) return '<p class="muted">Исходный файл недоступен.</p>';
  return `<audio id="${prefix}-${esc(r.artifact_id)}" controls preload="none" aria-label="Исходное аудио" aria-keyshortcuts="Space" title="Воспроизведение и пауза · Space при фокусе на плеере" src="${esc(artifactURL(artifact))}"></audio><a class="audio-original" href="${esc(artifactURL(artifact))}" target="_blank" rel="noopener noreferrer" title="Открыть исходное аудио · Enter" aria-keyshortcuts="Enter">Оригинал</a>`;
}
function speechNotice(r) {
  return {
    queued: "Ожидает распознавания",
    running: "Распознавание ещё не завершено",
    no_speech: "Речь не распознана",
    failed: "Не удалось получить расшифровку",
    cancelled: "Распознавание отменено",
    unavailable: "Распознавание недоступно",
  }[r.status] || "Расшифровка не сохранена";
}
function voiceMessageRow(row, results) {
  const first = results[0], m = row?.message;
  const source = results.find((r) => r.original_text !== undefined && r.original_text !== null);
  const timestamp = first.received_at || row?.recorded_at || first.queued_at;
  const caption = source?.original_text;
  const key = row ? `event-${row.seq}` : `event-voice-${esc(first.incoming_id)}`;
  return `<article class="event voice-message" id="${key}" data-incoming="${esc(first.incoming_id)}"><div class="event-meta"><time title="${first.received_at ? "Получено в Telegram: " : "Сохранено: "}${esc(date(timestamp))}">${esc(timeOnly(timestamp))}</time><span class="role role-user">USER ${row ? `<span class="seq">#${row.seq}</span>` : ""}</span></div><div class="event-content"><div class="event-line"><span class="event-name">${esc(m?.author_name || first.author_name || "Пользователь")}</span><span class="voice-badge">Голосовое сообщение</span>${row ? `<span class="row-actions">${recordButton(row.seq)}</span>` : ""}</div>${caption ? messageText(caption, `caption-${first.incoming_id}`) : ""}${results.map((r) => `<div class="voice-attachment">${sourcePlayer(r, "voice-player")}<div class="voice-caption"><span class="muted">${r.duration_seconds ? `${esc(r.duration_seconds.toFixed(1))} с · ` : ""}Автоматическая расшифровка</span>${r.text ? messageText(r.text, `voice-${r.artifact_id}`) : `<p class="muted">${esc(speechNotice(r))}</p>`}</div></div>`).join("")}${!source && m?.content ? `<details class="voice-context" data-open-key="voice-context-${esc(first.incoming_id)}"><summary title="Сохранённый текст сообщения · Enter" aria-keyshortcuts="Enter Space">Сохранённый текст сообщения</summary>${messageText(m.content, `saved-voice-${first.incoming_id}`)}</details>` : ""}</div></article>`;
}
function speechRow(r, orphanTrace = null) {
  const attempts = orphanTrace ? [orphanTrace] : state.data.traces.filter((t) => t.kind === "transcription" && t.call_id === r.artifact_id);
  const timestamp = r.started_at && !r.started_at.startsWith("0001-") ? r.started_at : r.queued_at;
  const elapsed = r.ended_at && timestamp && !r.ended_at.startsWith("0001-") ? duration({ started_at: timestamp, ended_at: r.ended_at }) : "";
  const info = [r.model, r.cached ? "Из кэша · нового вызова STT не было" : elapsed].filter(Boolean).join(" · ");
  return `<details class="tool-event speech-event" data-open-key="speech-${esc(orphanTrace?.id || r.artifact_id)}"><summary title="Распознавание речи · Enter" aria-keyshortcuts="Enter Space"><span class="event-meta"><time title="${esc(date(timestamp))}">${esc(timeOnly(timestamp))}</time><span class="role role-speech">STT</span></span><span class="tool-summary"><span class="tool-name">Распознавание речи</span><span class="argument-preview">${esc(info)}</span><span class="tool-state">${pill(r.status)}</span><span class="expand-mark" aria-hidden="true">▸</span></span></summary><div class="tool-expanded speech-expanded"><div class="tool-payloads"><section><h4>Результат</h4>${r.text ? messageText(r.text, `speech-${r.artifact_id}`) : `<p class="muted">${esc(speechNotice(r))}</p>`}${r.error_code ? `<p class="error">${esc(r.error_code)}</p>` : ""}${orphanTrace ? '<p class="muted">Результат вложения не сохранён; доступна трасса попытки.</p>' : ""}</section><section><h4>Обработка</h4><p class="muted">${esc(r.model || "Модель не сохранена")}</p>${r.cached ? '<p class="muted">Использована сохранённая расшифровка. Время ниже относится к исходному распознаванию.</p>' : ""}${r.decode_ms !== undefined ? `<p>Декодирование: ${esc(r.decode_ms)} мс</p>` : ""}${r.inference_ms !== undefined ? `<p>STT: ${esc(r.inference_ms)} мс</p>` : ""}${r.duration_seconds ? `<p>Запись: ${esc(r.duration_seconds.toFixed(1))} с</p>` : ""}</section></div><div class="tool-footer"><span class="mono muted">${esc(r.artifact_id)}</span><span class="row-actions">${!orphanTrace ? recordButton(`audio:${r.artifact_id}`, "Данные распознавания") : ""}${attempts.map((t) => traceButton(t.id, `Попытка ${t.attempt} · ${label(t.status)}`)).join("")}</span></div></div></details>`;
}
function messageRow(row) {
  const speech = speechForMessage(row);
  if (speech.length) return voiceMessageRow(row, speech);
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
  return `<article class="event${m.role === "tool" ? " other-tool" : ""}" id="event-${row.seq}"><div class="event-meta"><time title="${esc(date(row.recorded_at))}">${esc(timeOnly(row.recorded_at))}</time><span class="role role-${esc(m.role)}">${role} <span class="seq">#${row.seq}</span></span></div><div class="event-content"><div class="event-line"><span class="event-name">${esc(name)}</span><span class="row-actions">${trace ? traceButton(trace.id) : ""}${recordButton(row.seq)}</span></div>${m.content ? messageText(m.role === "tool" ? pretty(m.content) : m.content, row.seq) : ""}${m.reasoning ? `<details class="reasoning" data-open-key="reasoning-${row.seq}"><summary>Reasoning · ${m.reasoning.length.toLocaleString("ru-RU")} символов</summary><div class="prose">${esc(m.reasoning)}</div></details>` : ""}${m.images?.length ? `<span class="muted">Изображения: ${m.images.length}</span>` : ""}${!m.content && !m.reasoning && !m.images?.length ? '<span class="muted">Завершение без текста</span>' : ""}</div></article>`;
}
function toolRow(row, call) {
  const source =
    call.name === "codemode" && typeof call.arguments?.code === "string"
      ? call.arguments.code
      : null;
  const argumentPreview = source === null
    ? JSON.stringify(call.arguments)
    : source.trimStart();
  const argumentsMarkup = source === null
    ? `<h4>Аргументы</h4>${json(call.arguments)}`
    : `<h4>JavaScript</h4><pre id="source-${row.seq}-${esc(call.id)}" class="raw code-source" tabindex="0" aria-label="Код JavaScript"><code class="language-javascript">${esc(source)}</code></pre>`;
  const key = assistantTraceKey(row).match(/^(.*)\/assistant\/(\d+)$/);
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
  return `<details class="tool-event tool-call${call.name === "codemode" ? "" : " other-tool"}" id="event-call-${row.seq}-${esc(call.id)}" data-open-key="call-${row.seq}-${esc(call.id)}"><summary><span class="event-meta"><time title="${esc(date(row.recorded_at))}">${esc(timeOnly(row.recorded_at))}</time><span class="role role-tool">TOOL <span class="seq">#${row.seq}</span></span></span><span class="tool-summary"><span class="tool-name">${esc(call.name)}</span><span class="argument-preview">${esc(excerpt(argumentPreview, 150))}</span><span class="tool-state">${result ? pill(status) : '<span class="muted">Нет результата</span>'}</span><span class="expand-mark" aria-hidden="true">▸</span></span></summary><div class="tool-expanded"><div class="tool-payloads${source === null ? "" : " codemode-payloads"}"><section>${argumentsMarkup}</section><section><h4>Результат ${result ? `<span class="muted">#${result.seq}</span>` : ""}</h4>${result ? json(result.message.content) : '<p class="muted">Результата нет в загруженной истории.</p>'}</section></div><div class="tool-footer"><span class="mono muted" title="${esc(call.id)}">${esc(call.id)}</span><span class="row-actions">${model ? traceButton(model.id) : ""}${recordButton(row.seq, "Запись вызова")}${result ? recordButton(result.seq, "Запись результата") : ""}${attempts.map((t) => traceButton(t.id, `Попытка ${t.attempt} · ${duration(t)}`)).join("")}</span></div></div></details>`;
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
  const represented = new Set();
  const events = d.messages
    .map((row) => {
      if (paired.has(row.seq)) return "";
      const calls = row.message.tool_calls || [];
      const speech = speechForMessage(row);
      speech.forEach((r) => represented.add(r.artifact_id));
      return (
        (row.message.content ||
        row.message.reasoning ||
        row.message.images?.length ||
        !calls.length
          ? messageRow(row)
          : "") + speech.map((r) => speechRow(r)).join("") + calls.map((call) => toolRow(row, call)).join("")
      );
    })
    .join("");
  const pending = new Map();
  for (const r of d.audio || []) {
    if (represented.has(r.artifact_id)) continue;
    const group = pending.get(r.incoming_id) || [];
    group.push(r);pending.set(r.incoming_id, group);
  }
  const pendingEvents = [...pending.values()].map((group) => voiceMessageRow(null, group) + group.map((r) => speechRow(r)).join("")).join("");
  const orphanSpeech = d.traces.filter((t) => t.kind === "transcription" && !(d.audio || []).some((r) => r.artifact_id === t.call_id)).map((t) => speechRow({ artifact_id: t.call_id, model: t.name, status: t.status, started_at: t.started_at, ended_at: t.ended_at }, t)).join("");
  const unfinished = d.traces.filter((t) => t.status === "running" && t.kind !== "transcription");
  const failures = d.traces.filter(
    (t) =>
      t.kind === "tool" &&
      t.status === "failed" &&
      !d.messages.some((m) => m.message.tool_call_id === t.call_id),
  );
  const filter = `<label class="timeline-filter">
    <input id="show-other-tools" type="checkbox" aria-keyshortcuts="Space Alt+T"${state.showOtherTools ? " checked" : ""}>
    <span>Показывать другие вызовы инструментов</span>
    <kbd aria-hidden="true">${navigator.platform?.includes("Mac") ? "⌥ T" : "Alt T"}</kbd>
  </label>`;
  return `${filter}${unfinished.map((t) => `<div class="activity-line${t.kind === "tool" && t.name !== "codemode" ? " other-tool" : ""}">Нет сохранённого завершения: <strong>${esc(t.name || "модель")}</strong> · ${esc(date(t.started_at))} ${traceButton(t.id, "Подробности")}</div>`).join("")}<div class="timeline">${events + pendingEvents + orphanSpeech}${failures.map((t) => `<div class="error${t.name === "codemode" ? "" : " other-tool"}">${esc(t.name)}: ${esc(t.error)} ${traceButton(t.id, `Попытка ${t.attempt}`)}</div>`).join("")}<div id="timeline-empty" class="empty-panel hidden">Сообщений пока нет.</div><div id="filtered-tools-empty" class="empty-panel hidden">Другие вызовы инструментов скрыты.</div></div>${d.next_after ? `<div class="pagination"><span>${d.messages.length} из ${d.session.message_count} записей</span><button id="more-messages" class="button">Загрузить следующие события</button></div>` : ""}`;
}
function updateToolVisibility() {
  const panel = $("#panel");
  const hide = !state.showOtherTools;
  if (hide && document.activeElement.closest(".other-tool"))
    $("#show-other-tools").focus({ preventScroll: true });
  panel.classList.toggle("hide-other-tools", hide);
  const entries = [...panel.querySelectorAll(".timeline > :not(.empty-panel), .activity-line")];
  const filteredOnly = hide && entries.length > 0 &&
    entries.every((entry) => entry.classList.contains("other-tool"));
  $("#timeline-empty").classList.toggle("hidden", entries.length > 0);
  $("#filtered-tools-empty").classList.toggle("hidden", !filteredOnly);
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
  if (state.tab === "timeline") updateToolVisibility();
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
    if (state.routeTrace && $("#drawer").classList.contains("hidden"))
      openTrace(state.routeTrace);
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
    state.memoryBusy ||
    state.memoryStatsBusy ||
    state.soulBusy ||
    !$("#drawer").classList.contains("hidden")
  )
    return;
  const token = ++state.refreshToken;
  setBusy(true);
  try {
    if (state.view === "memory") {
      if (state.memoryTab === "analytics") {
        state.memoryEntriesDirty = true;
        await Promise.all([loadMemoryStats(true), loadSoul()]);
      } else {
        ++state.memoryStatsToken;
        state.memoryStatsBusy = false;
        state.memoryStats = null;
        state.memoryStatsScope = null;
        await Promise.all([loadMemory(), loadSoul()]);
      }
    } else if (state.view === "files") await loadFiles();
    else
      await Promise.all([
        loadList(false, true),
        state.id ? loadDetail(state.id, true) : Promise.resolve(),
      ]);
    if (token === state.refreshToken) announce("Данные обновлены");
  } finally {
    if (token === state.refreshToken) setBusy(false);
  }
}
function route() {
  const view = /^#\/memory(?:\?|$)/.test(location.hash)
    ? "memory"
    : location.hash === "#/files" ? "files" : "sessions";
  const changedView = view !== state.view;
  state.view = view;
  document.body.classList.toggle("has-files", view === "files");
  document.body.classList.toggle("has-memory", view === "memory");
  if (changedView) {
    clearTimeout(fileDebounce);
    clearTimeout(memoryDebounce);
    ++state.refreshToken;
    ++state.fileToken;
    ++state.memoryToken;
    ++state.memoryStatsToken;
    ++state.soulToken;
    state.fileBusy = state.memoryBusy = state.memoryStatsBusy = state.soulBusy = false;
    setBusy(false);
  }
  for (const name of ["sessions", "memory", "files"]) {
    const link = $("#nav-" + name);
    if (view === name) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (view === "memory") {
    clearTimeout(memoryDebounce);
    ++state.detailToken;
    ++state.refreshToken;
    ++state.memoryToken;
    ++state.memoryStatsToken;
    state.memoryBusy = state.memoryStatsBusy = false;
    setBusy(false);
    closeDrawer(false);
    state.routeTrace = "";
    const params = new URLSearchParams(location.hash.split("?")[1] || "");
    const requested = params.get("conversation_id");
    if (requested) state.memoryScope = requested;
    else if (!state.memoryScope) state.memoryScope = state.data?.session.conversation_id || "";
    state.memoryTab = params.get("tab") === "analytics" ? "analytics" : "entries";
    memoryShell();
    loadSoul();
    if (state.memoryTab === "analytics") loadMemoryStats(true);
    else loadMemory();
    return;
  }
  if (view === "files") {
    ++state.detailToken;
    closeDrawer(false);
    state.routeTrace = "";
    fileShell();
    loadFiles();
    return;
  }
  let match = location.hash.match(/^#\/session\/([^?]+)(?:\?(.*))?$/);
  let id = "";
  const trace = match ? new URLSearchParams(match[2] || "").get("trace") || "" : "";
  if (match) {
    try {
      id = decodeURIComponent(match[1]);
    } catch {}
  }
  if (id === state.id && !changedView) {
    if (trace !== state.routeTrace) {
      closeDrawer(false);
      state.routeTrace = trace;
      if (trace && state.data) openTrace(trace);
    }
    return;
  }
  state.id = id;
  state.data = null;
  state.tab = "timeline";
  closeDrawer(false);
  state.routeTrace = trace;
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

const memoryKinds = {
  profile: "Профиль",
  preference: "Предпочтение",
  fact: "Факт",
  episode: "Эпизод",
  hypothesis: "Предположение",
};
const memoryTabs = ["entries", "analytics"];
const triggerOrder = ["message", "task", "initiative", "coding", "unknown"];
const triggerLabels = {
  message: "Сообщение",
  task: "Задача",
  initiative: "Инициатива",
  coding: "Кодинг",
  unknown: "Неизвестно",
};
const triggerShort = {
  message: "С",
  task: "З",
  initiative: "И",
  coding: "К",
  unknown: "?",
};
const isMac = navigator.platform?.includes("Mac");

function memoryHash(scope = state.memoryScope, tab = state.memoryTab) {
  const params = new URLSearchParams();
  if (scope) params.set("conversation_id", scope);
  if (tab === "analytics") params.set("tab", "analytics");
  const qs = params.toString();
  return qs ? "#/memory?" + qs : "#/memory";
}

function syncMemoryTabUI() {
  if (state.view !== "memory") return;
  const isEntries = state.memoryTab === "entries";
  const entriesTab = $("#memory-tab-entries");
  const analyticsTab = $("#memory-tab-analytics");
  if (!entriesTab || !analyticsTab) return;
  entriesTab.setAttribute("aria-selected", String(isEntries));
  entriesTab.tabIndex = isEntries ? 0 : -1;
  analyticsTab.setAttribute("aria-selected", String(!isEntries));
  analyticsTab.tabIndex = isEntries ? -1 : 0;
  $("#memory-panel-entries").classList.toggle("hidden", !isEntries);
  $("#memory-panel-analytics").classList.toggle("hidden", isEntries);
  for (const el of main.querySelectorAll(".memory-entries-only")) {
    el.classList.toggle("hidden", !isEntries);
  }
  $(".memory-filters")?.classList.toggle("analytics-mode", !isEntries);
  const subtitle = $("#memory-subtitle");
  if (subtitle) {
    subtitle.textContent = isEntries
      ? "Текущие записи · сначала обновлённые"
      : "Рост памяти, бюджет USER.md и источники";
  }
  document.title = isEntries
    ? "Память · OpenAlbert Inspector"
    : "Аналитика памяти · OpenAlbert Inspector";
}

function resetMemoryEntriesView() {
  state.memories = [];
  state.memoryNext = 0;
  const list = $("#memory-list");
  if (list) {
    list.setAttribute("aria-busy", "true");
    list.innerHTML = '<div class="loading-line" role="status">Загружаем память…</div>';
  }
  if ($("#memory-count")) $("#memory-count").textContent = "";
  if ($("#memory-tab-entries-count")) $("#memory-tab-entries-count").textContent = "";
  $("#more-memory")?.classList.add("hidden");
  $("#memory-profile")?.classList.add("hidden");
  if ($("#memory-profile pre")) $("#memory-profile pre").textContent = "";
  const group = $("#memory-group");
  if (group && group.dataset.scope !== state.memoryScope) {
    group.classList.add("hidden");
    if ($("#memory-group pre")) $("#memory-group pre").textContent = "";
    $("#group-error")?.classList.add("hidden");
  }
  $("#memory-error")?.classList.add("hidden");
}

function setMemoryTab(tab, { focusTab = false, forceReload = false } = {}) {
  if (state.view !== "memory" || !memoryTabs.includes(tab)) return;
  const changed = state.memoryTab !== tab;
  state.memoryTab = tab;
  history.replaceState(null, "", memoryHash(state.memoryScope, state.memoryTab));
  const needsEntriesReload =
    tab === "entries" &&
    (forceReload ||
      state.memoryEntriesDirty ||
      state.memoryEntriesScope !== state.memoryScope);
  if (needsEntriesReload) resetMemoryEntriesView();
  syncMemoryTabUI();
  if (focusTab) $(`#memory-tab-${tab}`)?.focus();
  if (tab === "analytics") {
    if (forceReload || state.memoryStatsScope !== state.memoryScope || !state.memoryStats) {
      loadMemoryStats(false);
    }
  } else if (needsEntriesReload) {
    loadMemory();
  }
  if (changed) {
    announce(tab === "analytics" ? "Открыта вкладка «Аналитика»" : "Открыта вкладка «Записи»");
  }
}

function populateMemoryScopes(conversations) {
  if (!state.memoryScope) state.memoryScope = conversations[0]?.id || "";
  const choices = [...conversations];
  // Keep a bookmarked scope accessible even if its conversation metadata is gone.
  if (state.memoryScope && !choices.some((c) => c.id === state.memoryScope))
    choices.push({ id: state.memoryScope, title: "Разговор без метаданных" });
  $("#memory-scope").innerHTML = choices.length
    ? choices
        .map(
          (c) =>
            `<option value="${esc(c.id)}" ${c.id === state.memoryScope ? "selected" : ""}>${esc(c.title || (c.private ? "Личный разговор" : "Групповой разговор"))} · ${esc(c.id)}</option>`,
        )
        .join("")
    : '<option value="">Разговоров пока нет</option>';
  $("#memory-scope").disabled = !choices.length;
  return conversations.find((c) => c.id === state.memoryScope);
}

function memoryShell() {
  state.memories = [];
  state.memoryNext = 0;
  state.memoryStats = null;
  state.memoryStatsScope = null;
  state.memoryEntriesScope = null;
  state.memoryEntriesDirty = false;
  const altPrefix = isMac ? "⌥" : "Alt";
  const altTitle = isMac ? "Alt" : "Alt";
  main.innerHTML = `<div class="session-toolbar memory-toolbar"><div class="session-heading"><h2>Память</h2><span id="memory-subtitle" class="muted">Текущие записи · сначала обновлённые</span></div><div class="memory-filters"><label>Разговор<select id="memory-scope"><option value="">Загружаем разговоры…</option></select></label><label class="memory-entries-only">Поиск<div class="search-wrap"><input id="memory-search" type="search" placeholder="Текст, ID, источник или участник…" autocomplete="off" value="${esc(state.memoryQuery)}" aria-keyshortcuts="Meta+K Control+K"><kbd>${isMac ? "⌘ K" : "Ctrl K"}</kbd></div></label><label class="memory-entries-only">Тип<select id="memory-kind"><option value="">Все типы</option>${Object.entries(memoryKinds).map(([key, title]) => `<option value="${key}" ${state.memoryKind === key ? "selected" : ""}>${title}</option>`).join("")}</select></label></div><p class="memory-hint memory-entries-only">Все слова запроса должны встречаться в одной записи. Поиск без учёта регистра.</p><nav class="tabs" role="tablist" aria-label="Разделы памяти"><button class="tab" id="memory-tab-entries" role="tab" data-memory-tab="entries" aria-controls="memory-panel-entries" aria-selected="true" tabindex="0" title="Список записей (${altTitle}+1)" aria-keyshortcuts="Alt+1">Записи <small id="memory-tab-entries-count"></small><kbd>${altPrefix} 1</kbd></button><button class="tab" id="memory-tab-analytics" role="tab" data-memory-tab="analytics" aria-controls="memory-panel-analytics" aria-selected="false" tabindex="-1" title="Аналитика памяти (${altTitle}+2)" aria-keyshortcuts="Alt+2">Аналитика <kbd>${altPrefix} 2</kbd></button></nav></div><section id="memory-panel-entries" role="tabpanel" aria-labelledby="memory-tab-entries"><div id="memory-error" class="error hidden" role="alert"></div><details id="memory-soul" class="analysis-row"><summary title="Раскрыть или свернуть SOUL.md · Enter" aria-keyshortcuts="Enter">Личность SOUL.md <span class="muted">Имя, характер и правила общения OpenAlbert · общие для всех разговоров</span></summary><div class="run-body" aria-busy="true"><div id="soul-error" class="error hidden" role="alert"></div><pre class="prose" role="status">Загружаем SOUL.md…</pre></div></details><details id="memory-group" class="analysis-row hidden"><summary title="Раскрыть или свернуть GROUP.md · Enter / Пробел" aria-keyshortcuts="Enter Space">Правила группы GROUP.md <span class="muted">Общие для всех топиков выбранной группы</span></summary><div class="run-body" aria-busy="false"><div id="group-error" class="error hidden" role="alert"></div><pre class="prose" role="status"></pre></div></details><details id="memory-profile" class="analysis-row hidden"><summary>Профиль USER.md <span class="muted">Контекст новых сессий этого разговора</span></summary><div class="run-body"><pre class="prose"></pre></div></details><div id="memory-list" aria-busy="true"><div class="loading-line" role="status">Загружаем память…</div></div><div class="pagination"><span id="memory-count" role="status"></span><button id="more-memory" class="button hidden" title="Загрузить ещё записи · Enter" aria-keyshortcuts="Enter">Ещё записи</button></div></section><section id="memory-panel-analytics" class="hidden" role="tabpanel" aria-labelledby="memory-tab-analytics"><div id="memory-analytics-error" class="error hidden" role="alert"></div><div id="memory-analytics" aria-busy="false"><div class="loading-line" role="status">Загружаем аналитику памяти…</div></div></section>`;
  syncMemoryTabUI();
}
async function loadSoul() {
  if (state.view !== "memory") return;
  const token = ++state.soulToken;
  state.soulBusy = true;
  setBusy(state.busy);
  $("#memory-soul .run-body").setAttribute("aria-busy", "true");
  $("#soul-error").classList.add("hidden");
  try {
    const { soul } = await api("memory/soul");
    if (token !== state.soulToken || state.view !== "memory") return;
    $("#memory-soul pre").textContent = soul || "Файл SOUL.md пуст.";
  } catch (error) {
    if (token !== state.soulToken || state.view !== "memory") return;
    $("#soul-error").textContent = `Не удалось загрузить SOUL.md. ${error.message} Нажмите «Обновить» (Alt+R).`;
    $("#soul-error").classList.remove("hidden");
    $("#memory-soul pre").textContent = "";
  } finally {
    if (token === state.soulToken && state.view === "memory") {
      state.soulBusy = false;
      setBusy(state.busy);
      $("#memory-soul .run-body").setAttribute("aria-busy", "false");
    }
  }
}

async function loadGroupRules(scope, isGroup, token) {
  const section = $("#memory-group");
  section.dataset.scope = scope;
  section.classList.toggle("hidden", !isGroup);
  $("#group-error").classList.add("hidden");
  $("#memory-group pre").textContent = "";
  $("#memory-group .run-body").setAttribute("aria-busy", "false");
  if (!isGroup) return;
  $("#memory-group .run-body").setAttribute("aria-busy", "true");
  $("#memory-group pre").textContent = "Загружаем GROUP.md…";
  try {
    const { group } = await api("memory/group?" + new URLSearchParams({ conversation_id: scope }));
    if (token !== state.memoryToken || state.view !== "memory") return;
    $("#memory-group pre").textContent = group?.trim()
      ? group
      : "Дополнительные правила не заданы: GROUP.md не создан или пуст. Действуют общие правила и SOUL.md.";
  } catch (error) {
    if (token !== state.memoryToken || state.view !== "memory") return;
    $("#group-error").textContent = `Не удалось загрузить GROUP.md. ${error.message} Нажмите «Обновить» (Alt+R).`;
    $("#group-error").classList.remove("hidden");
    $("#memory-group pre").textContent = "";
  } finally {
    if (token === state.memoryToken && state.view === "memory")
      $("#memory-group .run-body").setAttribute("aria-busy", "false");
  }
}

function scheduleMemory() {
  clearTimeout(memoryDebounce);
  ++state.memoryToken;
  state.memoryBusy = true;
  setBusy(state.busy);
  resetMemoryEntriesView();
  memoryDebounce = setTimeout(() => loadMemory(), 250);
}
async function loadMemory(more = false) {
  if (state.view !== "memory") return;
  clearTimeout(memoryDebounce);
  const restoreMore = more && document.activeElement === $("#more-memory");
  const token = ++state.memoryToken;
  state.memoryBusy = true;
  setBusy(state.busy);
  $("#memory-list").setAttribute("aria-busy", "true");
  $("#more-memory").disabled = true;
  $("#memory-error").classList.add("hidden");
  let groupLoad;
  try {
    if (!more) {
      const { conversations } = await api("memory/conversations");
      if (token !== state.memoryToken || state.view !== "memory") return;
      const selected = populateMemoryScopes(conversations);
      groupLoad = loadGroupRules(state.memoryScope, selected && !selected.private && selected.chat_id < 0, token);
    }
    const requestedScope = state.memoryScope;
    const result = requestedScope
      ? await api("memory?" + new URLSearchParams({
          conversation_id: requestedScope,
          q: state.memoryQuery,
          kind: state.memoryKind,
          after: more ? state.memoryNext : 0,
        }))
      : { entries: [], total: 0, next_after: 0, profile: "" };
    if (
      token !== state.memoryToken ||
      state.view !== "memory" ||
      requestedScope !== state.memoryScope
    )
      return;
    const focusedID = document.activeElement?.dataset.memory;
    state.memories = more ? [...state.memories, ...result.entries] : result.entries;
    state.memoryNext = result.next_after || 0;
    state.memoryEntriesScope = requestedScope;
    state.memoryEntriesDirty = false;
    $("#memory-profile").classList.toggle("hidden", !result.profile);
    $("#memory-profile pre").textContent = result.profile || "";
    $("#memory-list").innerHTML = state.memories.length
      ? `<ul class="memory-list">${state.memories.map((e) => `<li><button class="memory-row" data-memory="${esc(e.id)}" title="Просмотреть запись · Enter" aria-keyshortcuts="Enter"><span class="memory-meta"><span>${esc(memoryKinds[e.kind] || e.kind)}</span><time>Обновлено ${esc(date(e.updated_at))}</time>${e.subject_user_id ? `<span>Участник ${esc(e.subject_user_id)}</span>` : ""}</span><span class="prose memory-preview">${esc(e.content)}</span><span class="mono muted">${esc(e.id)}</span></button></li>`).join("")}</ul>`
      : `<div class="empty-panel">${!state.memoryScope ? "Разговоров пока нет. Память появится после общения с OpenAlbert." : state.memoryQuery || state.memoryKind ? "Записей с такими условиями нет. Измените или очистите фильтры." : "В этом разговоре пока нет записей памяти."}</div>`;
    if (focusedID)
      [...main.querySelectorAll("[data-memory]")].find((el) => el.dataset.memory === focusedID)?.focus({ preventScroll: true });
    $("#memory-count").textContent = `Записи: ${state.memories.length} из ${result.total}`;
    const tabCountEl = $("#memory-tab-entries-count");
    if (tabCountEl) tabCountEl.textContent = result.total ? String(result.total) : "";
    $("#more-memory").classList.toggle("hidden", !state.memoryNext);
    $("#memory-list").setAttribute("aria-busy", "false");
  } catch (error) {
    if (token !== state.memoryToken || state.view !== "memory") return;
    $("#memory-error").textContent = error.message;
    $("#memory-error").classList.remove("hidden");
    if (!state.memories.length)
      $("#memory-list").innerHTML = '<div class="empty-panel">Не удалось загрузить память. Нажмите «Обновить» (Alt+R).</div>';
  } finally {
    await groupLoad;
    if (token === state.memoryToken && state.view === "memory") {
      state.memoryBusy = false;
      setBusy(state.busy);
      $("#memory-list").setAttribute("aria-busy", "false");
      $("#more-memory").disabled = false;
      if (restoreMore && document.activeElement === document.body)
        (state.memoryNext ? $("#more-memory") : [...main.querySelectorAll("[data-memory]")].at(-1))?.focus({ preventScroll: true });
    }
  }
}
async function loadMemoryStats(withConversations = false) {
  if (state.view !== "memory") return;
  const token = ++state.memoryStatsToken;
  state.memoryStatsBusy = true;
  setBusy(state.busy);
  const container = $("#memory-analytics");
  const errBox = $("#memory-analytics-error");
  if (!container || !errBox) return;
  errBox.classList.add("hidden");
  container.setAttribute("aria-busy", "true");
  if (!state.memoryStats || state.memoryStatsScope !== state.memoryScope) {
    container.innerHTML = '<div class="loading-line" role="status">Загружаем аналитику памяти…</div>';
  }
  try {
    if (withConversations || $("#memory-scope")?.options.length <= 1) {
      const { conversations } = await api("memory/conversations");
      if (token !== state.memoryStatsToken || state.view !== "memory") return;
      populateMemoryScopes(conversations);
    }
    if (!state.memoryScope) {
      state.memoryStats = null;
      state.memoryStatsScope = "";
      container.innerHTML =
        '<div class="empty-panel">Разговоров пока нет. Аналитика памяти появится после общения с OpenAlbert.</div>';
      return;
    }
    const requestedScope = state.memoryScope;
    const stats = await api(
      "memory/stats?" + new URLSearchParams({ conversation_id: requestedScope }),
    );
    if (
      token !== state.memoryStatsToken ||
      state.view !== "memory" ||
      requestedScope !== state.memoryScope
    )
      return;
    state.memoryStats = stats;
    state.memoryStatsScope = requestedScope;
    const tabCountEl = $("#memory-tab-entries-count");
    if (tabCountEl) {
      tabCountEl.textContent =
        !state.memoryQuery && !state.memoryKind && stats.total_entries
          ? String(stats.total_entries)
          : "";
    }
    renderMemoryAnalytics(stats);
  } catch (error) {
    if (token !== state.memoryStatsToken || state.view !== "memory") return;
    errBox.textContent = error.message;
    errBox.classList.remove("hidden");
    if (!state.memoryStats || state.memoryStatsScope !== state.memoryScope) {
      container.innerHTML =
        '<div class="empty-panel">Не удалось загрузить аналитику памяти. Нажмите «Обновить» (Alt+R).</div>';
    }
  } finally {
    if (token === state.memoryStatsToken && state.view === "memory") {
      state.memoryStatsBusy = false;
      setBusy(state.busy);
      container.setAttribute("aria-busy", "false");
    }
  }
}

const fmtNum = (n) => Number(n || 0).toLocaleString("ru-RU");
const fmtPct = (n) =>
  Number(n || 0).toLocaleString("ru-RU", { maximumFractionDigits: 1 });
const fmtDayShort = (iso) => {
  const parts = String(iso || "").split("-");
  return parts.length === 3 ? `${parts[2]}.${parts[1]}` : iso;
};
const fmtSigned = (n) => (n > 0 ? `+${fmtNum(n)}` : n < 0 ? `−${fmtNum(Math.abs(n))}` : "0");

function activeTriggers(counts) {
  return triggerOrder.filter(
    (key) => key === "message" || key === "task" || key === "initiative" || (counts?.[key] || 0) > 0,
  );
}

function renderDailyGrowthChart(stats) {
  const series = stats.daily_growth || [];
  const legendKeys = activeTriggers(stats.triggers);
  const legend = `<div class="analytics-legend" aria-label="Легенда источников">${legendKeys
    .map(
      (key) =>
        `<span class="legend-item"><svg class="legend-swatch" viewBox="0 0 10 10" width="10" height="10" aria-hidden="true"><rect width="10" height="10" rx="2" class="bar-${key}"></rect></svg>${esc(triggerLabels[key])}</span>`,
    )
    .join("")}</div>`;
  if (!series.length) {
    return `<section class="analytics-panel"><div class="analytics-panel-header"><h3>Динамика по дням</h3>${legend}</div><p class="analytics-empty">Нет записей с сохранённой датой создания.</p></section>`;
  }
  const width = 960,
    height = 210,
    padLeft = 38,
    padRight = 14,
    padTop = 14,
    padBottom = 28;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;
  const maxVal = Math.max(1, ...series.map((d) => d.total || 0));
  const yTicks = [0, Math.ceil(maxVal / 2), maxVal];
  const uniqueTicks = [...new Set(yTicks)];
  const slot = plotW / series.length;
  const barW = Math.max(4, Math.min(34, slot * 0.62));
  const labelStep = Math.max(1, Math.ceil(series.length / 10));

  const gridMarkup = uniqueTicks
    .map((tick) => {
      const y = padTop + plotH - (tick / maxVal) * plotH;
      return `<line x1="${padLeft}" y1="${y.toFixed(1)}" x2="${width - padRight}" y2="${y.toFixed(1)}" class="chart-grid"></line><text x="${padLeft - 6}" y="${(y + 4).toFixed(1)}" text-anchor="end" class="chart-axis-text">${tick}</text>`;
    })
    .join("");

  const barsMarkup = series
    .map((day, idx) => {
      const cx = padLeft + slot * idx + slot / 2;
      const x = cx - barW / 2;
      let cursorY = padTop + plotH;
      const tipParts = legendKeys
        .filter((k) => (day[k] || 0) > 0)
        .map((k) => `${triggerLabels[k]}: ${day[k]}`);
      const tip = `${day.date} — всего ${day.total}${tipParts.length ? ` (${tipParts.join(", ")})` : ""}`;
      const rects = triggerOrder
        .map((key) => {
          const val = day[key] || 0;
          if (!val) return "";
          const h = Math.max(1, (val / maxVal) * plotH);
          cursorY -= h;
          return `<rect x="${x.toFixed(1)}" y="${cursorY.toFixed(1)}" width="${barW.toFixed(1)}" height="${h.toFixed(1)}" class="bar-${key}"><title>${esc(tip)}</title></rect>`;
        })
        .join("");
      const lastIdx = series.length - 1;
      const showLabel =
        idx === 0 ||
        idx === lastIdx ||
        (idx % labelStep === 0 && lastIdx - idx >= Math.ceil(labelStep * 0.6));
      const xLabel = showLabel
        ? `<text x="${cx.toFixed(1)}" y="${height - 8}" text-anchor="middle" class="chart-axis-text">${esc(fmtDayShort(day.date))}</text>`
        : "";
      return `<g class="chart-day" data-date="${esc(day.date)}">${rects}${xLabel}</g>`;
    })
    .join("");

  const notes = [];
  if (stats.daily_growth_window_days) {
    notes.push(`Показаны последние ${fmtNum(stats.daily_growth_window_days)} дн.`);
  }
  if (stats.fallback_date_entries > 0) {
    notes.push(
      `Для ${fmtNum(stats.fallback_date_entries)} зап. без связанной сессии использована дата обновления.`,
    );
  }
  const activeRows = series.filter((d) => d.total > 0);
  const tableRows = (activeRows.length ? activeRows : series)
    .map(
      (d) =>
        `<tr><td class="mono nowrap">${esc(d.date)}</td><td class="mono">${fmtNum(d.total)}</td>${legendKeys.map((k) => `<td class="mono">${fmtNum(d[k] || 0)}</td>`).join("")}</tr>`,
    )
    .join("");
  const dataTable = `<details class="chart-table-details"><summary title="Таблица по дням · Enter / Пробел" aria-keyshortcuts="Enter Space">Таблица по дням (${activeRows.length || series.length})</summary><div class="table-scroll"><table class="data-table chart-data-table"><thead><tr><th>Дата</th><th>Всего</th>${legendKeys.map((k) => `<th>${esc(triggerLabels[k])}</th>`).join("")}</tr></thead><tbody>${tableRows}</tbody></table></div></details>`;

  const summaryLabel = `Ежедневные добавления памяти: ${series.length} дн., максимум ${maxVal} зап. в день.`;
  return `<section class="analytics-panel"><div class="analytics-panel-header"><h3>Динамика по дням</h3>${legend}</div>${notes.length ? `<p class="chart-note muted">${esc(notes.join(" "))}</p>` : ""}<div class="chart-wrap" tabindex="0" aria-label="График динамики по дням"><svg class="growth-chart" viewBox="0 0 ${width} ${height}" role="img" aria-label="${esc(summaryLabel)}">${gridMarkup}${barsMarkup}</svg></div>${dataTable}</section>`;
}

function renderProfileBudgetPanel(stats) {
  const b = stats.profile_budget || {
    limit: 8000,
    total_chars: 0,
    included_chars: 0,
    truncated_chars: 0,
    usage_percent: 0,
    status: "ok",
    total_entries: 0,
    included_entries: 0,
    truncated_entries: 0,
    truncated_items: [],
  };
  const limit = b.limit || 8000;
  const barW = 400;
  const barH = 16;
  let barContent = "";
  if (b.total_chars > limit) {
    const incW = Math.max(4, (limit / b.total_chars) * barW);
    const truncW = Math.max(2, barW - incW);
    barContent = `<rect x="0" y="0" width="${incW.toFixed(1)}" height="${barH}" class="bar-included"></rect><rect x="${incW.toFixed(1)}" y="0" width="${truncW.toFixed(1)}" height="${barH}" class="bar-truncated"></rect><line x1="${incW.toFixed(1)}" y1="0" x2="${incW.toFixed(1)}" y2="${barH}" class="bar-limit-mark"></line>`;
  } else if (b.total_chars > 0) {
    const fillW = Math.max(2, (b.total_chars / limit) * barW);
    const fillClass = b.status === "warning" ? "bar-warning" : "bar-included";
    barContent = `<rect x="0" y="0" width="${fillW.toFixed(1)}" height="${barH}" rx="3" class="${fillClass}"></rect>`;
  }
  const items = b.truncated_items || [];
  const truncatedMarkup = items.length
    ? `<div class="truncated-heading">Усечённые записи (${items.length})</div><ul class="truncated-list">${items
        .map(
          (item) =>
            `<li><button class="truncated-row" data-memory="${esc(item.id)}" title="Открыть запись памяти · Enter" aria-keyshortcuts="Enter"><span class="truncated-meta"><span class="truncated-kind">${esc(memoryKinds[item.kind] || item.kind)}</span><span class="state ${item.partial ? "pending" : "failed"}">${item.partial ? "Частично усечена" : "Усечена полностью"}</span><span class="mono muted">${fmtNum(item.chars)} симв.</span></span><span class="prose truncated-preview">${esc(item.content)}</span><span class="mono muted">${esc(item.id)}</span></button></li>`,
        )
        .join("")}</ul>`
    : `<p class="analytics-empty">${b.total_chars > 0 ? "Все записи профиля и предпочтений целиком помещаются в лимит 8 000 символов." : "Файл USER.md для этого разговора пуст."}</p>`;

  const barAria = `Бюджет USER.md: ${b.total_chars} из ${limit} символов (${fmtPct(b.usage_percent)}%), усечено записей: ${b.truncated_entries}.`;
  return `<section class="analytics-panel budget-panel budget-${esc(b.status)}"><div class="analytics-panel-header"><h3>Бюджет USER.md и усечение</h3><span class="budget-badge budget-badge-${esc(b.status)}">${fmtNum(b.total_chars)} / ${fmtNum(limit)} (${fmtPct(b.usage_percent)}%)</span></div><svg class="budget-bar-svg" viewBox="0 0 ${barW} ${barH}" preserveAspectRatio="none" role="img" aria-label="${esc(barAria)}"><rect x="0" y="0" width="${barW}" height="${barH}" rx="3" class="bar-track"></rect>${barContent}</svg><div class="budget-stats"><span>В промпте: <strong>${fmtNum(b.included_chars)}</strong> симв. (${b.included_entries} зап.)</span><span>Усечено: <strong>${fmtNum(b.truncated_chars)}</strong> симв. (${b.truncated_entries} зап.)</span></div>${truncatedMarkup}</section>`;
}

function renderKindBreakdownPanel(stats) {
  const kinds = stats.kinds || [];
  const maxTotal = Math.max(1, ...kinds.map((k) => k.total || 0));
  const barW = 240,
    barH = 10;
  const rows = kinds
    .map((k) => {
      const labelText = memoryKinds[k.kind] || k.kind;
      const totalW = k.total ? Math.max(4, (k.total / maxTotal) * barW) : 0;
      let cursor = 0;
      const segs = triggerOrder
        .map((t) => {
          const v = k[t] || 0;
          if (!v || !k.total) return "";
          const w = (v / k.total) * totalW;
          const x = cursor;
          cursor += w;
          return `<rect x="${x.toFixed(1)}" y="0" width="${Math.max(1, w).toFixed(1)}" height="${barH}" class="bar-${t}"></rect>`;
        })
        .join("");
      const parts = activeTriggers(k)
        .filter((t) => (k[t] || 0) > 0)
        .map((t) => `${triggerShort[t]} ${k[t]}`)
        .join(" · ");
      const title = `Показать записи типа «${labelText}» · Enter`;
      return `<li><button class="kind-bar-row" data-filter-kind="${esc(k.kind)}" title="${esc(title)}" aria-label="${esc(title)}" aria-keyshortcuts="Enter"><span class="kind-bar-name">${esc(labelText)}</span><svg class="kind-bar-svg" viewBox="0 0 ${barW} ${barH}" preserveAspectRatio="none" aria-hidden="true"><rect x="0" y="0" width="${barW}" height="${barH}" rx="2" class="bar-track"></rect>${segs}</svg><span class="kind-bar-total">${fmtNum(k.total)}</span><span class="kind-bar-split muted">${esc(parts || "—")}</span></button></li>`;
    })
    .join("");
  return `<section class="analytics-panel"><div class="analytics-panel-header"><h3>Типы записей по источникам</h3><span class="muted analytics-sub">Нажмите, чтобы отфильтровать записи</span></div><ul class="kind-bar-list">${rows}</ul></section>`;
}

function renderGeneratorsPanel(stats) {
  const gens = stats.top_generators || [];
  if (!gens.length) {
    return `<section class="analytics-panel"><div class="analytics-panel-header"><h3>Основные фоновые источники</h3></div><p class="analytics-empty">Фоновые задачи и инициатива пока не создавали записей памяти в этом разговоре.</p></section>`;
  }
  const maxCount = Math.max(1, ...gens.map((g) => g.count || 0));
  const barW = 120,
    barH = 8;
  const rows = gens
    .map((g) => {
      const name =
        g.trigger === "initiative"
          ? "Инициатива"
          : g.title || (g.task_id ? `Задача ${g.task_id}` : "Фоновая задача");
      const w = Math.max(3, (g.count / maxCount) * barW);
      return `<li class="generator-row"><div class="generator-info"><div class="generator-title"><span class="generator-name">${esc(name)}</span><span class="trigger-tag trigger-tag-${esc(g.trigger)}">${esc(triggerLabels[g.trigger] || g.trigger)}</span></div><div class="generator-meta muted">${g.task_id ? `<span class="mono">${esc(g.task_id)}</span><span>·</span>` : ""}<span>${fmtNum(g.chars)} симв.</span>${g.latest_at ? `<span>·</span><time>${esc(shortDate(g.latest_at))}</time>` : ""}</div></div><div class="generator-metric"><svg class="generator-bar-svg" viewBox="0 0 ${barW} ${barH}" preserveAspectRatio="none" aria-hidden="true"><rect x="0" y="0" width="${barW}" height="${barH}" rx="2" class="bar-track"></rect><rect x="0" y="0" width="${w.toFixed(1)}" height="${barH}" rx="2" class="bar-${esc(g.trigger)}"></rect></svg><span class="mono generator-count">${fmtNum(g.count)}</span></div></li>`;
    })
    .join("");
  return `<section class="analytics-panel"><div class="analytics-panel-header"><h3>Основные фоновые источники</h3><span class="muted analytics-sub">По количеству записей</span></div><ul class="generator-list">${rows}</ul></section>`;
}

function renderConsolidationPanel(stats) {
  const c = stats.latest_consolidation;
  if (!c) return "";
  const entryDelta = (c.after_entries || 0) - (c.before_entries || 0);
  const charDelta = c.profile_chars_delta ?? (c.after_profile_chars || 0) - (c.before_profile_chars || 0);
  const ops = c.operations || {};
  const opEntries = [
    ["Объединено", ops.merged],
    ["Перенесено", ops.reclassified],
    ["Удалено", ops.deleted],
    ["Обновлено", ops.updated],
    ["Сохранено", ops.kept],
    ["Пропущено", ops.skipped],
  ].filter(([, count]) => count > 0);
  return `<section class="analytics-panel consolidation-panel"><div class="analytics-panel-header"><h3>Последняя консолидация</h3><span class="muted analytics-sub">${c.completed_at ? esc(date(c.completed_at)) : esc(c.id)}</span></div><dl class="consolidation-metrics"><div><dt>Записи</dt><dd class="mono">${fmtNum(c.before_entries)} → ${fmtNum(c.after_entries)} <span class="${entryDelta < 0 ? "trend-good" : "muted"}">(${fmtSigned(entryDelta)})</span></dd></div><div><dt>USER.md</dt><dd class="mono">${fmtNum(c.before_profile_chars)} → ${fmtNum(c.after_profile_chars)} симв. <span class="${charDelta < 0 ? "trend-good" : "muted"}">(${fmtSigned(charDelta)})</span></dd></div></dl>${opEntries.length ? `<div class="consolidation-ops">${opEntries.map(([name, count]) => `<span class="op-pill">${esc(name)}: <strong>${fmtNum(count)}</strong></span>`).join("")}</div>` : ""}<div class="consolidation-actions"><button id="open-consolidation-log" class="button" data-consolidation-log="true" title="Открыть журнал операций консолидации · Enter" aria-keyshortcuts="Enter">Журнал операций</button></div></section>`;
}

function renderMemoryAnalytics(stats) {
  const container = $("#memory-analytics");
  if (!container) return;
  const focusedMemory = document.activeElement?.dataset.memory;
  const focusedKind = document.activeElement?.dataset.filterKind;
  const focusedConsolidation = document.activeElement?.id === "open-consolidation-log";

  const t = stats.triggers || {};
  const b = stats.profile_budget || { limit: 8000, total_chars: 0, usage_percent: 0, status: "ok", truncated_entries: 0 };
  const tr = stats.trend || { last_7_days: 0, prev_7_days: 0, delta: 0, avg_per_day: 0 };
  const deltaText =
    tr.delta_percent !== undefined && tr.delta_percent !== null
      ? `${fmtSigned(tr.delta)} (${fmtSigned(tr.delta_percent)}%) к прошлым 7 дн.`
      : tr.prev_7_days === 0 && tr.last_7_days > 0
        ? `За прошлые 7 дн.: 0 · ~${fmtPct(tr.avg_per_day)}/дн.`
        : `За прошлые 7 дн.: ${fmtNum(tr.prev_7_days)}`;
  const trendClass = tr.delta > 0 ? "kpi-delta-up" : tr.delta < 0 ? "kpi-delta-down" : "muted";
  const hasConsolidation = Boolean(stats.latest_consolidation);

  const kpis = `<div class="analytics-kpis">
    <div class="kpi-card">
      <div class="kpi-label">Всего записей</div>
      <div class="kpi-value">${fmtNum(stats.total_entries)}</div>
      <div class="kpi-sub muted">${esc(fileSize(stats.total_bytes || 0))} · ${fmtNum(stats.total_chars)} симв.${stats.span_days ? ` · ${fmtNum(stats.span_days)} дн.` : ""}</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Фоновые сессии</div>
      <div class="kpi-value">${fmtPct(stats.background_share_percent)}%</div>
      <div class="kpi-sub muted">Инициатива ${fmtNum(t.initiative)} · Задача ${fmtNum(t.task)} · Сообщения ${fmtNum(t.message)}</div>
    </div>
    <div class="kpi-card kpi-budget-${esc(b.status)}">
      <div class="kpi-label">Бюджет USER.md</div>
      <div class="kpi-value">${fmtNum(b.total_chars)} / ${fmtNum(b.limit)}</div>
      <div class="kpi-sub ${b.status === "over" ? "kpi-sub-danger" : b.status === "warning" ? "kpi-sub-warning" : "muted"}">${fmtPct(b.usage_percent)}% · ${b.truncated_entries > 0 ? `Усечено: ${fmtNum(b.truncated_entries)}` : b.total_chars > 0 ? "Помещается полностью" : "USER.md пуст"}</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Рост за 7 дней</div>
      <div class="kpi-value">${fmtNum(tr.last_7_days)}</div>
      <div class="kpi-sub ${trendClass}">${esc(deltaText)}</div>
    </div>
  </div>`;

  if (stats.total_entries === 0 && !b.total_chars && !hasConsolidation) {
    container.innerHTML = `<div class="analytics-root">${kpis}<div class="empty-panel">В этом разговоре пока нет записей памяти. После первых сессий здесь появятся графики роста, бюджет USER.md и разбивка по источникам.</div></div>`;
    return;
  }

  container.innerHTML = `<div class="analytics-root">
    ${kpis}
    ${renderDailyGrowthChart(stats)}
    <div class="analytics-grid">
      ${renderProfileBudgetPanel(stats)}
      ${renderKindBreakdownPanel(stats)}
    </div>
    <div class="analytics-grid ${hasConsolidation ? "" : "analytics-grid-single"}">
      ${renderGeneratorsPanel(stats)}
      ${renderConsolidationPanel(stats)}
    </div>
  </div>`;

  if (focusedMemory) {
    [...container.querySelectorAll("[data-memory]")].find((el) => el.dataset.memory === focusedMemory)?.focus({ preventScroll: true });
  } else if (focusedKind) {
    [...container.querySelectorAll("[data-filter-kind]")].find((el) => el.dataset.filterKind === focusedKind)?.focus({ preventScroll: true });
  } else if (focusedConsolidation) {
    $("#open-consolidation-log")?.focus({ preventScroll: true });
  }
}

function openConsolidationLog(opener) {
  const c = state.memoryStats?.latest_consolidation;
  if (!c) return;
  state.drawerMemoryID = null;
  state.drawerFileKey = state.drawerTraceID = state.drawerRecordID = null;
  state.drawerConsolidation = true;
  showDrawer("Журнал консолидации памяти", opener);
  const ops = c.operations || {};
  const opItems = [
    ["Объединено", ops.merged],
    ["Перенесено", ops.reclassified],
    ["Удалено", ops.deleted],
    ["Обновлено", ops.updated],
    ["Сохранено", ops.kept],
    ["Пропущено", ops.skipped],
  ].filter(([, n]) => n > 0);
  const logOps = Array.isArray(c.log?.operations)
    ? c.log.operations
    : Array.isArray(c.log?.ops)
      ? c.log.ops
      : [];
  const opsMarkup = logOps.length
    ? `<h3>Операции <small>${logOps.length}</small></h3><ul class="consolidation-log-list">${logOps
        .map(
          (item) =>
            `<li><div class="consolidation-log-line"><strong>${esc(item.op || item.action || item.type || "operation")}</strong>${Array.isArray(item.ids) && item.ids.length ? `<span class="mono muted">${esc(item.ids.join(", "))}</span>` : item.id ? `<span class="mono muted">${esc(item.id)}</span>` : ""}</div>${item.reason ? `<div class="prose">${esc(item.reason)}</div>` : ""}</li>`,
        )
        .join("")}</ul>`
    : "";
  $("#drawer-content").innerHTML = `<div class="trace-meta">${c.completed_at ? `<span>${esc(date(c.completed_at))}</span>` : ""}<span>Записи: ${fmtNum(c.before_entries)} → ${fmtNum(c.after_entries)}</span><span>USER.md: ${fmtNum(c.before_profile_chars)} → ${fmtNum(c.after_profile_chars)} симв. (${fmtSigned(c.profile_chars_delta)})</span></div><p class="mono trace-id">${esc(c.id)}</p>${opItems.length ? `<div class="consolidation-ops">${opItems.map(([name, n]) => `<span class="op-pill">${esc(name)}: <strong>${fmtNum(n)}</strong></span>`).join("")}</div>` : ""}${opsMarkup}<details class="trace-section" open><summary>Полный отчёт JSON</summary>${json(c.log || c)}</details>`;
}

async function openMemory(id, opener) {
  state.drawerMemoryID = id;
  state.drawerFileKey = state.drawerTraceID = state.drawerRecordID = null;
  state.drawerConsolidation = false;
  const token = showDrawer("Запись памяти", opener);
  $("#drawer-content").innerHTML = '<div class="loading-line" role="status">Загружаем запись…</div>';
  try {
    const e = await api("memory/entry?" + new URLSearchParams({ conversation_id: state.memoryScope, id }));
    if (token !== state.drawerToken) return;
    const session = e.source?.replace(/^session:/, "");
    $("#drawer-content").innerHTML = `<div class="trace-meta"><span>${esc(memoryKinds[e.kind] || e.kind)}</span><span>Обновлено ${esc(date(e.updated_at))}</span></div><h3>Содержание</h3><div class="prose">${esc(e.content)}</div><h3>Происхождение</h3><dl class="memory-origin"><dt>ID записи</dt><dd class="mono">${esc(e.id)}</dd><dt>Разговор</dt><dd class="mono">${esc(e.scope)}</dd>${e.subject_user_id ? `<dt>Участник Telegram</dt><dd class="mono">${esc(e.subject_user_id)}</dd>` : ""}<dt>Источник</dt><dd>${session ? `<a href="#/session/${encodeURIComponent(session)}" title="Открыть сессию-источник · Enter" aria-keyshortcuts="Enter">${esc(e.source)}</a>` : "Не сохранён"}</dd>${e.source_message_id ? `<dt>ID сообщения</dt><dd class="mono">${esc(e.source_message_id)}</dd>` : ""}</dl>${e.evidence ? `<h3>Цитата-основание</h3><blockquote class="prose memory-evidence">${esc(e.evidence)}</blockquote>` : ""}<details class="trace-section"><summary>Исходная запись JSON</summary>${json(e)}</details>`;
  } catch (error) {
    if (token === state.drawerToken)
      $("#drawer-content").innerHTML = `<div class="error" role="alert">${esc(error.message)}</div><p class="muted">Закройте подробности и откройте запись ещё раз.</p>`;
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
  state.drawerMemoryID = null;
  state.drawerFileKey = key;
  state.drawerTraceID = state.drawerRecordID = null;
  state.drawerConsolidation = false;
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
  state.drawerMemoryID = null;
  state.drawerFileKey = null;
  state.drawerTraceID = null;
  state.drawerRecordID = id;
  state.drawerConsolidation = false;
  let title = "Исходная запись",
    record;
  if (id.startsWith("outgoing:")) {
    record = state.data.outgoing.find((o) => o.id === id.slice(9));
    title = "Доставка";
  } else if (id.startsWith("turn:")) {
    const index = Number(id.slice(5));
    record = state.data.turns[index];
    title = `Запуск ${index + 1}`;
  } else if (id.startsWith("audio:")) {
    record = state.data.audio.find((r) => r.artifact_id === id.slice(6));
    title = "Данные распознавания";
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
  state.drawerMemoryID = null;
  state.drawerFileKey = null;
  state.drawerTraceID = id;
  state.drawerRecordID = null;
  state.drawerConsolidation = false;
  state.routeTrace = id;
  history.replaceState(null, "", `#/session/${encodeURIComponent(state.id)}?${new URLSearchParams({ trace: id })}`);
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

function closeDrawer(clearRoute = true) {
  $(".workspace").inert = false;
  $(".topbar").inert = false;
  const wasOpen = !$("#drawer").classList.contains("hidden");
  ++state.drawerToken;
  $("#drawer").classList.add("hidden");
  $("#drawer-backdrop").classList.add("hidden");
  if (wasOpen) {
    const opener = state.drawerOpener?.isConnected
      ? state.drawerOpener
      : state.drawerConsolidation
        ? $("#open-consolidation-log")
        : [
            ...main.querySelectorAll("[data-trace],[data-record],[data-file],[data-memory]"),
          ].find((el) =>
            state.drawerMemoryID
              ? el.dataset.memory === state.drawerMemoryID
              : state.drawerFileKey
                ? el.dataset.file === state.drawerFileKey
                : state.drawerRecordID
                  ? el.dataset.record === state.drawerRecordID
                  : el.dataset.trace === state.drawerTraceID,
          );
    (opener || main).focus();
  }
  state.drawerOpener = null;
  state.drawerConsolidation = false;
  if (clearRoute && state.routeTrace) {
    state.routeTrace = "";
    history.replaceState(null, "", `#/session/${encodeURIComponent(state.id)}`);
  }
}
$("#refresh").addEventListener("click", refresh);
$("#nav-logs").addEventListener("click", (event) => {
  if (state.busy || state.fileBusy || state.memoryBusy || state.memoryStatsBusy || state.soulBusy)
    event.preventDefault();
});
$("#more-sessions").addEventListener("click", () => loadList(true));
let debounce;
$("#search").addEventListener("input", () => {
  clearTimeout(debounce);
  debounce = setTimeout(() => loadList(), 250);
});
$("#trigger").addEventListener("change", () => loadList());
$("#status").addEventListener("change", () => loadList());
main.addEventListener("toggle", async (event) => {
  const row = event.target;
  if (!row.open) return;
  const code = $(".code-source > code", row);
  if (!code || code.dataset.highlighted) return;
  code.dataset.highlighted = "true";
  const source = await formatJavaScript(code.textContent);
  code.textContent = source;
  if (source.length > 32 * 1024) return;
  try {
    code.innerHTML = javascriptToken(tokenizeJavaScript(source));
  } catch {
    // The plain source remains readable if tokenization fails.
  }
}, true);

main.addEventListener("click", (event) => {
  const memoryTab = event.target.closest("[data-memory-tab]");
  if (memoryTab) {
    setMemoryTab(memoryTab.dataset.memoryTab);
    return;
  }
  const filterKind = event.target.closest("[data-filter-kind]");
  if (filterKind) {
    state.memoryKind = filterKind.dataset.filterKind;
    if ($("#memory-kind")) $("#memory-kind").value = state.memoryKind;
    state.memoryEntriesDirty = true;
    setMemoryTab("entries", { forceReload: true });
    $("#memory-kind")?.focus();
    announce(`Фильтр типа: ${memoryKinds[state.memoryKind] || state.memoryKind}`);
    return;
  }
  const consolidationBtn = event.target.closest("[data-consolidation-log]");
  if (consolidationBtn) {
    openConsolidationLog(consolidationBtn);
    return;
  }
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
  const memory = event.target.closest("[data-memory]");
  if (memory) {
    openMemory(memory.dataset.memory, memory);
    return;
  }
  if (event.target.closest("#more-memory")) {
    if (!state.memoryBusy) loadMemory(true);
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
let fileDebounce, memoryDebounce;
main.addEventListener("change", (event) => {
  if (event.target.id === "show-other-tools") {
    state.showOtherTools = event.target.checked;
    updateToolVisibility();
  } else if (event.target.id === "memory-scope") {
    state.memoryScope = event.target.value;
    history.replaceState(null, "", memoryHash(state.memoryScope, state.memoryTab));
    ++state.memoryStatsToken;
    state.memoryStatsBusy = false;
    state.memoryStats = null;
    state.memoryStatsScope = null;
    state.memoryEntriesDirty = true;
    if (state.memoryTab === "analytics") {
      clearTimeout(memoryDebounce);
      ++state.refreshToken;
      ++state.memoryToken;
      state.memoryBusy = false;
      setBusy(false);
      resetMemoryEntriesView();
      loadMemoryStats(false);
    } else {
      ++state.refreshToken;
      setBusy(false);
      scheduleMemory();
    }
  } else if (event.target.id === "memory-kind") {
    state.memoryKind = event.target.value;
    scheduleMemory();
  }
});
main.addEventListener("input", (event) => {
  if (event.target.id === "memory-search") {
    state.memoryQuery = event.target.value;
    scheduleMemory();
    return;
  }
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
    event.keyCode === 229 ||
    event.repeat ||
    !event.target.matches("[role=tab]") ||
    !["ArrowRight", "ArrowLeft", "Home", "End"].includes(event.key)
  )
    return;
  event.preventDefault();
  if (event.target.dataset.memoryTab) {
    let index = memoryTabs.indexOf(state.memoryTab);
    index =
      event.key === "Home"
        ? 0
        : event.key === "End"
          ? memoryTabs.length - 1
          : (index + (event.key === "ArrowRight" ? 1 : -1) + memoryTabs.length) %
            memoryTabs.length;
    setMemoryTab(memoryTabs[index], { focusTab: true });
    return;
  }
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
$("#close-drawer").addEventListener("click", () => closeDrawer());
$("#drawer-backdrop").addEventListener("click", () => closeDrawer());
document.addEventListener("keydown", (event) => {
  if (event.isComposing || event.keyCode === 229 || event.repeat) {
    if (
      (event.key === "Enter" || event.key === " ") &&
      event.target.closest("button,summary,#download-file,#nav-logs,#nav-memory,#show-other-tools")
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
    if (state.view === "memory") {
      if (state.memoryTab !== "entries") setMemoryTab("entries");
      $("#memory-search").focus();
      $("#memory-search").select();
      return;
    }
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
    event.code === "KeyL"
  ) {
    event.preventDefault();
    if (!state.busy && !state.fileBusy && !state.memoryBusy && !state.memoryStatsBusy && !state.soulBusy)
      $("#nav-logs").click();
  }
  if (
    state.view === "memory" &&
    event.altKey &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.shiftKey &&
    (event.code === "Digit1" || event.code === "Digit2") &&
    !event.target.closest("input, textarea, select, [contenteditable]")
  ) {
    event.preventDefault();
    setMemoryTab(event.code === "Digit1" ? "entries" : "analytics");
  }
  if (
    event.altKey &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.shiftKey &&
    event.code === "KeyT" &&
    !event.target.closest("input:not(#show-other-tools), textarea, select, [contenteditable]")
  ) {
    const filter = $("#show-other-tools");
    if (filter) {
      event.preventDefault();
      filter.click();
    }
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
