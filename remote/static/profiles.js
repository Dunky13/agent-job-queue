import { profileCatalogSchema, profileDetailSchema, csrfSchema } from "./schema.mjs";

const byId = (id) => document.getElementById(id);
const gib = 1073741824;
const bytes = (value) => value > 0 && value / gib < 0.01 ? `${value.toLocaleString()} bytes` : `${(value / gib).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB`;
const number = (value) => value > 0 && value < 0.01 ? value.toExponential(2) : value.toLocaleString(undefined, { maximumFractionDigits: 2 });
const when = (value) => value === null ? "No runs yet" : new Date(value * 1000).toLocaleString();
const message = (error) => error instanceof Error ? error.message : "The learning request could not be completed.";
function node(tag, className = "", text) {
  const result = document.createElement(tag);
  result.className = className;
  if (text !== undefined) result.textContent = text;
  return result;
}
function button(text, action, className = "quiet-button") {
  const result = node("button", className, text);
  result.type = "button";
  result.addEventListener("click", action);
  return result;
}
function field(parent, label, id, value, options = {}) {
  const wrapper = node("div", "profile-field");
  const caption = node("label", "", label);
  caption.htmlFor = id;
  const input = node("input");
  Object.assign(input, { id, name: id, value, ...options });
  wrapper.append(caption, input);
  parent.append(wrapper);
  return input;
}
function definitions(rows) {
  const list = node("dl", "metadata profile-metadata");
  for (const [label, value] of rows) list.append(node("dt", "", label), node("dd", "", value));
  return list;
}

