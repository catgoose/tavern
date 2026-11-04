import { test, expect } from "./fixtures.js";

async function openLab(viewer, who, options) {
  const page = await viewer(options);
  await page.goto(`/showcase/lab?as=${who}`);
  for (const region of ["votes", "editor"]) {
    await expect(page.locator(`#lab-state-${region}`)).toHaveText("Up to date");
  }
  await expect(page.locator("#lab-log-list li").first()).toBeAttached();
  return page;
}

const voteCard = (page, title) => page.locator("#lab-votes-list article", { hasText: title });
const votesOf = async (page, title) => Number(await voteCard(page, title).locator("[data-votes]").textContent());
const editCard = (page, title) => page.locator("#lab-edit-list > div", { hasText: title });

// addTask creates a task with a unique title and waits for it to show.
async function addTask(page, title) {
  await page.getByLabel("New task").fill(title);
  await page.getByRole("button", { name: "Add task" }).click();
  await expect(voteCard(page, title)).toBeVisible();
  return title;
}

// recordOutcomes notes every command outcome the document sees: its type,
// where it was dispatched, whether it names the element that sent the
// command, and whether that element was still connected. Bounded to 20.
async function recordOutcomes(page) {
  await page.evaluate(() => {
    window.__outcomes = [];
    let sent = null;
    document.addEventListener("tavern:command-sent", (e) => (sent = e.detail.element));
    for (const type of ["tavern:command-success", "tavern:command-error"]) {
      document.addEventListener(type, (e) => {
        if (window.__outcomes.length >= 20) return;
        window.__outcomes.push({
          type,
          url: e.detail.url,
          sameElement: e.detail.element === sent,
          connected: e.detail.element.isConnected,
          origin: e.target === e.detail.element ? "element" : e.target.id || e.target.nodeName,
          error: e.detail.error ? e.detail.error.message : null,
          status: e.detail.response ? e.detail.response.status : null,
        });
      });
    }
  });
  return () => page.evaluate(() => window.__outcomes);
}

// commandsTo counts the command POSTs the page sends to URLs matching re.
function commandsTo(page, re) {
  const seen = [];
  page.on("request", (req) => {
    if (req.method() === "POST" && re.test(req.url())) seen.push(req.url());
  });
  return seen;
}

test("vote commands: one per gesture, keyboard too, double clicks deduplicated", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const title = await addTask(alice, `Votes ${Date.now()}`);
  const sent = commandsTo(alice, /\/vote\?as=alice$/);
  const button = voteCard(alice, title).getByRole("button", { name: "Vote" });

  await button.click(); // pointerdown sends; the click that follows does not
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
  expect(sent).toHaveLength(1);

  await voteCard(alice, title).getByRole("button", { name: "Vote" }).focus();
  await alice.keyboard.press("Enter");
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("2");
  expect(sent).toHaveLength(2);

  await voteCard(alice, title).getByRole("button", { name: "Vote" }).dblclick();
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("3");
  await alice.waitForTimeout(500); // past the 400 ms dedup window
  expect(sent).toHaveLength(3);
  await expect(alice.locator("#lab-client-log")).toContainText("sent /showcase/lab/tasks/");
  await expect(alice.locator("#lab-log-list")).toContainText(`Alice voted for "${title}" (3)`);
});

test("a refused command is reported, and click commands are deduplicated", async ({ viewer }) => {
  // The second burst is refused while the first runs.
  const alice = await openLab(viewer, "alice", {
    expected: [
      /^409 http:\/\/127\.0\.0\.1:\d+\/showcase\/lab\/burst\?as=alice$/,
      "console error: Failed to load resource: the server responded with a status of 409 (Conflict)",
    ],
  });
  const sent = commandsTo(alice, /\/burst\?as=alice$/);
  const simulate = alice.getByRole("button", { name: "Simulate 8 votes" });
  await simulate.click();
  await simulate.click(); // within the 1 s dedup window: not sent
  await expect(alice.locator("#lab-log-list")).toContainText("Alice started 8 simulated votes");
  expect(sent).toHaveLength(1);

  await alice.waitForTimeout(1_050);
  const outcomes = await recordOutcomes(alice);
  await simulate.click(); // the burst is still running
  await expect(alice.locator("#lab-command-status")).toHaveText(
    "Failed: Simulate 8 votes (/showcase/lab/burst?as=alice): Tavern.command: 409 Conflict",
  );
  expect(sent).toHaveLength(2);
  // The button stayed, so the outcome comes from it, once.
  expect(await outcomes()).toEqual([
    {
      type: "tavern:command-error",
      url: "/showcase/lab/burst?as=alice",
      sameElement: true,
      connected: true,
      origin: "element",
      error: "Tavern.command: 409 Conflict",
      status: null,
    },
  ]);
});

