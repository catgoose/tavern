import { test, expect } from "./fixtures.js";

// htmx logs the htmx:sseError event each time a stream ends.
const SSE_ERROR = "console error: [object Event]";
const sseErrors = (n) => Array(n).fill(SSE_ERROR);

async function openScenario(page, name, action) {
  await page.goto(`/showcase/recovery/${name}${action ? `?action=${action}` : ""}`);
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await expect(page.locator("#recovery-value, #recovery-left").first()).not.toHaveText("…");
  return page;
}

const serverValues = (page, name) =>
  page.evaluate((n) => fetch(`/showcase/recovery/${n}/state`).then((r) => r.json()).then((s) => s.values), name);

async function serverValue(page, name) {
  const values = await serverValues(page, name);
  return String(Object.values(values)[0]);
}

const events = (page) => page.locator("#recovery-events");
const button = (page, name) => page.getByRole("button", { name, exact: true });
const reconnects = (page) => events(page).locator("li", { hasText: "tavern:reconnected" }).count();

// reconnected runs action and waits for one more server-confirmed resume.
async function reconnected(page, action) {
  const before = await reconnects(page);
  await action();
  await expect.poll(() => reconnects(page)).toBeGreaterThan(before);
}

// converges waits until the window shows the server's current value.
async function converges(page, name) {
  await expect(async () => {
    expect(await page.locator("#recovery-value").textContent()).toBe(await serverValue(page, name));
  }).toPass();
}

async function serverSaw(page, text) {
  await expect(async () => {
    await button(page, "Show what the server saw").click();
    await expect(page.locator("#recovery-observations")).toContainText(text, { timeout: 500 });
  }).toPass();
}

test("a short disconnect resumes with Last-Event-ID and the server confirms it", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(1) }), "resume");
  await button(page, "Disconnect and miss 3 changes").click();
  await expect(events(page)).toContainText('tavern:reconnected {"replayDelivered":3,"replayDropped":0}');
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(page, "resume");
  await expect(events(page)).not.toContainText("tavern:replay-gap");
  await serverSaw(page, "3 missed, 3 replayed, 0 dropped");
});

test("an absence beyond retention is a gap answered with a snapshot", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(1) }), "resume");
  await page.evaluate(() => {
    const region = document.getElementById("recovery-region");
    window.__states = [];
    new MutationObserver(() => {
      const state = ["is-live", "is-stale", "is-reconnecting"].filter((c) => region.classList.contains(c)).join(" ") || "none";
      if (window.__states[window.__states.length - 1] !== state) window.__states.push(state);
    }).observe(region, { attributes: true, attributeFilter: ["class"] });
  });
  await button(page, "Disconnect and miss 8 changes").click();
  await expect(events(page)).toContainText("tavern:replay-gap");
  await expect(events(page)).toContainText("tavern:stale");
  await expect(events(page)).toContainText("tavern:reconnected");
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(page, "resume");
  await serverSaw(page, "is no longer kept");
  // tavern-js adds the stale class alongside the reconnecting one.
  const states = await page.evaluate(() => window.__states);
  expect(states).toEqual(["is-reconnecting", "is-stale is-reconnecting", "is-live"]);
  await expect(page.locator("#recovery-region [tavern-status-stale]")).toBeHidden();
});

test("a silent gap goes live with an old value until the next change", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(1) }), "resume-silent");
  const before = await page.locator("#recovery-value").textContent();
  await button(page, "Disconnect and miss 8 changes").click();
  await expect(events(page)).toContainText("tavern:reconnected");
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await expect(events(page)).not.toContainText("tavern:replay-gap");
  await page.waitForTimeout(300);
  await expect(page.locator("#recovery-value")).toHaveText(before);
  expect(Number(await serverValue(page, "resume-silent"))).toBeGreaterThanOrEqual(Number(before) + 8);
  await serverSaw(page, "is no longer kept"); // only the server knows

  await button(page, "Change").click();
  await converges(page, "resume-silent");
});

