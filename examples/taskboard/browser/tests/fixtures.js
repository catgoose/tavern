import { test as base, expect } from "@playwright/test";

// openPage opens a page in its own browser context and records anything
// that went wrong on it: page errors, console errors, failed or 4xx/5xx
// responses, and requests that tried to leave the test server.
async function openPage(browser, baseURL) {
  const context = await browser.newContext({ baseURL });
  const problems = [];
  const origin = new URL(baseURL).origin;
  await context.route("**/*", (route) => {
    const url = route.request().url();
    if (new URL(url).origin === origin) return route.continue();
    problems.push(`external request: ${url}`);
    return route.abort();
  });
  const page = await context.newPage();
  page.on("pageerror", (err) => problems.push(`page error: ${err.message}`));
  page.on("console", (msg) => {
    if (msg.type() === "error") problems.push(`console error: ${msg.text()}`);
  });
  page.on("response", (res) => {
    if (res.status() >= 400) problems.push(`${res.status()} ${res.url()}`);
  });
  page.on("requestfailed", (req) => {
    // Event streams are cut when the page closes or the demo drops them.
    if (req.resourceType() === "eventsource") return;
    problems.push(`request failed: ${req.url()} ${req.failure()?.errorText}`);
  });
  return { context, page, problems };
}

export const test = base.extend({
  baseURL: async ({}, use) => use(process.env.TASKBOARD_URL),
  // viewer opens a page that fails the test if anything went wrong on it.
  // expected lists problems the test causes on purpose, as exact strings or
  // regular expressions; each must occur.
  viewer: async ({ browser, baseURL }, use) => {
    const opened = [];
    await use(async ({ expected = [] } = {}) => {
      const v = await openPage(browser, baseURL);
      opened.push({ ...v, expected });
      return v.page;
    });
    for (const v of opened) {
      await v.context.close();
      const unexpected = [...v.problems];
      for (const e of v.expected) {
        const i = unexpected.findIndex((p) => (e instanceof RegExp ? e.test(p) : p === e));
        expect(i, `expected problem: ${e}`).toBeGreaterThanOrEqual(0);
        unexpected.splice(i, 1);
      }
      expect(unexpected, "problems on the page").toEqual([]);
    }
  },
});

export { expect };
