import { test, expect } from "./fixtures.js";

// streams returns tavern-js's scoped stream registry as { scope: state }.
const streams = (page) =>
  page.evaluate(() => {
    const out = {};
    for (const [k, v] of Object.entries(Tavern.streams())) out[k] = v.state;
    return out;
  });

// server returns what the server holds for this window's connection.
const server = (page) =>
  page.evaluate(async () => {
    const conn = new URL(document.getElementById("shell-lifeline").getAttribute("sse-connect"), location.href).searchParams.get("conn");
    const as = new URL(location.href).searchParams.get("as");
    const res = await fetch(`/showcase/shell/diagnostics?as=${as}&conn=${conn}`);
    return res.json();
  });

async function openShell(viewer, who, options) {
  const page = await viewer(options);
  const lifelineRequests = [];
  page.on("request", (req) => {
    if (req.url().includes("/showcase/shell/lifeline")) lifelineRequests.push(req.url());
  });
  await page.goto(`/showcase/shell?as=${who}`);
  await expect(page.locator("#shell-notices li").first()).toBeAttached();
  await expect.poll(() => streams(page)).toEqual({ board: "active" });
  return { page, lifelineRequests };
}

async function go(page, view) {
  await page.locator("#shell-nav").getByRole("button", { name: view, exact: true }).click();
  await expect.poll(() => streams(page)).toEqual({ [view.toLowerCase()]: "active" });
}

test("switching views keeps one lifeline and releases each view's stream", async ({ viewer }) => {
  const { page, lifelineRequests } = await openShell(viewer, "alice");
  expect(await page.evaluate(() => Tavern.lifeline().id)).toBe("shell-lifeline");
  await expect.poll(async () => (await server(page)).boardStreams).toBe(1);
  const counts = async () => (await server(page)).topicCounts;
  const start = await counts();

  for (let round = 0; round < 2; round++) {
    await go(page, "Feeds");
    await expect.poll(async () => (await server(page)).boardStreams).toBe(0);
    await page.getByRole("button", { name: "Post a notice" }).click();
    await go(page, "Ops");
    await go(page, "Board");
    await expect.poll(async () => (await server(page)).boardStreams).toBe(1);
  }
  await expect(page.locator("#shell-notices")).toContainText("Alice posted notice");

  // Back where it started: the same lifeline, the same subscribers.
  expect(lifelineRequests).toHaveLength(1);
  await expect.poll(counts).toEqual(start);
  await expect(page.locator("#shell-log")).toContainText("retired feeds");
});

test("the server adds and removes lifeline topics", async ({ viewer }) => {
  const { page: alice } = await openShell(viewer, "alice");
  const { page: bob } = await openShell(viewer, "bob");

  const releases = alice.locator(".shell-channel", { hasText: "releases" });
  await releases.getByRole("button", { name: "Follow", exact: true }).click();
  await expect(alice.locator("#shell-topics")).toHaveText("shell/channel/releases, shell/notices");
  await expect.poll(async () => (await server(alice)).lifelineTopics).toEqual(["shell/channel/releases", "shell/notices"]);
  // Following delivers the channel's latest post straight away.
  await expect(alice.locator("#shell-channel-releases")).not.toHaveText("not followed");
  await releases.getByRole("button", { name: "Post", exact: true }).click();
  await expect(alice.locator("#shell-channel-releases")).toContainText("Alice posted");
  await expect(bob.locator("#shell-channel-releases")).toHaveText("not followed");

  await releases.getByRole("button", { name: "Unfollow", exact: true }).click();
  await expect(alice.locator("#shell-topics")).toHaveText("shell/notices");
  await expect(alice.locator("#shell-channel-releases")).toHaveText("not followed");
  await bob.locator(".shell-channel", { hasText: "releases" }).getByRole("button", { name: "Post", exact: true }).click();
  await alice.waitForTimeout(300);
  await expect(alice.locator("#shell-channel-releases")).toHaveText("not followed");

  // A lead adds the leads channel to every lead's lifeline, by role.
  await alice.getByRole("button", { name: "Add for every lead" }).click();
  await expect(alice.locator("#shell-topics")).toContainText("shell/channel/leads");
  expect((await server(bob)).lifelineTopics).toEqual(["shell/notices"]);
  await alice.locator(".shell-channel", { hasText: "leads" }).getByRole("button", { name: "Post", exact: true }).click();
  await expect(alice.locator("#shell-channel-leads")).toContainText("Alice posted");
  expect(await bob.content()).not.toContain("shell-channel-leads");

  await alice.getByRole("button", { name: "Note to leads" }).click();
  await expect(alice.locator("#shell-role-notice")).toContainText("Alice sent a note to team leads");
  await alice.waitForTimeout(300);
  await expect(bob.locator("#shell-role-notice")).toHaveText("none yet");
  await alice.getByRole("button", { name: "Remove for every lead" }).click();
  await expect(alice.locator("#shell-topics")).toHaveText("shell/notices");
});

