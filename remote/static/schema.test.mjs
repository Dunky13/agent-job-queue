import test from "node:test";
import assert from "node:assert/strict";
import { snapshotSchema, logSchema, csrfSchema, eligible } from "./schema.mjs";

const job = {
  id: 2, state: "queued", title: "A validation batch", project: "project", branch: "main",
  directory: "/example", executable: "sh", priority: 0, queuePosition: 1,
  submittedAt: "2026-10-18T21:06:21.570177+02:00", startedAt: null, finishedAt: null,
  exitCode: null, logPath: "/example/2.log", canReorder: true, canStart: true,
  canCancel: true, phase: "Queued",
};
const snapshot = { updatedAt: "2026-10-18T19:44:39.127731+00:00", queue: { status: "running", limit: 1, running: 0, waiting: 1, legacyOccupied: false }, jobs: [job] };
const host = { sampledAt: 1792340000, totalMemoryBytes: 17179869184, availableMemoryBytes: 4294967296,
  cpuCores: 8, cpuBusyCores: 2.5, memoryPressure: "normal", swapUsedBytes: 1073741824,
  swapInBytesPerSecond: 0, swapOutBytesPerSecond: 0, diskFreeBytes: 21474836480, machine: "arm64", complete: false };
const resources = { prediction: { memoryBytes: 2147483648, cpuCores: 2, durationSeconds: 60,
  samples: 3, confidence: "low", source: "family" }, observed: { memoryBytes: 1073741824,
  peakMemoryBytes: 1610612736, cpuSeconds: 20, cpuCores: 1.5, durationSeconds: 15,
  samples: 5, complete: false }, decision: { allowed: false, reason: "Waiting for memory headroom" } };

test("accepts the real queue timestamp precision and explicit nullable lifecycle fields", () => {
  assert.deepEqual(snapshotSchema.parse(snapshot), snapshot);
});
test("accepts optional scheduler telemetry and typed job resources without inventing missing values", () => {
  assert.equal(snapshotSchema.parse(snapshot).scheduler, undefined);
  assert.equal(snapshotSchema.parse(snapshot).jobs[0].resources, undefined);
  const value = { ...snapshot, scheduler: { mode: "shadow", available: true, reason: "Learning",
    updatedAt: snapshot.updatedAt, host }, jobs: [{ ...job, resources }] };
  assert.deepEqual(snapshotSchema.parse(value), value);
  assert.equal(snapshotSchema.safeParse({ ...value, scheduler: { ...value.scheduler, mode: "auto" } }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...value, scheduler: { ...value.scheduler, host: { ...host, availableMemoryBytes: -1 } } }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...value, jobs: [{ ...job, resources: { ...resources, prediction: { ...resources.prediction, confidence: "certain" } } }] }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...value, jobs: [{ ...job, resources: { ...resources, observed: { ...resources.observed, cpuSeconds: -1 } } }] }).success, false);
  const unavailable = { ...snapshot, scheduler: { mode: "adaptive", available: false, reason: "Sampling failed", updatedAt: null, host: null } };
  assert.deepEqual(snapshotSchema.parse(unavailable), unavailable);
});
test("rejects missing permissions, invalid limits, malformed timestamps, and duplicate IDs", () => {
  assert.equal(snapshotSchema.safeParse({ ...snapshot, queue: { ...snapshot.queue, limit: 9 } }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...snapshot, updatedAt: "yesterday" }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...snapshot, jobs: [job, job] }).success, false);
  assert.equal(snapshotSchema.safeParse({ ...snapshot, jobs: [{ ...job, canStart: undefined }] }).success, false);
});
test("removes undeclared fields instead of exposing unexpected command or environment data", () => {
  const parsed = snapshotSchema.parse({ ...snapshot, environment: { PRIVATE: "value" }, jobs: [{ ...job, argv: ["secret"] }] });
  assert.equal("environment" in parsed, false);
  assert.equal("argv" in parsed.jobs[0], false);
});
test("job controls require both current lifecycle and explicit server permission", () => {
  assert.equal(eligible(job, "start"), true);
  assert.equal(eligible({ ...job, state: "running" }, "start"), false);
  assert.equal(eligible({ ...job, canStart: false }, "start"), false);
  assert.equal(eligible({ ...job, canReorder: false }, "next"), false);
  assert.equal(eligible({ ...job, canCancel: false }, "cancel"), false);
  assert.equal(eligible(null, "cancel"), false);
  assert.equal(eligible(job, "invalid-action"), false);
});
test("bounded log and CSRF contracts reject unexpected or unbounded data", () => {
  assert.deepEqual(logSchema.parse({ output: "<script>text</script>", truncated: true }), { output: "<script>text</script>", truncated: true });
  assert.equal(logSchema.safeParse({ output: "x".repeat(262145), truncated: true }).success, false);
  assert.equal(logSchema.safeParse({ output: "", truncated: "false" }).success, false);
  assert.equal(csrfSchema.safeParse({ token: "" }).success, false);
});

