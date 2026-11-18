import { test, expect } from '@playwright/test';
import { readFile } from 'node:fs/promises';

const html = await readFile(new URL('../../../../apps/backend/internal/widgets/schedules.html', import.meta.url), 'utf8');

async function mount(page, state) {
  await page.goto('/');
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
  await page.setContent(html);
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
