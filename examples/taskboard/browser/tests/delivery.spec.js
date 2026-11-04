import { test, expect } from "./fixtures.js";

const SSE_ERROR = "console error: [object Event]";

const state = (page, family) =>
  page.evaluate((f) => fetch(`/showcase/delivery/${f}/state`).then((r) => r.json()), family);
const button = (page, name) => page.getByRole("button", { name, exact: true });
const count = async (page, id) => Number(await page.locator(`[data-count-for="${id}"]`).textContent());

async function open(viewer, family, options) {
  const page = await viewer(options);
  await page.goto(`/showcase/delivery/${family}`);
  await expect(page.locator("[sse-connect]").first()).toBeAttached();
  return page;
}

test("every delivery page loads from the showcase", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/showcase");
  await page.getByRole("link", { name: "Delivery lab" }).click();
  await expect(page).toHaveURL(/\/showcase\/delivery$/);
  for (const name of ["Immediate, debounced and throttled", "Reactive workflow"]) {
    await expect(page.getByRole("link", { name })).toBeVisible();
  }
  await page.getByRole("link", { name: "Scheduled sections" }).click();
  await expect(page.locator("#dash-load")).not.toHaveText("…");
});

test("a burst ends on the same value published at once, debounced and throttled", async ({ viewer }) => {
  const page = await open(viewer, "timing");
  await expect(page.locator("#timing-now output")).toBeAttached();
  const before = { now: await count(page, "timing-now"), deb: await count(page, "timing-debounced"), thr: await count(page, "timing-throttled") };
  await button(page, "Make 20 changes").click();
  await expect.poll(async () => (await state(page, "timing")).running, { timeout: 5000 }).toBe(false);
  const { value } = await state(page, "timing");
  for (const id of ["timing-now", "timing-debounced", "timing-throttled"]) {
    await expect(page.locator(`#${id} output`)).toHaveText(String(value));
  }
  await page.waitForTimeout(700); // past the debounce and throttle intervals
  for (const id of ["timing-now", "timing-debounced", "timing-throttled"]) {
    await expect(page.locator(`#${id} output`)).toHaveText(String(value));
  }
  expect(await count(page, "timing-now")).toBe(before.now + 20);
  expect(await count(page, "timing-debounced")).toBe(before.deb + 1);
  const throttled = (await count(page, "timing-throttled")) - before.thr;
  expect(throttled).toBeGreaterThanOrEqual(2);
  expect(throttled).toBeLessThan(20);
});

test("an expiring notice leaves open windows; later windows never get it", async ({ viewer }) => {
  const a = await open(viewer, "ttl");
  await expect.poll(async () => (await state(a, "ttl")).windowTopics.length).toBeGreaterThan(0);
  await button(a, "Post a notice").click();
  await button(a, "Post a banner").click();
  const notice = a.locator("#ttl-notices li").first();
  await expect(notice).toBeVisible();
  const text = await notice.textContent();
  await expect(a.locator("#ttl-banner")).toHaveText(/^Banner \d+/);
  const banner = await a.locator("#ttl-banner").textContent();

  const b = await open(viewer, "ttl");
  await expect(b.locator("#ttl-notices li", { hasText: text })).toBeVisible(); // from the replay cache
  await expect(b.locator("#ttl-banner")).toHaveText(banner);

  await expect(a.locator("#ttl-notices li", { hasText: text })).toHaveCount(0, { timeout: 5000 });
  await expect(b.locator("#ttl-notices li", { hasText: text })).toHaveCount(0);
  await expect(a.locator("#ttl-banner")).toHaveText(banner, { timeout: 100 }); // no auto-removal

  const c = await open(viewer, "ttl");
  await c.waitForTimeout(500);
  await expect(c.locator("#ttl-notices li", { hasText: text })).toHaveCount(0);
  await expect(c.locator("#ttl-banner")).not.toHaveText(banner);
});

// settledFlow waits until the window shows the model: its rows and the
// summary's count match the server's visible rows.
async function settledFlow(page) {
  await expect(async () => {
    const { visible } = await state(page, "flow");
    expect(await page.locator("#order-rows li").count()).toBe(visible.length);
    await expect(page.locator("#order-summary [data-orders]")).toHaveAttribute("data-orders", String(visible.length), { timeout: 100 });
  }).toPass();
}

