import { test, expect } from "./fixtures.mjs";

async function openQueue(page) {
  await page.goto("/");
  await expect(page.locator("#connection-text")).toHaveText("Connected · Updates every 5 seconds");
  await expect(page.locator("#waiting-jobs .job-card")).toHaveCount(2);
}

async function backgroundPoll(page) {
  const [response] = await Promise.all([
    page.waitForResponse((response) => response.url().endsWith("/api/status")),
    page.clock.runFor(5100),
  ]);
  // Flush body decoding and render microtasks without another poll or a
  // manual-refresh DOM change.
  await response.finished();
  await page.clock.runFor(25);
}

test("unchanged background polls preserve DOM, focus and a pending limit choice", async ({ page, queueFixture }) => {
  await page.clock.install();
  await openQueue(page);
  await page.locator("#limit").selectOption("2");
  await page.locator("#limit").focus();
  await page.evaluate(() => {
    window.fixtureMutations = [];
    window.fixtureStartButton = document.querySelector('[data-focus-key="102-start"]');
    window.fixtureObserver = new MutationObserver((records) => {
      window.fixtureMutations.push(...records.map((record) => `${record.type}:${record.target.nodeName}`));
    });
    window.fixtureObserver.observe(document.body, { attributes: true, childList: true, characterData: true, subtree: true });
  });
  await backgroundPoll(page);
  await backgroundPoll(page);
  expect(await page.evaluate(() => window.fixtureMutations)).toEqual([]);
  expect(await page.evaluate(() => window.fixtureStartButton === document.querySelector('[data-focus-key="102-start"]'))).toBe(true);
  await expect(page.locator("#limit")).toHaveValue("2");
  await expect(page.locator("#limit")).toBeFocused();
  await expect(page.locator("#apply-limit")).toBeEnabled();
  expect(await queueFixture.events()).toEqual([]);
});