test("a vote's success is reported after its button was replaced", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const title = await addTask(alice, `Accepted ${Date.now()}`);
  const outcomes = await recordOutcomes(alice);

  // The server handles the vote and publishes the new card at once; only
  // the HTTP response waits until the original button is gone.
  let releaseResponse;
  const held = new Promise((resolve) => (releaseResponse = resolve));
  const handled = new Promise((resolve) => {
    alice.route(/\/vote\?as=alice$/, async (route) => {
      const response = await route.fetch();
      resolve();
      await held;
      await route.fulfill({ response });
    });
  });
  const original = await voteCard(alice, title).getByRole("button", { name: "Vote" }).elementHandle();
  try {
    await original.click();
    await handled;
    await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
    await expect.poll(() => original.evaluate((el) => el.isConnected)).toBe(false);
    await expect(alice.locator("#lab-command-status")).toHaveText(/^Sending Vote /);
  } finally {
    releaseResponse();
  }

  await expect(alice.locator("#lab-command-status")).toHaveText(/^Accepted: Vote \(\/showcase\/lab\/tasks\/\d+\/vote\?as=alice\)$/);
  await alice.waitForTimeout(200);
  expect(await outcomes()).toEqual([
    expect.objectContaining({ type: "tavern:command-success", sameElement: true, connected: false, origin: "lab-votes", status: 204 }),
  ]);
});

test("a vote's failure is reported after its task was deleted", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice", {
    expected: [
      /^404 http:\/\/127\.0\.0\.1:\d+\/showcase\/lab\/tasks\/\d+\/vote\?as=alice$/,
      "console error: Failed to load resource: the server responded with a status of 404 (Not Found)",
    ],
  });
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Vanishing ${Date.now()}`);
  await expect(editCard(bob, title)).toBeVisible();
  const outcomes = await recordOutcomes(alice);

  // Alice's vote waits in the browser while Bob deletes the task; then it
  // reaches the server, which no longer has the task.
  let sendVote;
  const waiting = new Promise((resolve) => (sendVote = resolve));
  const intercepted = new Promise((resolve) => {
    alice.route(/\/vote\?as=alice$/, async (route) => {
      resolve();
      await waiting;
      await route.continue();
    });
  });
  const original = await voteCard(alice, title).getByRole("button", { name: "Vote" }).elementHandle();
  try {
    await original.click();
    await intercepted;
    await editCard(bob, title).getByRole("button", { name: "Delete" }).click();
    await expect(voteCard(alice, title)).toHaveCount(0);
    expect(await original.evaluate((el) => el.isConnected)).toBe(false);
  } finally {
    sendVote();
  }

  await expect(alice.locator("#lab-command-status")).toHaveText(
    /^Failed: Vote \(\/showcase\/lab\/tasks\/\d+\/vote\?as=alice\): Tavern\.command: 404 Not Found$/,
  );
  await alice.waitForTimeout(200);
  expect(await outcomes()).toEqual([
    expect.objectContaining({
      type: "tavern:command-error",
      sameElement: true,
      connected: false,
      origin: "lab-votes",
      error: "Tavern.command: 404 Not Found",
    }),
  ]);
  await expect(voteCard(bob, title)).toHaveCount(0);
});

test("a held vote converges after release outside the region, with no further updates", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Held ${Date.now()}`);
  await expect(voteCard(bob, title)).toBeVisible();

  // Alice holds the pointer down on the card (not on its button).
  const target = voteCard(alice, title).locator(".title");
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  await alice.mouse.move(box.x + 5, box.y + 5);
  await alice.mouse.down();
  await expect(alice.locator("#lab-state-votes")).toHaveAttribute("data-state", "paused");

  // One vote from Bob, then nothing more.
  await voteCard(bob, title).getByRole("button", { name: "Vote" }).click();
  await expect(voteCard(bob, title).locator("[data-votes]")).toHaveText("1");
  await alice.waitForTimeout(300);
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("0");

  // Release outside the region.
  await alice.mouse.move(5, 5);
  await alice.mouse.up();
  await expect(alice.locator("#lab-state-votes")).toHaveText("Up to date");
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
  await expect(alice.locator("#lab-client-log")).toContainText("votes: protection ended, 1 held updates discarded");
});

// pressTitle holds the pointer down on a vote card's title, which is
// inside the region but not a command.
async function pressTitle(page, title) {
  const target = voteCard(page, title).locator(".title");
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  await page.mouse.move(box.x + 5, box.y + 5);
  await page.mouse.down();
  await expect(page.locator("#lab-state-votes")).toHaveAttribute("data-state", "paused");
}

async function releaseOutside(page) {
  await page.mouse.move(5, 5);
  await page.mouse.up();
}

