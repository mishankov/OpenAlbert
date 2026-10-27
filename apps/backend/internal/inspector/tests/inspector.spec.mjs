import {
  test,
  expect,
} from "../../../../../docs/node_modules/@playwright/test/index.mjs";
const conv = { id: "personal:7", private: true, title: "Личный разговор" };
const session = {
  id: "test-session",
  seq: 1,
  conversation_id: conv.id,
  conversation: conv,
  created_at: "2026-10-20T13:38:00Z",
  status: "closed",
  trigger: "initiative",
  message_count: 4,
  tool_count: 1,
  trace_count: 2,
  analyzed: true,
  preview: "Проверить новые задачи",
};
const modelTrace = {
  id: "trace-model",
  kind: "model",
  name: "configured-model",
  session_id: session.id,
  turn_id: "turn",
  step: 0,
  status: "completed",
  attempt: 1,
  started_at: "2026-10-20T13:38:00Z",
  ended_at: "2026-10-20T13:38:01Z",
};
const toolTrace = {
  ...modelTrace,
  id: "trace-tool",
  kind: "tool",
  name: "task_find",
  call_id: "c1",
};
const detail = {
  session,
  messages: [
    {
      seq: 1,
      key: "incoming-1",
      message: {
        role: "user",
        content: "Что нового? <script>window.XSS=true</script>",
      },
    },
    {
      seq: 2,
      key: "turn/assistant/0",
      message: {
        role: "assistant",
        content: "Проверю задачи.",
        reasoning: "Сводка, возвращённая провайдером",
        tool_calls: [
          { id: "c1", name: "task_find", arguments: { query: "Лейкерс" } },
        ],
      },
    },
    {
      seq: 3,
      key: "turn/tool/0/c1",
      message: { role: "tool", tool_call_id: "c1", content: '{"tasks":[]}' },
    },
    {
      seq: 4,
      key: "turn/assistant/1",
      message: { role: "assistant", content: "Проверил. Молчу." },
    },
  ],
  traces: [modelTrace, toolTrace],
  turns: [
    {
      Input: { Key: "turn", StartedAt: modelTrace.started_at },
      Stats: {
        FinishReason: "no_reply",
        ModelCalls: 2,
        ToolCalls: 1,
        InputTokens: 100,
        OutputTokens: 20,
      },
      LastText: "Проверил. Молчу.",
      Sent: false,
    },
  ],
  outgoing: [],
  artifacts: [],
};
async function fixture(page, currentDetail = detail, sessions = [session]) {
  await page.route("**/api/inspect/**", async (route) => {
    const url = new URL(route.request().url());
    let body;
    if (url.pathname === "/api/inspect/sessions")
      body = {
        sessions: url.searchParams.get("q") === "missing" ? [] : sessions,
        next_before: 0,
      };
    else if (url.pathname.endsWith("/test-session"))
      body = {
        ...currentDetail,
        messages:
          Number(url.searchParams.get("after")) > 0
            ? []
            : currentDetail.messages,
      };
    else if (url.pathname.endsWith("/trace"))
      body =
        url.searchParams.get("id") === "trace-tool"
          ? {
              ...toolTrace,
              input: { query: "Лейкерс" },
              output: '{"tasks":[]}',
            }
          : {
              ...modelTrace,
              input: {
                messages: [
                  { role: "system", content: "Точные инструкции системы" },
                  { role: "user", content: "Что нового?" },
                ],
                tools: [
                  {
                    name: "task_find",
                    description: "Find tasks",
                    parameters: { type: "object" },
                  },
                ],
              },
              output: {
                Message: detail.messages[1].message,
                InputTokens: 100,
                OutputTokens: 20,
              },
            };
    else body = {};
    await route.fulfill({ json: body });
  });
  await page.goto("/inspect/");
}
async function openSession(page) {
  await page.locator(".session-link").first().focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#event-1")).toBeVisible();
}

test("ссылка из лога открывает нужный вызов, Escape закрывает и сохраняет навигацию", async ({ page }) => {
  await fixture(page);
  await page.goto("/inspect/#/session/test-session?trace=trace-tool");
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.locator("#drawer-title")).toHaveText("task_find");
  await expect(page.locator("#drawer-content")).toContainText("Лейкерс");
  await expect(page.locator("#close-drawer")).toBeFocused();
  await page.locator("body").dispatchEvent("keydown", { key: "Escape", isComposing: true, bubbles: true });
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(page).toHaveURL(/#\/session\/test-session$/);
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeEnabled();
  await expect(page.getByRole("dialog")).toBeHidden();
  await page.goto("/inspect/#/session/test-session?trace=trace-model");
  await expect(page.locator("#drawer-title")).toHaveText("configured-model");
  await page.reload();
  await expect(page.locator("#drawer-title")).toHaveText("configured-model");
  await page.keyboard.press("Escape");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: "/tmp/openalbert-logging-inspector-mobile.png" });
  await page.locator("a.back-link").click();
  await expect(page.locator(".session-link")).toBeVisible();
});

