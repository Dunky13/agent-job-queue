import assert from "node:assert/strict";
import test from "node:test";
import { parseLatest } from "../../site/download.js";

const base = "https://github.com/Dunky13/agent-job-queue/releases/download/v1.2.3/";
const valid = () => ({
  schemaVersion: 1,
  version: "1.2.3",
  build: 45,
  commit: "a".repeat(40),
  tag: "v1.2.3",
  publishedAt: "2026-11-01T10:00:00Z",
  releaseUrl: "https://github.com/Dunky13/agent-job-queue/releases/tag/v1.2.3",
  dmgUrl: `${base}Agent-Queue-1.2.3-universal.dmg`,
  dmgSha256: "b".repeat(64),
  zipUrl: `${base}Agent-Queue-1.2.3-universal.zip`,
  zipSha256: "c".repeat(64),
});

test("accepts a valid manifest", () => {
  const v = valid();
  assert.deepEqual(parseLatest(v), { version: "1.2.3", build: "45", dmgUrl: v.dmgUrl });
});

test("rejects non-objects and wrong schema versions", () => {
  for (const bad of [null, undefined, [], "x", 1, { ...valid(), schemaVersion: 2 }, { ...valid(), schemaVersion: "1" }]) {
    assert.equal(parseLatest(bad), null);
  }
});

test("rejects missing or mistyped fields", () => {
  for (const field of Object.keys(valid())) {
    const missing = valid();
    delete missing[field];
    assert.equal(parseLatest(missing), null, `missing ${field}`);
  }
  for (const [field, value] of [["version", 1], ["tag", ""], ["build", 0], ["build", 1.5], ["build", "45"], ["build", "x"], ["build", null], ["dmgSha256", "abc"], ["zipSha256", "G".repeat(64)]]) {
    assert.equal(parseLatest({ ...valid(), [field]: value }), null, `${field}=${value}`);
  }
});

test("rejects URLs outside https github.com releases", () => {
  const bad = [
    "http://github.com/Dunky13/agent-job-queue/releases/x.dmg",
    "https://evil.example/Dunky13/agent-job-queue/releases/x.dmg",
    "https://github.com.evil.example/Dunky13/agent-job-queue/releases/x.dmg",
    "https://user@github.com/Dunky13/agent-job-queue/releases/x.dmg",
    "https://github.com:8443/Dunky13/agent-job-queue/releases/x.dmg",
    "https://github.com/Other/agent-job-queue/releases/x.dmg",
    "https://github.com/Dunky13/agent-job-queue/archive/x.dmg",
    "javascript:alert(1)",
    "not a url",
  ];
  for (const field of ["dmgUrl", "zipUrl", "releaseUrl"]) {
    for (const url of bad) assert.equal(parseLatest({ ...valid(), [field]: url }), null, `${field} ${url}`);
  }
});
