import { test, expect } from "../../../../../docs/node_modules/@playwright/test/index.mjs";

const entries = [
  { id: "pref <script>", scope: "personal:7", content: "Предпочитает короткие ответы <script>window.XSS=true</script>", kind: "preference", source: "session-7", updated_at: "2026-10-23T12:00:00Z" },
  { id: "place", scope: "personal:7", content: "Живёт в Москве", kind: "profile", source: "session-7", updated_at: "2026-10-22T12:00:00Z" },
  { id: "project", scope: "personal:7", content: "Разрабатывает OpenAlbert", kind: "fact", source: "session-7", updated_at: "2026-10-21T12:00:00Z" },
  { id: "participant", scope: "group:8", content: "Любит пончики", kind: "preference", subject_user_id: 42, source: "group-session", source_message_id: "message-42", evidence: "Я люблю пончики <img src=x onerror=alert(1)>", updated_at: "2026-10-23T12:00:00Z" },
];
async function fixture(page, { empty = false } = {}) {
  const requests = [];
  await page.route("**/api/inspect/**", (route) => {
    const u = new URL(route.request().url());
    requests.push(u);
    if (u.pathname.endsWith("/soul")) return route.fulfill({ json: { soul: "# OpenAlbert\n\nТы — OpenAlbert. Отвечай по-русски. <script>window.SOUL_XSS=true</script>" } });
    if (u.pathname.endsWith("/group")) return route.fulfill({ json: { group_id: u.searchParams.get("conversation_id") === "group:empty" ? -9 : -8, group: u.searchParams.get("conversation_id") === "group:empty" ? "" : "# Правила группы\nОтвечай по обращению. <script>window.GROUP_XSS=true</script>" } });
    if (u.pathname.endsWith("/consolidations")) return route.fulfill({ json: { consolidations: [] } });
    if (u.pathname.endsWith("/sessions")) return route.fulfill({ json: { sessions: [], next_before: 0 } });
    if (u.pathname.endsWith("/conversations")) return route.fulfill({ json: { conversations: empty ? [] : [{ id: "personal:7", private: true, chat_id: 7, title: "Личный разговор" }, { id: "group:8", chat_id: -8, title: "Команда OpenAlbert" }, { id: "group:8:topic:9", chat_id: -8, thread_id: 9, title: "Топик команды" }, { id: "group:empty", chat_id: -9, title: "Пустой разговор" }] } });
    if (u.pathname.includes("/sessions/")) return route.fulfill({ status: 404, json: {} });
    const scope = u.searchParams.get("conversation_id");
    if (u.pathname.endsWith("/entry")) {
      const e = entries.find((e) => e.id === u.searchParams.get("id") && e.scope === scope);
      return route.fulfill(e ? { json: e } : { status: 404, json: {} });
    }
    const q = u.searchParams.get("q").toLowerCase().split(/\s+/).filter(Boolean);
    const kind = u.searchParams.get("kind");
    const found = entries.filter((e) => e.scope === scope && (!kind || e.kind === kind) && q.every((term) => JSON.stringify(e).toLowerCase().includes(term)));
    const after = Number(u.searchParams.get("after"));
    return route.fulfill({ json: { entries: found.slice(after, after + 2), total: found.length, next_after: after + 2 < found.length ? after + 2 : 0, profile: scope === "personal:7" ? "# User\n\nЖивёт в Москве\nПредпочитает короткие ответы" : "" } });
  });
  await page.goto("/inspect/#/memory");
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");
  return requests;
}

test("просмотр памяти: профиль, страницы, происхождение и клавиатура", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const requests = await fixture(page);
  await expect(page.locator("#nav-memory")).toHaveAttribute("aria-current", "page");
  await expect(page.locator(".memory-row")).toHaveCount(2);
  await expect(page.locator("#memory-count")).toHaveText("Записи: 2 из 3");
  await page.locator("#memory-profile summary").focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#memory-profile pre")).toContainText("Живёт в Москве");
  await page.locator(".memory-row").first().focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText(entries[0].content);
  await expect(page.locator("#close-drawer")).toBeFocused();
  expect(await page.evaluate(() => window.XSS)).toBeUndefined();
  const count = requests.length;
  await page.keyboard.press("Alt+r");
  await page.keyboard.press("Control+k");
  expect(requests.length).toBe(count);
  await expect(page.locator("#memory-search")).not.toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(page.getByRole("dialog").locator("summary")).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.locator("#close-drawer")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.locator(".memory-row").first()).toBeFocused();
  await page.locator("#more-memory").focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".memory-row")).toHaveCount(3);
  await expect(page.locator(".memory-row").last()).toBeFocused();
  await page.locator("#memory-scope").selectOption("group:8");
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await expect(page.locator("#memory-profile")).toBeHidden();
  await page.locator(".memory-row").focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("42");
  await expect(page.getByRole("dialog")).toContainText("message-42");
  await expect(page.getByRole("dialog")).toContainText(entries[3].evidence);
  await expect(page.getByRole("dialog").locator("img")).toHaveCount(0);
  await page.getByRole("dialog").getByRole("link", { name: "group-session", exact: true }).click();
  await expect(page).toHaveURL(/#\/session\/group-session$/);
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(errors).toEqual([]);
});

