import { test, expect } from '@playwright/test';
import { readFile } from 'node:fs/promises';

const html = await readFile(new URL('../../../../apps/backend/internal/widgets/schedules.html', import.meta.url), 'utf8');
const customHTML = await readFile(new URL('../../../../apps/backend/internal/widgets/testdata/custom-summary.html', import.meta.url), 'utf8');

async function mount(page, state, source = html) {
  await page.goto('about:blank');
  await page.evaluate((state) => {
    window.requests = [];
    window.events = {};
    window.TelegramWidget = {
      chatData: JSON.stringify(state),
      ready() {},
      onEvent(name, fn) { window.events[name] = fn; },
      request(data) {
        window.requests.push(JSON.parse(data));
        return new Promise((resolve, reject) => {
          window.answer = resolve;
          window.fail = reject;
        });
      },
    };
  }, state);
  // The shipped widget runs against the documented TelegramWidget API stub.
  await page.setContent(source);
}

test('schedule controls show server state, disable while busy, and recover from errors', async ({ page }, testInfo) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await mount(page, [{ t: 'НБА', s: 0, v: 1 }, { t: 'Полка', s: 1, v: 9 }]);
  await page.setViewportSize({ width: 320, height: 700 });
  await expect(page.getByText(/Клавиатура: \/schedules/)).toBeVisible();
  await page.getByRole('button', { name: 'Приостановить: НБА', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Включить: Полка' })).toBeDisabled();
  expect(await page.evaluate(() => window.requests)).toEqual([{ k: 0, s: 1, v: 1 }]);
  await page.evaluate(() => {
    TelegramWidget.chatData = JSON.stringify([{ t: 'НБА', s: 1, v: 2 }, { t: 'Полка', s: 1, v: 9 }]);
    window.events.chatDataChanged();
    window.answer({ result: 'Обновлено' });
  });
  await expect(page.getByRole('button', { name: 'Включить: НБА' })).toBeEnabled();
  await page.getByRole('button', { name: 'Включить: НБА' }).click();
  expect(await page.evaluate(() => window.requests[1])).toEqual({ k: 0, s: 0, v: 2 });
  await page.evaluate(() => window.fail({ code: 'TIMEOUT' }));
  await expect(page.getByText(/Не удалось обновить/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Включить: НБА' })).toBeEnabled();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('schedule-controls.png') });
  expect(errors).toEqual([]);
});

test('custom HTML/JavaScript filters locally and preserves the view when Albert updates data', async ({ page }, testInfo) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await mount(page, {
    title: 'Расходы за ноябрь',
    items: [{ label: 'Кофе', amount: 250 }, { label: 'Продукты', amount: 1200 }],
  }, customHTML);
  await page.setViewportSize({ width: 320, height: 700 });
  await expect(page.getByRole('heading', { name: 'Расходы за ноябрь' })).toBeVisible();
  await expect(page.locator('#total')).toHaveText(/1\s450\s₽/);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.locator('main').screenshot({ path: testInfo.outputPath('custom-widget-light.png') });
  await page.emulateMedia({ colorScheme: 'dark' });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.locator('main').screenshot({ path: testInfo.outputPath('custom-widget-dark.png') });
  await page.emulateMedia({ colorScheme: 'light' });
  await page.getByRole('button', { name: 'От 1000 ₽' }).click();
  await expect(page.locator('#total')).toHaveText(/1\s200\s₽/);
  await expect(page.locator('#rows')).not.toContainText('Кофе');
  await page.evaluate(() => {
    TelegramWidget.chatData = JSON.stringify({ title: 'Новые расходы', items: [
      { label: 'Чай', amount: 300 }, { label: '<img src=x onerror="alert(1)">', amount: 2400 },
    ] });
    window.events.chatDataChanged();
  });
  await expect(page.getByRole('heading', { name: 'Новые расходы' })).toBeVisible();
  await expect(page.locator('#total')).toHaveText(/2\s400\s₽/);
  await expect(page.getByRole('button', { name: 'От 1000 ₽' })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#rows')).toContainText('<img src=x onerror="alert(1)">');
  await expect(page.locator('img')).toHaveCount(0);
  expect(await page.evaluate(() => window.requests)).toEqual([]);
  await page.getByRole('button', { name: 'Все расходы' }).click();
  await expect(page.locator('#total')).toHaveText(/2\s700\s₽/);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.evaluate(() => {
    TelegramWidget.chatData = JSON.stringify({ title: 'Пустая сводка', items: [] });
    window.events.chatDataChanged();
  });
  await expect(page.getByText('Нет расходов', { exact: true })).toBeVisible();
  await expect(page.locator('#total')).toHaveText('0 ₽');
  expect(errors).toEqual([]);
});

test('empty, missing, and untrusted titles render as text', async ({ page }) => {
  await mount(page, []);
  await expect(page.getByText('Нет расписаний', { exact: true })).toBeVisible();
  await page.evaluate(() => {
    TelegramWidget.chatData = JSON.stringify([{ t: '<img src=x onerror="alert(1)">', s: 0, v: 0 }]);
    window.events.chatDataChanged();
  });
  await expect(page.getByRole('button')).toHaveText('Приостановить: <img src=x onerror="alert(1)">');
  await expect(page.getByRole('button')).toBeDisabled();
  await expect(page.locator('img')).toHaveCount(0);
});
