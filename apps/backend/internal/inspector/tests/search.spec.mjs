import {
  test,
  expect,
} from "../../../../../docs/node_modules/@playwright/test/index.mjs";

const activeReport = {
  runtime: {
    provider: "exa",
    enabled: true,
    monthly_budget_usd: 10,
    rate_limit_qps: 10,
    max_results_per_call: 10,
    default_num_results: 5,
    allowed_modes: ["auto", "lite"],
    summary_disabled: true,
    billing_url: "https://dashboard.exa.ai/billing",
    last_rate_limit_at: "2026-11-24T11:30:00Z",
    last_retry_after_seconds: 3,
  },
  month: "2026-11",
  period_start: "2026-11-01T00:00:00Z",
  period_end: "2026-12-01T00:00:00Z",
  reset_at: "2026-12-01T00:00:00Z",
  is_current_month: true,
  available_months: ["2026-11", "2026-10"],
  budget: {
    monthly_budget_usd: 10,
    spent_usd: 3.42,
    remaining_usd: 6.58,
    usage_percent: 34.2,
    status: "ok",
    projected_month_end_usd: 4.1,
    avg_daily_usd: 0.1368,
    est_remaining_auto: 940,
    est_remaining_lite: 1645,
  },
  totals: {
    total_calls: 510,
    succeeded_calls: 508,
    error_calls: 2,
    quota_402_calls: 0,
    rate_limit_429_calls: 2,
    other_error_calls: 0,
    avg_results_per_call: 5.2,
  },
  by_mode: [
    { mode: "auto", calls: 440, cost_usd: 3.08, avg_results: 5.3 },
    { mode: "lite", calls: 70, cost_usd: 0.34, avg_results: 4.8 },
  ],
  by_trigger: [
    { trigger: "message", calls: 450, cost_usd: 3.02, share_percent: 88.2 },
    { trigger: "task", calls: 55, cost_usd: 0.37, share_percent: 10.8 },
    { trigger: "initiative", calls: 5, cost_usd: 0.03, share_percent: 1.0 },
    { trigger: "coding", calls: 0, cost_usd: 0, share_percent: 0 },
  ],
  daily: [
    { date: "2026-11-23", total: 18, auto: 15, lite: 3, other: 0, errors: 0, quota_402: 0, rate_limit_429: 0, cost_usd: 0.117 },
    { date: "2026-11-24", total: 22, auto: 17, lite: 4, other: 0, errors: 1, quota_402: 0, rate_limit_429: 1, cost_usd: 0.142 },
    { date: "2026-11-25", total: 12, auto: 9, lite: 2, other: 0, errors: 1, quota_402: 0, rate_limit_429: 1, cost_usd: 0.078 },
  ],
  recent_errors: [
    {
      trace_id: "trace-429",
      session_id: "sess-1",
      started_at: "2026-11-24T11:30:00Z",
      provider: "exa",
      mode: "auto",
      trigger: "message",
      http_status: 429,
    },
  ],
  previous_month: {
    month: "2026-10",
    total_calls: 210,
    spent_usd: 1.47,
  },
};

const previousReport = {
  ...activeReport,
  month: "2026-10",
  period_start: "2026-10-01T00:00:00Z",
  period_end: "2026-11-01T00:00:00Z",
  reset_at: "2026-11-01T00:00:00Z",
  is_current_month: false,
  budget: {
    monthly_budget_usd: 10,
    spent_usd: 1.47,
    remaining_usd: 8.53,
    usage_percent: 14.7,
    status: "ok",
    avg_daily_usd: 0.0474,
    est_remaining_auto: 1218,
    est_remaining_lite: 2132,
  },
  totals: {
    total_calls: 210,
    succeeded_calls: 210,
    error_calls: 0,
    quota_402_calls: 0,
    rate_limit_429_calls: 0,
    other_error_calls: 0,
    avg_results_per_call: 5.0,
  },
  recent_errors: [],
  previous_month: undefined,
};

const exhaustedReport = {
  ...activeReport,
  runtime: {
    ...activeReport.runtime,
    last_quota_error_at: "2026-11-25T10:00:00Z",
  },
  budget: {
    ...activeReport.budget,
    spent_usd: 10,
    remaining_usd: 0,
    usage_percent: 100,
    status: "exhausted",
    exhausted_by_402: true,
    est_remaining_auto: 0,
    est_remaining_lite: 0,
  },
  totals: {
    ...activeReport.totals,
    quota_402_calls: 1,
    error_calls: 3,
  },
  recent_errors: [
    {
      trace_id: "trace-402",
      session_id: "sess-1",
      started_at: "2026-11-25T10:00:00Z",
      provider: "exa",
      mode: "auto",
      trigger: "message",
      http_status: 402,
    },
  ],
};

