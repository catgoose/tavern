import { test, expect } from "./fixtures.js";

const SSE_ERROR = "console error: [object Event]";

const state = (page, family) =>
  page.evaluate((f) => fetch(`/showcase/ops/${f}/state`).then((r) => r.json()), family);
const button = (page, name) => page.getByRole("button", { name, exact: true });

async function presenceTab(viewer, as, options) {
  const page = await viewer(options);
  await page.goto(`/showcase/ops/presence?as=${as}`);
  await expect(page.locator(`#people [data-user="${as}"]`)).toBeVisible();
  return page;
}

const person = (page, user) => page.locator(`#people [data-user="${user}"]`);

// settled waits until a tab shows exactly the server's people.
async function settled(page) {
  await expect(async () => {
    const { present } = await state(page, "presence");
    const shown = await page.locator("#people [data-user]").evaluateAll((els) =>
      els.map((e) => ({ user: e.dataset.user, status: e.dataset.status, tabs: Number(e.dataset.tabs) })),
    );
    expect(shown).toEqual((present || []).map((p) => ({ user: p.user, status: p.status, tabs: p.tabs })));
  }).toPass();
}

test("the operations lab is reached from the showcase", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/showcase");
  await page.getByRole("link", { name: "Operations lab" }).click();
  await expect(page).toHaveURL(/\/showcase\/ops$/);
  for (const name of ["Presence across tabs", "Connections and metrics", "Admission limits"]) {
    await expect(page.getByRole("link", { name })).toBeVisible();
  }
});

test("presence counts tabs per person; closing one tab keeps the person, the last one removes them", async ({ viewer }) => {
  const alice1 = await presenceTab(viewer, "alice");
  const alice2 = await presenceTab(viewer, "alice");
  const bob = await presenceTab(viewer, "bob");
  for (const page of [alice1, alice2, bob]) {
    await expect(person(page, "alice")).toHaveAttribute("data-tabs", "2");
    await expect(person(page, "bob")).toHaveAttribute("data-tabs", "1");
  }

  await button(alice1, "Status: busy").click();
  for (const page of [alice2, bob]) await expect(person(page, "alice")).toHaveAttribute("data-status", "busy");

  await alice1.close();
  await expect(person(bob, "alice")).toHaveAttribute("data-tabs", "1");
  await expect(person(alice2, "alice")).toHaveAttribute("data-status", "busy");

  await button(alice2, "Leave the room").click();
  await expect(alice2.locator("#presence-left")).toBeVisible();
  await expect(person(bob, "alice")).toHaveCount(0);
  await settled(bob);

  const late = await presenceTab(viewer, "bob");
  await expect(person(late, "bob")).toHaveAttribute("data-tabs", "2");
  await expect(person(late, "alice")).toHaveCount(0);
  await settled(late);
});

test("a tab that stops heartbeats goes stale only when no other tab of that person sends them", async ({ viewer }) => {
  const alice1 = await presenceTab(viewer, "alice");
  const alice2 = await presenceTab(viewer, "alice");
  const bob = await presenceTab(viewer, "bob");
  await expect(person(bob, "alice")).toHaveAttribute("data-tabs", "2");

  await button(alice1, "Stop this tab's heartbeats").click();
  await bob.waitForTimeout(3500);
  await expect(person(bob, "alice")).toHaveAttribute("data-tabs", "2"); // the other tab keeps her here

  await button(alice2, "Stop this tab's heartbeats").click();
  await expect(person(bob, "alice")).toHaveCount(0, { timeout: 5000 });
  await expect(person(alice1, "alice")).toHaveCount(0);
  const { observations } = await state(bob, "presence");
  expect(observations.join("\n")).toContain("Alice removed (stale: no heartbeat for 2 s)");

  await button(alice1, "Resume them").click();
  await expect(person(bob, "alice")).toHaveAttribute("data-tabs", "2", { timeout: 3000 });
  await settled(bob);
  await settled(alice2);
});

