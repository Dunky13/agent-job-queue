import { snapshotSchema, csrfSchema, resultSchema, errorSchema, logSchema, schedulingModeSchema, waiting, running, eligible } from "./schema.mjs";

const byId = (id) => document.getElementById(id);
const state = { snapshot: null, lastSuccess: 0, error: "", busy: false, readController: null, generation: 0, timer: null, staleTimer: null, selected: null, confirmation: null, logController: null, logJob: null, listKey: "", detailKey: "", renderKey: "", limitDirty: false, modeDirty: false };
const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
const POLL_MS = 5000;
const STALE_MS = 20000;
const stale = () => !state.snapshot || Boolean(state.error) || Date.now() - state.lastSuccess > STALE_MS;
const actionsDisabled = () => state.busy || stale();
const jobName = (job) => job.title || job.executable || `Job #${job.id}`;
const readable = (value) => value.replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
const jobStatus = (job) => job.exitCode !== null && job.exitCode !== 0 ? `Failed (${job.exitCode})` : readable(job.state);
const when = (value) => value ? dateFormat.format(new Date(value)) : "—";
const errorMessage = (error) => error instanceof Error ? error.message : "The request could not be completed.";
const modeLabel = (mode) => ({ fixed: "Fixed", shadow: "Learn only", adaptive: "Adaptive" })[mode];
const modeInput = () => document.querySelector('input[name="mode"]:checked');
const bytes = (value) => `${(value / 1073741824).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB`;
const cores = (value) => `${value.toLocaleString(undefined, { maximumFractionDigits: 1 })} CPU cores`;
const predictionText = (prediction) => `Estimate ${bytes(prediction.memoryBytes)} · ${cores(prediction.cpuCores)} · ${prediction.confidence} confidence (${prediction.samples} runs, ${prediction.source})`;
const observedText = (observed) => `Memory ${bytes(observed.memoryBytes)} · peak ${bytes(observed.peakMemoryBytes)} · ${cores(observed.cpuCores)}${observed.complete ? "" : observed.samples ? " · partial sample coverage" : " · collecting"}`;

async function request(path, schema, options = {}) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  options.signal?.addEventListener("abort", abort, { once: true });
  if (options.signal?.aborted) abort();
  let timedOut = false;
  const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, 25000);
  try {
    const response = await fetch(path, { credentials: "same-origin", cache: "no-store", ...options, signal: controller.signal });
    if (!response.ok) {
      if (response.status === 401) throw new Error("Access expired. Reload this page to sign in again.");
      const result = errorSchema.safeParse(await response.json().catch(() => null));
      throw new Error(result.success ? result.data.error : `Your Mac could not complete the request (${response.status}).`);
    }
    const result = schema.safeParse(await response.json());
    if (!result.success) throw new Error("The queue returned an unexpected response. Refresh to try again.");
    return result.data;
  } catch (error) {
    if (timedOut) throw new Error("The request timed out. Refresh to check the queue before trying again.", { cause: error });
    throw error;
  } finally {
    clearTimeout(timeout);
    options.signal?.removeEventListener("abort", abort);
  }
}

function remember(snapshot) {
  state.snapshot = snapshot;
  state.lastSuccess = Date.now();
  clearTimeout(state.staleTimer);
  state.staleTimer = setTimeout(render, STALE_MS + 1);
  state.error = "";
  if (!state.limitDirty) byId("limit").value = String(snapshot.queue.limit);
  if (!state.modeDirty) {
    for (const input of document.querySelectorAll('input[name="mode"]')) input.checked = input.value === snapshot.scheduler?.mode;
  }
  if (state.confirmation && !eligible(snapshot.jobs.find((job) => job.id === state.confirmation.id), state.confirmation.action)) {
    state.confirmation = null;
    announce("That job has changed. The queue has been updated.");
  }
}

function schedule() {
  clearTimeout(state.timer);
  state.timer = null;
  if (!document.hidden && !state.busy) state.timer = setTimeout(refresh, POLL_MS);
}