test("отсутствующий вызов по ссылке показывает ошибку и оставляет сессию доступной", async ({ page }) => {
  await fixture(page);
  await page.route("**/api/inspect/trace?**", route => route.fulfill({ status: 404, json: {} }));
  await page.goto("/inspect/#/session/test-session?trace=missing");
  await expect(page.getByRole("dialog").getByRole("alert")).toContainText("не найдены");
  await page.keyboard.press("Escape");
  await expect(page.locator("#event-1")).toBeVisible();
});

test("логи: кнопка и Alt L открывают одинаковый фильтр и сохраняют поиск", async ({ page, context }) => {
  await context.route("http://127.0.0.1:9428/**", route => route.fulfill({ body: "VMUI fixture", contentType: "text/html" }));
  await fixture(page);
  await page.locator("#search").fill("Лейкерс");
  await expect(page.locator("#nav-logs")).toHaveAttribute("aria-keyshortcuts", "Alt+L");
  const clicked = page.waitForEvent("popup");
  await page.locator("#nav-logs").click();
  const buttonPage = await clicked;
  await buttonPage.waitForLoadState();
  const target = buttonPage.url();
  const query = new URLSearchParams(new URL(target).hash.slice(3)).get("query");
  expect(query).toContain("NOT service:temporal-ui");
  expect(query).toContain("http_status:>=400");
  expect(query).toContain("duration_ms:>=1000");
  await buttonPage.close();
  for (const extra of [{ repeat: true }, { isComposing: true }]) {
    await page.locator("body").dispatchEvent("keydown", { code: "KeyL", key: "д", altKey: true, bubbles: true, ...extra });
  }
  expect(context.pages()).toHaveLength(1);
  const keyboard = page.waitForEvent("popup");
  await page.locator("body").dispatchEvent("keydown", { code: "KeyL", key: "д", altKey: true, bubbles: true });
  const keyboardPage = await keyboard;
  await keyboardPage.waitForLoadState();
  expect(keyboardPage.url()).toBe(target);
  await keyboardPage.close();
  await expect(page.locator("#search")).toHaveValue("Лейкерс");
  await openSession(page);
  await page.getByRole("button", { name: "Контекст", exact: true }).first().click();
  await page.keyboard.press("Alt+l");
  expect(context.pages()).toHaveLength(1);
  await page.keyboard.press("Escape");
});

test("логи: загрузка одинаково блокирует кнопку и сочетание", async ({ page, context }) => {
  await fixture(page);
  let finish;
  const pending = new Promise(resolve => { finish = resolve; });
  await page.route("**/api/inspect/sessions?**", async route => {
    await pending;
    await route.fulfill({ json: { sessions: [session], next_before: 0 } });
  });
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#nav-logs")).toHaveAttribute("aria-disabled", "true");
  await page.locator("#nav-logs").dispatchEvent("click");
  await page.keyboard.press("Alt+l");
  expect(context.pages()).toHaveLength(1);
  finish();
  await expect(page.locator("#nav-logs")).toHaveAttribute("aria-disabled", "false");
});