test("поиск и тип сохраняются, обновление и сочетания работают в русской раскладке", async ({ page }) => {
  const requests = await fixture(page);
  for (const extra of [{ isComposing: true }, { repeat: true }, { keyCode: 229 }]) {
    await page.locator("body").dispatchEvent("keydown", { key: "л", code: "KeyK", ctrlKey: true, bubbles: true, ...extra });
    await expect(page.locator("#memory-search")).not.toBeFocused();
  }
  await page.locator("body").dispatchEvent("keydown", { key: "л", code: "KeyK", ctrlKey: true, bubbles: true });
  await expect(page.locator("#memory-search")).toBeFocused();
  await page.locator("#memory-search").fill("КОРОТКИЕ ответы");
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await page.locator("#memory-kind").selectOption("preference");
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");
  const before = requests.length;
  for (const extra of [{ isComposing: true }, { repeat: true }]) {
    await page.locator("body").dispatchEvent("keydown", { key: "к", code: "KeyR", altKey: true, bubbles: true, ...extra });
  }
  expect(requests.length).toBe(before);
  await page.locator("body").dispatchEvent("keydown", { key: "к", code: "KeyR", altKey: true, bubbles: true });
  await expect.poll(() => requests.length).toBeGreaterThan(before);
  await expect(page.locator("#refresh")).toBeEnabled();
  await expect(page.locator("#memory-search")).toHaveValue("КОРОТКИЕ ответы");
  await page.locator("#nav-sessions").click();
  await page.locator("#nav-memory").focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#memory-search")).toHaveValue("КОРОТКИЕ ответы");
  await expect(page.locator("#memory-kind")).toHaveValue("preference");
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await page.locator("#memory-kind").selectOption("fact");
  await expect(page.locator("#memory-list")).toContainText("Записей с такими условиями нет");
  await page.locator("#memory-scope").selectOption("group:empty");
  await page.locator("#memory-search").fill("");
  await page.locator("#memory-kind").selectOption("");
  await expect(page.locator("#memory-list")).toContainText("В этом разговоре пока нет записей");
  await page.reload();
  await expect(page.locator("#memory-scope")).toHaveValue("group:empty");
});

test("ошибки, повтор загрузки, занятость и запоздавшие ответы", async ({ page }) => {
  await fixture(page);
  let release, started = 0;
  const pending = new Promise((resolve) => { release = resolve; });
  await page.route("**/api/inspect/memory?*", async (route) => {
    started++;
    await pending;
    await route.fulfill({ status: 500, json: {} });
  });
  await page.keyboard.press("Alt+r");
  await expect.poll(() => started).toBe(1);
  await expect(page.locator("#refresh")).toBeDisabled();
  await page.keyboard.press("Alt+r");
  expect(started).toBe(1);
  release();
  await expect(page.locator("#memory-error")).toBeVisible();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/memory?*");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#memory-error")).toBeHidden();
  let releaseOld, oldStarted = false;
  const old = new Promise((resolve) => { releaseOld = resolve; });
  await page.route("**/api/inspect/memory?*", async (route) => {
    const u = new URL(route.request().url());
    if (u.searchParams.get("q") === "old") {
      oldStarted = true;
      await old;
      return route.fulfill({ json: { entries: [entries[0]], total: 1, next_after: 0, profile: "old" } });
    }
    return route.fulfill({ json: { entries: [], total: 0, next_after: 0, profile: "" } });
  });
  await page.locator("#memory-search").fill("old");
  await expect.poll(() => oldStarted).toBe(true);
  await page.locator("#memory-search").fill("new");
  await expect(page.locator("#memory-list")).toContainText("Записей с такими условиями нет");
  const response = page.waitForResponse((r) => new URL(r.url()).searchParams.get("q") === "old");
  releaseOld();
  await response;
  await expect(page.locator(".memory-row")).toHaveCount(0);
  await expect(page.locator("#memory-search")).toHaveValue("new");
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/memory?*");
  await page.locator("#memory-search").fill("");
  await expect(page.locator(".memory-row")).toHaveCount(2);
  await page.route("**/api/inspect/memory/entry?*", (route) => route.fulfill({ status: 404, json: {} }));
  await page.locator(".memory-row").first().click();
  await expect(page.getByRole("dialog")).toContainText("не найдены");
  await page.keyboard.press("Escape");
  await expect(page.locator(".memory-row").first()).toBeFocused();
});

test("пустая установка и узкий экран", async ({ page }, testInfo) => {
  await fixture(page, { empty: true });
  await expect(page.locator("#memory-list")).toContainText("Разговоров пока нет");
  await expect(page.locator("#memory-scope")).toBeDisabled();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/**");
  await page.goto("about:blank");
  await fixture(page);
  await page.screenshot({ path: testInfo.outputPath("memory-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator(".sidebar")).toBeHidden();
  await expect(page.locator(".memory-row").first()).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("memory-mobile.png") });
  await page.locator(".memory-row").first().focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".memory-row").first()).toBeFocused();
});

test("закрытие подробностей отбрасывает поздний ответ и возвращает фокус после обновления списка", async ({ page }) => {
  await fixture(page);
  let releaseList, listStarted = false;
  const listPending = new Promise((resolve) => { releaseList = resolve; });
  await page.route("**/api/inspect/memory?*", async (route) => {
    listStarted = true;
    await listPending;
    await route.fulfill({ json: { entries: entries.slice(0, 2), total: 2, next_after: 0, profile: "" } });
  });
  await page.keyboard.press("Alt+r");
  await expect.poll(() => listStarted).toBe(true);
  await page.locator(".memory-row").first().click();
  await expect(page.getByRole("dialog")).toContainText(entries[0].content);
  releaseList();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.keyboard.press("Escape");
  await expect(page.locator(".memory-row").first()).toBeFocused();
  let releaseEntry;
  const entryPending = new Promise((resolve) => { releaseEntry = resolve; });
  await page.route("**/api/inspect/memory/entry?*", async (route) => {
    await entryPending;
    await route.fulfill({ json: { ...entries[0], content: "late" } });
  });
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  const late = page.waitForResponse((r) => new URL(r.url()).pathname.endsWith("/entry"));
  releaseEntry();
  await late;
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(page.locator(".memory-row").first()).toBeFocused();
});