const disabledReport = {
  runtime: { provider: "", enabled: false },
  month: "2026-11",
  period_start: "2026-11-01T00:00:00Z",
  period_end: "2026-12-01T00:00:00Z",
  reset_at: "2026-12-01T00:00:00Z",
  is_current_month: true,
  available_months: ["2026-11", "2026-10"],
  budget: {
    monthly_budget_usd: 0,
    spent_usd: 0,
    remaining_usd: 0,
    usage_percent: 0,
    status: "disabled",
    avg_daily_usd: 0,
    est_remaining_auto: 0,
    est_remaining_lite: 0,
  },
  totals: {
    total_calls: 0,
    succeeded_calls: 0,
    error_calls: 0,
    quota_402_calls: 0,
    rate_limit_429_calls: 0,
    other_error_calls: 0,
    avg_results_per_call: 0,
  },
  by_mode: [],
  by_trigger: [],
  daily: [],
  recent_errors: [],
};

async function mountRoutes(page, { current = activeReport, previous = previousReport, onLimitReq } = {}) {
  await page.route("**/api/inspect/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/inspect/sessions") {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify({ sessions: [], next_before: 0 }) });
      return;
    }
    if (url.pathname === "/api/inspect/search/limits") {
      const month = url.searchParams.get("month") || "";
      onLimitReq?.(month);
      const body = month === "2026-10" ? previous : current;
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(body) });
      return;
    }
    await route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ error: "Not Found" }) });
  });
}

test("search limits dashboard renders Exa budget, guardrails, chart, and keyboard shortcuts", async ({ page }) => {
  const seenMonths = [];
  await mountRoutes(page, { onLimitReq: (m) => seenMonths.push(m) });

  await page.goto("/inspect/");
  await page.locator("#nav-search").click();
  await expect(page).toHaveURL(/#\/search$/);
  await expect(page.locator("#nav-search")).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#search-provider-badge")).toHaveText("Exa Search");
  await expect(page.locator("#search-billing-link")).toHaveAttribute("href", "https://dashboard.exa.ai/billing");
  await expect(page.locator("#search-limits-body")).toContainText("$3.42 / $10.00");
  await expect(page.locator("#search-limits-body")).toContainText("~940 auto");
  await expect(page.locator("#search-limits-body")).toContainText("Rate limit: 10 QPS");
  await expect(page.locator("#search-limits-body")).toContainText("HTTP 429");
  await expect(page.locator(".growth-chart rect")).toHaveCount(8);
  await expect(page.locator('a[href="#/session/sess-1?trace=trace-429"]')).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("search-limits-desktop.png"), fullPage: true });

  // Narrow viewport must not horizontally overflow
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("search-limits-mobile.png"), fullPage: true });
  await page.setViewportSize({ width: 1280, height: 800 });

  // Alt+2 from inside the report switches to previous month AND focuses the selected tab
  // so ArrowLeft works immediately without manually focusing the tab (R3 #5).
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-labelledby", "search-tab-current");
  await page.locator('[data-search-focus="billing-footnote"]').focus();
  await page.keyboard.press("Alt+Digit2");
  await expect(page.locator("#search-tab-previous")).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#search-tab-previous")).toBeFocused();
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-labelledby", "search-tab-previous");
  await expect(page.locator("#search-limits-body")).toContainText("$1.47 / $10.00");
  await expect(page).toHaveURL(/#\/search\?month=2026-10$/);

  // Immediate ArrowLeft switches back to current month without manual re-focus
  await page.keyboard.press("ArrowLeft");
  await expect(page.locator("#search-tab-current")).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#search-tab-current")).toBeFocused();
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-labelledby", "search-tab-current");
  await expect(page.locator("#search-limits-body")).toContainText("$3.42 / $10.00");

  // Alt+R refreshes active search dashboard while preserving open table and focus
  const dailySummary = page.locator("#search-daily-table > summary");
  await dailySummary.click();
  await expect(page.locator("#search-daily-table")).toHaveAttribute("open", "");
  await dailySummary.focus();
  const beforeCount = seenMonths.length;
  await page.keyboard.press("Alt+KeyR");
  await expect.poll(() => seenMonths.length).toBeGreaterThan(beforeCount);
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#search-daily-table")).toHaveAttribute("open", "");
  await expect(dailySummary).toBeFocused();

  // Alt+R also preserves focus on the footnote billing link
  const footnoteBilling = page.locator('[data-search-focus="billing-footnote"]');
  await footnoteBilling.focus();
  const beforeFootnoteRefresh = seenMonths.length;
  await page.keyboard.press("Alt+KeyR");
  await expect.poll(() => seenMonths.length).toBeGreaterThan(beforeFootnoteRefresh);
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-busy", "false");
  await expect(footnoteBilling).toBeFocused();
});

