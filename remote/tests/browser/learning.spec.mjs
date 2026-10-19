import { test, expect } from "./fixtures.mjs";
import { z } from "../../static/vendor/zod.mjs";

const profileId = "a".repeat(64);
const targetId = "b".repeat(64);
const editSchema = z.object({ revision: z.number().int(), label: z.string(), memoryFloorBytes: z.number().int().nullable(), cpuFloorCores: z.number().nullable() });
async function openLearning(page) {
  await page.goto("/");
  await expect(page.locator("#connection-text")).toContainText("Connected");
  await page.getByRole("button", { name: "Learned workloads", exact: true }).click();
  await expect(page.locator("#profiles-list")).toContainText("Browser tests");
}
async function openProfile(page) {
  await openLearning(page);
  await page.getByRole("button", { name: "Browser tests", exact: true }).click();
  await expect(page.locator("#profile-name")).toHaveText("Browser tests");
}

test("learning exposes measured evidence and saves explicit floors independently of learned peaks", async ({ page, queueFixture }) => {
  await openProfile(page);
  await expect(page.locator("#profiles-controller")).toContainText("Pending controller refresh");
  await expect(page.locator("#profile-detail")).toContainText("5 successful runs · 1 incomplete or failed");
  await expect(page.locator("#profile-detail")).toContainText("20 CPU-seconds");
  await expect(page.locator("#profile-detail")).toContainText("Group estimate");
  await expect(page.locator("#profile-detail")).toContainText("Memory: Automatic · CPU: Automatic");
  await page.getByLabel("Workload name", { exact: true }).fill("Shared browser checks");
  await page.getByLabel("Minimum memory (GiB)", { exact: true }).fill("3");
  await page.getByLabel("Minimum CPU cores", { exact: true }).fill("2.5");
  const pending = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith(`/api/profiles/${profileId}/edit`));
  await page.getByRole("button", { name: "Save corrections", exact: true }).click();
  const sent = editSchema.parse((await pending).postDataJSON());
  expect(sent).toEqual({ revision: 4, label: "Shared browser checks", memoryFloorBytes: 3221225472, cpuFloorCores: 2.5 });
  await expect(page.locator("#profile-name")).toHaveText("Shared browser checks");
  await expect(page.locator("#profile-detail")).toContainText("Measured peaks1.5 GiB");
  await expect(page.locator("#profile-detail")).toContainText("Memory: 3 GiB · CPU: 2.5 cores");
  await page.getByLabel("Minimum memory (GiB)", { exact: true }).fill("");
  await page.getByLabel("Minimum CPU cores", { exact: true }).fill("");
  await page.getByRole("button", { name: "Save corrections", exact: true }).click();
  await expect(page.locator("#profile-detail")).toContainText("Memory: Automatic · CPU: Automatic");
  expect(await queueFixture.events()).toEqual([["profiles", "edit", profileId, "--json"], ["profiles", "edit", profileId, "--json"]]);
  for (const width of [375, 390, 1280]) {
    await page.setViewportSize({ width, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
});

test("queue polls preserve learning drafts and stale revisions require explicit refresh", async ({ page, queueFixture }) => {
  await page.clock.install();
  await openProfile(page);
  let historyRequests = 0;
  page.on("request", (request) => { if (request.url().includes("/api/profiles")) historyRequests += 1; });
  // Finish the optional merge-target lookup before measuring periodic traffic.
  await expect(page.locator("#profile-merge-target option")).toHaveCount(2);
  historyRequests = 0;
  await page.getByLabel("Workload name", { exact: true }).fill("My unsaved correction");
  await page.getByLabel("Workload name", { exact: true }).focus();
  const poll = page.waitForResponse((response) => response.url().endsWith("/api/status"));
  await page.clock.runFor(5100);
  await (await poll).finished();
  await page.clock.runFor(25);
  expect(historyRequests).toBe(0);
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("My unsaved correction");
  await expect(page.getByLabel("Workload name", { exact: true })).toBeFocused();
  await queueFixture.update({ profileRevision: 5 });
  await page.getByRole("button", { name: "Save corrections", exact: true }).click();
  await expect(page.locator("#profiles-error")).toContainText("Learning changed");
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("My unsaved correction");
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button", { name: "Refresh learning", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toContainText("Discard unsaved corrections?");
  await page.getByRole("button", { name: "Keep profile", exact: true }).click();
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("My unsaved correction");
  await page.getByRole("button", { name: "Refresh learning", exact: true }).click();
  await page.getByRole("button", { name: "Discard draft and refresh", exact: true }).click();
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("Browser tests");
  await page.getByLabel("Workload name", { exact: true }).fill("Reviewed correction");
  const sent = page.waitForRequest((request) => request.url().endsWith(`/api/profiles/${profileId}/edit`));
  await page.getByRole("button", { name: "Save corrections", exact: true }).click();
  expect(editSchema.parse((await sent).postDataJSON()).revision).toBe(5);
  await expect(page.locator("#profile-name")).toHaveText("Reviewed correction");
});

test("split, share and reset require confirmation and follow the returned profile identity", async ({ page, queueFixture }) => {
  await openProfile(page);
  await page.getByRole("button", { name: "Separate family", exact: true }).first().click();
  await expect(page.locator("#profile-confirmation")).toContainText("Active jobs continue");
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button", { name: "Separate family", exact: true }).last().click();
  await expect(page.locator("#profile-name")).toHaveText("Browser tests task 1");
  await page.locator("#profile-merge-target").selectOption(targetId);
  await page.getByRole("button", { name: "Share learning…", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toContainText("use its name and corrections");
  await page.getByRole("button", { name: "Keep profile", exact: true }).click();
  expect((await queueFixture.events()).length).toBe(1);
  await page.getByRole("button", { name: "Share learning…", exact: true }).click();
  await page.getByRole("button", { name: "Share learning", exact: true }).click();
  await expect(page.locator("#profile-name")).toHaveText("Client build");
  await page.getByRole("button", { name: "Reset measurements…", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toContainText("This cannot be undone");
  expect((await queueFixture.events()).length).toBe(2);
  await page.getByRole("button", { name: "Reset measurements", exact: true }).click();
  await expect(page.locator("#profile-detail")).toContainText("No retained measurements");
  expect(await queueFixture.events()).toEqual([["profiles", "split", profileId, "--json"], ["profiles", "merge", "3".repeat(64), "--json"], ["profiles", "reset", targetId, "--json"]]);
});

test("job details link the matching profile and unavailable or malformed learning remains actionable", async ({ page, queueFixture }) => {
  await queueFixture.update({ resources: { prediction: { memoryBytes: 2147483648, cpuCores: 2, durationSeconds: null, samples: 0, confidence: "unknown", source: "prior" }, observed: null, decision: null, learning: { id: profileId, label: "Browser tests", reason: "Operator shared these job families" } } });
  await page.goto("/");
  await page.locator('[data-focus-key="102-details"]').click();
  await expect(page.locator("#detail-profile-reason")).toHaveText("Operator shared these job families");
  await page.getByRole("button", { name: "View learned workload: Browser tests", exact: true }).click();
  await expect(page.locator("#profile-name")).toHaveText("Browser tests");
  await page.getByRole("button", { name: "‹ All learned workloads", exact: true }).click();
  await page.route("**/api/profiles?*", (route) => route.fulfill({ contentType: "application/json", body: '{"profiles":"malformed"}' }), { times: 1 });
  await page.getByRole("button", { name: "Refresh learning", exact: true }).click();
  await expect(page.locator("#profiles-error")).toContainText("unexpected response");
  await queueFixture.update({ profiles: {} });
  await page.getByRole("button", { name: "Refresh learning", exact: true }).click();
  await expect(page.locator("#profiles-list")).toContainText("Your Mac has not learned any workloads yet");
  await expect(page.locator("#profiles-error")).toBeHidden();
});

test("learning catalog searches and pages through bounded responses", async ({ page, queueFixture }) => {
  await queueFixture.update({ profileCount: 101 });
  await page.goto("/");
  await page.getByRole("button", { name: "Learned workloads", exact: true }).click();
  await expect(page.locator("#profiles-count")).toHaveText("1–100 of 101 workloads");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(page.locator("#profiles-count")).toHaveText("101–101 of 101 workloads");
  await expect(page.getByRole("button", { name: "Next", exact: true })).toBeDisabled();
  await page.getByLabel("Find a workload", { exact: true }).fill("Workload 100");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect(page.locator("#profiles-count")).toHaveText("1–1 of 1 workloads");
  await expect(page.locator("#profiles-list .job-card")).toHaveCount(1);
});

test("renaming preserves valid tiny and large exact stored reservation floors", async ({ page, queueFixture }) => {
  for (const floors of [{ memoryFloorBytes: 1, cpuFloorCores: 1e-20 }, { memoryFloorBytes: 2 ** 50 - 1, cpuFloorCores: 0.00000000000000000001 }]) {
    await queueFixture.update({ profileFloors: floors });
    await openProfile(page);
    await expect(page.locator("#profile-detail")).toContainText("1.00e-20 cores");
    if (floors.memoryFloorBytes === 1) await expect(page.locator("#profile-detail")).toContainText("1 bytes");
    await page.getByLabel("Workload name", { exact: true }).fill("Renamed without rounding");
    const pending = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith(`/api/profiles/${profileId}/edit`));
    await page.getByRole("button", { name: "Save corrections", exact: true }).click();
    const sent = editSchema.parse((await pending).postDataJSON());
    expect(sent.memoryFloorBytes).toBe(floors.memoryFloorBytes);
    expect(sent.cpuFloorCores).toBe(floors.cpuFloorCores);
    await expect(page.locator("#profile-name")).toHaveText("Renamed without rounding");
    // Restore only the fixture name so the next iteration can use the same entry.
    await page.getByLabel("Workload name", { exact: true }).fill("Browser tests");
    await page.getByRole("button", { name: "Save corrections", exact: true }).click();
    await expect(page.locator("#profile-name")).toHaveText("Browser tests");
  }
  await page.getByLabel("Minimum CPU cores", { exact: true }).fill("0");
  await page.getByRole("button", { name: "Save corrections", exact: true }).click();
  await expect(page.locator("#profiles-error")).toContainText("positive reservation floors");
  expect((await queueFixture.events()).length).toBe(4);
});

test("refresh cancels a destructive confirmation and a renewed confirmation uses the reviewed revision", async ({ page, queueFixture }) => {
  await openProfile(page);
  await expect(page.locator("#profile-target-query")).toHaveAttribute("maxlength", "120");
  await page.getByRole("button", { name: "Reset measurements…", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toBeVisible();
  await queueFixture.update({ profileRevision: 5 });
  await page.getByRole("button", { name: "Refresh learning", exact: true }).click();
  await expect(page.locator("#profiles-controller")).toContainText("saved revision 5");
  await expect(page.locator("#profile-confirmation")).toBeHidden();
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button", { name: "Reset measurements…", exact: true }).click();
  const pending = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith(`/api/profiles/${profileId}/reset`));
  await page.getByRole("button", { name: "Reset measurements", exact: true }).click();
  expect(z.object({ revision: z.number().int(), confirm: z.literal(true) }).parse((await pending).postDataJSON())).toEqual({ revision: 5, confirm: true });
  await expect(page.locator("#profile-detail")).toContainText("No retained measurements");
});