async function refresh({ manual = false } = {}) {
  if (state.busy || state.readController || document.hidden) return;
  clearTimeout(state.timer);
  const controller = new AbortController();
  const generation = ++state.generation;
  state.readController = controller;
  if (manual) byId("refresh").textContent = "Refreshing…";
  try {
    const snapshot = await request("/api/status", snapshotSchema, { signal: controller.signal });
    if (generation === state.generation) remember(snapshot);
  } catch (error) {
    if (!controller.signal.aborted && generation === state.generation) state.error = `Can't reach your queue. ${errorMessage(error)}${state.snapshot ? " Showing the last received data." : ""}`;
  } finally {
    if (generation === state.generation) {
      state.readController = null;
      if (manual) byId("refresh").textContent = "Refresh";
      render();
      schedule();
    }
  }
}

async function mutate(action, id = null, value = null) {
  if (actionsDisabled()) return;
  // Reserve the action before any await. Polls never discard or compete with a click.
  state.busy = true;
  clearTimeout(state.timer);
  state.readController?.abort();
  state.readController = null;
  state.generation += 1;
  byId("refresh").textContent = "Refresh";
  render();
  try {
    remember(await request("/api/status", snapshotSchema));
    if (!["limit", "scheduling"].includes(action) && !eligible(state.snapshot.jobs.find((job) => job.id === id), action)) {
      throw new Error("That job has changed. Check its current state before trying again.");
    }
    const { token } = await request("/api/csrf", csrfSchema);
    if (action === "scheduling" && !state.snapshot.scheduler) throw new Error("Scheduling controls are unavailable. Update the queue controller.");
    const path = ["limit", "scheduling"].includes(action) ? `/api/${action}` : `/api/jobs/${id}/${action}`;
    const body = action === "limit" ? { limit: value } : action === "scheduling" ? { mode: value } : {};
    await request(path, resultSchema, {
      method: "POST", headers: { "Content-Type": "application/json", "X-CSRFToken": token }, body: JSON.stringify(body),
    });
    state.confirmation = null;
    if (action === "limit") state.limitDirty = false;
    if (action === "scheduling") state.modeDirty = false;
    remember(await request("/api/status", snapshotSchema));
    announce(action === "limit" ? `Parallel limit set to ${value}.` : action === "scheduling" ? `Scheduling set to ${modeLabel(value)}.` : action === "start" ? `Job #${id} was requested to start now.` : action === "cancel" ? `Job #${id} was cancelled.` : `Job #${id} was moved ${action === "next" ? "to the front" : "to the end"}.`);
  } catch (error) {
    state.error = errorMessage(error);
  } finally {
    state.busy = false;
    render();
    schedule();
  }
}

function announce(message) { byId("announcement").textContent = message; }
function element(tag, className, content) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (content !== undefined) node.textContent = content;
  return node;
}
function actionButton(text, job, action, className = "") {
  const button = element("button", className, text);
  button.type = "button";
  button.dataset.focusKey = `${job.id}-${action}`;
  button.setAttribute("aria-label", `${text}: ${jobName(job)}, job ${job.id}`);
  button.disabled = actionsDisabled();
  button.addEventListener("click", () => {
    if (action === "details") openDetail(job.id);
    else if (action === "start" || action === "cancel") confirmAction(job.id, action);
    else void mutate(action, job.id);
  });
  if (action === "details") button.disabled = false;
  return button;
}

function renderJob(job) {
  const li = element("li", "job-card");
  const top = element("div", "job-top");
  top.append(element("h3", "job-name", jobName(job)));
  const statusClass = job.exitCode !== null && job.exitCode !== 0 ? "failed" : running(job) ? "running" : "";
  top.append(element("span", `job-state ${statusClass}`, jobStatus(job)));
  li.append(top);
  li.append(element("p", "job-context", [`#${job.id}`, job.project, job.branch].filter(Boolean).join(" · ")));
  if (waiting(job) && job.queuePosition) li.append(element("p", "job-context", `Position ${job.queuePosition} · Priority ${job.priority}`));
  if (job.phase && job.phase.toLowerCase().replaceAll("_", " ") !== job.state.toLowerCase().replaceAll("_", " ")) li.append(element("p", "job-phase", readable(job.phase)));
  if (job.resources) {
    li.append(element("p", "job-context", predictionText(job.resources.prediction)));
    if (job.resources.observed) li.append(element("p", "job-context", observedText(job.resources.observed)));
    if (waiting(job) && job.resources.decision) li.append(element("p", "job-phase", job.resources.decision.reason));
  }
  const actions = element("div", "job-actions");
  if (job.canStart && waiting(job)) actions.append(actionButton("Start now", job, "start"));
  if (job.canReorder && waiting(job)) {
    if (job.queuePosition !== 1) actions.append(actionButton("Run next", job, "next"));
    actions.append(actionButton("Move to end", job, "last"));
  }
  if (job.canCancel) actions.append(actionButton("Cancel", job, "cancel", "destructive"));
  actions.append(actionButton("Details", job, "details", "detail-button"));
  li.append(actions);
  return li;
}

