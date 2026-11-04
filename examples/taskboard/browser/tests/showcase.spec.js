import { test, expect } from "./fixtures.js";

test.beforeAll(async ({ browser }) => {
  console.log(`browser: ${browser.browserType().name()} ${browser.version()}`);
});

test("task board loads, accepts a task and links to the showcase", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/");
  await expect(page.locator("#status .live")).toBeVisible();
  const todo = page.locator("section.column").first();
  await expect(todo.locator("h2")).toContainText("To do");
  await expect(todo).not.toContainText("Loading");

  const title = `Browser task ${Date.now()}`;
  await page.getByLabel("New task").fill(title);
  await page.getByRole("button", { name: "Add task" }).click();
  await expect(todo).toContainText(title);

  await page.getByRole("link", { name: "Showcase" }).click();
  await expect(page).toHaveURL(/\/showcase$/);
  await page.getByRole("link", { name: "Connection and live updates" }).click();
  await expect(page.locator("#counter-region [tavern-status-live]")).toBeVisible();
});

test("tavern-js reports a live connection and every window gets updates", async ({ viewer }) => {
  const a = await viewer();
  const b = await viewer();
  await a.goto("/showcase/connection");
  await b.goto("/showcase/connection");

  for (const page of [a, b]) {
    const region = page.locator("#counter-region");
    await expect(region).toHaveClass(/\bis-live\b/);
    await expect(region.locator("[tavern-status-live]")).toBeVisible();
    await expect(region.locator(".connecting")).toBeHidden();
    await expect(page.locator('#events [data-event="tavern:live"]')).toHaveCount(1);
  }
  const before = Number(await a.locator("#count").textContent());

  await a.getByRole("button", { name: "Increment", exact: true }).click();
  await expect(a.locator("#count")).toHaveText(String(before + 1));
  await expect(b.locator("#count")).toHaveText(String(before + 1));
  await expect(b.locator('#events [data-event="tavern:region-updated"]').first()).toBeAttached();
});

test("a dropped window recovers the update it missed and goes live after the server confirms", async ({ viewer }) => {
  // htmx logs the htmx:sseError event when the stream ends.
  const a = await viewer({ expected: ["console error: [object Event]"] });
  const b = await viewer();
  await a.goto("/showcase/connection");
  await b.goto("/showcase/connection");
  const region = a.locator("#counter-region");
  await expect(region).toHaveClass(/\bis-live\b/);
  await expect(b.locator("#counter-region")).toHaveClass(/\bis-live\b/);
  const before = Number(await a.locator("#count").textContent());

  await a.getByRole("button", { name: "Drop, then increment while away" }).click();

  // The browser waits before reconnecting, so the lost state is visible.
  await expect(region).toHaveClass(/\bis-reconnecting\b/);
  await expect(region.locator("[tavern-status]")).toBeVisible();
  await expect(region.locator("[tavern-status-live]")).toBeHidden();

  // The window that stayed connected sees the change live.
  await expect(b.locator("#count")).toHaveText(String(before + 1));

  const reconnected = a.locator('#events [data-event="tavern:reconnected"]');
  await expect(reconnected).toHaveCount(1, { timeout: 15_000 });
  await expect(reconnected).toContainText('"replayDelivered":1');
  await expect(a.locator("#count")).toHaveText(String(before + 1));
  await expect(region).toHaveClass(/\bis-live\b/);
  await expect(region).not.toHaveClass(/\bis-reconnecting\b/);
  await expect(region.locator("[tavern-status]")).toBeHidden();

  // The log is newest first; reverse it to check the order of the reconnect.
  const order = await a.locator("#events li").evaluateAll((items) =>
    items.map((li) => li.dataset.event).reverse(),
  );
  const seq = [
    "tavern:transport-closed",
    "tavern:disconnected",
    "tavern:recovering",
    "tavern:transport-open",
    "tavern:reconnected",
  ].map((e) => order.indexOf(e));
  expect(seq.every((i) => i >= 0), `events: ${order.join(", ")}`).toBe(true);
  expect(seq).toEqual([...seq].sort((x, y) => x - y));

  await expect(b.locator('#events [data-event="tavern:disconnected"]')).toHaveCount(0);
});