test("gap actions: banner, custom event and reload all end on the current value", async ({ viewer }) => {
  const banner = await openScenario(await viewer({ expected: sseErrors(1) }), "resume", "banner");
  await button(banner, "Disconnect and miss 8 changes").click();
  await expect(banner.locator("[tavern-gap-banner]")).toBeVisible();
  await expect(banner.locator("[tavern-gap-banner] [role=alert]")).toHaveText("Some updates were missed while this window was away.");
  await expect(banner.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(banner, "resume");

  const custom = await openScenario(await viewer({ expected: sseErrors(1) }), "resume", "recovery-gap");
  await button(custom, "Disconnect and miss 8 changes").click();
  await expect(events(custom)).toContainText(/custom handler: (applied|fetched) revision \d+/);
  await expect(custom.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(custom, "resume");

  const reload = await viewer({ expected: sseErrors(1) });
  let loads = 0;
  reload.on("load", () => loads++);
  await openScenario(reload, "resume", "reload");
  await button(reload, "Disconnect and miss 8 changes").click();
  await expect.poll(() => loads).toBe(2);
  await expect(reload.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(reload, "resume");
  await reload.waitForTimeout(1000);
  expect(loads, "the fresh page starts from a snapshot, so it does not reload again").toBe(2);
});

test("two topics resume from one Last-Event-ID", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(1) }), "multi");
  await button(page, "Disconnect and miss 3 changes").click();
  await expect(events(page)).toContainText("tavern:reconnected");
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await expect(async () => {
    const values = await serverValues(page, "multi");
    expect(await page.locator("#recovery-left").textContent()).toBe(String(values["recovery/left"]));
    expect(await page.locator("#recovery-right").textContent()).toBe(String(values["recovery/right"]));
  }).toPass();
  await serverSaw(page, "recovery/right: resumed");
});

test("a truncated replay is repaired, and only a real resume is confirmed", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(2) }), "truncation");
  await button(page, "Disconnect and miss 10 changes").click();
  await serverSaw(page, "10 missed, 4 replayed, 6 dropped");
  await converges(page, "truncation");
  await expect(page.locator("#recovery-region")).not.toHaveClass(/\bis-live\b/);
  await expect(events(page)).not.toContainText("tavern:reconnected");

  await button(page, "Reconnect now").click();
  await expect(events(page)).toContainText('tavern:reconnected {"replayDelivered":0,"replayDropped":0}');
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(page, "truncation");
});

// overflow holds the window back while changes fill its queue, then lets
// it go.
async function overflow(page) {
  await button(page, "Hold back this window").click();
  await button(page, "Make 10 changes").click();
  await button(page, "Make 10 changes").click();
  await button(page, "Release it").click();
}

for (const name of ["drop-newest", "drop-oldest"]) {
  test(`full queue (${name}) ends on the current value`, async ({ viewer }) => {
    const page = await openScenario(await viewer(), name);
    await overflow(page);
    await converges(page, name);
    await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  });
}

for (const [name, reason] of [["evict", "evicted"], ["loss-aware", "queue-loss"]]) {
  test(`full queue (${name}) ends the stream and the reconnect is current`, async ({ viewer }) => {
    const page = await openScenario(await viewer({ expected: sseErrors(1) }), name);
    await overflow(page);
    await serverSaw(page, `stream ended: ${reason}`);
    await expect(events(page)).toContainText("tavern:disconnected");
    await converges(page, name);
  });
}

