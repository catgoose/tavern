import { test, expect } from "./fixtures.js";

const board = (page) => page.locator("main.collab-board");
const column = (page, name) => page.locator(`#collab-col-${name}`);
const card = (page, title) => board(page).locator("article.task", { hasText: title });
const count = (page, col) => page.locator(`#collab-stats [data-column="${col}"]`);
const countOf = async (page, col) => Number(await count(page, col).textContent());

async function open(viewer, who, options) {
  const page = await viewer(options);
  await page.goto(`/showcase/collaboration?as=${who}`);
  await expect(page.locator("#collab-status .live")).toBeVisible();
  await expect(column(page, "todo")).not.toContainText("Loading");
  await expect(page.locator("#collab-stats strong").first()).toBeAttached();
  return page;
}

// cardsIn lists the task titles in each column, in order.
async function cardsIn(page) {
  return board(page).evaluate((el) =>
    [...el.querySelectorAll('[id^="collab-col-"]')].map((col) =>
      [...col.querySelectorAll("article.task .title")].map((t) => t.firstChild.textContent.trim()),
    ),
  );
}

test("two people share CRUD changes; internal tasks and notices stay with their person", async ({ viewer }) => {
  const alice = await open(viewer, "alice");
  const bob = await open(viewer, "bob");

  await expect(card(alice, "Plan the reorg")).toBeVisible();
  await expect(bob.locator("body")).not.toContainText("Plan the reorg");
  expect(await bob.content()).not.toContain("Plan the reorg");

  // Bob adds a task; the title is shown as text on both pages.
  const raw = "Check <i>escaping</i> & quotes";
  await bob.getByLabel("New task").fill(raw);
  await bob.getByRole("button", { name: "Add task" }).click();
  for (const page of [alice, bob]) {
    await expect(column(page, "todo").locator("article.task").last()).toContainText(raw);
    await expect(board(page).locator("i")).toHaveCount(0);
  }

  // Alice renames it from the editor outside the board.
  await card(alice, raw).getByRole("button", { name: "Edit" }).click();
  await alice.locator("#collab-editor input[name=title]").fill("Check escaping");
  await alice.locator("#collab-editor").getByRole("button", { name: "Save" }).click();
  await expect(alice.locator("#collab-editor")).toBeEmpty();
  for (const page of [alice, bob]) {
    await expect(card(page, "Check escaping")).toBeVisible();
    await expect(board(page)).not.toContainText(raw);
  }

  // Alice moves it and assigns it to Bob. Only Bob gets the notice.
  const doing = await countOf(bob, "doing");
  await card(alice, "Check escaping").getByRole("button", { name: "In progress" }).click();
  await expect(column(bob, "doing")).toContainText("Check escaping");
  await card(alice, "Check escaping").getByRole("button", { name: "Assign Bob" }).click();
  await expect(card(bob, "Check escaping").locator(".assignee")).toHaveText("Bob");
  await expect(bob.locator("#collab-inbox .notice[data-kind=assigned]").first()).toContainText(
    'Alice assigned "Check escaping" to you',
  );
  await expect(alice.locator("#collab-inbox")).not.toContainText("Check escaping");

  // The feed (a topic group) updates counts and activity per role.
  await expect(count(bob, "doing")).toHaveText(String(doing + 1));
  await expect(bob.locator("#collab-activity")).toContainText('Alice moved "Check escaping" to In progress');

  // Alice adds an internal task: Bob's board, counts and activity do not change.
  const [aliceTodo, bobTodo] = [await countOf(alice, "todo"), await countOf(bob, "todo")];
  await alice.getByLabel("New task").fill("Budget review");
  await alice.getByLabel("Internal (leads only)").check();
  await alice.getByRole("button", { name: "Add task" }).click();
  await expect(card(alice, "Budget review")).toBeVisible();
  await expect(count(alice, "todo")).toHaveText(String(aliceTodo + 1));
  await expect(bob.locator("#collab-activity")).not.toContainText("Budget review");
  expect(await countOf(bob, "todo")).toBe(bobTodo);
  await expect(bob.locator("body")).not.toContainText("Budget review");

  // Bob deletes his task on both pages.
  await card(bob, "Check escaping").getByRole("button", { name: "Delete" }).click();
  for (const page of [alice, bob]) await expect(card(page, "Check escaping")).toHaveCount(0);
});

