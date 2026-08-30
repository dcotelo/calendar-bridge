// Captures the documentation screenshots of the calendar-bridge web UI.
//
// Run it through capture.sh, which builds the binary, generates a synthetic
// fixture, and starts the server. This file only drives the browser.
//
// Every state is produced from synthetic data. Two of them (a pass that just
// ran, and an expired token) are states the daemon reaches but the standalone
// `ui` subcommand cannot, so they are rendered by calling the page's own
// renderStatus() with a synthetic payload — the same function the live
// /api/status response goes through, so the screenshot shows the real
// rendering path rather than a mock-up.

import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";

const BASE = process.env.CB_UI_URL ?? "http://127.0.0.1:8791";
const OUT = process.env.CB_SCREENSHOT_DIR ?? "docs/assets/screenshots";

// A fixed viewport and scale factor, so a re-run produces a byte-comparable
// image rather than one that depends on the machine that took it.
const VIEWPORT = { width: 1440, height: 900 };
const SCALE = 2;

/** A pass that just completed successfully. */
const HEALTHY_STATUS = {
  running: true,
  push_enabled: false,
  accounts: 3,
  last_sync: null, // filled in relative to a pinned clock below
  last_attempt: null,
  last_duration_ms: 1840,
  created: 6,
  updated: 2,
  deleted: 1,
  skipped: 4,
  account_status: [
    { name: "personal", healthy: true },
    { name: "work-acme", healthy: true },
    { name: "work-globex", healthy: true },
  ],
};

/** One account's token has expired. */
const EXPIRED_TOKEN_STATUS = {
  running: true,
  push_enabled: false,
  accounts: 3,
  last_sync: null,
  last_attempt: null,
  last_duration_ms: 920,
  created: 0,
  updated: 0,
  deleted: 0,
  skipped: 0,
  account_status: [
    { name: "personal", healthy: true },
    { name: "work-acme", healthy: true },
    { name: "work-globex", healthy: false },
  ],
  last_error:
    "listing events for account work-globex: oauth2: cannot fetch token: 400 Bad Request invalid_grant",
};

async function shoot(page, name, scheme) {
  const file = `${OUT}/${name}-${scheme}.png`;
  await page.screenshot({ path: file, animations: "disabled" });
  console.log(`  wrote ${file}`);
}

/** Render a status payload through the page's own renderStatus(). */
async function setStatus(page, status, minutesAgo) {
  await page.evaluate(
    ({ status, minutesAgo }) => {
      const t = new Date(Date.now() - minutesAgo * 60_000).toISOString();
      // eslint-disable-next-line no-undef
      renderStatus({ ...status, last_sync: t, last_attempt: t });
    },
    { status, minutesAgo },
  );
  // The relative timestamp ("2 min ago") is computed at render time; give the
  // DOM a tick to settle before capturing.
  await page.waitForTimeout(50);
}

async function captureScheme(browser, scheme) {
  console.log(`capturing ${scheme} scheme…`);
  const context = await browser.newContext({
    viewport: VIEWPORT,
    deviceScaleFactor: SCALE,
    colorScheme: scheme,
    reducedMotion: "reduce",
    // Pin the locale and timezone so the rendered timestamps don't shift with
    // the machine taking the screenshot.
    locale: "en-GB",
    timezoneId: "UTC",
  });
  const page = await context.newPage();

  page.on("pageerror", (err) => {
    console.error(`  PAGE ERROR: ${err.message}`);
    process.exitCode = 1;
  });

  await page.goto(BASE, { waitUntil: "networkidle" });

  // 1. Dashboard as a freshly-started `ui` process sees it.
  await shoot(page, "dashboard", scheme);

  // 2. A pass that just ran, with the blocks it wrote.
  await setStatus(page, HEALTHY_STATUS, 2);
  await shoot(page, "sync-complete", scheme);

  // 3. An expired token on one account.
  await setStatus(page, EXPIRED_TOKEN_STATUS, 41);
  await shoot(page, "error-expired-token", scheme);

  // 4. The accounts list.
  await setStatus(page, HEALTHY_STATUS, 2);
  await page.locator("#accounts").scrollIntoViewIfNeeded();
  await shoot(page, "accounts", scheme);

  // 5. The add-account form, with a new empty row focused.
  await page.locator("#addAcct").click();
  await page.locator("#accounts .acct").last().scrollIntoViewIfNeeded();
  await shoot(page, "add-account", scheme);

  // 6. Sync settings.
  await page.locator("#pollInterval").scrollIntoViewIfNeeded();
  await shoot(page, "settings", scheme);

  // 7. Field-level validation, which is what a mistyped value actually looks
  //    like. Uses the real submit path, not a forced class.
  await page.locator("#pollInterval").fill("5 minutes");
  await page.locator("#saveBtn").click();
  await page.waitForSelector("#pollInterval-err:not([hidden])");
  await page.locator("#pollInterval").scrollIntoViewIfNeeded();
  await shoot(page, "validation-error", scheme);

  // 8. Narrow viewport, to show the layout holds up on a phone.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload({ waitUntil: "networkidle" });
  // Chrome restores the previous scroll position across a reload, which would
  // capture whatever the last step had scrolled to rather than the top.
  await page.evaluate(() => window.scrollTo(0, 0));
  await setStatus(page, HEALTHY_STATUS, 2);
  await page.waitForTimeout(50);
  await shoot(page, "mobile", scheme);

  await context.close();
}

const browser = await chromium.launch();
await mkdir(OUT, { recursive: true });
try {
  for (const scheme of ["dark", "light"]) {
    await captureScheme(browser, scheme);
  }
} finally {
  await browser.close();
}
console.log("done.");
