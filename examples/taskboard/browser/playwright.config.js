import { defineConfig } from "@playwright/test";

// TASKBOARD_BROWSER_CHANNEL=chrome runs the installed Google Chrome instead
// of the Chromium build that matches this Playwright version.
const channel = process.env.TASKBOARD_BROWSER_CHANNEL || undefined;

export default defineConfig({
  testDir: "tests",
  globalSetup: "./server.js",
  fullyParallel: false,
  workers: 1,
  forbidOnly: true,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: "list",
  use: {
    browserName: "chromium",
    channel,
    baseURL: process.env.TASKBOARD_URL,
    trace: "retain-on-failure",
  },
});
