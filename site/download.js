// Points the download button at the latest DMG named by same-origin latest.json.
// Any failure leaves the default releases link in place.
const RELEASES_PREFIX = "/Dunky13/agent-job-queue/releases/";
const STRING_FIELDS = ["version", "commit", "tag", "publishedAt", "releaseUrl", "dmgUrl", "dmgSha256", "zipUrl", "zipSha256"];

function isReleaseUrl(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname === "github.com" && url.username === "" && url.password === "" && url.port === "" && url.pathname.startsWith(RELEASES_PREFIX);
  } catch {
    return false;
  }
}

// Returns { version, build, dmgUrl } for a valid manifest, otherwise null.
export function parseLatest(data) {
  if (typeof data !== "object" || data === null || Array.isArray(data)) return null;
  if (data.schemaVersion !== 1) return null;
  for (const field of STRING_FIELDS) {
    if (typeof data[field] !== "string" || data[field] === "") return null;
  }
  const buildOk = Number.isInteger(data.build) ? data.build > 0 : typeof data.build === "string" && /^[1-9][0-9]*$/.test(data.build);
  if (!buildOk) return null;
  if (!isReleaseUrl(data.releaseUrl) || !isReleaseUrl(data.dmgUrl) || !isReleaseUrl(data.zipUrl)) return null;
  if (!/^[0-9a-f]{64}$/.test(data.dmgSha256) || !/^[0-9a-f]{64}$/.test(data.zipSha256)) return null;
  return { version: data.version, build: String(data.build), dmgUrl: data.dmgUrl };
}

async function enhance() {
  const button = document.querySelector("[data-download]");
  const note = document.querySelector("[data-download-note]");
  if (!button || !note) return;
  try {
    const response = await fetch("latest.json", { cache: "no-cache" });
    if (!response.ok) return;
    const latest = parseLatest(await response.json());
    if (!latest) return;
    button.href = latest.dmgUrl;
    note.textContent = `Version ${latest.version} (build ${latest.build})`;
  } catch {
    // Keep the default link.
  }
}

if (typeof document !== "undefined") void enhance();
