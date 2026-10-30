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