test("an order reaches the page through the mutation hook, middleware and a derived summary", async ({ viewer }) => {
  const page = await open(viewer, "flow");
  await expect(page.locator("#order-clock")).toContainText(/worker \d+ at/, { timeout: 5000 });
  await settledFlow(page);
  const rows = page.locator("#order-rows li");
  await button(page, "Order coffee").click();
  const row = rows.first();
  await expect(row).toContainText("checked by orders:*");
  await expect(row).toContainText("coffee");
  await expect(page.locator("#audit-rows li").first()).toContainText("unchecked");
  await settledFlow(page);
  await expect(page.locator("#order-summary")).toContainText("checked by orders:*");
  const { steps } = await state(page, "flow");
  expect(steps["derived summaries"]).toBeGreaterThan(0);
  const shown = await rows.allTextContents();

  await button(page, "Place a test order").click();
  await expect(page.locator("#audit-rows li").first()).toContainText("(test)");
  await settledFlow(page);
  const after = await rows.allTextContents();
  expect(after.some((r) => r.includes("tea") && !shown.includes(r))).toBe(false); // the test row is held back
  expect(after.every((r) => shown.includes(r))).toBe(true);
});

test("slow windows: rate-limited and coalescing end on the latest value, plain falls behind", async ({ viewer }) => {
  const page = await open(viewer, "shaping");
  for (const mode of ["plain", "rate", "coalesce"]) {
    await expect(page.locator(`#shape-${mode}`)).toBeAttached();
  }
  await page.waitForTimeout(300); // all three subscribed
  await button(page, "Publish 50 values").click();
  await expect.poll(async () => (await state(page, "shaping")).running, { timeout: 5000 }).toBe(false);
  const { value } = await state(page, "shaping");
  for (const mode of ["rate", "coalesce"]) {
    await expect(page.locator(`#shape-${mode}`)).toHaveText(String(value));
  }
  await page.waitForTimeout(800);
  await expect(page.locator("#shape-rate")).toHaveText(String(value));
  await expect(page.locator("#shape-coalesce")).toHaveText(String(value));
  await expect(page.locator("#shape-plain")).not.toHaveText(String(value));
  for (const mode of ["plain", "rate", "coalesce"]) {
    const received = Number(await page.locator(`[data-mode="${mode}"] [data-received]`).textContent());
    expect(received).toBeLessThan(50);
  }
});

test("a held-back window is throttled, then recovers to the current value", async ({ viewer }) => {
  const page = await open(viewer, "pressure");
  await expect(page.locator("#gauge [data-value]")).toBeAttached();
  await button(page, "Hold back this window").click();
  await button(page, "Make 10 changes").click();
  await button(page, "Release it").click();
  const { value } = await state(page, "pressure");
  await expect(page.locator("#gauge [data-value]")).toHaveAttribute("data-value", String(value), { timeout: 5000 });
  // The server records every tier change. The tavern-backpressure hint for
  // the reset is sent without waiting, so it is dropped if the queue is full
  // at that moment; only the server's record is required.
  await expect.poll(async () => (await state(page, "pressure")).tiers.join("\n")).toMatch(/→ normal/);
  const { tiers } = await state(page, "pressure");
  expect(tiers.join("\n")).toMatch(/normal → throttle/);
  // When the reset hint was delivered, tavern-js reports it.
  const hints = await page.locator("#delivery-events li", { hasText: 'tavern-backpressure {"tier":"normal"' }).count();
  if (hints > 0) await expect(page.locator("#delivery-events")).toContainText("tavern:stream-restored");
  test.info().annotations.push({ type: "reset hints received", description: String(hints) });
  await page.waitForTimeout(500);
  expect(await page.locator("#gauge [data-value]").getAttribute("data-value")).toBe(String((await state(page, "pressure")).value));
});

