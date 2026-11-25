import assert from "node:assert/strict";
import test from "node:test";
import { enhance, parseLatest } from "../../site/download.js";

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

test("rejects URLs outside the exact GitHub release paths", () => {
  const dl = "/Dunky13/agent-job-queue/releases/download/v1/x";
  const bad = [
    `http://github.com${dl}.dmg`,
    `https://evil.example${dl}.dmg`,
    `https://github.com.evil.example${dl}.dmg`,
    `https://user@github.com${dl}.dmg`,
    `https://github.com:8443${dl}.dmg`,
    "https://github.com/Other/agent-job-queue/releases/download/v1/x.dmg",
    "https://github.com/dunky13/agent-job-queue/releases/download/v1/x.dmg",
    "https://github.com/Dunky13/agent-job-queue/archive/x.dmg",
    "https://github.com/Dunky13/agent-job-queue/releases/tag/v1/x.dmg",
    `https://github.com${dl}.zip`,
    "javascript:alert(1)",
    "not a url",
  ];
  for (const url of bad) assert.equal(parseLatest({ ...valid(), dmgUrl: url }), null, `dmgUrl ${url}`);
  assert.equal(parseLatest({ ...valid(), zipUrl: `https://github.com${dl}.dmg` }), null, "zip must end in .zip");
  assert.equal(parseLatest({ ...valid(), releaseUrl: `https://github.com${dl}.zip` }), null, "release must be under tag/");
  assert.equal(parseLatest({ ...valid(), releaseUrl: "https://evil.example/Dunky13/agent-job-queue/releases/tag/v1" }), null);
});

const response = (body, ok = true) => ({ ok, json: async () => body });
const releases = "https://github.com/Dunky13/agent-job-queue/releases";
const fresh = () => ({ button: { href: releases }, note: { textContent: "macOS 15 or later." } });

test("enhance keeps the default link and note on every failure", async () => {
  const fetchers = [
    async () => { throw new Error("offline"); },
    async () => response(valid(), false),
    async () => ({ ok: true, json: async () => { throw new SyntaxError("bad json"); } }),
    async () => response({ ...valid(), schemaVersion: 2 }),
    async () => response([]),
  ];
  for (const fetcher of fetchers) {
    const { button, note } = fresh();
    await enhance(button, note, fetcher);
    assert.equal(button.href, releases);
    assert.equal(note.textContent, "macOS 15 or later.");
  }
});

test("enhance points the button at the DMG and keeps the requirements", async () => {
  const { button, note } = fresh();
  await enhance(button, note, async () => response(valid()));
  assert.equal(button.href, valid().dmgUrl);
  assert.equal(note.textContent, "macOS 15 or later. Version 1.2.3 (build 45).");
});