test("learning schemas accept bounded numeric evidence and reject malformed profiles", async () => {
  const { profileCatalogSchema, profileDetailSchema } = await import("./schema.mjs");
  const profile = { id: "a".repeat(64), label: "Browser tests", memberCount: 1, variantCount: 1,
    successCount: 5, incompleteCount: 1, peakMemoryBytes: 1610612736, peakCpuCores: 1.5,
    lastRunAt: 1792404000, memoryFloorBytes: null, cpuFloorCores: 2 };
  const controller = { revision: 4, controllerAppliedRevision: null, controllerStatus: "Pending next fresh pass" };
  const catalog = { ...controller, profiles: [profile], total: 1, offset: 0, limit: 100 };
  assert.deepEqual(profileCatalogSchema.parse(catalog), catalog);
  const detail = { ...controller, profile, members: [{ family: "b".repeat(64), label: "Browser tests", project: "example", executable: "python" }],
    variants: [], runs: [{ id: "c".repeat(64), family: "b".repeat(64), variant: "d".repeat(64), machine: "e".repeat(64),
      sampledAt: 1792404000, memoryBytes: 1610612736, cpuCores: 1.5, cpuSeconds: 20, durationSeconds: 15, success: false }],
    prediction: null, predictionMachine: null, truncated: { members: false, variants: false, runs: false } };
  assert.deepEqual(profileDetailSchema.parse(detail), detail);
  for (const invalid of [{ ...catalog, limit: 101 }, { ...catalog, controllerAppliedRevision: -1 },
    { ...catalog, profiles: [{ ...profile, memoryFloorBytes: -1 }] }, { ...catalog, profiles: [{ ...profile, id: "not-a-hash" }] }]) {
    assert.equal(profileCatalogSchema.safeParse(invalid).success, false);
  }
  assert.equal(profileDetailSchema.safeParse({ ...detail, runs: [{ ...detail.runs[0], success: "false" }] }).success, false);
  assert.equal(profileDetailSchema.safeParse({ ...detail, members: Array(257).fill(detail.members[0]) }).success, false);
  const measurements = { variantCount: 2, successCount: 0, incompleteCount: 1, peakMemoryBytes: 1024, peakCpuCores: 0.5, lastRunAt: null };
  const inspected = { ...detail, members: [{ ...detail.members[0], measurements }],
    memberPage: { total: 1, offset: 0, limit: 256, query: "" }, inspectedFamily: detail.members[0].family };
  assert.deepEqual(profileDetailSchema.parse(inspected), inspected);
  assert.equal(profileDetailSchema.safeParse({ ...inspected, memberPage: { ...inspected.memberPage, limit: 257 } }).success, false);
  assert.equal(profileDetailSchema.safeParse({ ...inspected, members: [{ ...inspected.members[0], measurements: { ...measurements, successCount: -1 } }] }).success, false);
  const learnedJob = { ...job, resources: { ...resources, learning: { id: profile.id, label: profile.label, reason: "Operator shared this family" } } };
  assert.deepEqual(snapshotSchema.parse({ ...snapshot, jobs: [learnedJob] }).jobs[0], learnedJob);
});