const speechResult = {
  session_id: session.id, conversation_id: conv.id, incoming_id: "voice", artifact_id: "voice-file",
  model: "nemotron-3.5", author_id: 42, author_name: "Анна", status: "completed",
  text: "Купить три яблока <script>window.XSS=true</script>", original_text: "Подпись [Voice transcript]",
  received_at: modelTrace.started_at, queued_at: modelTrace.started_at, started_at: modelTrace.started_at,
  ended_at: modelTrace.ended_at, duration_seconds: 2, decode_ms: 12, inference_ms: 200, cached: false,
};
const speechTrace = { ...modelTrace, id: "speech-trace", kind: "transcription", name: "nemotron-3.5", call_id: "voice-file" };
function voiceDetail(result = {}) {
  return { ...detail,
    audio: [{ ...speechResult, ...result }],
    messages: [{ ...detail.messages[0], recorded_at: modelTrace.ended_at,
      message: { role: "user", author_name: "Анна", author_id: 42, source_message_id: "voice", content: "Подпись [Voice transcript]\n[Attachment: voice.ogg; type=audio/ogg; artifact_id=voice-file]\n[Voice transcript; automatically recognized from audio]\n" + speechResult.text } }, ...detail.messages.slice(1)],
    traces: [...detail.traces, speechTrace],
    artifacts: [{ id: "voice-file", conversation_id: conv.id, name: "voice.ogg", mime: "audio/ogg" }],
  };
}
async function playableAudio(page) {
  const wav = Buffer.alloc(44 + 32000 * 10);
  wav.write("RIFF");wav.writeUInt32LE(wav.length - 8, 4);wav.write("WAVEfmt ", 8);wav.writeUInt32LE(16, 16);wav.writeUInt16LE(1, 20);wav.writeUInt16LE(1, 22);wav.writeUInt32LE(16000, 24);wav.writeUInt32LE(32000, 28);wav.writeUInt16LE(2, 32);wav.writeUInt16LE(16, 34);wav.write("data", 36);wav.writeUInt32LE(wav.length - 44, 40);
  await page.route("**/api/inspect/artifact?**", (route) => route.fulfill({ contentType: "audio/wav", body: wav }));
}