test("connections: the window's subscriptions, events, metrics and a targeted disconnect", async ({ viewer }) => {
  const page = await viewer({ expected: [SSE_ERROR] });
  await page.goto("/showcase/ops/conns?as=alice");
  const other = await viewer();
  await other.goto("/showcase/ops/conns?as=bob");
  const conn = await page.locator("#conns-id").textContent();
  await expect(page.locator("#conns-events li").first()).toContainText('"event":"subscribe"');
  await expect.poll(async () => (await state(page, "conns")).feedSubscribers.map((s) => s.id)).toContain(conn);
  const mine = (await state(page, "conns")).feedSubscribers.find((s) => s.id === conn);
  expect(mine.meta).toEqual({ persona: "alice", page: "conns" });

  const before = (await state(page, "conns")).metrics.topics["ops:feed"]?.Published ?? 0;
  await button(page, "Publish 10 messages").click();
  let last = (await state(page, "conns")).lastMessage;
  for (const p of [page, other]) await expect(p.locator("#conns-feed [data-seq]")).toHaveText(`message ${last}`);
  const after = (await state(page, "conns")).metrics.topics["ops:feed"].Published;
  expect(after).toBeGreaterThanOrEqual(before + 20); // two windows, ten messages each

  await button(page, "Disconnect this window").click();
  await expect(other.locator("#conns-events li").first()).toContainText('"event":"disconnect"');
  await expect(other.locator("#conns-events li").first()).toContainText('"event":"subscribe"', { timeout: 6000 }); // the window reconnects
  await expect.poll(async () => (await state(page, "conns")).feedSubscribers.map((s) => s.id), { timeout: 6000 }).toContain(conn);
  await button(other, "Publish 10 messages").click();
  last = (await state(page, "conns")).lastMessage;
  for (const p of [page, other]) await expect(p.locator("#conns-feed [data-seq]")).toHaveText(`message ${last}`);
});

test("admission: a full limit refuses with its reason, and room returns", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/showcase/ops/admit");
  const result = page.locator("#admit-global");
  for (let i = 0; i < 8; i++) {
    await button(page.locator('[data-policy="global"]'), "Remove the oldest").click();
    if ((await result.textContent()) === "nothing to remove") break;
  }
  const add = page.locator('[data-policy="global"]').getByRole("button", { name: "Subscribe to ops:admit:a" });
  for (let i = 1; i <= 3; i++) {
    await add.click();
    await expect(result).toHaveText(`ops:admit:a admitted; ${i} held`);
  }
  await add.click();
  await expect(result).toHaveText("ops:admit:a refused (at most 3 subscriptions on the broker); 3 held");
  await button(page, "Request a stream from the global broker").click();
  await expect(page.locator("#admit-stream")).toHaveText("503 Service Unavailable");
  await button(page.locator('[data-policy="global"]'), "Remove the oldest").click();
  await add.click();
  await expect(result).toHaveText("ops:admit:a admitted; 3 held");

  const topic = page.locator('[data-policy="topic"]');
  await button(topic, "Both at once, all or nothing").click();
  await expect(page.locator("#admit-topic")).toContainText("both topics");
});

test("transport: the raw wire shows keepalives, heartbeats, the retry value and a finite lifetime", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/showcase/ops/wire");
  await button(page, "Capture SSEHandler's wire").click();
  const capture = page.locator("#wire-capture");
  await expect(capture).toContainText("retry: 1500", { timeout: 6000 });
  await expect(capture).toContainText(": keepalive");
  await expect(capture).toContainText("event: tick");
  await expect(capture).toContainText("the server ended the response");
  await button(page, "Capture StreamSSE's wire").click();
  await expect(capture).toContainText(": via custom writer", { timeout: 6000 });
  await expect(capture).toContainText("retry: 2500"); // the tick's own
  await expect(capture).not.toContainText("retry: 1500");
  await button(page, "Fail the flush").click();
  await expect(page.locator("#wire-failure")).toHaveText(/^synthetic flush failure: WriteError at flush/);
});

test("shutting down the child ends its streams and publisher; a new child serves new streams", async ({ viewer }) => {
  const page = await viewer();
  await page.goto("/showcase/ops/child");
  await button(page, "Open the streams").click();
  const cell = (mode, what) => page.locator(`[data-mode="${mode}"] [data-${what}]`);
  await expect(cell("live", "state")).toHaveText("open");
  await expect(cell("slow", "state")).toHaveText("open");
  await expect(cell("held", "state")).toHaveText("connecting"); // its headers are held too
  await expect(cell("live", "tick")).toHaveText(/child \d+, tick \d+/);
  const gen = Number((await cell("live", "tick").textContent()).match(/child (\d+)/)[1]);

  await button(page, "Shut the child down").click();
  for (const mode of ["live", "held", "slow"]) await expect(cell(mode, "state")).toHaveText("ended");
  const { lastShutdown, running } = await state(page, "child");
  expect(running).toBe(false);
  expect(lastShutdown.streamsBefore).toBeGreaterThanOrEqual(3);
  expect(lastShutdown.streamsLeft).toBe(0);
  expect(lastShutdown.subscriptionsLeft).toBe(0);

  await button(page, "Start a new child").click();
  await button(page, "Open the streams").click();
  await expect(cell("live", "state")).toHaveText("open");
  await expect(cell("live", "tick")).toHaveText(new RegExp(`child ${gen + 1}, tick \\d+`));
});
