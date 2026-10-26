import { test, expect } from '@playwright/test';
import { navigation } from '../../build.mjs';

const pageIds = navigation.map(([id]) => id);

test('все страницы читаются, схемы загружаются, внутренние ссылки работают', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto('/');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Архитектура OpenAlbert');
  await expect(page).toHaveTitle(/OpenAlbert/);
  const brand = page.getByRole('link', { name: 'OpenAlbert — обзор' });
  await expect(brand).toContainText('OpenAlbert');
  expect(await brand.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);
  for (const id of pageIds) {
    await page.goto(`/#/${id}`);
    await expect(page.locator('.page-nav a[aria-current="page"]').first()).toHaveAttribute('href', `#/${id}`);
    await page.locator('article img').evaluateAll(async (images) => {
      images.forEach((image) => { image.loading = 'eager'; });
      await Promise.all(images.map((image) => image.decode()));
      if (images.some((image) => !image.naturalWidth)) throw new Error('Broken diagram');
    });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
  await page.goto('/#/sessions');
  await page.getByRole('link', { name: 'Закрытие сессии', exact: true }).click();
  await expect(page).toHaveURL(/sessions\/закрытие-сессии|sessions\/%D0/i);
  await expect(page.locator('#закрытие-сессии')).toBeFocused();
  expect(errors).toEqual([]);
});

test('клавиатура: поиск, результат, схема, масштаб и возврат фокуса', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: /Поиск по документации/ }).focus();
  await page.keyboard.press('Meta+k');
  await expect(page.locator('#search-dialog')).toBeVisible();
  await expect(page.locator('#search-input')).toBeFocused();
  await page.locator('#search-input').fill('SSH');
  await expect(page.locator('[data-result]').first()).toBeVisible();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(page.locator('#search-dialog')).not.toBeVisible();
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
  await page.goto('/#/architecture');
  const diagram = page.getByRole('button', { name: /Увеличить схему/ }).first();
  await diagram.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#diagram-dialog')).toBeVisible();
  await page.keyboard.press('+');
  await expect(page.locator('#zoom-level')).toHaveText('125%');
  await page.keyboard.press('0');
  await expect(page.locator('#zoom-level')).toHaveText('100%');
  await page.keyboard.press('Meta+k');
  await expect(page.locator('#search-dialog')).not.toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#diagram-dialog')).not.toBeVisible();
  await expect(diagram).toBeFocused();
});

test('поиск сохраняет запрос, имеет пустое состояние и учитывает IME и повтор', async ({ page }) => {
  await page.goto('/');
  await page.locator('[data-action="search"]').click();
  const input = page.locator('#search-input');
  await input.fill('совсемнесуществующийраздел');
  await expect(page.getByText('Ничего не найдено')).toBeVisible();
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+k');
  await expect(input).toHaveValue('совсемнесуществующийраздел');
  await input.fill('Temporal');
  await input.dispatchEvent('keydown', { code: 'Enter', key: 'Enter', isComposing: true, bubbles: true });
  await expect(page.locator('#search-dialog')).toBeVisible();
  await input.dispatchEvent('keydown', { code: 'Enter', key: 'Enter', repeat: true, bubbles: true });
  await expect(page.locator('#search-dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await page.locator('body').dispatchEvent('keydown', { code: 'KeyK', key: 'л', metaKey: true, bubbles: true });
  await expect(page.locator('#search-dialog')).toBeVisible();
  await expect(input).toHaveValue('Temporal');
});

test('узкий экран: меню, страницы и таблицы не растягивают документ', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  await page.getByRole('button', { name: 'Открыть разделы', exact: true }).click();
  await expect(page.locator('#menu-dialog')).toBeVisible();
  await page.locator('#menu-dialog').getByRole('link', { name: 'Инструменты', exact: true }).click();
  await expect(page.locator('#menu-dialog')).not.toBeVisible();
  await expect(page.getByRole('heading', { name: 'Начальные инструменты', exact: true })).toBeVisible();
  for (const id of pageIds) {
    await page.goto(`/#/${id}`);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
});

test('панель T3: при промежуточной ширине текст использует всю область', async ({ page }) => {
  for (const width of [360, 560, 780, 1024, 1280]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/#/architecture');
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Открыть разделы', exact: true })).toBeVisible();
    const dimensions = await page.evaluate(() => ({
      width: innerWidth,
      content: document.querySelector('main').clientWidth,
      sidebar: getComputedStyle(document.querySelector('.sidebar')).display,
      overflow: document.documentElement.scrollWidth > innerWidth,
    }));
    expect(dimensions.sidebar).toBe('none');
    expect(dimensions.content).toBeGreaterThan(Math.min(width - 80, 820));
    expect(dimensions.overflow).toBe(false);
    await page.getByRole('button', { name: 'Оглавление текущей страницы' }).click();
    await page.locator('#toc-dialog').getByRole('link', { name: 'Граница Computer box', exact: true }).click();
    await expect(page.locator('#toc-dialog')).not.toBeVisible();
    await expect(page.locator('#граница-computer-box')).toBeFocused();
  }
});

test('все схемы открываются в максимально вписанном размере, включая основной экран', async ({ page }) => {
  for (const viewport of [{ width: 1280, height: 900 }, { width: 560, height: 780 }]) {
    await page.setViewportSize(viewport);
    for (const id of pageIds) {
      await page.goto(`/#/${id}`);
      await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
      const diagrams = page.locator('article [data-diagram]');
      for (let index = 0; index < await diagrams.count(); index++) {
        const src = await diagrams.nth(index).getAttribute('data-diagram');
        const source = await (await page.request.get(src)).text();
        const viewBox = source.match(/\bviewBox="([^\"]+)"/)[1].split(/\s+/).map(Number);
        await diagrams.nth(index).click();
        await expect(page.locator('#diagram-dialog')).toBeVisible();
        const size = await page.evaluate(() => {
          const stage = document.querySelector('#diagram-stage');
          const image = document.querySelector('#diagram-image');
          const css = getComputedStyle(stage);
          // HTMLImageElement.width/height round to integers, which distorts the
          // measured aspect ratio of short diagrams. Assert actual CSS bounds.
          const bounds = image.getBoundingClientRect();
          return { imageWidth: bounds.width, imageHeight: bounds.height,
            availableWidth: stage.clientWidth - parseFloat(css.paddingLeft) - parseFloat(css.paddingRight),
            availableHeight: stage.clientHeight - parseFloat(css.paddingTop) - parseFloat(css.paddingBottom) };
        });
        expect(size.imageWidth).toBeLessThanOrEqual(size.availableWidth + 2);
        expect(size.imageHeight).toBeLessThanOrEqual(size.availableHeight + 2);
        expect(size.availableHeight - size.imageHeight).toBeLessThan(4);
        expect(size.imageWidth / size.imageHeight).toBeCloseTo(viewBox[2] / viewBox[3], 2);
        expect(Math.max(size.imageWidth / size.availableWidth, size.imageHeight / size.availableHeight)).toBeGreaterThan(0.97);
        await page.keyboard.press('w');
        const fitWidth = await page.locator('#diagram-image').evaluate((image) => image.getBoundingClientRect().width);
        expect(fitWidth).toBeCloseTo(size.availableWidth, -1);
        await page.keyboard.press('Escape');
      }
    }
  }
});