test("real queue changes arrive on a background poll and mobile layout fits", async ({ page, queueFixture }) => {
  await page.clock.install();
  await openQueue(page);
  await queueFixture.update({ title: "Updated test batch" });
  await backgroundPoll(page);
  await expect(page.locator("#waiting-jobs")).toContainText("Updated test batch");
  for (const width of [375, 390, 1280]) {
    await page.setViewportSize({ width, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  }
  expect(await queueFixture.events()).toEqual([]);
});

test("start confirmation declines safely and accepted start runs exactly once in parallel", async ({ page, queueFixture }) => {
  await openQueue(page);
  const start = page.locator('[data-focus-key="102-start"]');
  await start.click();
  await expect(page.locator("#confirmation")).toBeVisible();
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button", { name: "Keep waiting", exact: true }).click();
  await expect(page.locator("#confirmation")).toBeHidden();
  expect(await queueFixture.events()).toEqual([]);
  await start.click();
  await page.getByRole("button", { name: "Start in parallel", exact: true }).click();
  await expect(page.locator("#running-count")).toHaveText("2");
  await expect(page.locator("#limit-count")).toHaveText("1");
  await expect(page.locator("#confirmation")).toBeHidden();
  expect(await queueFixture.events()).toEqual([["start", "102"]]);
});

test("details show metadata and actual log output, and back clears a pending confirmation", async ({ page, queueFixture }) => {
  await openQueue(page);
  await page.locator('[data-focus-key="102-start"]').click();
  await page.locator('[data-focus-key="102-details"]').click();
  await expect(page.locator("#detail-view")).toBeVisible();
  await expect(page.locator("#detail-title")).toHaveText("Run browser tests");
  await expect(page.locator("#detail-metadata")).toContainText("example-project");
  await expect(page.locator("#log-output")).toContainText("Passed fixture step");
  await page.getByRole("button", { name: "Refresh output", exact: true }).click();
  await expect(page.locator("#log-output")).toContainText("Starting Run browser tests");
  await page.locator("#back").click();
  await expect(page.locator("#queue-view")).toBeVisible();
  await expect(page.locator("#detail-view")).toBeHidden();
  await expect(page.locator("#confirmation")).toBeHidden();
  await expect(page.locator('[data-focus-key="102-details"]')).toBeFocused();
  expect(await queueFixture.events()).toEqual([]);
});

test("failed polls retain data, disable mutations and recover without sending stale actions", async ({ page, queueFixture }) => {
  await page.clock.install();
  await openQueue(page);
  await queueFixture.update({ offline: true });
  await backgroundPoll(page);
  await expect(page.locator("#error")).toContainText("Showing the last received data");
  await expect(page.locator("#running-count")).toHaveText("1");
  await expect(page.locator("#limit")).toBeDisabled();
  await expect(page.locator('[data-focus-key="102-start"]')).toBeDisabled();
  await expect(page.locator('[data-focus-key="101-cancel"]')).toBeDisabled();
  await expect(page.locator('[data-focus-key="102-details"]')).toBeEnabled();
  await queueFixture.update({ offline: false });
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.locator("#error")).toBeHidden();
  await expect(page.locator('[data-focus-key="102-start"]')).toBeEnabled();
  expect(await queueFixture.events()).toEqual([]);
});

test("limit, queue order and confirmed cancellation call only their selected controls", async ({ page, queueFixture }) => {
  await openQueue(page);
  await page.locator("#limit").selectOption("2");
  await page.getByRole("button", { name: "Apply", exact: true }).click();
  await expect(page.locator("#limit-count")).toHaveText("2");
  await page.locator('[data-focus-key="103-next"]').click();
  await expect(page.locator("#waiting-jobs .job-name").first()).toHaveText("Typecheck client");
  await page.locator('[data-focus-key="101-cancel"]').click();
  await page.getByRole("button", { name: "Keep job", exact: true }).click();
  expect(await queueFixture.events()).toEqual([["limit", "2"], ["next", "103"]]);
  await page.locator('[data-focus-key="101-cancel"]').click();
  await page.getByRole("button", { name: "Cancel job", exact: true }).click();
  await expect(page.locator("#running-count")).toHaveText("0");
  expect(await queueFixture.events()).toEqual([["limit", "2"], ["next", "103"], ["cancel", "101"]]);
});

const scheduler = { mode: "shadow", available: true, reason: "Learning resource profiles", updatedAt: "2026-10-19T10:00:00Z",
  host: { sampledAt: 1792404000, totalMemoryBytes: 17179869184, availableMemoryBytes: 4294967296,
    cpuCores: 8, cpuBusyCores: 2.5, memoryPressure: "warning", swapUsedBytes: 1073741824,
    swapInBytesPerSecond: 0, swapOutBytesPerSecond: 0, diskFreeBytes: 21474836480, machine: "arm64", complete: false } };
const resources = { prediction: { memoryBytes: 2147483648, cpuCores: 2, durationSeconds: 60,
  samples: 3, confidence: "low", source: "family" }, observed: { memoryBytes: 1073741824,
  peakMemoryBytes: 1610612736, cpuSeconds: 20, cpuCores: 1.5, durationSeconds: 15,
  samples: 5, complete: false }, decision: { allowed: false, reason: "Waiting for memory headroom" } };

test("missing telemetry remains unavailable and disables mode changes", async ({ page }) => {
  await openQueue(page);
  await expect(page.locator("#scheduler-status")).toContainText("Resource monitoring unavailable");
  await expect(page.locator("#host-status")).toHaveText("Host measurements unavailable.");
  await expect(page.getByRole("radio", { name: "Adaptive", exact: true })).toBeDisabled();
});

test("resource telemetry, admission reasons and mode controls work on mobile and desktop", async ({ page, queueFixture }) => {
  await queueFixture.update({ scheduler, resources });
  await page.clock.install();
  await openQueue(page);
  await expect(page.locator("#scheduler-status")).toContainText("Learn only");
  await expect(page.locator("#host-status")).toContainText("Memory pressure warning");
  await expect(page.locator("#host-status")).toContainText("Collecting rates");
  await expect(page.locator("#waiting-jobs")).toContainText("low confidence (3 runs, family)");
  await expect(page.locator("#waiting-jobs")).toContainText("peak 1.5 GiB");
  await expect(page.locator("#waiting-jobs")).toContainText("Waiting for memory headroom");
  await page.getByRole("radio", { name: "Adaptive", exact: true }).check();
  await page.evaluate(() => {
    window.fixtureTelemetryMutations = [];
    window.fixtureTelemetryObserver = new MutationObserver((records) => window.fixtureTelemetryMutations.push(...records.map((record) => record.type)));
    window.fixtureTelemetryObserver.observe(document.body, { attributes: true, childList: true, characterData: true, subtree: true });
  });
  await queueFixture.update({ scheduler: { ...scheduler, updatedAt: "2026-10-19T10:00:05Z", host: { ...scheduler.host, sampledAt: scheduler.host.sampledAt + 5 } } });
  await backgroundPoll(page);
  expect(await page.evaluate(() => window.fixtureTelemetryMutations)).toEqual([]);
  await expect(page.getByRole("radio", { name: "Adaptive", exact: true })).toBeChecked();
  await page.getByRole("button", { name: "Set mode", exact: true }).click();
  await expect(page.locator("#scheduler-status")).toContainText("Adaptive");
  expect(await queueFixture.events()).toEqual([["scheduling", "adaptive"]]);
  for (const width of [375, 390, 1280]) {
    await page.setViewportSize({ width, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  }
  await page.locator('[data-focus-key="102-details"]').click();
  await expect(page.locator("#detail-metadata")).toContainText("20.0 CPU-seconds");
  await expect(page.locator("#detail-metadata")).toContainText("Waiting for memory headroom");
});

test("unavailable sampler permits fixed and learning while rejecting adaptive honestly", async ({ page, queueFixture }) => {
  await queueFixture.update({ scheduler: { ...scheduler, mode: "fixed", available: false, reason: "Sampling failed", host: null } });
  await openQueue(page);
  await expect(page.locator("#scheduler-status")).toContainText("Sampling failed");
  await page.getByRole("radio", { name: "Learn only", exact: true }).check();
  await page.getByRole("button", { name: "Set mode", exact: true }).click();
  await expect(page.locator("#scheduler-status")).toContainText("Learn only");
  await page.getByRole("radio", { name: "Adaptive", exact: true }).check();
  await page.getByRole("button", { name: "Set mode", exact: true }).click();
  await expect(page.locator("#error")).toContainText("Resource sampler unavailable");
  await expect(page.locator("#scheduler-status")).toContainText("Learn only");
});
