import {
  test,
  expect,
} from "../../../docs/node_modules/@playwright/test/index.mjs";

const files = [
  {
    key: "artifacts/hash/file",
    name: "Заметки <script>.txt",
    mime: "text/plain",
    size: 2048,
    modified_at: "2026-10-22T12:00:00Z",
    conversation_id: "personal:7",
    artifact_id: "notes",
  },
  {
    key: "uploads/скрин +#?.png",
    name: "Скриншот.png",
    mime: "image/png",
    size: 512,
    modified_at: "2026-10-22T12:00:00Z",
  },
  {
    key: "uploads/archive.zip",
    name: "archive.zip",
    mime: "application/zip",
    size: 2000000,
    modified_at: "2026-10-22T12:00:00Z",
  },
];
const png = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aPz8AAAAASUVORK5CYII=",
  "base64",
);

async function fixture(page, { imageFailure = false } = {}) {
  const requests = [];
  await page.route("**/api/inspect/**", async (route) => {
    const u = new URL(route.request().url());
    requests.push(u);
    if (u.pathname.endsWith("/sessions"))
      return route.fulfill({ json: { sessions: [], next_before: 0 } });
    if (u.pathname.endsWith("/files")) {
      const prefix = u.searchParams.get("prefix");
      const after = u.searchParams.get("after");
      return route.fulfill({
        json: {
          bucket: "openalbert",
          files: prefix
            ? files.filter((f) => f.key.startsWith(prefix))
            : after
              ? [files[2]]
              : files.slice(0, 2),
          next_after: !prefix && !after ? files[1].key : "",
        },
      });
    }
    const file = files.find((f) => f.key === u.searchParams.get("key"));
    if (!file)
      return route.fulfill({ status: 404, json: { error: "Not Found" } });
    if (u.pathname.endsWith("/content") && imageFailure)
      return route.fulfill({ status: 500, body: "unavailable" });
    if (u.pathname.endsWith("/content"))
      return route.fulfill({
        body: file.mime === "image/png" ? png : Buffer.from("file bytes"),
        headers: {
          "Content-Type": file.mime,
          ...(u.searchParams.get("download")
            ? { "Content-Disposition": 'attachment; filename="download.txt"' }
            : {}),
        },
      });
    return route.fulfill({
      json: {
        file,
        preview_kind:
          file.mime === "text/plain"
            ? "text"
            : file.mime === "image/png"
              ? "image"
              : "unsupported",
        text: "Привет <script>window.XSS=true</script>",
        truncated: true,
      },
    });
  });
  await page.goto("/inspect/#/files");
  await expect(page.locator(".file-row")).toHaveCount(2);
  return requests;
}

test("файлы: страницы, текст, изображение, скачивание и клавиатура", async ({
  page,
}) => {
  const requests = await fixture(page);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await expect(page.locator("#nav-files")).toHaveAttribute(
    "aria-current",
    "page",
  );
  await expect(page.locator("#file-bucket")).toHaveText("openalbert");
  await page.locator(".file-row").first().focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("personal:7");
  await expect(page.locator(".file-text")).toContainText(
    "<script>window.XSS=true</script>",
  );
  await expect(page.getByRole("dialog")).toContainText(
    "Показаны первые 256 КБ",
  );
  expect(await page.evaluate(() => window.XSS)).toBeUndefined();
  await page.keyboard.press("Meta+k");
  await expect(page.locator("#file-prefix")).not.toBeFocused();
  for (const extra of [{ isComposing: true }, { repeat: true }]) {
    await page.locator("#close-drawer").dispatchEvent("keydown", {
      code: "KeyS",
      key: "Ы",
      altKey: true,
      shiftKey: true,
      bubbles: true,
      ...extra,
    });
  }
  expect(requests.filter((u) => u.searchParams.has("download"))).toHaveLength(
    0,
  );
  const download = page.waitForEvent("download");
  await page.locator("#close-drawer").dispatchEvent("keydown", {
    code: "KeyS",
    key: "Ы",
    altKey: true,
    shiftKey: true,
    bubbles: true,
  });
  await download;
  await page.keyboard.press("Escape");
  await expect(page.locator(".file-row").first()).toBeFocused();
  await page.locator(".file-row").nth(1).focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".file-preview")).toBeVisible();
  await expect
    .poll(() =>
      page.locator(".file-preview").evaluate((img) => img.naturalWidth),
    )
    .toBeGreaterThan(0);
  await page.keyboard.press("Escape");
  await page.locator("#more-files").focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".file-row")).toHaveCount(3);
  await expect(page.locator(".file-row").last()).toBeFocused();
  expect(
    requests.some((u) => u.searchParams.get("after") === files[1].key),
  ).toBe(true);
  await page.locator(".file-row").last().click();
  await expect(page.getByRole("dialog")).toContainText(
    "Предпросмотр этого формата недоступен",
  );
  await page.keyboard.press("Escape");
  expect(errors).toEqual([]);
});

