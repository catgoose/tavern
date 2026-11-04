// Builds the task board, runs it on a free local port for the test run, and
// stops it afterwards. Tests reach it through TASKBOARD_URL.
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const root = resolve(import.meta.dirname, "../../..");

function freePort() {
  return new Promise((ok, fail) => {
    const srv = createServer();
    srv.once("error", fail);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => ok(port));
    });
  });
}

async function waitUntilServing(url, app, deadline) {
  while (Date.now() < deadline) {
    if (app.exitCode !== null) throw new Error(`task board exited with ${app.exitCode}`);
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // not listening yet
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`task board did not answer on ${url}`);
}

function stop(app) {
  if (app.exitCode !== null) return Promise.resolve();
  return new Promise((done) => {
    const kill = setTimeout(() => app.kill("SIGKILL"), 10_000);
    app.once("exit", () => {
      clearTimeout(kill);
      done();
    });
    app.kill("SIGTERM");
  });
}

export default async function globalSetup() {
  const work = mkdtempSync(join(tmpdir(), "taskboard-browser-"));
  const bin = join(work, "taskboard");
  execFileSync("go", ["build", "-o", bin, "./examples/taskboard"], { cwd: root, stdio: "inherit" });

  const port = await freePort();
  const url = `http://127.0.0.1:${port}`;
  const app = spawn(bin, ["-addr", `127.0.0.1:${port}`], { stdio: ["ignore", "inherit", "inherit"] });
  try {
    await waitUntilServing(url, app, Date.now() + 15_000);
  } catch (err) {
    await stop(app);
    rmSync(work, { recursive: true, force: true });
    throw err;
  }
  process.env.TASKBOARD_URL = url;

  return async () => {
    await stop(app);
    rmSync(work, { recursive: true, force: true });
  };
}