test("хронология: голосовое сообщение и отдельная операция распознавания", async ({ page }) => {
  await fixture(page, voiceDetail());await playableAudio(page);
  await page.route("**/api/inspect/trace?**", (route) => route.fulfill({ json: { ...speechTrace, input: { artifact_id: "voice-file" }, output: speechResult } }));
  await openSession(page);
  const voice = page.locator(".voice-message"), speech = page.locator(".speech-event");
  await expect(voice).toHaveCount(1);await expect(speech).toHaveCount(1);
  const order = await page.locator(".timeline > *").evaluateAll((nodes) => nodes.map((n) => n.className));
  expect(order[0]).toContain("voice-message");expect(order[1]).toContain("speech-event");
  await expect(voice).toContainText("Голосовое сообщение");await expect(voice).toContainText("Анна");
  await expect(voice).toContainText("Подпись [Voice transcript]");await expect(voice).toContainText(speechResult.text);
  await expect(voice).not.toContainText("artifact_id=");await expect(voice).not.toContainText("automatically recognized");
  await expect(voice.locator("time")).toHaveAttribute("title", /Получено в Telegram/);
  const player = voice.locator("audio");await player.focus();await page.keyboard.press("Space");
  await expect.poll(() => player.evaluate((a) => a.paused)).toBe(false);await page.keyboard.press("Space");
  await expect.poll(() => player.evaluate((a) => a.paused)).toBe(true);
  const summary = speech.locator(":scope > summary");await summary.focus();await page.keyboard.press("Enter");
  await expect(speech).toHaveAttribute("open", "");
  await summary.dispatchEvent("keydown", { key: "Enter", code: "Enter", repeat: true });
  await summary.dispatchEvent("keydown", { key: "Enter", code: "Enter", isComposing: true });
  await expect(speech).toHaveAttribute("open", "");
  const data = speech.getByRole("button", { name: "Данные распознавания", exact: true });
  await data.focus();await page.keyboard.press("Enter");await expect(page.getByRole("dialog")).toContainText("original_text");
  await page.keyboard.press("Escape");await expect(data).toBeFocused();
  const attempt = speech.getByRole("button", { name: /Попытка 1/ });await attempt.focus();await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("Купить три яблока");await page.keyboard.press("Escape");await expect(attempt).toBeFocused();
  expect(await page.evaluate(() => window.XSS)).toBeUndefined();
  await page.screenshot({ path: test.info().outputPath("voice-timeline-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(speech.locator(":scope > summary .tool-state")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("voice-timeline-mobile.png"), fullPage: true });
});

test("хронология: ожидание сменяется результатом без дублирования голосового", async ({ page }) => {
  const current = voiceDetail({ status: "running", text: "", ended_at: undefined });
  current.messages = [];current.traces = [{ ...speechTrace, status: "running", ended_at: undefined }];
  await fixture(page, current);await page.locator(".session-link").first().click();
  await expect(page.locator(".voice-message")).toHaveCount(1);await expect(page.locator(".speech-event")).toHaveCount(1);
  await expect(page.locator(".voice-message")).toContainText("Распознавание ещё не завершено");
  await expect(page.locator(".activity-line")).toHaveCount(0);
  const summary = page.locator(".speech-event > summary");await summary.focus();await page.keyboard.press("Enter");
  Object.assign(current, voiceDetail());await page.keyboard.press("Alt+r");
  await expect(page.locator("#event-1.voice-message")).toBeVisible();await expect(page.locator(".voice-message")).toHaveCount(1);
  await expect(page.locator(".speech-event")).toHaveCount(1);await expect(page.locator(".speech-event")).toHaveAttribute("open", "");
  await expect(page.locator(".speech-event > summary")).toBeFocused();await expect(page.locator(".voice-message")).toContainText(speechResult.text);
});

test("хронология: кэш и исходы без текста показаны явно", async ({ page }) => {
  for (const [status, notice] of [["queued", "Ожидает распознавания"], ["failed", "Не удалось получить расшифровку"], ["cancelled", "Распознавание отменено"], ["no_speech", "Речь не распознана"], ["unavailable", "Распознавание недоступно"]]) {
    await test.step(status, async () => {
      const d = voiceDetail({ status, text: "", error_code: status === "failed" ? "stt_http_422" : undefined });
      d.traces = [];await fixture(page, d);await openSession(page);
      await expect(page.locator(".voice-message")).toContainText(notice);
      await page.locator(".speech-event > summary").click();await expect(page.locator(".speech-event")).toContainText(notice);
      if (status === "failed") await expect(page.locator(".speech-event")).toContainText("stt_http_422");
      await expect(page.locator(".speech-event [data-trace]")).toHaveCount(0);
    });
  }
  const cached = voiceDetail({ cached: true });cached.traces = [];await fixture(page, cached);await openSession(page);
  await expect(page.locator(".speech-event > summary")).toContainText("Из кэша");await expect(page.locator(".speech-event [data-trace]")).toHaveCount(0);
});

test("хронология: незавершённая попытка без результата вложения остаётся видна", async ({ page }) => {
  await fixture(page, { ...detail, traces: [{ ...speechTrace, status: "running", ended_at: undefined }] });await openSession(page);
  await expect(page.locator(".voice-message")).toHaveCount(0);await expect(page.locator(".speech-event")).toHaveCount(1);
  await page.locator(".speech-event > summary").click();await expect(page.locator(".speech-event")).toContainText("Результат вложения не сохранён");
  await expect(page.locator(".speech-event [data-trace]")).toHaveCount(1);await expect(page.locator(".speech-event [data-record]")).toHaveCount(0);
});

test("аудио: исходник, текст, диагностика и клавиатурная навигация", async ({ page }) => {
  const speech = { session_id: session.id, conversation_id: conv.id, incoming_id: "voice", artifact_id: "voice-file", model: "nemotron-3.5", author_id: 42, author_name: "Анна", status: "completed", text: "Три яблока <script>window.XSS=true</script>", queued_at: modelTrace.started_at, started_at: modelTrace.started_at, ended_at: modelTrace.ended_at, duration_seconds: 2, decode_ms: 12, inference_ms: 200, cached: false };
  const tr = { ...modelTrace, id: "speech-trace", kind: "transcription", name: "nemotron-3.5", call_id: "voice-file" };
  await fixture(page, { ...detail, audio: [speech], traces: [...detail.traces, tr], artifacts: [{ id: "voice-file", conversation_id: conv.id, name: "voice.ogg", mime: "audio/ogg" }] });
  const wav = Buffer.alloc(44 + 32000 * 10);
  wav.write("RIFF");wav.writeUInt32LE(wav.length - 8, 4);wav.write("WAVEfmt ", 8);wav.writeUInt32LE(16, 16);wav.writeUInt16LE(1, 20);wav.writeUInt16LE(1, 22);wav.writeUInt32LE(16000, 24);wav.writeUInt32LE(32000, 28);wav.writeUInt16LE(2, 32);wav.writeUInt16LE(16, 34);wav.write("data", 36);wav.writeUInt32LE(wav.length - 44, 40);
  await page.route("**/api/inspect/artifact?**", (route) => route.fulfill({ contentType: "audio/wav", body: wav }));
  await page.route("**/api/inspect/trace?**", (route) => route.fulfill({ json: { ...tr, input: { artifact_id: "voice-file" }, output: speech } }));
  await openSession(page);
  await page.locator("#tab-timeline").focus();
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await expect(page.locator("#tab-audio")).toBeFocused();
  await expect(page.locator("#panel")).toContainText("Анна");
  await expect(page.locator("#panel")).toContainText(speech.text);
  await expect(page.locator("audio")).toHaveAttribute("preload", "none");
  await expect(page.locator("audio")).toHaveAttribute("src", /artifact.*voice-file/);
  // Native media controls need their first stable layout before keyboard input.
  // focus() alone does not wait for actionability or the UA controls to render.
  await page.locator("audio").scrollIntoViewIfNeeded();
  await page.locator("audio").focus();
  await expect(page.locator("audio")).toBeFocused();
  await page.keyboard.press("Space");
  await expect.poll(() => page.locator("audio").evaluate((a) => a.paused)).toBe(false);
  await page.keyboard.press("Space");await expect.poll(() => page.locator("audio").evaluate((a) => a.paused)).toBe(true);
  const attempt = page.getByRole("button", { name: "Попытка 1", exact: true });
  await attempt.focus();await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("Три яблока");
  await page.keyboard.press("Escape");await expect(attempt).toBeFocused();
  expect(await page.evaluate(() => window.XSS)).toBeUndefined();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("audio.png"), fullPage: true });
  await page.locator("#tab-audio").focus();await page.keyboard.press("Home");await expect(page.locator("#tab-timeline")).toBeFocused();
});

test("аудио из кэша не изображает новый вызов STT", async ({ page }) => {
  await fixture(page, { ...detail, audio: [{ incoming_id: "voice", artifact_id: "missing", model: "nemotron", status: "completed", text: "Сохранённый текст", cached: true, queued_at: modelTrace.started_at, started_at: modelTrace.started_at, ended_at: modelTrace.ended_at }] });
  await openSession(page);await page.getByRole("tab", { name: /Аудио/ }).click();
  await expect(page.locator("#panel")).toContainText("Из кэша");
  await expect(page.locator("#panel")).toContainText("Исходный файл недоступен");
  await expect(page.locator("#panel [data-trace]")).toHaveCount(0);
});

test("история, аргументы и результат инструмента, контекст, финальный текст и отсутствие доставки", async ({
  page,
}) => {
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await fixture(page);
  await openSession(page);
  await page.locator(".tool-call summary").click();
  await expect(page.locator(".tool-call")).toContainText("Лейкерс");
  await expect(page.locator(".tool-call")).toContainText("tasks");
  await page
    .getByRole("button", { name: "Контекст", exact: true })
    .first()
    .focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(
    page
      .getByRole("dialog")
      .getByText("Сводка, возвращённая провайдером", { exact: true }),
  ).toBeVisible();
  await page.getByText("1. system", { exact: true }).click();
  await expect(
    page
      .getByRole("dialog")
      .locator(".context-body")
      .getByText("Точные инструкции системы", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(
    page.getByRole("button", { name: "Контекст", exact: true }).first(),
  ).toBeFocused();
  await page.getByRole("tab", { name: /Доставка/ }).click();
  await expect(page.getByText("Сообщения не отправлялись")).toBeVisible();
  await page.getByRole("tab", { name: /Запуски/ }).click();
  await page.locator(".run-row > summary").click();
  await expect(
    page.getByRole("heading", { name: "Финальный текст модели" }),
  ).toBeVisible();
  await expect(page.locator("#panel")).toContainText("Проверил. Молчу.");
  expect(await page.evaluate(() => window.XSS)).toBeUndefined();
  expect(errors).toEqual([]);
});

test("клавиатура: поиск с русской раскладкой, повтор, IME, диалог и обновление", async ({
  page,
}) => {
  await fixture(page);
  await openSession(page);
  await page.locator("body").dispatchEvent("keydown", {
    code: "KeyK",
    key: "л",
    metaKey: true,
    isComposing: true,
    bubbles: true,
  });
  await expect(page.locator("#search")).not.toBeFocused();
  await page.locator("body").dispatchEvent("keydown", {
    code: "KeyK",
    key: "л",
    metaKey: true,
    repeat: true,
    bubbles: true,
  });
  await expect(page.locator("#search")).not.toBeFocused();
  await page.locator("body").dispatchEvent("keydown", {
    code: "KeyK",
    key: "л",
    metaKey: true,
    bubbles: true,
  });
  await expect(page.locator("#search")).toBeFocused();
  await page.locator("#search").fill("Лейкерс");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeEnabled();
  await expect(page.locator("#search")).toHaveValue("Лейкерс");
  await page.getByRole("tab", { name: /Хронология/ }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: /Модель/ })).toBeFocused();
  await page.getByRole("button", { name: "Контекст", exact: true }).click();
  await expect(page.locator("#close-drawer")).toBeFocused();
  await page.keyboard.press("Meta+k");
  await expect(page.locator("#search")).not.toBeFocused();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "Контекст", exact: true }),
  ).toBeFocused();
});

