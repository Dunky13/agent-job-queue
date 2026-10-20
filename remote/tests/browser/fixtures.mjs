import { spawn } from "node:child_process";
import { once } from "node:events";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { test as base, expect } from "@playwright/test";
import { z } from "../../static/vendor/zod.mjs";

const readySchema = z.object({ url: z.string().regex(/^http:\/\/127\.0\.0\.1:[1-9]\d{0,4}$/) });
const csrfSchema = z.object({ token: z.string().min(1) });
const eventsSchema = z.object({ events: z.array(z.array(z.string())) });

export const test = base.extend({
  fixtureURL: [async ({ playwright }, use) => {
    expect(playwright.chromium.name()).toBe("chromium");
    const preview = process.env.AGENT_QUEUE_TEST_PREVIEW || fileURLToPath(new URL("../../../build/queue-preview", import.meta.url));
    const child = spawn(preview, [], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stderr = "";
    child.stderr.on("data", (data) => { stderr += data; });
    const lines = createInterface({ input: child.stdout });
    try {
      const ready = await new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error(`Browser fixture startup timed out: ${stderr}`)), 10000);
        const finish = (error, value) => {
          clearTimeout(timeout);
          if (error) reject(error);
          else resolve(value);
        };
        child.once("error", (error) => finish(error));
        child.once("exit", (code) => finish(new Error(`Browser fixture exited (${code}): ${stderr}`)));
        lines.once("line", (line) => {
          try { finish(null, readySchema.parse(JSON.parse(line))); }
          catch (error) { finish(error); }
        });
      });
      await use(ready.url);
    } finally {
      lines.close();
      if (child.pid !== undefined && child.exitCode === null && child.signalCode === null) {
        const exited = once(child, "exit");
        child.kill("SIGTERM");
        const timeout = setTimeout(() => child.kill("SIGKILL"), 3000);
        await exited;
        clearTimeout(timeout);
      }
    }
  }, { scope: "worker" }],
  baseURL: async ({ fixtureURL }, use) => { await use(fixtureURL); },
  queueFixture: async ({ request, fixtureURL }, use) => {
    const response = await request.get(`${fixtureURL}/api/csrf`);
    expect(response.ok()).toBeTruthy();
    const { token } = csrfSchema.parse(await response.json());
    const post = async (path, data = {}) => {
      const result = await request.post(`${fixtureURL}${path}`, {
        data,
        headers: { Origin: fixtureURL, Referer: `${fixtureURL}/`, "X-CSRFToken": token },
      });
      expect(result.ok()).toBeTruthy();
    };
    await post("/__fixture/reset");
    await use({
      update: (data) => post("/__fixture/state", data),
      events: async () => {
        const result = await request.get(`${fixtureURL}/__fixture/events`);
        expect(result.ok()).toBeTruthy();
        return eventsSchema.parse(await result.json()).events;
      },
    });
  },
});

export { expect };
