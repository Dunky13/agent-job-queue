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

test("accepts the real queue timestamp precision and explicit nullable lifecycle fields", () => {
  assert.deepEqual(snapshotSchema.parse(snapshot), snapshot);
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