test("a second catch-up needed while the first request is open is still made", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Overlap ${Date.now()}`);
  await expect(voteCard(bob, title)).toBeVisible();

  // The server handles every resync at once and sends its snapshot over
  // SSE; only the first HTTP response is held back.
  let resyncs = 0;
  let releaseFirst;
  const firstHeld = new Promise((resolve) => (releaseFirst = resolve));
  await alice.route("**/showcase/lab/resync**", async (route) => {
    resyncs++;
    const response = await route.fetch();
    if (resyncs === 1) await firstHeld;
    await route.fulfill({ response });
  });

  try {
    await pressTitle(alice, title);
    await voteCard(bob, title).getByRole("button", { name: "Vote" }).click();
    await expect(voteCard(bob, title).locator("[data-votes]")).toHaveText("1");
    await releaseOutside(alice);
    await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
    await expect(alice.locator("#lab-state-votes")).toHaveText("Up to date");

    // While the first request is still open, another update is held.
    await pressTitle(alice, title);
    await voteCard(bob, title).getByRole("button", { name: "Vote" }).click();
    await expect(voteCard(bob, title).locator("[data-votes]")).toHaveText("2");
    await alice.waitForTimeout(200);
    await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
    await releaseOutside(alice);
    await expect(alice.locator("#lab-state-votes")).toHaveText("Catching up…");
    expect(resyncs).toBe(1);
  } finally {
    releaseFirst();
  }

  // Nothing else changes; the second catch-up is made once the first ends.
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("2");
  await expect(alice.locator("#lab-state-votes")).toHaveText("Up to date");
  await alice.waitForTimeout(500);
  expect(resyncs).toBe(2);
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("2");
});

test("a vote whose button is replaced during the gesture is sent once", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Replaced ${Date.now()}`);
  const sent = commandsTo(alice, /\/vote\?as=alice$/);

  const target = voteCard(alice, title).getByRole("button", { name: "Vote" });
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  await alice.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await alice.mouse.down(); // the command is sent now
  await voteCard(bob, title).getByRole("button", { name: "Vote" }).click();
  await expect(voteCard(bob, title).locator("[data-votes]")).toHaveText("2");
  await alice.mouse.up(); // the click that follows is not a second command

  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("2");
  await alice.waitForTimeout(300);
  expect(sent).toHaveLength(1);
});