test("a window held back too long is evicted and reconnects to the current value", async ({ viewer }) => {
  const page = await open(viewer, "pressure", { expected: [SSE_ERROR] });
  await expect(page.locator("#gauge [data-value]")).toBeAttached();
  await button(page, "Hold back this window").click();
  await button(page, "Make 30 changes").click();
  await expect.poll(async () => (await state(page, "pressure")).tiers.join("\n"), { timeout: 5000 }).toMatch(/→ disconnect/);
  await button(page, "Release it").click();
  const { value } = await state(page, "pressure");
  await expect(page.locator("#gauge [data-value]")).toHaveAttribute("data-value", String(value), { timeout: 8000 });
  await expect(page.locator("#gauge [data-fidelity]")).toHaveAttribute("data-fidelity", "full");
});

test("a failing scheduled section falls back, then recovers", async ({ viewer }) => {
  const page = await open(viewer, "sched");
  try {
    await expect(page.locator('#dash-load [data-state="live"]')).toBeVisible({ timeout: 3000 });
    await expect(page.locator("#dash-clock")).not.toHaveText("…");
    await button(page, "Make the load source fail").click();
    await expect(page.locator('#dash-load [data-state="fallback"]')).toBeVisible({ timeout: 3000 });
    const s = await state(page, "sched");
    expect(s.errors).toBeGreaterThanOrEqual(2);
    expect(s.observations.join("\n")).toMatch(/section load failed \(2 in a row\)/);
    await button(page, "Repair it").click();
    await expect(page.locator('#dash-load [data-state="live"]')).toBeVisible({ timeout: 4000 });
  } finally {
    await page.evaluate(() => fetch("/showcase/delivery/sched/fail?on=0", { method: "POST" }));
  }
});

test("change-only fields skip repeats; the report renders only while a panel is open", async ({ viewer }) => {
  const page = await open(viewer, "render");
  await expect(page.locator("#render-status")).not.toHaveText("…");
  const current = await page.locator("#render-status [data-value]").getAttribute("data-value");
  const next = current === "amber" ? "red" : "amber";
  await button(page, `Status ${next}`).click();
  await expect(page.locator("#render-status")).toHaveText(next);
  const updates = await count(page, "render-status");
  await button(page, `Status ${next}`).click();
  await page.waitForTimeout(300);
  expect(await count(page, "render-status")).toBe(updates);
  await button(page, "Forget the last status").click();
  await button(page, `Status ${next}`).click();
  await expect.poll(() => count(page, "render-status")).toBe(updates + 1);

  const rendered = async () => (await state(page, "render")).report.rendered;
  const before = await rendered();
  await button(page, "Refresh the report").click();
  await page.waitForTimeout(200);
  if (!(await state(page, "render")).reportSubscribers) expect(await rendered()).toBe(before);
  await button(page, "Open the report panel").click();
  await expect(page.locator("#render-report")).toContainText("Waiting for a refresh.");
  await expect.poll(async () => (await state(page, "render")).reportSubscribers).toBe(true);
  await button(page, "Refresh the report").click();
  await expect(page.locator("#render-report")).toContainText(`Status ${next}`);
  await button(page, "Close it").click();
  await expect.poll(async () => (await state(page, "render")).reportSubscribers).toBe(false);
  const closed = await rendered();
  await button(page, "Refresh the report").click();
  await page.waitForTimeout(200);
  expect(await rendered()).toBe(closed);

  await button(page, "Add activity").click();
  await expect(page.locator("#render-activity li").first()).toContainText(`status ${next}`);
});

test("a batch arrives as one message per topic and scope", async ({ viewer }) => {
  const page = await open(viewer, "batch");
  await page.waitForTimeout(200);
  await button(page, "Publish a batch").click();
  await expect(page.locator("#batch-board li").last()).toContainText("second board update");
  await expect(page.locator("#batch-feed li").last()).toContainText("feed item");
  await expect(page.locator("#batch-team li")).toHaveCount(2);
  const conn = await page.evaluate(() => document.querySelector("[sse-connect]").getAttribute("sse-connect").split("conn=")[1]);
  const s = await page.evaluate((c) => fetch(`/showcase/delivery/batch/state?conn=${c}`).then((r) => r.json()), conn);
  expect(s.channelMessages).toEqual({ board: 1, feed: 1, "board/red": 1, "feed/red": 1 });

  await button(page, "Build a batch and discard it").click();
  await page.waitForTimeout(300);
  await expect(page.locator("#batch-team li")).toHaveCount(2);

  await button(page, "Run concurrent producers").click();
  await expect(page.locator('#order-result [data-topic="delivery/order/ordered"]')).toHaveAttribute("data-same", "true");
});