function renderList(id, jobs, empty) {
  const list = byId(id);
  list.replaceChildren(...(jobs.length ? jobs.map(renderJob) : [element("li", "empty", empty)]));
}

function render() {
  const disabled = actionsDisabled();
  // Response timestamps change on every poll. Only rendered data and interaction
  // state should invalidate the view, preserving focus, selection and scroll.
  const schedulerContent = state.snapshot?.scheduler && { ...state.snapshot.scheduler, updatedAt: null,
    host: state.snapshot.scheduler.host && { ...state.snapshot.scheduler.host, sampledAt: 0 } };
  const renderKey = JSON.stringify({ queue: state.snapshot?.queue, jobs: state.snapshot?.jobs, scheduler: schedulerContent,
    disabled, error: state.error, busy: state.busy, selected: state.selected,
    confirmation: state.confirmation, limit: byId("limit").value, mode: modeInput()?.value });
  if (renderKey === state.renderKey) return;
  state.renderKey = renderKey;
  byId("error").hidden = !state.error;
  byId("error").textContent = state.error;
  byId("refresh").disabled = state.busy;
  byId("limit").disabled = disabled;
  byId("apply-limit").disabled = disabled || Number(byId("limit").value) === state.snapshot?.queue.limit;
  byId("scheduling-modes").disabled = disabled || !state.snapshot?.scheduler;
  byId("apply-scheduling").disabled = disabled || !state.snapshot?.scheduler || !modeInput() || modeInput().value === state.snapshot.scheduler.mode;
  byId("connection").className = `connection ${stale() ? "stale" : "live"}`;
  byId("connection-text").textContent = state.busy ? "Updating queue…" : !state.snapshot ? state.error ? "Disconnected" : "Connecting to your Mac…" : stale() ? `Offline · Last updated ${dateFormat.format(new Date(state.lastSuccess))}` : "Connected · Updates every 5 seconds";
  if (!state.snapshot) return;
  const { queue, jobs, scheduler } = state.snapshot;
  byId("scheduler-status").textContent = scheduler ? `${modeLabel(scheduler.mode)} · ${scheduler.available ? "Resource monitoring active" : "Resource monitoring unavailable"}${scheduler.reason ? ` · ${scheduler.reason}` : ""}` : "Resource monitoring unavailable. Update the queue controller to enable scheduling controls.";
  byId("host-status").textContent = scheduler?.host ? `Memory pressure ${scheduler.host.memoryPressure} · ${bytes(scheduler.host.availableMemoryBytes)} available of ${bytes(scheduler.host.totalMemoryBytes)} · CPU ${scheduler.host.cpuBusyCores.toFixed(1)}/${scheduler.host.cpuCores} cores · swap ${bytes(scheduler.host.swapUsedBytes)}${scheduler.host.complete === false ? " · Collecting rates" : ""}` : "Host measurements unavailable.";
  byId("running-count").textContent = String(queue.running);
  byId("waiting-count").textContent = String(queue.waiting);
  byId("limit-count").textContent = String(queue.limit);
  byId("running-label").textContent = `${queue.running} active`;
  byId("waiting-label").textContent = `${queue.waiting} waiting`;
  const notice = queue.legacyOccupied ? "A legacy validation batch holds the shared slot. Queued jobs will wait for it to finish." : queue.status !== "running" ? `Queue ${readable(queue.status).toLowerCase()}. Automatic scheduling is paused.` : "";
  byId("queue-notice").hidden = !notice;
  byId("queue-notice").textContent = notice;
  const listKey = JSON.stringify({ jobs, disabled });
  if (listKey !== state.listKey) {
    const focused = document.activeElement?.dataset.focusKey;
    state.listKey = listKey;
    renderList("running-jobs", jobs.filter(running), "No jobs running.");
    renderList("waiting-jobs", jobs.filter(waiting).sort((a, b) => (a.queuePosition ?? Number.MAX_SAFE_INTEGER) - (b.queuePosition ?? Number.MAX_SAFE_INTEGER) || a.id - b.id), "Nothing waiting. Your queue is clear.");
    const recent = jobs.filter((job) => !waiting(job) && !running(job)).slice(0, 12);
    byId("recent-count").textContent = String(recent.length);
    renderList("recent-jobs", recent, "Completed jobs will appear here.");
    if (focused) Array.from(document.querySelectorAll("[data-focus-key]")).find((node) => node.dataset.focusKey === focused && !node.disabled)?.focus({ preventScroll: true });
  }
  renderConfirmation();
  if (state.selected !== null) renderDetail();
}