test("replay store failures: a failed read falls back to a snapshot; strict mode ends the replay", async ({ viewer }) => {
  test.setTimeout(60_000);
  const page = await openScenario(await viewer({ expected: sseErrors(5) }), "store");
  try {
    await button(page, "Fail reads").click();
    await button(page, "Disconnect and miss 3 changes").click();
    await expect(events(page)).toContainText("tavern:replay-gap");
    await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
    await converges(page, "store");
    await serverSaw(page, "replay store after-id on recovery/stored failed");
    await button(page, "Repair the store").click();

    // Failed appends: the resume looks complete and the window stays behind.
    await button(page, "Fail appends").click();
    await reconnected(page, () => button(page, "Disconnect and miss 3 changes").click());
    await serverSaw(page, "replay store append on recovery/stored failed");
    const behind = await page.locator("#recovery-value").textContent();
    expect(Number(await serverValue(page, "store"))).toBeGreaterThan(Number(behind));
    // The same window recovers with nothing more changed: clear the
    // history, and its next resume is a gap answered with a snapshot.
    await button(page, "Repair the store").click();
    const canonical = await serverValue(page, "store");
    await button(page, "Clear replay history").click();
    await reconnected(page, () => button(page, "Reconnect now").click());
    await expect(page.locator("#recovery-value")).toHaveText(canonical);
    await page.waitForTimeout(300);
    await expect(page.locator("#recovery-value")).toHaveText(canonical);
    expect(await serverValue(page, "store")).toBe(canonical);
    await button(page, "Fail reads").click();

    const strict = await openScenario(page, "store-strict");
    await button(strict, "Disconnect and miss 3 changes").click();
    await serverSaw(strict, "replay failed; continuing from a snapshot");
    await converges(strict, "store-strict");
    await expect(strict.locator("#recovery-region")).not.toHaveClass(/\bis-live\b/);

    await button(strict, "Repair the store").click();
    await button(strict, "Reconnect now").click();
    await expect(events(strict)).toContainText("tavern:reconnected");
    await expect(strict.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
    await converges(strict, "store-strict");
  } finally {
    await page.evaluate(() => fetch("/showcase/recovery/store/fault?append=0&read=0", { method: "POST" }));
  }
});

// holdState holds the custom gap handler's state fetches: each is sent to
// the server when made, and its response is kept until released.
async function holdState(page, name) {
  const held = [];
  let arrived;
  await page.route(new RegExp(`/showcase/recovery/${name}/state\\?conn=`), async (route) => {
    const response = await route.fetch();
    held.push({ route, response });
    if (arrived) arrived();
  });
  return {
    held,
    next: (n) => expect.poll(() => held.length).toBeGreaterThanOrEqual(n),
    release: async (i) => {
      try {
        await held[i].route.fulfill({ response: held[i].response });
      } catch {
        // the page cancelled this fetch
      }
    },
  };
}

test("a custom gap fetch that returns after a newer update does not replace it", async ({ viewer }) => {
  const page = await openScenario(await viewer({ expected: sseErrors(1) }), "resume", "recovery-gap");
  const state = await holdState(page, "resume");
  await button(page, "Disconnect and miss 8 changes").click();
  await state.next(1); // fetched at the value after the absence
  await expect(page.locator("#recovery-region")).toHaveClass(/\bis-live\b/);
  await converges(page, "resume");

  await button(page, "Change").click(); // the stream delivers a newer value
  await converges(page, "resume");
  const newest = await page.locator("#recovery-value").textContent();
  await state.release(0); // now the older fetch returns
  await expect(events(page)).toContainText(/custom handler: fetched revision \d+, already showing \d+/);
  await page.waitForTimeout(300);
  await expect(page.locator("#recovery-value")).toHaveText(newest);
  expect(await serverValue(page, "resume")).toBe(newest);
});

test("overlapping custom gap fetches: the newer one wins and the older is cancelled", async ({ viewer }) => {
  const page = await openScenario(
    await viewer({ expected: [...sseErrors(2), /request failed: .*\/showcase\/recovery\/resume\/state\?conn=.* net::ERR_ABORTED/] }),
    "resume",
    "recovery-gap",
  );
  const state = await holdState(page, "resume");
  await button(page, "Disconnect and miss 8 changes").click();
  await state.next(1);
  await button(page, "Disconnect and miss 8 changes").click(); // a second gap while the first fetch is out
  await state.next(2);
  await state.release(1);
  await expect(events(page)).toContainText(/custom handler: (applied|fetched) revision \d+/);
  await state.release(0);
  await page.waitForTimeout(300);
  await expect(events(page).locator("li", { hasText: "custom handler:" })).toHaveCount(1);
  await converges(page, "resume");
});

test("a custom gap fetch that keeps failing gives up and leaves the stream in charge", async ({ viewer }) => {
  const page = await openScenario(
    await viewer({
      expected: [
        ...sseErrors(1),
        ...Array(3).fill(/^503 .*\/showcase\/recovery\/resume\/state\?conn=/),
        ...Array(3).fill("console error: Failed to load resource: the server responded with a status of 503 (Service Unavailable)"),
      ],
    }),
    "resume",
    "recovery-gap",
  );
  let calls = 0;
  await page.route(/\/showcase\/recovery\/resume\/state\?conn=/, (route) => {
    calls++;
    return route.fulfill({ status: 503, body: "unavailable" });
  });
  await button(page, "Disconnect and miss 8 changes").click();
  await expect(events(page)).toContainText("custom handler: gave up (HTTP 503)");
  await page.waitForTimeout(1500);
  expect(calls).toBe(3);
  await converges(page, "resume");
});
