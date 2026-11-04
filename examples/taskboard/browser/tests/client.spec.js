import { test, expect } from "./fixtures.js";

const button = (page, name) => page.getByRole("button", { name, exact: true });
const regionN = (page, n) => page.locator(`#client-region-${n}`);
const field = (page, n, name) => regionN(page, n).locator(`[data-${name}]`);
const subs = (page) => page.evaluate(() => fetch("/showcase/client/state").then((r) => r.json()).then((s) => s.subscriptions));

async function pulse(page) {
  const sent = page.waitForResponse((r) => r.url().endsWith("/showcase/client/pulse"));
  await button(page, "Send an update").click();
  await sent;
}

async function openClient(viewer, query = "") {
  const page = await viewer();
  await page.goto(`/showcase/client${query}`);
  await expect(page.locator("#client-subs")).not.toHaveText("…");
  return page;
}

test("regions added to the page are bound, go live and flash on each update", async ({ viewer }) => {
  const page = await openClient(viewer);
  await button(page, "Add a region").click();
  await expect(field(page, 1, "bound")).toHaveText("yes");
  await expect(regionN(page, 1)).toHaveClass(/\bis-live\b/);
  await expect(field(page, 1, "live")).toHaveText("1");
  await expect.poll(() => subs(page)).toBe(1);
  await expect(field(page, 1, "updates")).toHaveText("1"); // the stream opens with the current value

  await pulse(page);
  await expect(regionN(page, 1).locator("output")).toHaveText(/^pulse \d+$/);
  await expect(field(page, 1, "updates")).toHaveText("2");
  await expect(regionN(page, 1)).toHaveClass(/\bis-updated\b/);
  await expect(regionN(page, 1)).not.toHaveClass(/\bis-updated\b/, { timeout: 1500 }); // removed after 400 ms
});

test("binding again, and initializing again, add no second set of listeners", async ({ viewer }) => {
  const page = await openClient(viewer);
  await button(page, "Add a region").click();
  await expect(regionN(page, 1)).toHaveClass(/\bis-live\b/);
  for (let i = 0; i < 2; i++) {
    await button(page, "Tavern.bind() the first region again").click();
    await button(page, "Tavern.init()").click();
  }
  await expect(field(page, 1, "updates")).toHaveText("1");
  for (let i = 1; i <= 3; i++) {
    await pulse(page);
    await expect(field(page, 1, "updates")).toHaveText(String(i + 1));
  }
  await page.waitForTimeout(300);
  await expect(field(page, 1, "updates")).toHaveText("4");
  await expect(field(page, 1, "live")).toHaveText("1");
});

test("after destroy, new regions stream but are not bound until scanned or initialized", async ({ viewer }) => {
  const page = await openClient(viewer);
  await button(page, "Add a region").click();
  await expect(regionN(page, 1)).toHaveClass(/\bis-live\b/);
  await button(page, "Tavern.destroy()").click();
  await button(page, "Add a region").click();
  await expect.poll(() => subs(page)).toBe(2); // htmx opened its stream
  await expect(field(page, 2, "bound")).toHaveText("no");

  await pulse(page);
  await expect(regionN(page, 2).locator("output")).toHaveText(/^pulse \d+$/); // htmx still swaps it
  await expect(field(page, 2, "updates")).toHaveText("0"); // tavern-js does not see it
  await expect(field(page, 1, "updates")).toHaveText("2"); // the region bound before destroy still is

  await button(page, "Tavern.scanAndBind()").click();
  await expect(field(page, 2, "bound")).toHaveText("yes");
  await pulse(page);
  await expect(field(page, 2, "updates")).toHaveText("1");
  // It was bound after its connection opened, so tavern-js never saw it go
  // live; it stays "connecting" until it reconnects.
  await expect(field(page, 2, "live")).toHaveText("0");
  await expect(regionN(page, 2)).not.toHaveClass(/\bis-live\b/);

  await button(page, "Tavern.init()").click();
  await button(page, "Add a region").click();
  await expect(field(page, 3, "bound")).toHaveText("yes"); // the observer binds new regions again
  await expect(regionN(page, 3)).toHaveClass(/\bis-live\b/);
});

test("destroy empties the registry but leaves the stream to htmx; removing regions closes them", async ({ viewer }) => {
  const page = await openClient(viewer);
  await button(page, "Add a scoped region").click();
  await expect(page.locator("#client-registry")).toHaveText("region-1");
  await button(page, "Tavern.destroy()").click();
  await expect(page.locator("#client-registry")).toHaveText("none");
  await pulse(page);
  await expect(field(page, 1, "updates")).toHaveText("2"); // still bound and connected
  await button(page, "Tavern.init()").click();
  await expect(page.locator("#client-registry")).toHaveText("none"); // a bound element is not bound again
  await expect.poll(() => subs(page)).toBe(1);

  for (let round = 0; round < 5; round++) {
    await button(page, "Remove all regions").click();
    await expect.poll(() => subs(page)).toBe(0);
    await button(page, "Add a region").click();
    await button(page, "Add a region").click();
    await expect.poll(() => subs(page)).toBe(2);
  }
  const regions = page.locator(".client-region");
  await expect(regions).toHaveCount(2);
  await pulse(page);
  for (const el of await regions.all()) await expect(el.locator("[data-updates]")).toHaveText("2");
  await button(page, "Remove all regions").click();
  await expect.poll(() => subs(page)).toBe(0);
});

test("tavern-debug logs only where it is set", async ({ viewer }) => {
  const plain = await openClient(viewer);
  const plainLogs = [];
  plain.on("console", (m) => m.text().startsWith("[tavern]") && plainLogs.push(m.text()));
  await button(plain, "Add a region").click();
  await expect(regionN(plain, 1)).toHaveClass(/\bis-live\b/);
  await pulse(plain);
  await expect(field(plain, 1, "updates")).toHaveText("2");
  expect(plainLogs).toEqual([]);

  const debug = await openClient(viewer, "?debug=1");
  const logs = [];
  debug.on("console", (m) => m.type() === "debug" && m.text().startsWith("[tavern]") && logs.push(m.text()));
  await button(debug, "Add a region").click();
  await expect(regionN(debug, 1)).toHaveClass(/\bis-live\b/);
  await expect.poll(() => logs.some((l) => l.startsWith("[tavern] binding to"))).toBe(true);
});