function confirmAction(id, action) {
  if (actionsDisabled() || !eligible(state.snapshot?.jobs.find((job) => job.id === id), action)) return;
  state.confirmation = { id, action };
  renderConfirmation();
  byId("confirmation").scrollIntoView({ behavior: "instant", block: "nearest" });
  byId("confirmation-title").focus({ preventScroll: true });
}

function renderConfirmation() {
  const confirmation = state.confirmation;
  byId("confirmation").hidden = !confirmation;
  if (!confirmation) return;
  const job = state.snapshot.jobs.find((item) => item.id === confirmation.id);
  const start = confirmation.action === "start";
  byId("confirmation-title").textContent = `${start ? "Start" : "Cancel"} ${job ? jobName(job) : `job #${confirmation.id}`}?`;
  byId("confirmation-copy").textContent = start ? `Run this job now alongside ${state.snapshot.queue.running} active job${state.snapshot.queue.running === 1 ? "" : "s"}, bypassing the parallel limit and adaptive resource checks. This can increase memory pressure.` : "This stops the selected job and its owned child processes. Its output remains available.";
  byId("confirm-action").textContent = state.busy ? "Updating…" : start ? "Start in parallel" : "Cancel job";
  byId("confirm-action").className = start ? "" : "destructive";
  byId("confirm-action").disabled = actionsDisabled() || !eligible(job, confirmation.action);
  byId("dismiss-confirmation").textContent = start ? "Keep waiting" : "Keep job";
  byId("dismiss-confirmation").disabled = state.busy;
}

function openDetail(id) {
  state.selected = id;
  state.confirmation = null;
  renderConfirmation();
  byId("queue-view").hidden = true;
  byId("detail-view").hidden = false;
  render();
  byId("detail-title").focus();
  window.scrollTo(0, 0);
  void refreshLog();
}

function renderDetail() {
  const job = state.snapshot.jobs.find((item) => item.id === state.selected);
  const detailKey = JSON.stringify({ id: state.selected, job });
  if (detailKey === state.detailKey) return;
  state.detailKey = detailKey;
  if (!job) {
    byId("detail-title").textContent = `Job #${state.selected}`;
    byId("detail-status").textContent = "This job is no longer in the recent queue.";
    byId("detail-metadata").replaceChildren();
    return;
  }
  byId("detail-title").textContent = jobName(job);
  byId("detail-status").textContent = `Job #${job.id} · ${jobStatus(job)}`;
  document.title = `${jobName(job)} | Agent Queue`;
  const rows = [["Project", job.project || "—"], ["Branch", job.branch || "—"], ["Directory", job.directory || "—"], ["Executable", job.executable || "—"], ["Phase", readable(job.phase || job.state)], ["Submitted", when(job.submittedAt)], ["Started", when(job.startedAt)], ["Finished", when(job.finishedAt)]];
  if (job.exitCode !== null) rows.push(["Exit code", String(job.exitCode)]);
  if (job.resources) {
    rows.push(["Prediction", predictionText(job.resources.prediction)]);
    if (job.resources.prediction.durationSeconds !== null) rows.push(["Expected duration", `${Math.round(job.resources.prediction.durationSeconds)} seconds`]);
    if (job.resources.observed) {
      rows.push(["Measured usage", observedText(job.resources.observed)]);
      rows.push(["CPU cost", `${job.resources.observed.cpuSeconds.toFixed(1)} CPU-seconds over ${job.resources.observed.durationSeconds.toFixed(1)} seconds (${job.resources.observed.samples} samples)`]);
    } else rows.push(["Measured usage", "No measurements available."]);
    if (job.resources.decision) rows.push(["Admission", job.resources.decision.reason]);
  } else rows.push(["Resources", "No measurements or predictions available."]);
  byId("detail-metadata").replaceChildren(...rows.flatMap(([label, value]) => [element("dt", "", label), element("dd", "", value)]));
}