test("узкая панель и пустое состояние", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await fixture(page);
  await expect(page.locator(".sidebar")).toBeVisible();
  await openSession(page);
  await expect(page.locator(".sidebar")).toBeHidden();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("tab", { name: /Модель/ }).click();
  await page.getByRole("button", { name: "Контекст", exact: true }).click();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.keyboard.press("Escape");
  await page.keyboard.press("Meta+k");
  await expect(page.locator("#search")).toBeFocused();
  await page.locator("#search").fill("missing");
  await expect(page.getByText("Сессий с такими условиями нет.")).toBeVisible();
});

test("ошибка загрузки и восстановление по Alt R", async ({ page }) => {
  await fixture(page);
  await page.route("**/api/inspect/sessions?*", (route) =>
    route.fulfill({ status: 500, json: { error: "Internal Server Error" } }),
  );
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#list-error")).toBeVisible();
  await page.unroute("**/api/inspect/sessions?*");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#list-error")).toBeHidden();
  await expect(page.locator(".session-link")).toHaveCount(1);
});

test("плотность: история начинается сразу, на экране видны десятки строк без вложенных карточек", async ({
  page,
}) => {
  const sessions = Array.from({ length: 30 }, (_, i) => ({
    ...session,
    id: i ? `session-${i}` : session.id,
    seq: 30 - i,
    preview: `Сообщение ${i + 1}: проверить состояние фоновой задачи`,
  }));
  const messages = Array.from({ length: 40 }, (_, i) => ({
    seq: i + 1,
    key: `message-${i}`,
    recorded_at: "2026-10-20T13:38:00Z",
    message: {
      role: i % 2 ? "assistant" : "user",
      content: `Событие ${i + 1}: короткое сообщение с результатом проверки.`,
    },
  }));
  await page.setViewportSize({ width: 1280, height: 800 });
  await fixture(
    page,
    {
      ...detail,
      session: { ...session, message_count: 40 },
      messages,
      traces: [],
    },
    sessions,
  );
  await openSession(page);
  const dimensions = await page.evaluate(() => ({
    firstEvent: document.querySelector(".event").getBoundingClientRect().top,
    events: [...document.querySelectorAll(".event")].filter(
      (e) => e.getBoundingClientRect().bottom <= innerHeight,
    ).length,
    sessions: [...document.querySelectorAll(".session-link")].filter(
      (e) =>
        e.getBoundingClientRect().top >=
          document.querySelector("#sessions").getBoundingClientRect().top &&
        e.getBoundingClientRect().bottom <= innerHeight,
    ).length,
    overflow: document.documentElement.scrollWidth > innerWidth,
  }));
  expect(dimensions.firstEvent).toBeLessThan(170);
  expect(dimensions.events).toBeGreaterThanOrEqual(12);
  expect(dimensions.sessions).toBeGreaterThanOrEqual(8);
  expect(dimensions.overflow).toBe(false);
});

