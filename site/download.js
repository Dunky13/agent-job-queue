// Points the download button at the latest DMG named by same-origin latest.json.
// Any failure leaves the default releases link in place.
const REPO = "/Dunky13/agent-job-queue/releases/";
const STRING_FIELDS = ["version", "commit", "tag", "publishedAt", "releaseUrl", "dmgUrl", "dmgSha256", "zipUrl", "zipSha256"];

// https on github.com, no credentials or port, path under prefix and ending in suffix.
function isGithubUrl(value, prefix, suffix = "") {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname === "github.com" && url.username === "" && url.password === "" && url.port === "" && url.pathname.startsWith(REPO + prefix) && url.pathname.endsWith(suffix);
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
  if (!Number.isInteger(data.build) || data.build < 1) return null;
  if (!isGithubUrl(data.releaseUrl, "tag/") || !isGithubUrl(data.dmgUrl, "download/", ".dmg") || !isGithubUrl(data.zipUrl, "download/", ".zip")) return null;
  if (!/^[0-9a-f]{64}$/.test(data.dmgSha256) || !/^[0-9a-f]{64}$/.test(data.zipSha256)) return null;
  return { version: data.version, build: String(data.build), dmgUrl: data.dmgUrl };
}

// Any failure leaves the button and note untouched.
export async function enhance(button, note, fetchFn = fetch) {
  try {
    const response = await fetchFn("latest.json", { cache: "no-cache" });
    if (!response.ok) return;
    const latest = parseLatest(await response.json());
    if (!latest) return;
    button.href = latest.dmgUrl;
    note.textContent = `${note.textContent} Version ${latest.version} (build ${latest.build}).`;
  } catch {
    // Keep the default link.
  }
}

if (typeof document !== "undefined") {
  const button = document.querySelector("[data-download]");
  const note = document.querySelector("[data-download-note]");
  if (button && note) void enhance(button, note);
}
