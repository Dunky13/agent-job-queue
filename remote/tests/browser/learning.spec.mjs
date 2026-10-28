import { test, expect } from "./fixtures.mjs";
import { z } from "../../static/vendor/zod.mjs";
import { profileDetailSchema } from "../../static/schema.mjs";

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
  await page.locator(".profile-member summary").first().click();
  await page.getByRole("button", { name: "Ungroup workload…", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toContainText("Active jobs continue");
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button", { name: "Ungroup workload", exact: true }).click();
  await expect(page.getByRole("button", { name: "‹ Back to group: Browser tests", exact: true })).toBeVisible();
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
  await expect(page.locator("#profile-members")).not.toContainText("5 successful");
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

test("member inspection is read only, distinguishes empty history and returns to the group after ungrouping", async ({ page, queueFixture }) => {
  await openProfile(page);
  const members = page.locator("#profile-members");
  await expect(members).toContainText("5 successful · 1 incomplete or failed");
  const first = members.locator(".profile-member").first();
  const second = members.locator(".profile-member").nth(1);
  await first.locator(":scope > summary").click();
  await expect(first).toContainText("Measurements for this workload");
  await expect(first.locator(".measurement")).toHaveCount(1);
  await second.locator(":scope > summary").click();
  await expect(first).not.toHaveAttribute("open");
  await expect(second).toContainText("No retained measurements for this workload yet.");
  await expect(second.locator(".measurement")).toHaveCount(0);
  expect(await queueFixture.events()).toEqual([]);
  await second.getByRole("button", { name: "Ungroup workload…", exact: true }).click();
  await expect(page.locator("#profile-confirmation")).toContainText("all of its variants and measurements");
  await expect(page.locator("#profile-confirmation")).toContainText("excluded from automatic grouping");
  await page.getByRole("button", { name: "Keep profile", exact: true }).click();
  await expect(second.getByRole("button", { name: "Ungroup workload…", exact: true })).toBeFocused();
  expect(await queueFixture.events()).toEqual([]);
  await second.getByRole("button", { name: "Ungroup workload…", exact: true }).click();
  await page.getByRole("button", { name: "Ungroup workload", exact: true }).click();
  await expect(page.locator("#profile-name")).toHaveText("Browser tests task 2");
  await expect(page.locator("#profile-detail")).toContainText("0 successful runs · 0 incomplete or failed");
  await page.getByRole("button", { name: "‹ Back to group: Browser tests", exact: true }).click();
  await expect(page.locator("#profile-name")).toHaveText("Browser tests");
  await expect(members.locator(".profile-member")).toHaveCount(1);
  await expect(page.locator("#profile-detail")).toContainText("5 successful runs · 1 incomplete or failed");
  expect(await queueFixture.events()).toEqual([["profiles", "split", profileId, "--json"]]);
});

test("member reads fail visibly, retry and reject changed revisions without losing a draft", async ({ page, queueFixture }) => {
  await openProfile(page);
  const first = page.locator(".profile-member").first();
  await page.getByLabel("Workload name", { exact: true }).fill("Unsaved correction");
  await page.route(`**/api/profiles/${profileId}?family=*`, (route) => route.fulfill({ status: 503, json: { error: "Inspection unavailable" } }));
  await first.locator(":scope > summary").click();
  await expect(first).toContainText("Inspection unavailable");
  await expect(first.getByRole("button", { name: "Ungroup workload…", exact: true })).toHaveCount(0);
  await page.unroute(`**/api/profiles/${profileId}?family=*`);
  await first.getByRole("button", { name: "Retry inspection", exact: true }).click();
  await expect(first).toContainText("Measurements for this workload");
  await first.locator(":scope > summary").click();
  await queueFixture.update({ profileRevision: 5 });
  await first.locator(":scope > summary").click();
  await expect(first).toContainText("This group changed. Refresh learning");
  await expect(first.getByRole("button", { name: "Ungroup workload…", exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("Unsaved correction");
  expect(await queueFixture.events()).toEqual([]);
});

test("large groups page and search across all members while preserving corrections", async ({ page, queueFixture }) => {
  await queueFixture.update({ profileMemberCount: 300 });
  await openProfile(page);
  await expect(page.locator(".profile-member")).toHaveCount(256);
  await expect(page.locator("#profile-members")).toContainText("1–256 of 300 members");
  await expect(page.getByRole("button", { name: "Previous members", exact: true })).toBeDisabled();
  await page.getByLabel("Workload name", { exact: true }).fill("Unsaved group correction");
  await page.getByRole("button", { name: "Next members", exact: true }).click();
  await expect(page.locator(".profile-member")).toHaveCount(44);
  await expect(page.locator("#profile-members")).toContainText("257–300 of 300 members");
  await expect(page.getByRole("button", { name: "Next members", exact: true })).toBeDisabled();
  await page.getByLabel("Find a member in this group", { exact: true }).fill("Member 299");
  await page.getByRole("button", { name: "Search members", exact: true }).click();
  await expect(page.locator(".profile-member")).toHaveCount(1);
  await expect(page.locator(".profile-member summary")).toContainText("Member 299");
  await expect(page.getByLabel("Workload name", { exact: true })).toHaveValue("Unsaved group correction");
  await expect(page.getByLabel("Find a member in this group", { exact: true })).toBeFocused();
  expect(await queueFixture.events()).toEqual([]);
});

test("a late member response cannot overwrite another member's inspection", async ({ page, queueFixture, request, baseURL }) => {
  await openProfile(page);
  const family = "c".repeat(64);
  const response = await request.get(`${baseURL}/api/profiles/${profileId}?family=${family}`);
  const detail = profileDetailSchema.parse(await response.json());
  let release;
  let seen;
  const held = new Promise((resolve) => { release = resolve; });
  const started = new Promise((resolve) => { seen = resolve; });
  await page.route(`**/api/profiles/${profileId}?family=${family}`, async (route) => {
    seen();
    await held;
    await route.fulfill({ json: detail });
  });
  const first = page.locator(".profile-member").first();
  const second = page.locator(".profile-member").nth(1);
  try {
    await first.locator(":scope > summary").click();
    await started;
    await second.locator(":scope > summary").click();
    await expect(second).toContainText("No retained measurements for this workload yet.");
  } finally { release(); }
  await expect(first).not.toHaveAttribute("open");
  await expect(second).toContainText("0 successful · 0 incomplete or failed");
  await expect(second.locator(".measurement")).toHaveCount(0);
  expect(await queueFixture.events()).toEqual([]);
});

const combineSchema = z.object({ revision: z.number().int(), profileIds: z.array(z.string().regex(/^[a-f0-9]{64}$/)), families: z.array(z.string().regex(/^[a-f0-9]{64}$/)) });
const fixtureGroup = (number) => number.toString(16).padStart(64, "0");
async function openGeneratedLearning(page) {
  await page.goto("/");
  await page.getByRole("button", {name:"Learned workloads",exact:true}).click();
  await expect(page.getByRole("button", {name:"Workload 0",exact:true})).toBeVisible();
}

test("several groups merge in one confirmed transaction and retain destination identity", async ({ page, queueFixture }) => {
  await queueFixture.update({profileCount:3});
  await openGeneratedLearning(page);
  for (let index=0;index<3;index++) await page.getByRole("checkbox",{name:`Select group: Workload ${index}`,exact:true}).check();
  const panel=page.locator("#profile-combine");
  await expect(panel).toContainText("3 selected");
  await page.getByLabel("Destination group",{exact:true}).selectOption(fixtureGroup(2));
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  await expect(panel.locator(".confirmation")).toContainText("highest reservation floors");
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button",{name:"Keep selection",exact:true}).click();
  await expect(page.getByRole("checkbox",{name:"Select group: Workload 0",exact:true})).toBeChecked();
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  const pending=page.waitForRequest(r=>r.method()==="POST"&&r.url().endsWith(`/api/profiles/${fixtureGroup(2)}/combine`));
  await page.getByRole("button",{name:"Merge selected items",exact:true}).click();
  expect(combineSchema.parse((await pending).postDataJSON())).toEqual({revision:4,profileIds:[fixtureGroup(0),fixtureGroup(1)],families:[]});
  await expect(page.locator("#profile-name")).toHaveText("Workload 2");
  await expect(page.locator(".profile-member")).toHaveCount(6);
  await expect(page.locator("#profile-detail")).toContainText("15 successful runs");
  await expect(panel).toBeHidden();
  expect(await queueFixture.events()).toEqual([["profiles","combine",fixtureGroup(2),"--json"]]);
  for (const width of [375,390,1280]) {await page.setViewportSize({width,height:844});expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);}
});

test("a run plus a group moves the workload history and leaves its sibling independent", async ({ page, queueFixture }) => {
  await openProfile(page);
  await page.getByRole("button",{name:"Select this run’s workload",exact:true}).click();
  await expect(page.locator("#profile-combine")).toContainText("1 selected");
  await page.getByRole("button",{name:"‹ All learned workloads",exact:true}).click();
  await page.getByRole("checkbox",{name:"Select group: Client build",exact:true}).check();
  await page.getByLabel("Destination group",{exact:true}).selectOption(targetId);
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  const pending=page.waitForRequest(r=>r.method()==="POST"&&r.url().endsWith(`/api/profiles/${targetId}/combine`));
  await page.getByRole("button",{name:"Merge selected items",exact:true}).click();
  expect(combineSchema.parse((await pending).postDataJSON())).toEqual({revision:4,profileIds:[],families:["c".repeat(64)]});
  await expect(page.locator("#profile-name")).toHaveText("Client build");
  await expect(page.locator(".profile-member")).toHaveCount(2);
  await expect(page.locator("#profile-detail")).toContainText("10 successful runs");
  await page.getByRole("button",{name:"‹ All learned workloads",exact:true}).click();
  await page.getByRole("button",{name:"Browser tests",exact:true}).click();
  await expect(page.locator(".profile-member")).toHaveCount(1);
  await expect(page.locator("#profile-members")).toContainText("Browser tests task 2");
  await expect(page.locator("#profile-detail")).toContainText("0 successful runs");
  expect(await queueFixture.events()).toEqual([["profiles","combine",targetId,"--json"]]);
});

test("bulk selections survive catalog pagination and find a destination beyond the first page", async ({ page, queueFixture }) => {
  await queueFixture.update({profileCount:101});
  await openGeneratedLearning(page);
  await page.getByRole("checkbox",{name:"Select group: Workload 0",exact:true}).check();
  await page.getByRole("button",{name:"Next",exact:true}).click();
  await page.getByRole("checkbox",{name:"Select group: Workload 100",exact:true}).check();
  await expect(page.locator("#profile-combine")).toContainText("2 selected");
  await page.getByLabel("Find a destination group",{exact:true}).fill("Workload 100");
  await page.getByRole("button",{name:"Find destination groups",exact:true}).click();
  await page.getByLabel("Destination group",{exact:true}).selectOption(fixtureGroup(100));
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  await page.getByRole("button",{name:"Merge selected items",exact:true}).click();
  await expect(page.locator("#profile-name")).toHaveText("Workload 100");
  await expect(page.locator(".profile-member")).toHaveCount(4);
  expect(await queueFixture.events()).toEqual([["profiles","combine",fixtureGroup(100),"--json"]]);
});

test("stale bulk merges preserve the selection and never apply partially", async ({ page, queueFixture }) => {
  await openLearning(page);
  await page.getByRole("checkbox",{name:"Select group: Browser tests",exact:true}).check();
  await page.getByLabel("Destination group",{exact:true}).selectOption(targetId);
  await page.getByRole("checkbox",{name:"Select group: Client build",exact:true}).check();
  await queueFixture.update({profileRevision:5});
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  await page.getByRole("button",{name:"Merge selected items",exact:true}).click();
  await expect(page.locator("#profiles-error")).toContainText("Learning changed");
  await expect(page.locator("#profile-combine")).toContainText("2 selected");
  await expect(page.getByRole("checkbox",{name:"Select group: Browser tests",exact:true})).toBeChecked();
  expect(await queueFixture.events()).toEqual([]);
  await page.getByRole("button",{name:"Clear selection",exact:true}).click();
  await expect(page.locator("#profile-combine")).toBeHidden();
});

test("long selected group names wrap in the merge review on a narrow phone", async ({ page, queueFixture }) => {
  const label="x".repeat(120);
  await queueFixture.update({profileFloors:{label}});
  await page.setViewportSize({width:375,height:844});
  await page.goto("/");
  await page.getByRole("button",{name:"Learned workloads",exact:true}).click();
  await page.getByRole("checkbox",{name:`Select group: ${label}`,exact:true}).check();
  await page.getByLabel("Destination group",{exact:true}).selectOption(targetId);
  await page.getByRole("button",{name:"Merge selected…",exact:true}).click();
  await expect(page.locator("#profile-combine .confirmation")).toContainText(label);
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
  expect(await queueFixture.events()).toEqual([]);
});
