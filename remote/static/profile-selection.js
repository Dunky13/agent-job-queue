import { profileCatalogSchema, profileDetailSchema, csrfSchema } from "./schema.mjs";

const element = (tag, text = "") => { const value = document.createElement(tag); value.textContent = text; return value; };
const selectionKey = (item) => `${item.kind}:${item.id}`;

export function setupProfileSelection({ request, reportError, isBlocked, isDirty, setBusy, onMerged }) {
  const panel = document.getElementById("profile-combine");
  const selected = new Map();
  let revision = null, targetGeneration = 0, pending = null, loadingTargets = false;
  const heading = element("h3", "Merge selected groups and workloads");
  const count = element("p"); count.setAttribute("role", "status");
  const hint = element("p", "Select up to 32 items across searches and pages. Selecting a run includes its workload and all matching history, including future runs."); hint.className = "learning-copy";
  const review = element("details"), list = element("ul");
  review.append(element("summary", "Review selected items"), list);
  const clear = element("button", "Clear selection"); clear.type = "button"; clear.className = "quiet-button";
  const form = element("form"); form.className = "profile-search";
  const queryLabel = element("label", "Find a destination group"); queryLabel.htmlFor = "combine-target-query";
  const query = element("input"); query.id = "combine-target-query"; query.type = "search"; query.maxLength = 120;
  const find = element("button", "Find destination groups"); find.type = "submit";
  form.append(queryLabel, query, find);
  const targetLabel = element("label", "Destination group"); targetLabel.htmlFor = "combine-target";
  const target = element("select"); target.id = "combine-target";
  const merge = element("button", "Merge selected…"); merge.type = "button";
  const confirmation = element("section"); confirmation.className = "confirmation"; confirmation.hidden = true;
  const title = element("h4", "Merge the reviewed selection?"); title.tabIndex = -1;
  const copy = element("p"), confirm = element("button", "Merge selected items"), cancel = element("button", "Keep selection");
  confirm.type = cancel.type = "button"; cancel.className = "quiet-button";
  confirmation.append(title, copy, confirm, cancel);
  panel.append(heading, count, hint, review, clear, form, targetLabel, target, merge, confirmation);

  function dismiss() { pending = null; confirmation.hidden = true; }
  function controls() {
    for (const input of panel.querySelectorAll("button,input,select")) input.disabled = isBlocked();
    find.disabled = target.disabled = merge.disabled = isBlocked() || loadingTargets;
    for (const input of document.querySelectorAll("[data-merge-key]")) {
      const checked = selected.has(input.dataset.mergeKey);
      if (input.type === "checkbox") input.checked = checked;
      else input.setAttribute("aria-pressed", String(checked));
      input.disabled = isBlocked();
    }
  }
  function render() {
    panel.hidden = selected.size === 0;
    count.textContent = `${selected.size} selected · learning revision ${revision ?? "unavailable"}`;
    list.replaceChildren(...Array.from(selected, ([key, item]) => {
      const row = element("li"); row.append(element("span", `${item.kind === "group" ? "Group" : "Workload"}: ${item.label}`));
      const remove = element("button", `Remove ${item.label}`); remove.type = "button"; remove.className = "quiet-button";
      remove.addEventListener("click", () => { selected.delete(key); dismiss(); if (!selected.size) revision = null; render(); });
      row.append(remove); return row;
    }));
    controls();
  }
  function toggle(item, expectedRevision) {
    if (isBlocked()) { controls(); return; }
    const key = selectionKey(item);
    if (selected.has(key)) selected.delete(key);
    else {
      if (selected.size >= 32) { reportError("Select at most 32 groups or workloads per merge."); controls(); return; }
      if (revision !== null && revision !== expectedRevision) { reportError("Learning changed. Clear the selection, refresh learning and review the items again."); controls(); return; }
      selected.set(key, item); revision = expectedRevision;
    }
    dismiss(); if (!selected.size) revision = null;
    render(); if (selected.size === 1 && selected.has(key)) void loadTargets();
  }
  function checkbox(item, expectedRevision) {
    const label = element("label"); label.className = "profile-selection-control";
    const input = element("input"); input.type = "checkbox"; input.dataset.mergeKey = selectionKey(item); input.checked = selected.has(selectionKey(item));
    const text = `Select ${item.kind}: ${item.label}`; input.setAttribute("aria-label", text);
    input.addEventListener("change", () => toggle(item, expectedRevision));
    label.append(input, element("span", text)); return label;
  }
  function runButton(data, family, label) {
    const item = {kind: "workload", id: family, label, group: data.profile.id};
    const button = element("button", "Select this run’s workload"); button.type = "button"; button.className = "quiet-button";
    button.dataset.mergeKey = selectionKey(item); button.setAttribute("aria-pressed", String(selected.has(selectionKey(item))));
    button.addEventListener("click", () => toggle(item, data.revision)); return button;
  }
  async function loadTargets() {
    const generation = ++targetGeneration; loadingTargets = true; controls();
    try {
      const data = await request(`/api/profiles?${new URLSearchParams({offset: "0", limit: "100", query: query.value.trim()})}`, profileCatalogSchema);
      if (generation !== targetGeneration || !selected.size) return;
      if (data.revision !== revision) throw new Error("Learning changed. Clear the selection, refresh learning and review the items again.");
      const previous = target.value;
      const initial = element("option", data.profiles.length ? "Choose a destination group" : "No matching groups"); initial.value = "";
      target.replaceChildren(initial, ...data.profiles.map((profile) => { const option = element("option", profile.label); option.value = profile.id; return option; }));
      if (Array.from(target.options).some((option) => option.value === previous)) target.value = previous;
    } catch (error) { if (generation === targetGeneration) reportError(error instanceof Error ? error.message : "Could not load destination groups."); }
    finally { if (generation === targetGeneration) { loadingTargets = false; controls(); } }
  }
  function clearSelection() { selected.clear(); revision = null; targetGeneration += 1; loadingTargets = false; dismiss(); render(); }
  form.addEventListener("submit", (event) => { event.preventDefault(); if (!isBlocked()) void loadTargets(); });
  clear.addEventListener("click", clearSelection);
  target.addEventListener("change", dismiss);
  cancel.addEventListener("click", () => { dismiss(); merge.focus(); });
  merge.addEventListener("click", () => {
    if (isBlocked()) return;
    if (isDirty()) { reportError("Save your name and reservation corrections before merging the selection."); return; }
    if (!target.value) { reportError("Choose a destination group before merging."); target.focus(); return; }
    const items = Array.from(selected.values());
    const profileIds = items.filter((item) => item.kind === "group" && item.id !== target.value).map((item) => item.id);
    const families = items.filter((item) => item.kind === "workload").map((item) => item.id);
    if (!profileIds.length && !families.length) { reportError("Select another group or workload to merge into this destination."); return; }
    if (items.some((item) => item.kind === "workload" && item.group === target.value)) { reportError("A selected workload already belongs to this destination. Remove it from the selection first."); return; }
    pending = { id: target.value, revision, profileIds, families };
    copy.textContent = `Merge the selected groups and workloads into ${target.selectedOptions[0].textContent}? Destination naming is kept and the highest reservation floors are preserved. All matching history moves together; future runs share the group. Source groups keep any unselected workloads. Active jobs continue. Selected: ${items.map((item) => item.label).join(", ")}.`;
    confirmation.hidden = false; title.focus();
  });
  confirm.addEventListener("click", () => void apply());
  async function apply() {
    if (!pending || isBlocked()) return;
    if (isDirty()) { reportError("Save your draft before merging."); return; }
    const {id, ...payload} = pending;
    setBusy(true); controls(); reportError("");
    try {
      const {token} = await request("/api/csrf", csrfSchema);
      const data = await request(`/api/profiles/${id}/combine`, profileDetailSchema, {method: "POST", headers: {"Content-Type": "application/json", "X-CSRFToken": token}, body: JSON.stringify(payload)});
      if (data.profile.id !== id) throw new Error("The server returned another group. Refresh learning to check the result before retrying.");
      clearSelection(); onMerged(data);
    } catch (error) { dismiss(); reportError(`${error instanceof Error ? error.message : "The merge could not be confirmed."} Your selection is preserved. Refresh and review the result before retrying.`); }
    finally { setBusy(false); controls(); }
  }
  return {checkbox, runButton, controls, dismiss, clear: clearSelection};
}