test("the feeds view gets every topic under shell/feeds/ and nothing else", async ({ viewer }) => {
  const { page } = await openShell(viewer, "alice");
  await go(page, "Feeds");
  for (const name of ["web", "noise", "api"]) {
    const topic = { web: "shell/feeds/releases/web", noise: "shell/other/noise", api: "shell/feeds/releases/api" }[name];
    await page.getByRole("button", { name: `Publish to ${topic}` }).click();
  }
  const items = page.locator("#shell-feed-list li");
  await expect(items).toHaveCount(2);
  await expect(items.nth(0).locator("code")).toHaveText("shell/feeds/releases/api");
  await expect(items.nth(1).locator("code")).toHaveText("shell/feeds/releases/web");
});

test("the ops view is a static topic group", async ({ viewer }) => {
  const { page } = await openShell(viewer, "alice");
  await go(page, "Ops");
  const deploys = page.locator("#shell-ops-deploys");
  await expect(deploys).toHaveText(/^\d+ deploys$/);
  const before = parseInt(await deploys.textContent(), 10);
  await page.getByRole("button", { name: "Record a deploy" }).click();
  await expect(deploys).toHaveText(`${before + 1} deploys`);
  await expect(page.locator("#shell-ops-alerts")).toHaveText(/^\d+ alerts$/);
});

test("a broken view falls back to the lifeline, then recovers from a snapshot", async ({ viewer }) => {
  // htmx logs the htmx:sseError event when the view's stream ends.
  const { page: alice } = await openShell(viewer, "alice", { expected: ["console error: [object Event]"] });
  const { page: bob } = await openShell(viewer, "bob");

  await alice.getByRole("button", { name: "Break this view's stream" }).click();
  await expect(alice.locator("#shell-fallback")).toHaveText(/The board view lost its stream/);
  await expect.poll(() => streams(alice)).toEqual({ board: "warming" });
  await expect(alice.locator("#shell-log")).toContainText("fallback board");

  // The lifeline keeps working, and the board changes meanwhile.
  await bob.getByRole("button", { name: "Post a notice" }).click();
  await expect(alice.locator("#shell-notices")).toContainText("Bob posted notice");
  await bob.getByRole("button", { name: "Add a task" }).click();
  await expect(bob.locator("#shell-board-tasks")).toContainText("Bob's task");
  const latest = await bob.locator("#shell-board-tasks li").first().textContent();
  await expect(alice.locator("#shell-board-tasks")).not.toContainText(latest);

  await alice.getByRole("button", { name: "Let it reconnect" }).click();
  await expect.poll(() => streams(alice), { timeout: 15_000 }).toEqual({ board: "active" });
  await expect(alice.locator("#shell-board-tasks li").first()).toHaveText(latest);
  await expect(alice.locator("#shell-fallback")).toBeEmpty();
});

test("a slow view response from an earlier click does not replace the current view", async ({ viewer }) => {
  // The second click cancels the first request (hx-sync="this:replace");
  // htmx logs the cancellation.
  const { page } = await openShell(viewer, "alice", {
    expected: [
      "console error: htmx:sendAbort, [object HTMLDivElement]",
      "console error: htmx:afterRequest, [object HTMLDivElement]",
      /^request failed: http:\/\/127\.0\.0\.1:\d+\/showcase\/shell\/views\/board\?conn=\w+&as=alice net::ERR_ABORTED$/,
    ],
  });
  await go(page, "Ops");

  let release;
  const held = new Promise((resolve) => (release = resolve));
  await page.route(/\/showcase\/shell\/views\/board\?/, async (route) => {
    await held;
    await route.continue().catch(() => {}); // the request may have been cancelled
  });
  try {
    await page.locator("#shell-nav").getByRole("button", { name: "Board", exact: true }).click();
    await page.locator("#shell-nav").getByRole("button", { name: "Feeds", exact: true }).click();
    await expect.poll(() => streams(page)).toEqual({ feeds: "active" });
  } finally {
    release();
  }
  await page.waitForTimeout(500);
  expect(await streams(page)).toEqual({ feeds: "active" });
  expect((await server(page)).boardStreams).toBe(0);
  await expect(page.locator("#shell-view h2")).toHaveText("Feeds");
});