test("a cancelled touch releases the pointer protection", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Touch ${Date.now()}`);
  await expect(voteCard(bob, title)).toBeVisible();

  const target = voteCard(alice, title).locator(".title");
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  const cdp = await alice.context().newCDPSession(alice);
  await cdp.send("Input.dispatchTouchEvent", {
    type: "touchStart",
    touchPoints: [{ x: box.x + 5, y: box.y + 5 }],
  });
  await expect(alice.locator("#lab-state-votes")).toHaveAttribute("data-state", "paused");
  await voteCard(bob, title).getByRole("button", { name: "Vote" }).click();
  await expect(voteCard(bob, title).locator("[data-votes]")).toHaveText("1");
  await cdp.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] });

  await expect(alice.locator("#lab-state-votes")).toHaveText("Up to date");
  await expect(voteCard(alice, title).locator("[data-votes]")).toHaveText("1");
});

test("an editor keeps its draft and caret through updates, and a stale save is refused", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice", {
    expected: [
      /^409 http:\/\/127\.0\.0\.1:\d+\/showcase\/lab\/tasks\/\d+\/title$/,
      "console error: Failed to load resource: the server responded with a status of 409 (Conflict)",
    ],
  });
  const bob = await openLab(viewer, "bob");
  const stamp = Date.now();
  const mine = await addTask(alice, `Mine ${stamp}`);
  const other = await addTask(alice, `Other ${stamp}`);
  await expect(editCard(bob, other)).toBeVisible();

  await editCard(alice, mine).getByRole("button", { name: "Edit" }).click();
  const input = alice.locator("#lab-edit-list form.lab-editor input[name=title]");
  await expect(input).toBeFocused();
  await input.fill(`Mine ${stamp} draft`);
  await input.evaluate((el) => el.setSelectionRange(2, 6));
  await expect(alice.locator("#lab-state-editor")).toHaveAttribute("data-state", "paused");

  // Bob changes the other task while Alice types; her editor region holds it.
  await editCard(bob, other).getByRole("button", { name: "Edit" }).click();
  await bob.locator("#lab-edit-list form.lab-editor input[name=title]").fill(`Other ${stamp} by Bob`);
  await bob.locator("#lab-edit-list form.lab-editor").getByRole("button", { name: "Save" }).click();
  await expect(editCard(bob, `Other ${stamp} by Bob`)).toBeVisible();

  // Focus moves to Save and the pointer is pressed on the input: still held.
  const ended = () => alice.locator("#lab-client-log li", { hasText: "editor: protection ended" }).count();
  const endedBefore = await ended();
  await alice.keyboard.press("Tab");
  await input.click();
  await alice.waitForTimeout(200);
  await expect(editCard(alice, `Other ${stamp} by Bob`)).toHaveCount(0);
  await expect(alice.locator("#lab-state-editor")).toHaveAttribute("data-state", "paused");
  expect(await ended()).toBe(endedBefore);
  await input.evaluate((el) => el.setSelectionRange(2, 6));

  // Leaving the region ends both protections; the snapshot updates the other
  // card and keeps the editor as it was.
  await alice.locator("h1").click();
  await expect(alice.locator("#lab-state-editor")).toHaveText("Up to date");
  await expect(editCard(alice, `Other ${stamp} by Bob`)).toBeVisible();
  await expect(input).toHaveValue(`Mine ${stamp} draft`);
  expect(await input.evaluate((el) => [el.selectionStart, el.selectionEnd])).toEqual([2, 6]);

  // Bob renames Alice's task. Her open editor is not replaced; it says so.
  await editCard(bob, mine).getByRole("button", { name: "Edit" }).click();
  await bob.locator("#lab-edit-list form.lab-editor input[name=title]").fill(`Mine ${stamp} by Bob`);
  await bob.locator("#lab-edit-list form.lab-editor").getByRole("button", { name: "Save" }).click();
  await expect(alice.locator("[data-live-notice]")).toHaveText(/changed on the server/);
  await expect(input).toHaveValue(`Mine ${stamp} draft`);

  // Saving from the old revision is refused and keeps the draft.
  await alice.locator("#lab-edit-list form.lab-editor").getByRole("button", { name: "Save" }).click();
  await expect(alice.locator("#lab-edit-list [role=alert]")).toContainText(`Someone saved “Mine ${stamp} by Bob”`);
  await expect(input).toHaveValue(`Mine ${stamp} draft`);
  await expect(editCard(bob, `Mine ${stamp} by Bob`)).toBeVisible();

  // Saving over theirs is an explicit choice.
  await alice.getByRole("button", { name: "Save mine over theirs" }).click();
  await expect(editCard(alice, `Mine ${stamp} draft`)).toBeVisible();
  await expect(editCard(bob, `Mine ${stamp} draft`)).toBeVisible();
});

test("saving a task deleted meanwhile keeps the draft and does not recreate it", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice", {
    expected: [
      /^404 http:\/\/127\.0\.0\.1:\d+\/showcase\/lab\/tasks\/\d+\/title$/,
      "console error: Failed to load resource: the server responded with a status of 404 (Not Found)",
    ],
  });
  const bob = await openLab(viewer, "bob");
  const title = await addTask(alice, `Doomed ${Date.now()}`);
  await expect(editCard(bob, title)).toBeVisible();

  await editCard(alice, title).getByRole("button", { name: "Edit" }).click();
  const input = alice.locator("#lab-edit-list form.lab-editor input[name=title]");
  await input.fill(`${title} kept`);
  await editCard(bob, title).getByRole("button", { name: "Delete" }).click();
  await expect(editCard(bob, title)).toHaveCount(0);

  await alice.locator("#lab-edit-list form.lab-editor").getByRole("button", { name: "Save" }).click();
  await expect(alice.locator("#lab-edit-list [role=alert]")).toContainText("This task was deleted");
  await expect(input).toHaveValue(`${title} kept`);
  await alice.getByRole("button", { name: "Close" }).click();
  await expect(editCard(alice, title)).toHaveCount(0);
  await expect(editCard(bob, title)).toHaveCount(0);
  await expect(voteCard(bob, title)).toHaveCount(0);
});

test("internal tasks and their actions stay with leads in every region", async ({ viewer }) => {
  const alice = await openLab(viewer, "alice");
  const bob = await openLab(viewer, "bob");
  await expect(voteCard(alice, "Decide the budget")).toBeVisible();
  await expect(editCard(alice, "Decide the budget")).toBeVisible();

  await voteCard(alice, "Decide the budget").getByRole("button", { name: "Vote" }).click();
  await expect(alice.locator("#lab-log-list")).toContainText('Alice voted for "Decide the budget');
  const shared = await addTask(alice, `Shared ${Date.now()}`);
  await expect(bob.locator("#lab-log-list")).toContainText(`Alice added "${shared}"`);

  expect(await bob.content()).not.toContain("Decide the budget");
  const fresh = await openLab(viewer, "bob");
  await expect(fresh.locator("#lab-log-list")).toContainText(`Alice added "${shared}"`);
  expect(await fresh.content()).not.toContain("Decide the budget");
});