test("вызов и результат тула объединены, исходные записи открываются отдельно", async ({
  page,
}) => {
  await fixture(page);
  await openSession(page);
  await expect(page.locator("#event-3")).toHaveCount(0);
  await page.locator(".tool-call > summary").click();
  await page
    .getByRole("button", { name: "Запись результата", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toContainText("tool_call_id");
  await expect(page.getByRole("dialog")).toContainText("tasks");
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "Запись результата", exact: true }),
  ).toBeFocused();
  await expect(page.locator(".tool-call")).toHaveAttribute("open", "");
});

test("удержание Enter не закрывает только что открытые подробности", async ({
  page,
}) => {
  await fixture(page);
  await openSession(page);
  await page
    .getByRole("button", { name: "Контекст", exact: true })
    .first()
    .focus();
  await page.keyboard.down("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.locator("#close-drawer")).toBeFocused();
  await page.keyboard.down("Enter");
  await page.keyboard.up("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "Контекст", exact: true }).first(),
  ).toBeFocused();
});

test("обновление сохраняет раскрытый тул и фокус клавиатуры", async ({
  page,
}) => {
  await fixture(page);
  await openSession(page);
  const summary = page.locator(".tool-call > summary");
  await summary.focus();
  await page.keyboard.press("Enter");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeEnabled();
  await expect(page.locator(".tool-call")).toHaveAttribute("open", "");
  await expect(summary).toBeFocused();
});