test("a view left while its stream was still opening is never promoted", async ({ viewer }) => {
  // Breaking the active board makes htmx log one htmx:sseError.
  const { page } = await openShell(viewer, "alice", { expected: ["console error: [object Event]"] });

  // The server holds this window's board stream from now until recover, so
  // the next board view stays warming: its connection never opens.
  await page.getByRole("button", { name: "Break this view's stream" }).click();
  await expect(page.locator("#shell-fallback")).toHaveText(/The board view lost its stream/);
  await go(page, "Ops");
  await page.locator("#shell-nav").getByRole("button", { name: "Board", exact: true }).click();
  await expect(page.locator("#shell-view h2")).toHaveText("Board");
  await expect.poll(() => streams(page)).toEqual({ board: "warming" });
  const promotions = () => page.locator("#shell-log li", { hasText: "promoted board" }).count();
  const before = await promotions();

  await go(page, "Feeds");
  expect(await page.getByRole("button", { name: "Let it reconnect" }).count()).toBe(0);
  const conn = await page.evaluate(() =>
    new URL(document.getElementById("shell-lifeline").getAttribute("sse-connect"), location.href).searchParams.get("conn"),
  );
  const status = await page.evaluate(async (c) => (await fetch(`/showcase/shell/views/board/recover?conn=${c}&as=alice`, { method: "POST" })).status, conn);
  expect(status).toBe(204);

  await page.waitForTimeout(500);
  expect(await streams(page)).toEqual({ feeds: "active" });
  expect(await promotions()).toBe(before);
  await expect.poll(async () => (await server(page)).boardStreams).toBe(0);
});

test("with a slow lifeline, views wait for it instead of failing", async ({ viewer }) => {
  const page = await viewer();
  const viewRequests = [];
  page.on("request", (req) => {
    if (/\/showcase\/shell\/views\//.test(req.url())) viewRequests.push(req.url());
  });
  await page.goto("/showcase/shell?as=alice&slow=1");
  await expect(page.locator("#shell-nav").getByRole("button", { name: "Board", exact: true })).toBeDisabled();
  await expect(page.locator("#shell-starting")).toBeVisible();
  await page.waitForTimeout(500); // the lifeline is still opening
  expect(viewRequests).toEqual([]);

  await expect.poll(() => streams(page)).toEqual({ board: "active" });
  await expect(page.locator("#shell-starting")).toHaveCount(0);
  expect(viewRequests.filter((u) => /\/views\/board\?/.test(u))).toHaveLength(1);
  expect(viewRequests.filter((u) => /\/views\/board\/events/.test(u))).toHaveLength(1);
});

test("the ops view is not active until both of its values have arrived", async ({ viewer }) => {
  const { page } = await openShell(viewer, "alice");
  await go(page, "Ops");
  await page.getByRole("button", { name: "Forget the alerts count" }).click();
  await go(page, "Board");

  await page.locator("#shell-nav").getByRole("button", { name: "Ops", exact: true }).click();
  await expect(page.locator("#shell-ops-deploys")).toHaveText(/^\d+ deploys$/);
  await page.waitForTimeout(500);
  await expect(page.locator("#shell-ops-alerts")).toHaveText("…");
  expect((await streams(page)).ops).not.toBe("active");

  await page.getByRole("button", { name: "Raise an alert" }).click();
  await expect(page.locator("#shell-ops-alerts")).toHaveText(/^\d+ alerts$/);
  await expect.poll(() => streams(page)).toEqual({ ops: "active" });
});

test("a held-back lifeline and ops view catch up after the producer goes quiet", async ({ viewer }) => {
  const { page: alice } = await openShell(viewer, "alice");
  const { page: bob } = await openShell(viewer, "bob");
  await go(alice, "Ops");
  await go(bob, "Ops");

  await alice.getByRole("button", { name: "Hold back the lifeline and ops view" }).click();
  // More changes than the held-back queues can take, then nothing more.
  await bob.evaluate(async () => {
    for (let i = 0; i < 30; i++) {
      for (const path of ["/showcase/shell/notices", "/showcase/shell/ops/deploy"]) {
        const res = await fetch(path, { method: "POST", body: new URLSearchParams({ as: "bob" }) });
        if (res.status !== 204) throw new Error(`${path} returned ${res.status}`);
      }
    }
  });
  const latestNotice = await bob.locator("#shell-notices li").first().textContent();
  await expect(bob.locator("#shell-ops-deploys")).toHaveText(/^\d+ deploys$/);
  const deploys = await bob.locator("#shell-ops-deploys").textContent();

  await alice.getByRole("button", { name: "Release them" }).click();
  await expect(alice.locator("#shell-notices li").first()).toHaveText(latestNotice, { timeout: 10_000 });
  await expect(alice.locator("#shell-ops-deploys")).toHaveText(deploys, { timeout: 10_000 });
});