test("галерея показывает изображения сразу, сохраняет кадр и сообщает порядок", async ({
  page,
}) => {
  const requests = await fixture(page);
  const thumbnail = page.locator(".file-thumbnail img");
  await expect(page.locator(".files-order")).toHaveText(
    "По дате изменения ↓ · сначала новые",
  );
  await expect(thumbnail).toBeVisible();
  await expect
    .poll(() => thumbnail.evaluate((img) => img.naturalWidth))
    .toBeGreaterThan(0);
  await expect(page.locator(".file-thumbnail.is-loaded")).toHaveCount(1);
  expect(
    requests.some(
      (u) =>
        u.pathname.endsWith("/content") &&
        u.searchParams.get("key") === files[1].key,
    ),
  ).toBe(true);
  expect(requests.some((u) => u.pathname.endsWith("/file"))).toBe(false);
  const preview = await thumbnail.evaluate((img) => ({
    width: img.clientWidth,
    height: img.clientHeight,
    fit: getComputedStyle(img).objectFit,
  }));
  expect(preview.width).toBeGreaterThanOrEqual(200);
  expect(preview.height).toBeGreaterThanOrEqual(180);
  expect(preview.fit).toBe("contain");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(thumbnail).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.locator(".file-row").nth(1).focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".file-preview")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".file-row").nth(1)).toBeFocused();
});

test("ошибка превью не мешает открыть и скачать изображение", async ({
  page,
}) => {
  await fixture(page, { imageFailure: true });
  await expect(page.locator(".file-thumbnail.is-failed")).toContainText(
    "Не удалось загрузить изображение",
  );
  await page.locator(".file-row").nth(1).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.locator("#download-file")).toBeVisible();
  await expect(page.locator("#image-error")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".file-row").nth(1)).toBeFocused();
});

test("занятость и запоздавший ответ не заменяют новый фильтр", async ({
  page,
}) => {
  await fixture(page);
  let release,
    count = 0;
  const pending = new Promise((resolve) => {
    release = resolve;
  });
  await page.route("**/api/inspect/files?*", async (route) => {
    count++;
    await pending;
    await route.fulfill({
      json: { bucket: "openalbert", files: files.slice(0, 2) },
    });
  });
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeDisabled();
  await expect.poll(() => count).toBe(1);
  await page.keyboard.press("Alt+r");
  expect(count).toBe(1);
  release();
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.unroute("**/api/inspect/files?*");
  let releaseOld,
    started = false;
  const old = new Promise((resolve) => {
    releaseOld = resolve;
  });
  await page.route("**/api/inspect/files?*", async (route) => {
    const prefix = new URL(route.request().url()).searchParams.get("prefix");
    if (prefix === "uploads/") {
      started = true;
      await old;
      await route.fulfill({ json: { bucket: "openalbert", files: [files[1]] } });
    } else await route.fulfill({ json: { bucket: "openalbert", files: [] } });
  });
  await page.locator("#file-prefix").fill("uploads/");
  await expect.poll(() => started).toBe(true);
  await page.locator("#file-prefix").fill("missing/");
  await expect(page.locator("#files-list")).toContainText(
    "Файлов с таким префиксом нет",
  );
  const lateResponse = page.waitForResponse(
    (r) => new URL(r.url()).searchParams.get("prefix") === "uploads/",
  );
  releaseOld();
  await lateResponse;
  await expect(page.locator(".file-row")).toHaveCount(0);
  await expect(page.locator("#file-prefix")).toHaveValue("missing/");
  await expect(page.locator("#refresh")).toBeEnabled();
});

test("фильтр сохраняется при обновлении и переходах, ошибки можно повторить", async ({
  page,
}) => {
  await fixture(page);
  await page.locator("body").dispatchEvent("keydown", {
    code: "KeyK",
    key: "л",
    ctrlKey: true,
    bubbles: true,
  });
  await expect(page.locator("#file-prefix")).toBeFocused();
  await page.locator("#file-prefix").fill("uploads/");
  await expect(page.locator(".file-row").first()).toContainText("Скриншот.png");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#refresh")).toBeEnabled();
  await expect(page.locator("#file-prefix")).toHaveValue("uploads/");
  await page.locator("#nav-sessions").click();
  await page.locator("#nav-files").click();
  await expect(page.locator("#file-prefix")).toHaveValue("uploads/");
  await expect(page.locator("#refresh")).toBeEnabled();
  await page.route("**/api/inspect/files?*", (route) =>
    route.fulfill({ status: 500, json: { error: "failure" } }),
  );
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#files-error")).toBeVisible();
  await page.unroute("**/api/inspect/files?*");
  await page.keyboard.press("Alt+r");
  await expect(page.locator("#files-error")).toBeHidden();
  await page.locator("#file-prefix").fill("missing/");
  await expect(page.locator("#files-list")).toContainText(
    "Файлов с таким префиксом нет",
  );
  await page.locator("#file-prefix").fill("");
  await expect(page.locator(".file-row")).toHaveCount(2);
});

test("узкий экран, ошибка чтения и защита от запоздавшего ответа", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await fixture(page);
  await expect(page.locator(".sidebar")).toBeHidden();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.route("**/api/inspect/file?*", (route) =>
    route.fulfill({ status: 404, json: {} }),
  );
  await page.locator(".file-row").first().click();
  await expect(page.getByRole("dialog")).toContainText("не найдены");
  await page.keyboard.press("Escape");
  await page.unroute("**/api/inspect/file?*");
  let release;
  const pending = new Promise((resolve) => {
    release = resolve;
  });
  await page.route("**/api/inspect/file?*", async (route) => {
    await pending;
    await route.fulfill({
      json: { file: files[0], preview_kind: "text", text: "late" },
    });
  });
  await page.locator(".file-row").first().focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  release();
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(page.locator(".file-row").first()).toBeFocused();
});