test("search limits dashboard flags HTTP 402 exhaustion, local budget threshold, post-restart 429 card, period switch errors, and disabled provider cleanly", async ({ page }) => {
  await mountRoutes(page, { current: exhaustedReport });
  await page.goto("/inspect/#/search");
  const alert = page.locator(".search-alert-banner");
  await expect(alert).toBeVisible();
  await expect(alert).toContainText("HTTP 402 Payment Required");
  await expect(page.locator('a[href="#/session/sess-1?trace=trace-402"]')).toBeVisible();

  // Local EXA_MONTHLY_BUDGET_USD threshold without HTTP 402 shows warning, not automatic fallback claim;
  // Alt+R preserves focus on the threshold-warning billing link.
  const localThresholdReport = {
    ...activeReport,
    runtime: {
      ...activeReport.runtime,
      last_rate_limit_at: undefined,
      last_retry_after_seconds: 0,
    },
    budget: {
      ...activeReport.budget,
      spent_usd: 10.12,
      remaining_usd: 0,
      usage_percent: 100,
      status: "exhausted",
      exhausted_by_402: false,
      est_remaining_auto: 0,
      est_remaining_lite: 0,
    },
    totals: {
      ...activeReport.totals,
      error_calls: 0,
      rate_limit_429_calls: 1,
    },
  };
  await page.unroute("**/api/inspect/**");
  await mountRoutes(page, { current: localThresholdReport });
  await page.locator("#refresh").click();
  await expect(page.locator('.search-alert-banner[role="alert"]')).toHaveCount(0);
  const warning = page.locator('.search-alert-banner.search-alert-warning[role="status"]');
  await expect(warning).toBeVisible();
  await expect(warning).toContainText("EXA_MONTHLY_BUDGET_USD");
  await expect(warning).not.toContainText("автоматически переключается");
  // Post-restart persisted recovered 429 must not claim "Срабатываний 402/429 не было"
  await expect(page.locator("#search-limits-body")).toContainText("402: 0 · 429: 1");
  await expect(page.locator("#search-limits-body")).not.toContainText("Срабатываний 402/429 не было");
  const warningBilling = page.locator('[data-search-focus="billing-warning"]');
  await warningBilling.focus();
  await page.keyboard.press("Alt+KeyR");
  await expect(page.locator("#search-limits-body")).toHaveAttribute("aria-busy", "false");
  await expect(warningBilling).toBeFocused();

  // Failed period switch must clear stale report AND toolbar period metadata
  await page.unroute("**/api/inspect/**");
  await page.route("**/api/inspect/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/inspect/search/limits" && url.searchParams.get("month") === "2026-10") {
      await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "database unavailable" }) });
      return;
    }
    if (url.pathname === "/api/inspect/search/limits") {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(activeReport) });
      return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify({ sessions: [], next_before: 0 }) });
  });
  await page.locator("#refresh").click();
  await expect(page.locator("#search-limits-body")).toContainText("$3.42 / $10.00");
  await expect(page.locator("#search-period-meta")).toContainText("ноябрь 2026");
  await page.locator("#search-tab-previous").click();
  await expect(page.locator("#search-tab-previous")).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#search-limits-body")).not.toContainText("$3.42 / $10.00");
  await expect(page.locator("#search-period-meta")).toHaveText("");
  await expect(page.locator("#search-limits-body")).toContainText("Не удалось загрузить статистику поиска");

  await page.unroute("**/api/inspect/**");
  await mountRoutes(page, { current: disabledReport });
  await page.locator("#search-tab-current").click();
  await expect(page.locator("#search-provider-badge")).toHaveText("Поиск отключён");
  await expect(page.locator("#search-limits-body")).toContainText("EXA_API_KEY");
});

test("search limits dashboard clears loading badge on initial failure and restores on refresh", async ({ page }) => {
  let failInitial = true;
  await page.route("**/api/inspect/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/inspect/search/limits") {
      if (failInitial) {
        await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "unavailable" }) });
        return;
      }
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(activeReport) });
      return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify({ sessions: [], next_before: 0 }) });
  });

  await page.goto("/inspect/#/search");
  await expect(page.locator("#search-provider-badge")).toHaveText("Статус неизвестен");
  await expect(page.locator("#search-limits-body")).toContainText("Не удалось загрузить статистику поиска");

  failInitial = false;
  await page.locator("#refresh").click();
  await expect(page.locator("#search-provider-badge")).toHaveText("Exa Search");
  await expect(page.locator("#search-limits-body")).toContainText("$3.42 / $10.00");
});