test("clearing finished tasks removes them everywhere, and a new viewer sees the same board", async ({ viewer }) => {
  const alice = await open(viewer, "alice");
  const bob = await open(viewer, "bob");

  await bob.getByLabel("New task").fill("Short-lived");
  await bob.getByRole("button", { name: "Add task" }).click();
  await card(bob, "Short-lived").getByRole("button", { name: "Done" }).click();
  await expect(column(alice, "done")).toContainText("Short-lived");

  await alice.getByRole("button", { name: "Clear finished tasks" }).click();
  for (const page of [alice, bob]) {
    await expect(column(page, "done").locator("article.task")).toHaveCount(0);
  }
  await expect(bob.locator("#collab-activity li").first()).toContainText("Alice cleared");

  const fresh = await open(viewer, "alice");
  expect(await cardsIn(fresh)).toEqual(await cardsIn(alice));
});

test("a reconnecting window rebuilds from the server without duplicates or deleted cards", async ({ viewer }) => {
  // htmx logs the htmx:sseError event when the stream ends.
  const alice = await open(viewer, "alice", { expected: ["console error: [object Event]"] });
  const bob = await open(viewer, "bob");

  await bob.getByLabel("New task").fill("Will be deleted");
  await bob.getByRole("button", { name: "Add task" }).click();
  await bob.getByLabel("New task").fill("Will be renamed");
  await bob.getByRole("button", { name: "Add task" }).click();
  await expect(card(alice, "Will be renamed")).toBeVisible();

  await alice.getByText("Try a reconnect").click();
  await alice.getByRole("button", { name: "Disconnect this window" }).click();
  await expect(alice.locator("#collab-status .down")).toBeVisible();

  await card(bob, "Will be deleted").getByRole("button", { name: "Delete" }).click();
  await card(bob, "Will be renamed").getByRole("button", { name: "Edit" }).click();
  await bob.locator("#collab-editor input[name=title]").fill("Renamed while away");
  await bob.locator("#collab-editor").getByRole("button", { name: "Save" }).click();
  await bob.getByLabel("New task").fill("Added while away");
  await bob.getByRole("button", { name: "Add task" }).click();
  await expect(card(bob, "Added while away")).toBeVisible();
  // Alice's page still shows the old board.
  await expect(card(alice, "Will be deleted")).toBeVisible();

  await alice.getByRole("button", { name: "Reconnect" }).click();
  await expect(alice.locator("#collab-status .live")).toBeVisible({ timeout: 15_000 });
  await expect(card(alice, "Added while away")).toHaveCount(1);
  await expect(card(alice, "Renamed while away")).toHaveCount(1);
  await expect(card(alice, "Will be deleted")).toHaveCount(0);
  await expect(card(alice, "Will be renamed")).toHaveCount(0);

  const fresh = await open(viewer, "alice");
  expect(await cardsIn(alice)).toEqual(await cardsIn(fresh));
});

test("a feed that dropped updates catches up after the board goes quiet", async ({ viewer }) => {
  const alice = await open(viewer, "alice");
  const bob = await open(viewer, "bob");

  await alice.getByText("Try a slow feed").click();
  await alice.getByRole("button", { name: "Pause this window's feed" }).click();

  // More changes than Alice's paused feed can queue, then nothing more.
  const last = await bob.evaluate(async () => {
    let title = "";
    for (let i = 0; i < 40; i++) {
      title = `Burst ${i}`;
      const res = await fetch("/showcase/collaboration/tasks", {
        method: "POST",
        body: new URLSearchParams({ as: "bob", title }),
      });
      if (res.status !== 204) throw new Error(`create returned ${res.status}`);
    }
    return title;
  });
  await expect(card(alice, last)).toBeVisible(); // the board stream is not paused

  await alice.getByRole("button", { name: "Resume the feed" }).click();
  const fresh = await open(viewer, "alice");
  const todo = await countOf(fresh, "todo");
  await expect(count(alice, "todo")).toHaveText(String(todo), { timeout: 10_000 });
  await expect(alice.locator("#collab-activity li").first()).toContainText(`Bob added "${last}"`);
});