test("SOUL.md: полный текст, клавиатура, обновление и общий файл для всех разговоров", async ({ page }, testInfo) => {
  await fixture(page);
  const summary = page.locator("#memory-soul summary");
  await summary.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#memory-soul")).toHaveAttribute("open", "");
  await expect(page.locator("#memory-soul pre")).toContainText("Ты — OpenAlbert. Отвечай по-русски.");
  await expect(page.locator("#memory-soul pre")).toContainText("<script>window.SOUL_XSS=true</script>");
  expect(await page.evaluate(() => window.SOUL_XSS)).toBeUndefined();
  for (const extra of [{ repeat: true }, { isComposing: true }, { keyCode: 229 }]) {
    expect(await summary.evaluate((el, extra) => {
      const event = new KeyboardEvent("keydown", { key: "Enter", code: "Enter", bubbles: true, cancelable: true, ...extra });
      el.dispatchEvent(event);
      return event.defaultPrevented;
    }, extra)).toBe(true);
  }
  await page.locator("#memory-scope").selectOption("group:8");
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await expect(page.locator("#memory-soul pre")).toContainText("Ты — OpenAlbert.");
  await expect(page.locator("#memory-soul")).toHaveAttribute("open", "");
  await page.screenshot({ path: testInfo.outputPath("soul-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("soul-mobile.png") });
  await page.route("**/api/inspect/memory/soul", (route) => route.fulfill({ json: { soul: "# Атлас\n\nОбновлённый характер" } }));
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#memory-soul pre")).toHaveText("# Атлас\n\nОбновлённый характер");
  await expect(page.locator("#memory-soul")).toHaveAttribute("open", "");
  await summary.focus();
  await page.keyboard.press("Space");
  await expect(page.locator("#memory-soul")).not.toHaveAttribute("open", "");
});

test("SOUL.md доступен без разговоров; ошибка чтения не мешает памяти и повторяется по Alt R", async ({ page }) => {
  await fixture(page, { empty: true });
  await page.locator("#memory-soul summary").click();
  await expect(page.locator("#memory-soul pre")).toContainText("Ты — OpenAlbert.");
  await page.route("**/api/inspect/memory/soul", (route) => route.fulfill({ status: 500, json: {} }));
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#soul-error")).toContainText("Не удалось загрузить SOUL.md");
  await expect(page.locator("#memory-list")).toContainText("Разговоров пока нет");
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/memory/soul");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#soul-error")).toBeHidden();
  await expect(page.locator("#memory-soul pre")).toContainText("Ты — OpenAlbert.");
});

test("GROUP.md: общий файл топиков, клавиатура, актуальный текст и пустое состояние", async ({ page }, testInfo) => {
  const requests = await fixture(page);
  await expect(page.locator("#memory-group")).toBeHidden();
  expect(requests.filter((u) => u.pathname.endsWith("/group"))).toHaveLength(0);
  await page.locator("#memory-scope").selectOption("group:8");
  await expect(page.locator("#memory-group")).toBeVisible();
  const summary = page.locator("#memory-group summary");
  await expect(summary).toHaveAttribute("aria-keyshortcuts", "Enter Space");
  await summary.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#memory-group")).toHaveAttribute("open", "");
  await expect(page.locator("#memory-group pre")).toContainText("Отвечай по обращению.");
  await expect(page.locator("#memory-group pre")).toContainText("<script>window.GROUP_XSS=true</script>");
  expect(await page.evaluate(() => window.GROUP_XSS)).toBeUndefined();
  for (const key of ["Enter", " "]) {
    for (const extra of [{ repeat: true }, { isComposing: true }, { keyCode: 229 }]) {
      expect(await summary.evaluate((el, { key, extra }) => {
        const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...extra });
        el.dispatchEvent(event);
        return event.defaultPrevented;
      }, { key, extra })).toBe(true);
    }
  }
  await page.keyboard.press("Space");
  await expect(page.locator("#memory-group")).not.toHaveAttribute("open", "");
  await page.keyboard.press("Enter");
  await page.locator("#memory-scope").selectOption("group:8:topic:9");
  await expect(page.locator("#memory-group pre")).toContainText("Отвечай по обращению.");
  await expect(page.locator("#memory-list")).toContainText("В этом разговоре пока нет записей");
  await expect(page.locator("#memory-group")).toHaveAttribute("open", "");
  const updated = "# Новые правила\nОтвечай кратко.\n" + "длинноеслово".repeat(60);
  await page.route("**/api/inspect/memory/group?*", (route) => route.fulfill({ json: { group_id: -8, group: updated } }));
  await summary.focus();
  await expect(page.locator("#refresh")).toBeEnabled();
  for (const extra of [{ repeat: true }, { isComposing: true }, { keyCode: 229 }]) {
    const count = requests.length;
    await summary.dispatchEvent("keydown", { key: "к", code: "KeyR", altKey: true, bubbles: true, ...extra });
    expect(requests.length).toBe(count);
  }
  await summary.dispatchEvent("keydown", { key: "к", code: "KeyR", altKey: true, bubbles: true });
  await expect(page.locator("#memory-group pre")).toHaveText(updated);
  await expect(summary).toBeFocused();
  await expect(page.locator("#memory-group")).toHaveAttribute("open", "");
  await page.screenshot({ path: testInfo.outputPath("group-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("group-mobile.png") });
  await page.unroute("**/api/inspect/memory/group?*");
  await page.locator("#memory-scope").selectOption("group:empty");
  await expect(page.locator("#memory-group pre")).toContainText("GROUP.md не создан или пуст");
  await page.locator("#memory-scope").selectOption("personal:7");
  await expect(page.locator("#memory-group")).toBeHidden();
  await expect(page.locator("#memory-group pre")).toHaveText("");
});

test("GROUP.md: ошибка и занятость не скрывают память, повтор через Alt R соблюдает диалог", async ({ page }) => {
  await fixture(page);
  await page.locator("#memory-scope").selectOption("group:8");
  await page.locator("#memory-group summary").click();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.route("**/api/inspect/memory/group?*", (route) => route.fulfill({ status: 500, json: {} }));
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#group-error")).toContainText("Не удалось загрузить GROUP.md");
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await expect(page.locator("#memory-error")).toBeHidden();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/memory/group?*");
  let release, started = 0;
  const pending = new Promise((resolve) => { release = resolve; });
  await page.route("**/api/inspect/memory/group?*", async (route) => {
    started++;
    await pending;
    await route.fulfill({ json: { group_id: -8, group: "Обновлённые правила" } });
  });
  await page.keyboard.press("Alt+r");
  await expect.poll(() => started).toBe(1);
  await expect(page.locator("#memory-group .run-body")).toHaveAttribute("aria-busy", "true");
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#refresh")).toBeDisabled();
  await page.keyboard.press("Alt+r");
  expect(started).toBe(1);
  await page.locator(".memory-row").click();
  await expect(page.getByRole("dialog")).toBeVisible();
  release();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.keyboard.press("Alt+r");
  expect(started).toBe(1);
  await expect(page.locator("#memory-group pre")).toHaveText("Обновлённые правила");
  await page.keyboard.press("Escape");
  await expect(page.locator(".memory-row")).toBeFocused();
});

test("GROUP.md: поздний ответ не попадает в другой разговор или раздел", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await fixture(page);
  let release, started = false;
  const pending = new Promise((resolve) => { release = resolve; });
  await page.route("**/api/inspect/memory/group?*", async (route) => {
    started = true;
    await pending;
    await route.fulfill({ json: { group_id: -8, group: "LATE_GROUP_TEXT" } });
  });
  await page.locator("#memory-scope").selectOption("group:8");
  await expect.poll(() => started).toBe(true);
  await page.locator("#memory-scope").selectOption("personal:7");
  await expect(page.locator("#memory-group")).toBeHidden();
  await expect(page.locator("#memory-group pre")).toHaveText("");
  await expect(page.locator("#refresh")).toBeEnabled();
  const response = page.waitForResponse((r) => new URL(r.url()).pathname.endsWith("/group"));
  release();
  await response;
  await expect(page.locator("#memory-group")).toBeHidden();
  await expect(page.locator("#memory-group pre")).toHaveText("");
  await page.evaluate(() => { location.hash = "#/memory?conversation_id=group:orphan"; });
  await expect(page.locator("#memory-scope")).toHaveValue("group:orphan");
  await expect(page.locator("#memory-group")).toBeHidden();
  await page.unroute("**/api/inspect/memory/group?*");
  let releaseView, viewStarted = false;
  const viewPending = new Promise((resolve) => { releaseView = resolve; });
  await page.route("**/api/inspect/memory/group?*", async (route) => {
    viewStarted = true;
    await viewPending;
    await route.fulfill({ json: { group_id: -8, group: "LATE_VIEW_TEXT" } });
  });
  await page.locator("#memory-scope").selectOption("group:8");
  await expect.poll(() => viewStarted).toBe(true);
  await page.locator("#nav-sessions").click();
  const viewResponse = page.waitForResponse((r) => new URL(r.url()).pathname.endsWith("/group"));
  releaseView();
  await viewResponse;
  await expect(page.locator("#memory-group")).toHaveCount(0);
  expect(errors).toEqual([]);
});

const statsPersonal = {
  scope: "personal:7",
  total_entries: 12,
  total_chars: 9420,
  total_bytes: 16840,
  oldest_at: "2026-10-12T10:00:00Z",
  newest_at: "2026-10-23T12:00:00Z",
  span_days: 12,
  triggers: { message: 4, task: 5, initiative: 3, coding: 0, unknown: 0 },
  background_entries: 8,
  background_share_percent: 66.7,
  trend: { last_7_days: 7, prev_7_days: 5, delta: 2, delta_percent: 40, avg_per_day: 1 },
  daily_growth: [
    { date: "2026-10-19", total: 2, message: 1, task: 1, initiative: 0, coding: 0, unknown: 0 },
    { date: "2026-10-20", total: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { date: "2026-10-21", total: 3, message: 1, task: 1, initiative: 1, coding: 0, unknown: 0 },
    { date: "2026-10-22", total: 4, message: 1, task: 2, initiative: 1, coding: 0, unknown: 0 },
    { date: "2026-10-23", total: 3, message: 1, task: 1, initiative: 1, coding: 0, unknown: 0 },
  ],
  profile_budget: {
    limit: 8000,
    total_chars: 8640,
    included_chars: 8000,
    truncated_chars: 640,
    usage_percent: 108,
    status: "over",
    total_entries: 3,
    included_entries: 1,
    truncated_entries: 2,
    truncated_items: [
      {
        id: "pref <script>",
        kind: "preference",
        content: "Предпочитает короткие ответы <script>window.XSS=true</script>",
        chars: 420,
        start_char: 7800,
        end_char: 8220,
        partial: true,
        updated_at: "2026-10-23T12:00:00Z",
      },
      {
        id: "place",
        kind: "profile",
        content: "Живёт в Москве",
        chars: 220,
        start_char: 8221,
        end_char: 8441,
        partial: false,
        updated_at: "2026-10-22T12:00:00Z",
      },
    ],
  },
  kinds: [
    { kind: "profile", total: 2, chars: 2100, message: 2, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { kind: "preference", total: 3, chars: 2800, message: 1, task: 1, initiative: 1, coding: 0, unknown: 0 },
    { kind: "fact", total: 4, chars: 2900, message: 1, task: 2, initiative: 1, coding: 0, unknown: 0 },
    { kind: "episode", total: 2, chars: 1200, message: 0, task: 2, initiative: 0, coding: 0, unknown: 0 },
    { kind: "hypothesis", total: 1, chars: 420, message: 0, task: 0, initiative: 1, coding: 0, unknown: 0 },
  ],
  top_generators: [
    { trigger: "task", task_id: "task-digest", title: "Утренний дайджест <img src=x onerror=window.GEN_XSS=true>", count: 5, chars: 4100, latest_at: "2026-10-23T08:00:00Z" },
    { trigger: "initiative", count: 3, chars: 1900, latest_at: "2026-10-22T20:00:00Z" },
  ],
  latest_consolidation: {
    id: "memory-consolidation/personal:7/2026-10-23",
    completed_at: "2026-10-23T06:00:00Z",
    before_entries: 18,
    after_entries: 12,
    before_profile_chars: 10200,
    after_profile_chars: 8640,
    profile_chars_delta: -1560,
    operations: { merged: 3, reclassified: 1, deleted: 2, kept: 6, updated: 0, skipped: 0 },
    log: {
      scope: "personal:7",
      operations: [
        { op: "merge", ids: ["a", "b"], reason: "Объединены дубликаты <script>window.LOG_XSS=true</script>" },
        { op: "delete", ids: ["c"], reason: "Устаревшее напоминание" },
      ],
    },
  },
};

const statsEmpty = {
  scope: "group:empty",
  total_entries: 0,
  total_chars: 0,
  total_bytes: 0,
  span_days: 0,
  triggers: { message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
  background_entries: 0,
  background_share_percent: 0,
  trend: { last_7_days: 0, prev_7_days: 0, delta: 0, avg_per_day: 0 },
  daily_growth: [],
  profile_budget: {
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
  },
  kinds: [
    { kind: "profile", total: 0, chars: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { kind: "preference", total: 0, chars: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { kind: "fact", total: 0, chars: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { kind: "episode", total: 0, chars: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
    { kind: "hypothesis", total: 0, chars: 0, message: 0, task: 0, initiative: 0, coding: 0, unknown: 0 },
  ],
  top_generators: [],
};

test("аналитика памяти: вкладки, KPI, графики, бюджет USER.md, переходы и клавиатура", async ({ page }, testInfo) => {
  const errors = [];
  const consoleErrors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });
  await fixture(page);
  await page.route("**/api/inspect/memory/stats?*", (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    return route.fulfill({ json: scope === "group:empty" ? statsEmpty : statsPersonal });
  });

  // Switch to Analytics via WAI-ARIA tab keyboard navigation and verify tab state.
  const entriesTab = page.locator("#memory-tab-entries");
  const analyticsTab = page.locator("#memory-tab-analytics");
  await expect(entriesTab).toHaveAttribute("aria-selected", "true");
  await expect(analyticsTab).toHaveAttribute("aria-keyshortcuts", "Alt+2");
  await entriesTab.focus();
  await page.keyboard.press("ArrowRight");
  await expect(analyticsTab).toBeFocused();
  await expect(analyticsTab).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/tab=analytics/);
  await expect(page.locator("#memory-analytics")).toHaveAttribute("aria-busy", "false");

  // KPIs, daily SVG chart, USER.md truncation, kind breakdown, generators, consolidation.
  await expect(page.locator(".kpi-card")).toHaveCount(4);
  await expect(page.locator(".kpi-card").nth(0)).toContainText("12");
  await expect(page.locator(".kpi-card").nth(1)).toContainText("66,7%");
  await expect(page.locator(".kpi-card").nth(2)).toContainText("8\u00a0640 / 8\u00a0000");
  await expect(page.locator(".kpi-card").nth(2)).toHaveClass(/kpi-budget-over/);
  await expect(page.locator(".growth-chart .chart-day")).toHaveCount(5);
  await expect(page.locator(".truncated-row")).toHaveCount(2);
  await expect(page.locator(".generator-row")).toHaveCount(2);
  await expect(page.locator(".consolidation-panel")).toBeVisible();

  // Clicking a truncated USER.md item opens the existing Inspector entry detail drawer.
  const firstTruncated = page.locator(".truncated-row").first();
  await firstTruncated.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText(entries[0].content);
  await expect(page.locator("#close-drawer")).toBeFocused();
  // Shortcuts must be ignored while the drawer is open.
  await page.keyboard.press("Alt+1");
  await expect(analyticsTab).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(firstTruncated).toBeFocused();

  // Opening the consolidation operation log in the detail drawer + Escape focus return.
  const logBtn = page.locator("#open-consolidation-log");
  await logBtn.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("Объединены дубликаты");
  await expect(page.getByRole("dialog")).toContainText("Устаревшее напоминание");
  await page.keyboard.press("Escape");
  await expect(logBtn).toBeFocused();

  // Verify no XSS executed and no CSP/console errors occurred.
  expect(await page.evaluate(() => window.XSS || window.GEN_XSS || window.LOG_XSS)).toBeUndefined();
  expect(errors).toEqual([]);
  expect(consoleErrors).toEqual([]);

  // Verify keyboard-accessible daily data table exposes per-day trigger counts.
  const chartTableSummary = page.locator(".chart-table-details > summary");
  await chartTableSummary.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".chart-data-table")).toContainText("2026-10-22");
  await page.keyboard.press("Space");
  await expect(page.locator(".chart-table-details")).not.toHaveAttribute("open", "");

  // Capture desktop & mobile screenshots, verifying zero overflow and legible SVG labels at 390px.
  await page.screenshot({ path: testInfo.outputPath("memory-analytics-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const mobileLabelPx = await page.locator(".growth-chart .chart-axis-text").first().evaluate((el) => el.getBoundingClientRect().height);
  expect(mobileLabelPx).toBeGreaterThanOrEqual(8);
  await page.screenshot({ path: testInfo.outputPath("memory-analytics-mobile.png"), fullPage: true });
  await page.setViewportSize({ width: 1440, height: 1000 });

  // Pressing Enter on a kind breakdown bar switches to Entries pre-filtered and moves focus to #memory-kind.
  const factBar = page.locator('.kind-bar-row[data-filter-kind="fact"]');
  await factBar.focus();
  await page.keyboard.press("Enter");
  await expect(entriesTab).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#memory-kind")).toHaveValue("fact");
  await expect(page.locator("#memory-kind")).toBeFocused();
  await expect(page.locator(".memory-row")).toHaveCount(1);
  await expect(page.locator(".memory-row")).toContainText("Разрабатывает OpenAlbert");

  // Alt+2 / Alt+1 shortcuts respect IME composition, repeat, and non-English layout.
  for (const extra of [{ isComposing: true }, { repeat: true }, { keyCode: 229 }]) {
    await page.locator("body").dispatchEvent("keydown", { key: "2", code: "Digit2", altKey: true, bubbles: true, ...extra });
    await expect(entriesTab).toHaveAttribute("aria-selected", "true");
  }
  await page.locator("body").dispatchEvent("keydown", { key: "@", code: "Digit2", altKey: true, bubbles: true });
  await expect(analyticsTab).toHaveAttribute("aria-selected", "true");

  // Ctrl+K from Analytics switches to Entries and focuses #memory-search.
  await page.locator("body").dispatchEvent("keydown", { key: "л", code: "KeyK", ctrlKey: true, bubbles: true });
  await expect(entriesTab).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#memory-search")).toBeFocused();

  // Alt+2 back to Analytics, switch to empty scope, verify clean empty state without consolidation card.
  await page.locator("#memory-tab-analytics").click();
  await page.locator("#memory-scope").selectOption("group:empty");
  await expect(page.locator("#memory-analytics")).toContainText("В этом разговоре пока нет записей памяти");
  await expect(page.locator(".consolidation-panel")).toHaveCount(0);
});

test("аналитика памяти: ошибки, повтор по Alt R и отбрасывание позднего ответа", async ({ page }) => {
  await fixture(page);
  let failOnce = true;
  await page.route("**/api/inspect/memory/stats?*", (route) => {
    if (failOnce) {
      failOnce = false;
      return route.fulfill({ status: 500, json: {} });
    }
    return route.fulfill({ json: statsPersonal });
  });
  await page.locator("#memory-tab-analytics").click();
  await expect(page.locator("#memory-analytics-error")).toBeVisible();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#memory-analytics-error")).toBeHidden();
  await expect(page.locator(".kpi-card")).toHaveCount(4);

  // Late response from previous scope must not overwrite the newly selected scope.
  let releaseOld, oldStarted = false;
  const oldPending = new Promise((resolve) => { releaseOld = resolve; });
  await page.route("**/api/inspect/memory/stats?*", async (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope === "group:8") {
      oldStarted = true;
      await oldPending;
      return route.fulfill({ json: { ...statsPersonal, scope: "group:8", total_entries: 999 } });
    }
    return route.fulfill({ json: statsEmpty });
  });
  await page.locator("#memory-scope").selectOption("group:8");
  await expect.poll(() => oldStarted).toBe(true);
  await page.locator("#memory-scope").selectOption("group:empty");
  await expect(page.locator("#memory-analytics")).toContainText("В этом разговоре пока нет записей памяти");
  const late = page.waitForResponse((r) => new URL(r.url()).searchParams.get("conversation_id") === "group:8");
  releaseOld();
  await late;
  await expect(page.locator("#memory-analytics")).toContainText("В этом разговоре пока нет записей памяти");
  await expect(page.locator("#memory-analytics")).not.toContainText("999");
});

test("аналитика памяти: переключение разговора между вкладками, занятость и пустая установка", async ({ page }) => {
  await fixture(page);
  // S1: Delayed personal stats must not be cached under group:8 after switching tabs and scope.
  let releasePersonalStats;
  const personalPending = new Promise((resolve) => { releasePersonalStats = resolve; });
  await page.route("**/api/inspect/memory/stats?*", async (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope === "personal:7") {
      await personalPending;
      return route.fulfill({ json: statsPersonal });
    }
    return route.fulfill({ json: { ...statsEmpty, scope } });
  });
  await page.locator("#memory-tab-analytics").click();
  await page.locator("#memory-tab-entries").click();
  await page.locator("#memory-scope").selectOption("group:8");
  const personalResp = page.waitForResponse((r) => r.url().includes("/memory/stats") && r.url().includes("personal%3A7"));
  releasePersonalStats();
  await personalResp;
  await page.locator("#memory-tab-analytics").click();
  await expect(page.locator("#memory-analytics")).toContainText("В этом разговоре пока нет записей памяти");
  await expect(page.locator(".consolidation-panel")).toHaveCount(0);

  // S2 + S3: Pending Entries request invalidated by scope change in Analytics must not leak old rows/USER.md or leave #refresh disabled.
  await page.locator("#memory-tab-entries").click();
  await page.locator("#memory-scope").selectOption("personal:7");
  await expect(page.locator("#memory-profile")).toBeVisible();
  let releasePendingEntries;
  const entriesPending = new Promise((resolve) => { releasePendingEntries = resolve; });
  await page.route("**/api/inspect/memory?*", async (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope === "personal:7") {
      await entriesPending;
      return route.fulfill({ json: { entries: entries.slice(0, 2), total: 2, next_after: 0, profile: "# User\n\nPersonal" } });
    }
    return route.fulfill({ status: 500, json: {} });
  });
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeDisabled();
  await page.locator("#memory-tab-analytics").click();
  await page.locator("#memory-scope").selectOption("group:8");
  await expect(page.locator("#memory-analytics")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#refresh")).toBeEnabled();
  releasePendingEntries();
  await page.locator("#memory-tab-entries").click();
  await expect(page.locator("#memory-error")).toBeVisible();
  await expect(page.locator("#memory-profile")).toBeHidden();
  await expect(page.locator(".memory-row")).toHaveCount(0);

  // S6: Empty installation opened directly on Analytics loads Entries cleanly on tab switch.
  await page.unroute("**/api/inspect/**");
  await page.goto("about:blank");
  await page.route("**/api/inspect/**", (route) => {
    const u = new URL(route.request().url());
    if (u.pathname.endsWith("/soul")) return route.fulfill({ json: { soul: "# OpenAlbert" } });
    if (u.pathname.endsWith("/conversations")) return route.fulfill({ json: { conversations: [] } });
    if (u.pathname.endsWith("/sessions")) return route.fulfill({ json: { sessions: [], next_before: 0 } });
    return route.fulfill({ json: {} });
  });
  await page.goto("/inspect/#/memory?tab=analytics");
  await expect(page.locator("#memory-analytics")).toContainText("Разговоров пока нет");
  await page.locator("#memory-tab-entries").click();
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#memory-list")).toContainText("Разговоров пока нет");
});

test("аналитика памяти: закладка другого разговора, сброс счётчика вкладки и подписи длинной серии", async ({ page }) => {
  await fixture(page);
  // S2: Same-view hash navigation to group Analytics while personal Entries is pending must drop stale personal data.
  let releasePersonalEntries, personalEntriesStarted = false;
  const personalEntriesPending = new Promise((resolve) => { releasePersonalEntries = resolve; });
  await page.route("**/api/inspect/memory?*", async (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope === "personal:7") {
      personalEntriesStarted = true;
      await personalEntriesPending;
      return route.fulfill({ json: { entries: entries.slice(0, 2), total: 2, next_after: 0, profile: "# User\n\nPERSONAL_ONLY" } });
    }
    return route.fulfill({ json: { entries: [entries[3]], total: 1, next_after: 0, profile: "" } });
  });
  await page.route("**/api/inspect/memory/stats?*", (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope === "group:8") {
      // S8: 83-day series to verify adjacent X-axis labels never overlap at 390px.
      const base = new Date("2026-08-01T00:00:00Z");
      const longSeries = Array.from({ length: 83 }, (_, i) => {
        const d = new Date(base.getTime() + i * 86400000).toISOString().slice(0, 10);
        return { date: d, total: i % 3 === 0 ? 2 : 0, message: i % 3 === 0 ? 1 : 0, task: i % 3 === 0 ? 1 : 0, initiative: 0, coding: 0, unknown: 0 };
      });
      return route.fulfill({ json: { ...statsPersonal, scope: "group:8", daily_growth: longSeries } });
    }
    return route.fulfill({ json: { ...statsEmpty, scope } });
  });
  await page.keyboard.press("Alt+r");
  await expect.poll(() => personalEntriesStarted).toBe(true);
  await page.evaluate(() => { location.hash = "#/memory?conversation_id=group:8&tab=analytics"; });
  await expect(page.locator("#memory-scope")).toHaveValue("group:8");
  const stalePersonalResp = page.waitForResponse((r) => r.url().includes("/api/inspect/memory?") && r.url().includes("personal%3A7"));
  releasePersonalEntries();
  await stalePersonalResp;
  // Stale personal response must not populate the hidden Entries panel before switching tabs.
  await expect(page.locator("#memory-profile pre")).toHaveText("");
  await expect(page.locator("#memory-list")).not.toContainText("Предпочитает короткие ответы");
  await page.locator("#memory-tab-entries").click();
  await expect(page.locator("#memory-profile")).toBeHidden();
  await expect(page.locator("#memory-list")).toContainText("Любит пончики");
  await expect(page.locator("#memory-list")).not.toContainText("Предпочитает короткие ответы");

  // S8: Verify 83-day series X-axis labels do not overlap at 390px viewport.
  await page.locator("#memory-tab-analytics").click();
  await page.setViewportSize({ width: 390, height: 844 });
  const labelBoxes = await page.locator(".growth-chart .chart-day text.chart-axis-text").evaluateAll((nodes) =>
    nodes.map((n) => {
      const r = n.getBoundingClientRect();
      return { left: r.left, right: r.right, height: r.height };
    }),
  );
  expect(labelBoxes.length).toBeGreaterThanOrEqual(2);
  for (let i = 0; i < labelBoxes.length; i++) {
    expect(labelBoxes[i].height).toBeGreaterThanOrEqual(8);
    if (i > 0) expect(labelBoxes[i - 1].right).toBeLessThan(labelBoxes[i].left);
  }
  await page.setViewportSize({ width: 1440, height: 1000 });

  // S14: Filtering Entries to a kind, switching to Analytics, then choosing an empty scope must clear the Entries badge.
  await page.locator("#memory-tab-entries").click();
  await page.locator("#memory-kind").selectOption("preference");
  await expect(page.locator("#memory-tab-entries-count")).toHaveText("1");
  await page.locator("#memory-tab-analytics").click();
  await page.locator("#memory-scope").selectOption("group:empty");
  await expect(page.locator("#memory-analytics")).toContainText("В этом разговоре пока нет записей памяти");
  await expect(page.locator("#memory-tab-entries-count")).toHaveText("");
});

test("история консолидации: отчёт, операции, клавиатура и защита от XSS", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await fixture(page);
  await expect(page.locator("#memory-consolidations")).toBeHidden();
  await page.route("**/api/inspect/memory/consolidations?*", (route) => {
    const scope = new URL(route.request().url()).searchParams.get("conversation_id");
    if (scope !== "personal:7") return route.fulfill({ json: { consolidations: [] } });
    return route.fulfill({
      json: {
        consolidations: [{
          id: "run-1",
          scope: "personal:7",
          trigger: "threshold",
          started_at: "2026-11-25T10:00:00Z",
          completed_at: "2026-11-25T10:01:00Z",
          status: "completed",
          before_count: 25,
          after_count: 14,
          profile_chars_before: 6500,
          profile_chars_after: 3100,
          profile_chars_delta: -3400,
          operations: [
            { phase: "profile", op: "merge", ids: ["a", "b"], result_id: "a", status: "applied", reason: "дубликат <script>window.CONSOL_XSS=true</script>" },
          ],
        }],
      },
    });
  });
  await page.keyboard.press("Alt+r");
  const section = page.locator("#memory-consolidations");
  await expect(section).toBeVisible();
  const summary = section.locator("summary");
  await expect(summary).toHaveAttribute("aria-keyshortcuts", "Enter Space");
  await summary.focus();
  await page.keyboard.press("Enter");
  await expect(section).toHaveAttribute("open", "");
  await expect(section).toContainText("Записей: 25 → 14");
  await expect(section).toContainText("-3400");
  await expect(section).toContainText("profile/merge");
  await expect(section).toContainText("<script>window.CONSOL_XSS=true</script>");
  expect(await page.evaluate(() => window.CONSOL_XSS)).toBeUndefined();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.setViewportSize({ width: 1280, height: 800 });
  await summary.focus();
  await page.keyboard.press("Space");
  await expect(section).not.toHaveAttribute("open", "");
  await page.locator("#memory-scope").selectOption("group:8");
  await expect(section).toBeHidden();
  expect(errors).toEqual([]);
});

test("селектор памяти: скрывает coding:*, приоритезирует personal:* и группирует пустые разговоры", async ({ page }) => {
  const errors = [];
  const fetchedScopes = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/api/inspect/**", (route) => {
    const u = new URL(route.request().url());
    if (u.pathname.endsWith("/soul")) return route.fulfill({ json: { soul: "# OpenAlbert" } });
    if (u.pathname.endsWith("/group")) return route.fulfill({ json: { group_id: -8, group: "" } });
    if (u.pathname.endsWith("/consolidations")) return route.fulfill({ json: { consolidations: [] } });
    const codingSession = {
      id: "oa-coding-1",
      seq: 1,
      conversation_id: "coding:oa-coding-1",
      conversation: { id: "coding:oa-coding-1", private: true, title: "mishankov" },
      status: "closed",
      trigger: "coding",
      created_at: "2026-11-26T10:00:00Z",
      message_count: 0,
      tool_count: 0,
      trace_count: 0,
    };
    if (u.pathname.endsWith("/sessions")) {
      return route.fulfill({ json: { sessions: [codingSession], next_before: 0 } });
    }
    if (u.pathname.includes("/sessions/oa-coding-1")) {
      return route.fulfill({
        json: {
          session: codingSession,
          messages: [], traces: [], outgoing: [], turns: [], artifacts: [], next_after: 0,
        },
      });
    }
    if (u.pathname.endsWith("/conversations")) {
      // Deliberately return coding:* and empty groups ahead of personal:* to test client defense-in-depth + grouping.
      return route.fulfill({
        json: {
          conversations: [
            { id: "coding:oa-1", title: "mishankov", private: true, memory_count: 0 },
            { id: "group:empty-a", chat_id: -10, title: "Пустой топик A", memory_count: 0 },
            { id: "group:8", chat_id: -8, title: "Рабочий топик с очень длинным названием для проверки переполнения на узком экране", memory_count: 2, last_updated_at: "2026-11-26T09:00:00Z" },
            { id: "personal:7", chat_id: 7, private: true, title: "Личный разговор", memory_count: 146, has_profile: true, last_updated_at: "2026-11-26T10:00:00Z" },
            { id: "group:empty-b", chat_id: -11, title: "Пустой топик B", memory_count: 0 },
          ],
        },
      });
    }
    if (u.pathname.endsWith("/stats")) {
      const scope = u.searchParams.get("conversation_id");
      fetchedScopes.push(`stats:${scope}`);
      return route.fulfill({ json: scope === "personal:7" ? statsPersonal : { ...statsEmpty, scope } });
    }
    const scope = u.searchParams.get("conversation_id");
    fetchedScopes.push(`memory:${scope}`);
    const found = entries.filter((e) => e.scope === scope);
    return route.fulfill({ json: { entries: found, total: found.length, next_after: 0, profile: scope === "personal:7" ? "# User" : "" } });
  });

  // Start from an open coding:* session, then navigate to Memory -> must default to personal:7, NOT coding:oa-coding-1.
  await page.goto("/inspect/#/session/oa-coding-1");
  await expect(page.locator(".session-heading")).toContainText("mishankov");
  await page.locator("#nav-memory").click();
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");

  const scopeSelect = page.locator("#memory-scope");
  await expect(scopeSelect).toHaveValue("personal:7");

  const optionValues = await scopeSelect.locator("option").evaluateAll((els) => els.map((el) => el.value));
  expect(optionValues).toEqual(["personal:7", "group:8", "group:empty-a", "group:empty-b"]);

  const groups = await scopeSelect.locator("optgroup").evaluateAll((els) =>
    els.map((g) => ({ label: g.label, values: [...g.querySelectorAll("option")].map((o) => o.value) })),
  );
  expect(groups).toEqual([
    { label: "С памятью", values: ["personal:7", "group:8"] },
    { label: "Без записей памяти (2)", values: ["group:empty-a", "group:empty-b"] },
  ]);

  // Empty conversations remain selectable (e.g., to inspect GROUP.md or confirm 0 entries).
  await scopeSelect.selectOption("group:empty-a");
  await expect(page.locator("#memory-list")).toContainText("В этом разговоре пока нет записей памяти");

  // Existing coding:* bookmarks in Memory and Analytics must be discarded in favor of personal:7 without re-adding coding:*.
  await page.goto("about:blank");
  await page.goto("/inspect/#/memory?conversation_id=coding:old");
  await expect(page.locator("#memory-list")).toHaveAttribute("aria-busy", "false");
  await expect(scopeSelect).toHaveValue("personal:7");
  expect(await scopeSelect.locator("option").evaluateAll((els) => els.map((el) => el.value))).not.toContain("coding:old");

  await page.goto("about:blank");
  await page.goto("/inspect/#/memory?conversation_id=coding:old&tab=analytics");
  await expect(page.locator("#memory-analytics")).toHaveAttribute("aria-busy", "false");
  await expect(scopeSelect).toHaveValue("personal:7");
  expect(await scopeSelect.locator("option").evaluateAll((els) => els.map((el) => el.value))).not.toContain("coding:old");
  expect(fetchedScopes.filter((s) => s.includes("coding:"))).toEqual([]);

  // Fresh direct Analytics navigation without conversation_id also defaults to personal:7 and hides coding:*.
  await page.goto("about:blank");
  await page.goto("/inspect/#/memory?tab=analytics");
  await expect(page.locator("#memory-analytics")).toHaveAttribute("aria-busy", "false");
  await expect(scopeSelect).toHaveValue("personal:7");

  // Keyboard flow and narrow mobile viewport (390px) remain accessible without horizontal overflow.
  await page.setViewportSize({ width: 390, height: 844 });
  await scopeSelect.selectOption("group:8");
  await expect(scopeSelect).toHaveValue("group:8");
  await page.keyboard.press("Alt+Digit1");
  await expect(page.locator("#memory-tab-entries")).toHaveAttribute("aria-selected", "true");
  const overflow = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
    selectRight: Math.round(document.querySelector("#memory-scope").getBoundingClientRect().right),
  }));
  expect(overflow.scrollWidth).toBeLessThanOrEqual(overflow.clientWidth);
  expect(overflow.selectRight).toBeLessThanOrEqual(overflow.clientWidth);
  expect(errors).toEqual([]);
});