export function setupProfiles({ request, announce, show, queueBusy }) {
  const state = { detail: null, catalog: null, selected: null, query: "", offset: 0, busy: false, loading: false,
    dirty: false, confirmation: null, generation: 0, active: false, mergeGeneration: 0, mergeTargets: [] };
  const error = (text = "") => { byId("profiles-error").textContent = text; byId("profiles-error").hidden = !text; };
  function applied(data) {
    const current = data.controllerAppliedRevision;
    const progress = current === null ? "Controller has not acknowledged these corrections." : current >= data.revision ? `Applied by controller at revision ${current}.` : `Pending controller refresh (saved revision ${data.revision}, applied ${current}).`;
    byId("profiles-controller").textContent = `${progress} ${data.controllerStatus} Active jobs continue.`;
  }
  function controls() {
    byId("profiles-refresh").disabled = state.busy || state.loading;
    byId("profiles-previous").disabled = state.loading || state.offset === 0;
    byId("profiles-next").disabled = state.loading || !state.catalog || state.offset + state.catalog.profiles.length >= state.catalog.total;
    for (const control of byId("profile-detail").querySelectorAll("button, input, select")) control.disabled = state.busy || state.loading;
    byId("profile-confirm").disabled = state.busy;
    byId("profile-dismiss").disabled = state.busy;
  }
  function confirmation(action, payload, title, copy) {
    if (state.dirty && action !== "refresh") { error("Save your name and reservation corrections before sharing, separating or resetting learning."); return; }
    state.confirmation = { action, payload, id: state.detail?.profile.id, revision: state.detail?.revision };
    byId("profile-confirmation-title").textContent = title;
    byId("profile-confirmation-copy").textContent = copy;
    byId("profile-confirm").textContent = action === "reset" ? "Reset measurements" : action === "merge" ? "Share learning" : "Separate family";
    byId("profile-confirm").className = action === "reset" ? "destructive" : "";
    byId("profile-confirmation").hidden = false;
    byId("profile-confirmation-title").focus();
  }
  function dismiss() { state.confirmation = null; byId("profile-confirmation").hidden = true; }
  async function loadCatalog() {
    const generation = ++state.generation;
    state.loading = true;
    error(); controls();
    try {
      const query = new URLSearchParams({ offset: String(state.offset), limit: "100", query: state.query });
      const data = await request(`/api/profiles?${query}`, profileCatalogSchema);
      if (generation !== state.generation) return;
      state.catalog = data;
      applied(data);
      byId("profiles-count").textContent = data.total ? `${data.offset + 1}–${data.offset + data.profiles.length} of ${data.total} workloads` : "No matching workloads.";
      const items = data.profiles.map((profile) => {
        const item = node("li", "job-card");
        const link = button(profile.label, () => void open(profile.id), "profile-link");
        item.append(link, node("p", "job-context", `${profile.memberCount} job families · ${profile.variantCount} variants · ${profile.successCount} successful runs · ${profile.incompleteCount} incomplete or failed`),
          node("p", "job-context", `Measured peaks ${bytes(profile.peakMemoryBytes)} · ${number(profile.peakCpuCores)} CPU cores`),
          node("p", "job-context", `Last run: ${when(profile.lastRunAt)}`));
        return item;
      });
      byId("profiles-list").replaceChildren(...(items.length ? items : [node("li", "empty", state.query ? "Try another name or clear your search." : "Your Mac has not learned any workloads yet. Jobs build this history as they run; learning corrections will appear here.")]));
    } catch (failure) { if (generation === state.generation) error(`${message(failure)} Use Refresh learning to retry.`); }
    finally { if (generation === state.generation) { state.loading = false; controls(); } }
  }
  async function mergeTargets(query = "") {
    const generation = ++state.mergeGeneration;
    try {
      const data = await request(`/api/profiles?${new URLSearchParams({ offset: "0", limit: "100", query })}`, profileCatalogSchema);
      if (generation !== state.mergeGeneration || !state.detail || !state.active) return;
      const select = byId("profile-merge-target");
      if (!select) return;
      state.mergeTargets = data.profiles.filter((profile) => profile.id !== state.detail.profile.id);
      const initial = node("option", "", state.mergeTargets.length ? "Choose a workload" : "No other matching workloads");
      initial.value = "";
      select.replaceChildren(initial, ...state.mergeTargets.map((profile) => {
        const option = node("option", "", profile.label); option.value = profile.id; return option;
      }));
    } catch (failure) { if (generation === state.mergeGeneration) error(message(failure)); }
  }
  function renderDetail(data) {
    dismiss();
    state.detail = data;
    state.dirty = false;
    const detail = byId("profile-detail");
    const heading = node("h2", "", data.profile.label); heading.id = "profile-name"; heading.tabIndex = -1;
    const profile = data.profile;
    const summary = definitions([["Measured peaks", `${bytes(profile.peakMemoryBytes)} · ${number(profile.peakCpuCores)} CPU cores`],
      ["Evidence", `${profile.successCount} successful runs · ${profile.incompleteCount} incomplete or failed · ${profile.variantCount} variants`],
      ["Last run", when(profile.lastRunAt)], ["Group estimate", data.prediction ? `${bytes(data.prediction.memoryBytes)} · ${number(data.prediction.cpuCores)} CPU cores · ${data.prediction.confidence} confidence (${data.prediction.samples} exact runs, ${data.prediction.source})${data.prediction.durationSeconds === null ? "" : ` · ${number(data.prediction.durationSeconds)} seconds`}` : "Unavailable until this machine has fresh resource telemetry."],
      ["Operator floors", `Memory: ${profile.memoryFloorBytes === null ? "Automatic" : bytes(profile.memoryFloorBytes)} · CPU: ${profile.cpuFloorCores === null ? "Automatic" : `${number(profile.cpuFloorCores)} cores`}`]]);
    const form = node("form", "profile-panel"); form.id = "profile-edit-form"; form.method = "post"; form.action = `/api/profiles/${profile.id}/edit`;
    form.append(node("h3", "", "Name and minimum reservations"), node("p", "learning-copy", "These are your corrections. Floors raise the learned estimate; they never lower measured peaks. Leave a floor blank to return to automatic learning."));
    const name = field(form, "Workload name", "profile-label", profile.label, { required: true, maxLength: 120, autoComplete: "off" });
    const memoryText = profile.memoryFloorBytes === null ? "" : String(profile.memoryFloorBytes / gib);
    const cpuText = profile.cpuFloorCores === null ? "" : String(profile.cpuFloorCores);
    const memory = field(form, "Minimum memory (GiB)", "profile-memory", memoryText, { type: "number", min: "0", max: String(2 ** 50 / gib), step: "any", inputMode: "decimal" });
    const cpu = field(form, "Minimum CPU cores", "profile-cpu", cpuText, { type: "number", min: "0", max: "1024", step: "any", inputMode: "decimal" });
    const save = node("button", "", "Save corrections"); save.type = "submit"; form.append(save);
    form.addEventListener("input", () => { state.dirty = true; error(); });
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      const label = name.value.trim();
      if (!label) { name.setCustomValidity("Enter a workload name."); name.reportValidity(); return; }
      name.setCustomValidity("");
      const memoryFloorBytes = memory.value === memoryText ? profile.memoryFloorBytes : memory.value === "" ? null : Math.ceil(Number(memory.value) * gib);
      const cpuFloorCores = cpu.value === cpuText ? profile.cpuFloorCores : cpu.value === "" ? null : Number(cpu.value);
      if (memoryFloorBytes !== null && (!Number.isSafeInteger(memoryFloorBytes) || memoryFloorBytes <= 0 || memoryFloorBytes > 2 ** 50) || cpuFloorCores !== null && (!Number.isFinite(cpuFloorCores) || cpuFloorCores <= 0 || cpuFloorCores > 1024)) { error("Enter positive reservation floors within the shown limits, or leave them blank for automatic."); return; }
      void mutate("edit", { label, memoryFloorBytes, cpuFloorCores });
    });
    name.addEventListener("input", () => name.setCustomValidity(""));
    const members = node("section", "profile-panel"); members.append(node("h3", "", "Job families"), node("p", "learning-copy", "Different worktree paths and source revisions can use the same family. Separate a family when it should learn independently."));
    for (const member of data.members) {
      const item = node("div", "profile-member");
      item.append(node("p", "", member.label), node("p", "job-context", [member.project, member.executable, member.family.slice(0, 12)].filter(Boolean).join(" · ")));
      if (profile.memberCount > 1) item.append(button("Separate family", () => confirmation("split", { family: member.family }, `Separate ${member.label}?`, "This family will use its own profile and its original measured history. Other families keep sharing learning. Active jobs continue.")));
      members.append(item);
    }
    if (data.truncated.members) members.append(node("p", "learning-copy", "Only the first 256 job families are shown in this bounded view. Additional families still share this profile's learning."));
    const merging = node("section", "profile-panel"); merging.append(node("h3", "", "Share learning with another workload"), node("p", "learning-copy", "Combine similar job families so future jobs benefit from their shared history. Exact variant success counts stay separate."));
    const targetForm = node("form", "profile-search"); targetForm.action = "/api/profiles"; targetForm.method = "get";
    const targetQuery = field(targetForm, "Find a workload to share with", "profile-target-query", "", { type: "search", maxLength: 120, autoComplete: "off" });
    const find = node("button", "quiet-button", "Find workloads"); find.type = "submit"; targetForm.append(find);
    targetForm.addEventListener("submit", (event) => { event.preventDefault(); void mergeTargets(targetQuery.value.trim()); });
    const targetLabel = node("label", "", "Target workload"); targetLabel.htmlFor = "profile-merge-target";
    const target = node("select"); target.id = "profile-merge-target"; target.name = "targetId";
    merging.append(targetForm, targetLabel, target, button("Share learning…", () => {
      const chosen = state.mergeTargets.find((candidate) => candidate.id === target.value);
      if (!chosen) { error("Choose a target workload before sharing learning."); target.focus(); return; }
      confirmation("merge", { targetId: chosen.id }, `Share with ${chosen.label}?`, `The families in ${profile.label} will join ${chosen.label} and use its name and corrections. Measurements keep their family identities. Active jobs continue.`);
    }));
    const variants = node("details", "profile-panel"); variants.append(node("summary", "", `Variants (${profile.variantCount})`));
    for (const variant of data.variants) variants.append(node("p", "job-context", `${variant.id.slice(0, 12)} · ${variant.successCount} successful · ${variant.incompleteCount} incomplete or failed · ${bytes(variant.peakMemoryBytes)} · ${number(variant.peakCpuCores)} cores · ${when(variant.lastRunAt)}`));
    if (data.truncated.variants) variants.append(node("p", "learning-copy", "Additional variants omitted from this bounded view."));
    const runs = node("section", "profile-panel"); runs.append(node("h3", "", "Recent measurements"), node("p", "learning-copy", "Incomplete or failed runs can raise conservative reservations but do not count as successful evidence. Machine identifiers distinguish where the measurements were taken."));
    const runList = node("ul", "job-list");
    for (const run of data.runs) {
      const item = node("li", "measurement");
      item.append(node("p", "", `${run.success ? "Successful" : "Incomplete or failed"} · ${when(run.sampledAt)}`), node("p", "job-context", `${bytes(run.memoryBytes)} peak · ${number(run.cpuCores)} peak cores · ${number(run.cpuSeconds)} CPU-seconds · ${number(run.durationSeconds)} seconds`), node("p", "job-context", `Variant ${run.variant.slice(0, 12)} · Machine ${run.machine.slice(0, 12)}`)); runList.append(item);
    }
    if (!data.runs.length) runList.append(node("li", "empty", "No retained measurements. New runs will build evidence."));
    runs.append(runList);
    if (data.truncated.runs) runs.append(node("p", "learning-copy", "Older measurements omitted from this bounded view."));
    const reset = node("section", "profile-panel"); reset.append(node("h3", "", "Reset obsolete measurements"), node("p", "learning-copy", "Remove this profile's measured history and start conservatively again. Names, family grouping and your floors stay in place. Active jobs can add new measurements when they finish."), button("Reset measurements…", () => confirmation("reset", { confirm: true }, `Reset measurements for ${profile.label}?`, "Retained measurements and learned envelopes for these job families will be removed. This cannot be undone. Naming, sharing and reservation corrections remain. Active jobs continue.") , "destructive"));
    detail.replaceChildren(button("‹ All learned workloads", () => void open()), heading, summary, form, members, merging, variants, runs, reset);
    applied(data); controls(); heading.focus(); void mergeTargets();
  }
  async function loadDetail(id) {
    dismiss();
    const generation = ++state.generation;
    state.loading = true; error(); controls();
    try {
      const data = await request(`/api/profiles/${id}`, profileDetailSchema);
      if (generation === state.generation) renderDetail(data);
    } catch (failure) { if (generation === state.generation) error(`${message(failure)} Use Refresh learning to retry.`); }
    finally { if (generation === state.generation) { state.loading = false; controls(); } }
  }
  async function mutate(action, payload, reviewed = null) {
    if (state.busy || state.loading || queueBusy() || !state.detail) return;
    if (reviewed && (reviewed.id !== state.detail.profile.id || reviewed.revision !== state.detail.revision)) {
      dismiss(); error("The workload profile changed. Review its latest details before confirming this action."); return;
    }
    const id = reviewed ? reviewed.id : state.detail.profile.id;
    const revision = reviewed ? reviewed.revision : state.detail.revision;
    state.busy = true; error(); controls();
    try {
      const { token } = await request("/api/csrf", csrfSchema);
      const data = await request(`/api/profiles/${id}/${action}`, profileDetailSchema, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRFToken": token }, body: JSON.stringify({ revision, ...payload }) });
      state.selected = data.profile.id;
      dismiss(); renderDetail(data);
      announce(action === "reset" ? "Measurements reset. Active jobs continue." : "Learning corrections saved. Active jobs continue.");
    } catch (failure) { error(`${message(failure)} Your draft has been kept. Refresh learning to review the latest profile before retrying.`); }
    finally { state.busy = false; controls(); }
  }
  async function open(id = null) {
    if (state.busy) return;
    state.active = true;
    state.selected = id; state.dirty = false; dismiss(); show();
    byId("profiles-catalog").hidden = id !== null;
    byId("profile-detail").hidden = id === null;
    if (id === null) { document.title = "Learned workloads | Agent Queue"; byId("profiles-title").focus(); await loadCatalog(); }
    else { document.title = "Workload learning | Agent Queue"; await loadDetail(id); }
  }
  byId("profiles-search-form").addEventListener("submit", (event) => { event.preventDefault(); if (state.loading || state.busy) return; state.query = byId("profiles-query").value.trim(); state.offset = 0; void loadCatalog(); });
  byId("profiles-previous").addEventListener("click", () => { state.offset = Math.max(0, state.offset - 100); void loadCatalog(); });
  byId("profiles-next").addEventListener("click", () => { state.offset += 100; void loadCatalog(); });
  byId("profiles-refresh").addEventListener("click", () => {
    if (state.dirty) { confirmation("refresh", {}, "Discard unsaved corrections?", "Refresh will replace your draft with the latest saved profile. Use Keep profile to return to your draft."); byId("profile-confirm").textContent = "Discard draft and refresh"; }
    else if (state.selected) void loadDetail(state.selected); else void loadCatalog();
  });
  byId("profile-confirm").addEventListener("click", () => {
    const pending = state.confirmation;
    if (!pending) return;
    if (pending.action === "refresh") { dismiss(); state.dirty = false; void loadDetail(state.selected); }
    else void mutate(pending.action, pending.payload, pending);
  });
  byId("profile-dismiss").addEventListener("click", dismiss);
  return { open, leave() { if (state.busy) return false; state.active = false; state.generation += 1; state.mergeGeneration += 1; state.loading = false; dismiss(); return true; }, isBusy: () => state.busy };
}