async function refreshLog() {
  const id = state.selected;
  if (id === null || state.logController) return;
  const controller = new AbortController();
  state.logController = controller;
  byId("refresh-log").disabled = true;
  byId("log-error").hidden = true;
  if (state.logJob !== id) byId("log-output").textContent = "Loading output…";
  try {
    const log = await request(`/api/jobs/${id}/log`, logSchema, { signal: controller.signal });
    if (state.selected !== id) return;
    const output = log.output || "No output yet.";
    if (byId("log-output").textContent !== output) byId("log-output").textContent = output;
    state.logJob = id;
    const size = log.truncated ? "Latest 64 KB · Older output omitted" : "Latest output · Up to 64 KB";
    if (byId("log-size").textContent !== size) byId("log-size").textContent = size;
  } catch (error) {
    if (!controller.signal.aborted && state.selected === id) {
      byId("log-error").textContent = errorMessage(error);
      byId("log-error").hidden = false;
      if (state.logJob !== id) byId("log-output").textContent = "Output could not be loaded.";
    }
  } finally {
    if (state.logController === controller) {
      state.logController = null;
      byId("refresh-log").disabled = false;
    }
  }
}

byId("refresh").addEventListener("click", () => void refresh({ manual: true }));
byId("limit").addEventListener("change", () => { state.limitDirty = true; render(); });
byId("limit-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const limit = Number(byId("limit").value);
  if (Number.isInteger(limit) && limit >= 1 && limit <= 8) void mutate("limit", null, limit);
});
for (const input of document.querySelectorAll('input[name="mode"]')) input.addEventListener("change", () => { state.modeDirty = true; render(); });
byId("scheduling-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const mode = schedulingModeSchema.safeParse(modeInput()?.value);
  if (mode.success) void mutate("scheduling", null, mode.data);
});
byId("confirm-action").addEventListener("click", () => {
  if (state.confirmation) void mutate(state.confirmation.action, state.confirmation.id);
});
byId("dismiss-confirmation").addEventListener("click", () => {
  const confirmation = state.confirmation;
  state.confirmation = null;
  renderConfirmation();
  if (confirmation) Array.from(document.querySelectorAll("[data-focus-key]")).find((node) => node.dataset.focusKey === `${confirmation.id}-${confirmation.action}`)?.focus();
});
byId("back").addEventListener("click", () => {
  const id = state.selected;
  state.selected = null;
  state.detailKey = "";
  state.logController?.abort();
  state.logController = null;
  byId("refresh-log").disabled = false;
  byId("detail-view").hidden = true;
  byId("queue-view").hidden = false;
  document.title = "Agent Queue";
  render();
  Array.from(document.querySelectorAll("[data-focus-key]")).find((node) => node.dataset.focusKey === `${id}-details`)?.focus();
});
byId("refresh-log").addEventListener("click", () => void refreshLog());
document.addEventListener("visibilitychange", () => {
  clearTimeout(state.timer);
  if (document.hidden) {
    state.readController?.abort();
    state.readController = null;
    state.generation += 1;
    byId("refresh").textContent = "Refresh";
  } else { render(); void refresh(); }
});
void refresh();