test("at most five notices show; they all leave when they expire, and later windows get only those", async ({ viewer }) => {
  const page = await open(viewer, "ttl");
  await expect.poll(async () => (await state(page, "ttl")).windowTopics.length).toBeGreaterThan(0);
  await expect(page.locator("#ttl-notices li")).toHaveCount(0, { timeout: 5000 }); // earlier tests' notices expire
  for (let i = 0; i < 6; i++) await button(page, "Post a notice").click();
  await expect(page.locator("#ttl-notice-result")).toHaveText("5 notices are showing; wait for one to expire.");
  await expect(page.locator("#ttl-notices li")).toHaveCount(5);
  for (let i = 0; i < 3; i++) {
    const posted = page.waitForResponse((r) => r.url().endsWith("/ttl/team"));
    await button(page, "Post a team notice").click();
    await posted;
  }
  // The last update arrives over the stream after its POST returns.
  let team = "";
  await expect(async () => {
    const before = await page.locator("#ttl-team").textContent();
    await page.waitForTimeout(300);
    team = await page.locator("#ttl-team").textContent();
    expect(team).toBe(before);
  }).toPass();
  expect(team).toMatch(/^Team notice \d+/);
  const later = await open(viewer, "ttl");
  await expect(later.locator("#ttl-notices li")).toHaveCount(5); // from the replay cache
  await expect(later.locator("#ttl-team")).toHaveText(team);

  await expect(page.locator("#ttl-notices li")).toHaveCount(0, { timeout: 5000 });
  await expect(later.locator("#ttl-notices li")).toHaveCount(0);
  await page.waitForTimeout(500);
  await expect(page.locator("#ttl-notices li")).toHaveCount(0);
  await expect(page.locator("#ttl-team")).toHaveText(team); // the team notice stays after expiry
  await button(page, "Post a notice").click();
  await expect(page.locator("#ttl-notice-result")).toHaveText("Posted.");
  await expect(page.locator("#ttl-notices li")).toHaveCount(1);
});

test("workflow windows opened later, or reloaded, show the model's rows and summary", async ({ viewer }) => {
  const first = await open(viewer, "flow");
  await expect(first.locator("#order-summary")).toContainText("orders shown");
  for (const item of ["tea", "coffee", "cake"]) {
    await button(first, `Order ${item}`).click();
  }
  // Settled: this window shows exactly the model's rows and summary.
  const model = async () => (await state(first, "flow")).visible.length;
  await expect(async () => {
    const rows = await first.locator("#order-rows li").count();
    expect(rows).toBe(await model());
    await expect(first.locator("#order-summary [data-orders]")).toHaveAttribute("data-orders", String(rows), { timeout: 100 });
  }).toPass();
  await first.waitForTimeout(300);
  const rows = await first.locator("#order-rows li").allTextContents();
  const summary = await first.locator("#order-summary").textContent();
  const audit = await first.locator("#audit-rows li").allTextContents();
  expect(rows.length).toBe(await model());
  expect(rows.length).toBeLessThanOrEqual(6);
  expect(audit.length).toBeLessThanOrEqual(8);
  expect(rows.every((r) => r.includes("checked by orders:*"))).toBe(true);

  const second = await open(viewer, "flow");
  await expect(second.locator("#order-summary")).toHaveText(summary);
  expect(await second.locator("#order-rows li").allTextContents()).toEqual(rows);
  expect(await second.locator("#audit-rows li").allTextContents()).toEqual(audit);

  await first.reload();
  await expect(first.locator("#order-summary")).toHaveText(summary);
  expect(await first.locator("#order-rows li").allTextContents()).toEqual(rows);
  expect(await first.locator("#audit-rows li").allTextContents()).toEqual(audit);
});
