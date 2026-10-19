"""Persistent, bounded resource admission for the private Pueue queue.

Pueue owns execution and teardown. This controller only grants admission; a
helper must consume its grant under the existing ownership lease before exec.
"""
from __future__ import annotations

import contextlib
import math
import signal
import time
import uuid
from datetime import UTC, datetime

MODES = ("fixed", "shadow", "adaptive")
FRESH_SECONDS = 10
GRANT_SECONDS = 10
RECOVERY_SECONDS = 10
STARVATION_SECONDS = 60
GIB = 1024 ** 3


class SchedulingError(Exception):
    """Scheduling data cannot safely authorize new work."""


def _number(value, *, positive=False) -> bool:
    return type(value) in (int, float) and 0 <= value <= 2 ** 63 - 1 and math.isfinite(value) and (not positive or value > 0)


def _prediction(value) -> bool:
    return (isinstance(value, dict) and type(value.get("memoryBytes")) is int and _number(value["memoryBytes"], positive=True)
            and _number(value.get("cpuCores"), positive=True) and type(value.get("samples")) is int
            and value["samples"] >= 0 and value.get("source") in ("variant", "family", "prior")
            and value.get("confidence") in ("unknown", "low", "medium", "high")
            and (value.get("durationSeconds") is None or _number(value["durationSeconds"])))


def _host(value) -> bool:
    if not isinstance(value, dict):
        return False
    integers = ("totalMemoryBytes", "availableMemoryBytes", "cpuCores", "swapUsedBytes", "diskFreeBytes")
    rates = ("sampledAt", "cpuBusyCores", "swapInBytesPerSecond", "swapOutBytesPerSecond")
    return (all(type(value.get(key)) is int and _number(value[key]) for key in integers)
            and all(_number(value.get(key)) for key in rates) and value["totalMemoryBytes"] > 0
            and 0 <= value["availableMemoryBytes"] <= value["totalMemoryBytes"] and value["cpuCores"] > 0
            and value["cpuBusyCores"] <= value["cpuCores"] and type(value.get("complete")) is bool
            and value.get("memoryPressure") in ("normal", "warning", "critical")
            and isinstance(value.get("machine"), str) and bool(value["machine"]))


def settings(gate) -> dict:
    from agent_heavy import read_private
    try:
        value = read_private(gate.state / "scheduling.json")
    except FileNotFoundError:
        return {"mode": "fixed", "generation": "initial"}
    if set(value) != {"mode", "generation"} or value["mode"] not in MODES \
            or not isinstance(value["generation"], str) or not value["generation"]:
        raise SchedulingError("Invalid scheduling settings; new admissions are held")
    return value


def identity(gate, token: str) -> dict | None:
    from agent_heavy import read_private
    try:
        value = read_private(gate.state / "workloads" / f"{token}.json")
    except FileNotFoundError:
        return None
    if set(value) != {"family", "variant", "context"} or not all(
        isinstance(item, str) and len(item) == 64 and all(c in "0123456789abcdef" for c in item)
        for item in value.values()
    ):
        raise SchedulingError("Invalid workload identity; new admissions are held")
    return value


def ensure_identity(gate, token: str, task_id: int) -> dict | None:
    """Migrate acknowledged pre-feature jobs without retaining their payload."""
    from agent_heavy import read_private, write_private
    from job_profiles import identify_workload
    workload = identity(gate, token)
    if workload is not None:
        return workload
    try:
        payload = read_private(gate.payload_path(token))
    except FileNotFoundError:
        return None
    if payload.get("token") != token or payload.get("task_id") != task_id:
        raise SchedulingError("Workload payload does not match its acknowledged job")
    workload = identify_workload(payload.get("argv", []), payload.get("cwd", ""), payload.get("env", {}))
    write_private(gate.state / "workloads" / f"{token}.json", workload)
    return workload


def view(gate) -> dict:
    """Read a bounded-age heartbeat; a stale document never grants admission."""
    from agent_heavy import boot_identity, read_private
    configuration = settings(gate)
    unavailable = {"mode": configuration["mode"], "available": False,
                   "reason": "Resource controller is unavailable; adaptive jobs remain waiting",
                   "updatedAt": None, "host": None, "jobs": {}}
    try:
        value = read_private(gate.state / "resource-status.json")
    except FileNotFoundError:
        return unavailable
    if not _number(value.get("sampledAt")):
        raise SchedulingError("Invalid resource heartbeat timestamp")
    age = time.time() - value["sampledAt"]
    host = value.get("host")
    if host is not None and not _host(host):
        raise SchedulingError("Invalid host resource observations")
    host_age = time.time() - host.get("sampledAt", 0) if isinstance(host, dict) else 0
    if value.get("generation") != configuration["generation"] or value.get("boot") != boot_identity() \
            or not 0 <= age <= FRESH_SECONDS or not 0 <= host_age <= FRESH_SECONDS:
        return unavailable
    if not isinstance(value.get("jobs"), dict) or not isinstance(value.get("available"), bool) \
            or (value["available"] and host is None):
        raise SchedulingError("Invalid resource controller status")
    return {**value, "mode": configuration["mode"]}


def ledger(gate, configuration: dict) -> dict:
    from agent_heavy import boot_identity, read_private, TOKEN
    empty = {"boot": boot_identity(), "generation": configuration["generation"], "grants": {}}
    try:
        value = read_private(gate.state / "resource-admissions.json")
    except FileNotFoundError:
        return empty
    if value.get("boot") != empty["boot"] or value.get("generation") != empty["generation"]:
        return empty
    grants = value.get("grants")
    if not isinstance(grants, dict):
        raise SchedulingError("Invalid resource admission ledger")
    for token, grant in grants.items():
        if not isinstance(token, str) or not TOKEN.fullmatch(token) or not isinstance(grant, dict) \
                or type(grant.get("task_id")) is not int or grant["task_id"] < 0 or type(grant.get("manual")) is not bool \
                or not _number(grant.get("expires")) or not isinstance(grant.get("prediction"), dict) \
                or (not grant["manual"] and not _prediction(grant["prediction"])):
            raise SchedulingError("Invalid resource admission grant")
    return value


def permit(gate, marker: dict, member_count: int) -> bool:
    """Called only while the existing lease lock is held."""
    configuration = settings(gate)
    if configuration["mode"] != "adaptive" and not marker.get("resourceManaged"):
        return True
    admissions = ledger(gate, configuration)
    grant = admissions["grants"].get(marker["token"])
    if grant and grant["task_id"] == marker["task_id"] and time.time() <= grant["expires"]:
        if grant["manual"]:
            return True
        if configuration["mode"] == "adaptive":
            from agent_heavy import command_token, read_private, task_status, validate_limit
            state = view(gate)
            queue = gate.snapshot()
            group = queue["groups"]["default"]
            limit = group["parallel_tasks"]
            validate_limit(limit)
            if not state["available"] or not state.get("admissionReady") or member_count >= limit or group["status"] != "Running":
                return False
            if any(task_status(task)[0] in ("Running", "Paused") and command_token(task) is None
                   for task in queue["tasks"].values() if task.get("group", "default") == "default"):
                return False
            active = []
            if gate.slot.exists():
                for path in (gate.slot / "agent-heavy-jobs").iterdir():
                    member = read_private(path)
                    measured = state["jobs"].get(member["token"])
                    if measured is None or measured.get("observed") is None:
                        return False
                    active.append(measured)
            allowed, _reason = decision(state["host"], grant["prediction"], active, limit)
            # Queue/socket and filesystem reads above can block. Admission must
            # still be fresh after the final blocking read, not just before it.
            now = time.time()
            return (allowed and now <= grant["expires"] and 0 <= now - state["sampledAt"] <= FRESH_SECONDS
                    and 0 <= now - state["host"]["sampledAt"] <= FRESH_SECONDS)
    if configuration["mode"] != "adaptive":
        # A helper launched during an earlier adaptive generation still obeys
        # the current ceiling when the operator returns to a fixed policy.
        return member_count < gate.snapshot()["groups"]["default"]["parallel_tasks"]
    return False


def decision(host: dict, prediction: dict, active: list[dict], limit: int) -> tuple[bool, str]:
    """Account for current host use once, then reserve future job growth."""
    if not _host(host) or not _prediction(prediction):
        return False, "Waiting for valid resource observations and predictions"
    if len(active) >= limit:
        return False, "Waiting for the parallel job limit"
    if not host.get("complete"):
        return False, "Collecting a complete resource interval"
    if host["memoryPressure"] != "normal":
        return False, "Waiting for memory pressure to recover"
    if host["swapOutBytesPerSecond"] > 1024 ** 2:
        return False, "Waiting for active swapping to settle"
    reserve = max(GIB, int(host["totalMemoryBytes"] * 0.15))
    if prediction["memoryBytes"] > host["totalMemoryBytes"] - reserve:
        return False, "Predicted memory exceeds this machine's budget; reduce workers or inspect Start now"
    if host["diskFreeBytes"] < max(GIB, reserve):
        return False, "Waiting for free disk space"
    if active and (prediction["samples"] < 3 or any(job["prediction"].get("samples", 0) < 3 for job in active)):
        return False, "Learning this unfamiliar workload before overlapping it"
    if any(not job.get("observed", {}).get("complete", False) for job in active):
        return False, "Waiting for complete measurements of active jobs"
    if any(not _prediction(job["prediction"]) or not _number(job["observed"].get("memoryBytes"))
           or not _number(job["observed"].get("cpuCores")) for job in active):
        return False, "Waiting for valid active resource reservations"
    memory_growth = sum(max(0, job["prediction"]["memoryBytes"] - job["observed"]["memoryBytes"])
                        for job in active)
    if prediction["memoryBytes"] + reserve + memory_growth > host["availableMemoryBytes"]:
        return False, "Waiting for predicted peak memory headroom"
    cpu_budget = max(1.0, host["cpuCores"] * 0.8)
    cpu_growth = sum(max(0.0, min(cpu_budget, job["prediction"]["cpuCores"]) - job["observed"]["cpuCores"])
                     for job in active)
    if not active and host["cpuBusyCores"] > host["cpuCores"] * 0.6:
        return False, "Waiting for background CPU activity to settle"
    if prediction["cpuCores"] > cpu_budget:
        return False, "Predicted CPU demand exceeds this machine's budget; reduce workers or inspect Start now"
    if host["cpuBusyCores"] + cpu_growth + prediction["cpuCores"] > cpu_budget:
        return False, "Waiting for predicted CPU headroom"
    return True, "Predicted resource demand fits the current headroom"


def set_mode(gate, mode: str):
    from agent_heavy import command_token, control_budget, private_lock, task_status, write_private, save_barrier
    if mode not in MODES:
        raise SchedulingError("Scheduling mode must be fixed, shadow, or adaptive")
    gate.prepare()
    with control_budget(gate), private_lock(gate, "scheduling.lock"):
        previous = settings(gate)
        if previous["mode"] == mode:
            return
        if mode == "adaptive" and not view(gate)["available"]:
            raise SchedulingError("Start the resource controller and wait for fresh telemetry before enabling adaptive mode")
        with private_lock(gate):
            old_admissions = ledger(gate, previous)
            configuration = {"mode": mode, "generation": uuid.uuid4().hex}
            # Preserve active/manual reservations across mode changes. Pending
            # automatic grants will be re-evaluated by the next controller tick.
            old_admissions["generation"] = configuration["generation"]
            old_admissions["grants"] = {token: grant for token, grant in old_admissions["grants"].items()
                                        if grant["manual"] or "pgid" in (gate.receipt(grant["task_id"]) or {})}
            write_private(gate.state / "resource-admissions.json", old_admissions)
            write_private(gate.state / "scheduling.json", configuration)
        changed = False
        for task_id, task in gate.snapshot()["tasks"].items():
            if command_token(task) is None:
                continue
            state = task_status(task)[0]
            if mode == "adaptive" and state == "Queued":
                gate.pueue("stash", task_id, check=False)
                changed = True
            elif mode != "adaptive" and state == "Stashed":
                gate.pueue("enqueue", task_id)
                changed = True
        if changed:
            save_barrier(gate)


def manual_grant(gate, task_id: int, token: str):
    """Record an explicit operator override, never an automatic force start."""
    from agent_heavy import private_lock, write_private
    with private_lock(gate):
        configuration = settings(gate)
        admissions = ledger(gate, configuration)
        admissions["grants"][token] = {"task_id": task_id, "manual": True,
                                      "expires": time.time() + 30, "prediction": {}}
        write_private(gate.state / "resource-admissions.json", admissions)


class Controller:
    def __init__(self, gate, sampler=None, store=None):
        from agent_heavy import boot_identity
        from job_profiles import ProfileStore
        from resource_monitor import ResourceSampler
        self.gate = gate
        self.sampler = sampler if sampler is not None else ResourceSampler()
        self.store = store if store is not None else ProfileStore(gate.state)
        self.boot = boot_identity()
        self.recovery_until = 0.0
        self.runs = {}
        self.finalized = set()
        self.groups = {}

    def sample_run(self, task_id, token, marker, host, prediction):
        from agent_heavy import read_private, write_private
        from resource_monitor import SamplingError, MAX_SAMPLE_INTERVAL
        run = self.runs.get(token)
        if run is None:
            try:
                run = read_private(self.gate.state / "resource-runs" / f"{token}.json")
            except FileNotFoundError:
                observed_since = time.time()
                started = marker.get("executionStartedAt")
                timely = _number(started) and 0 <= observed_since - started <= MAX_SAMPLE_INTERVAL
                # Old/missing start receipts and a controller arriving late can
                # supply upper evidence, but never certify an unseen prefix.
                run = {"started": started if _number(started) and started <= observed_since else observed_since,
                       "observedSince": observed_since, "boot": self.boot, "samples": 0, "attempts": 0, "complete": timely,
                       "memoryBytes": 0, "peakMemoryBytes": 0, "cpuSeconds": 0.0,
                       "cpuCores": 0.0, "peakCpuCores": 0.0, "durationSeconds": 0.0}
            else:
                # A restarted sampler has lost process deltas and possibly peaks.
                run["complete"] = False
            self.runs[token] = run
        try:
            measured = self.sampler.group(marker["pgid"], marker["members"])
            run["latestComplete"] = measured["complete"]
            if not measured["complete"] and (run.get("attempts", 0) > 0 or not measured.get("baseline", False)):
                run["complete"] = False
            run["memoryBytes"] = measured["memoryBytes"]
            run["peakMemoryBytes"] = max(run["peakMemoryBytes"], measured["memoryBytes"])
            run["cpuSeconds"] = max(run["cpuSeconds"], measured["cpuSeconds"])
            run["cpuCores"] = measured["cpuCores"]
            run["peakCpuCores"] = max(run["peakCpuCores"], measured["cpuCores"])
            run["samples"] += int(measured["complete"])
        except SamplingError:
            run["complete"] = False
            run["latestComplete"] = False
        run["attempts"] = run.get("attempts", 0) + 1
        run["durationSeconds"] = max(0, time.time() - run["started"])
        run["sampledAt"] = time.time()
        run["task_id"] = task_id
        run["machine"] = host["machine"]
        write_private(self.gate.state / "resource-runs" / f"{token}.json", run)
        self.groups[token] = marker["pgid"]
        observed = {key: run[key] for key in ("memoryBytes", "peakMemoryBytes", "cpuSeconds", "cpuCores",
                                              "durationSeconds", "samples", "complete")}
        observed["complete"] = run.get("latestComplete", False)
        # Never shrink a live reservation below a peak already observed.
        prediction = {**prediction, "memoryBytes": max(prediction["memoryBytes"], int(run["peakMemoryBytes"] * 1.2)),
                      "cpuCores": max(prediction["cpuCores"], run["peakCpuCores"])}
        return {"prediction": prediction, "observed": observed, "decision": None}

    def tick(self):
        from agent_heavy import (command_token, private_lock, read_private, result_code, shared_manifest,
                                 task_status, validate_limit, validate_marker, write_private)
        gate = self.gate
        host = self.sampler.host()
        if host["memoryPressure"] != "normal" or host["swapOutBytesPerSecond"] > 1024 ** 2:
            self.recovery_until = time.monotonic() + RECOVERY_SECONDS
        jobs, active, candidates, audits = {}, [], [], []
        sampled_active = set()
        snapshot = gate.snapshot()
        limit = snapshot["groups"]["default"]["parallel_tasks"]
        validate_limit(limit)
        terminal = sorted(((key, task) for key, task in snapshot["tasks"].items() if task_status(task)[0] == "Done"),
                          key=lambda entry: (task_status(entry[1])[1].get("end", ""), int(entry[0])), reverse=True)[:30]
        terminal_tokens = {command_token(task) for _key, task in terminal}
        entries = [(key, task) for key, task in snapshot["tasks"].items()
                   if task_status(task)[0] != "Done" or command_token(task) in terminal_tokens | self.runs.keys()]
        for key, task in entries:
            token = command_token(task)
            if token is None:
                if task_status(task)[0] in ("Running", "Paused") and task.get("group", "default") == "default":
                    active.append({"prediction": {"memoryBytes": GIB, "cpuCores": 2.0}, "observed": {}})
                continue
            task_id = int(key)
            workload = ensure_identity(gate, token, task_id)
            if workload is None:
                if task_status(task)[0] in ("Running", "Paused"):
                    active.append({"prediction": {"memoryBytes": GIB, "cpuCores": 2.0}, "observed": {}})
                continue
            prediction = self.store.prediction(workload, host["machine"])
            state, details = task_status(task)
            resources = {"prediction": prediction, "observed": None, "decision": None}
            receipt = gate.receipt(task_id)
            if receipt and "pgid" in receipt:
                with private_lock(gate):
                    marker = validate_marker(gate, task_id)
                if marker is not None:
                    resources = self.sample_run(task_id, token, marker, host, prediction)
                    active.append(resources)
                    sampled_active.add(token)
            elif state in ("Running", "Paused"):
                candidates.append((task_id, token, task, resources))
            if state == "Done" and receipt is not None:
                # Pueue can report Done while its cleanup callback still owns
                # descendants. Their tail is not a completed cheap run.
                if token in self.runs:
                    self.runs[token]["complete"] = False
            elif state == "Done":
                run = self.runs.get(token)
                observed_here = run is not None
                if run is None:
                    with contextlib.suppress(FileNotFoundError):
                        run = read_private(gate.state / "resource-runs" / f"{token}.json")
                if run:
                    if not observed_here:
                        run["complete"] = False
                    from resource_monitor import MAX_SAMPLE_INTERVAL
                    try:
                        end_date = datetime.fromisoformat(details["end"].replace("Z", "+00:00"))
                        if end_date.utcoffset() is None:
                            raise ValueError("Execution end requires an offset")
                        ended = end_date.timestamp()
                    except (KeyError, AttributeError, TypeError, ValueError, OverflowError):
                        ended = None
                    observed_at = run.get("sampledAt", run["started"] + run["durationSeconds"])
                    if ended is None or not 0 <= ended - observed_at <= MAX_SAMPLE_INTERVAL or ended > time.time():
                        run["complete"] = False
                    if ended is not None and run["started"] <= ended <= time.time():
                        run["durationSeconds"] = max(run["durationSeconds"], ended - run["started"])
                    metrics = {"memoryBytes": run["peakMemoryBytes"], "cpuSeconds": run["cpuSeconds"],
                               "cpuCores": run["peakCpuCores"], "durationSeconds": run["durationSeconds"],
                               "sampledAt": run.get("sampledAt", run["started"] + run["durationSeconds"])}
                    if token not in self.finalized:
                        self.store.record(token, workload, run["machine"], metrics,
                                          complete=run["complete"] and run["samples"] >= 2 and run["boot"] == self.boot,
                                          exit_code=result_code(details.get("result")))
                    resources["observed"] = {key: run[key] for key in ("memoryBytes", "peakMemoryBytes", "cpuSeconds",
                                               "cpuCores", "durationSeconds", "samples", "complete")}
                self.finalized.add(token)
            elif state in ("Queued", "Stashed"):
                candidates.append((task_id, token, task, resources))
            jobs[token] = resources
        configuration = settings(gate)
        reason = "Collecting workload history" if configuration["mode"] == "fixed" else "Resource predictions are ready"
        with private_lock(gate, "scheduling.lock"), private_lock(gate):
            configuration = settings(gate)
            admissions = ledger(gate, configuration)
            current = gate.snapshot()
            group = current["groups"]["default"]
            limit = group["parallel_tasks"]
            validate_limit(limit)
            host_fresh = 0 <= time.time() - host["sampledAt"] <= FRESH_SECONDS
            admission_ready = (host_fresh and host.get("complete") and host["memoryPressure"] == "normal"
                               and host["swapOutBytesPerSecond"] <= 1024 ** 2
                               and time.monotonic() >= self.recovery_until)
            # Sampling ran outside the lock. Any newly acquired lease must be
            # counted even if it was absent from that earlier census.
            if gate.slot.exists() and shared_manifest(gate) is not None:
                for path in (gate.slot / "agent-heavy-jobs").iterdir():
                    member = read_private(path)
                    if member["token"] not in sampled_active:
                        active.append({"prediction": {"memoryBytes": GIB, "cpuCores": 2.0}, "observed": {}})
            for token, grant in list(admissions["grants"].items()):
                task = current["tasks"].get(str(grant["task_id"]))
                receipt = gate.receipt(grant["task_id"])
                pending = not (receipt and "pgid" in receipt)
                if task_status(task)[0] in ("Missing", "Done") or (pending and (
                    grant["expires"] < time.time() or (not grant["manual"] and not admission_ready)
                )):
                    del admissions["grants"][token]
                elif not (receipt and "pgid" in receipt):
                    # Include reservations for starts that have not yet acquired
                    # the lease, so two ticks cannot spend the same capacity.
                    active.append({"prediction": grant["prediction"] or {"memoryBytes": GIB, "cpuCores": 2.0},
                                   "observed": {}})
            candidates.sort(key=lambda entry: (-entry[2].get("priority", 0), entry[0]))
            selected = None
            for index, (task_id, token, task, resources) in enumerate(candidates):
                allowed, why = decision(host, resources["prediction"], active, limit)
                if not host_fresh:
                    allowed, why = False, "Waiting for a fresh host resource sample"
                if time.monotonic() < self.recovery_until:
                    allowed, why = False, "Waiting for a stable recovery interval"
                if group["status"] != "Running":
                    allowed, why = False, "Queue is paused"
                if gate.slot.exists() and shared_manifest(gate) is None:
                    allowed, why = False, "Waiting for the external shared validation owner"
                if index and candidates[0][2].get("priority", 0) > task.get("priority", 0):
                    allowed, why = False, "Waiting behind a higher-priority job"
                if index:
                    created = candidates[0][2].get("created_at")
                    try:
                        age = time.time() - datetime.fromisoformat(created.replace("Z", "+00:00")).timestamp()
                    except (AttributeError, TypeError, ValueError):
                        age = STARVATION_SECONDS
                    if age >= STARVATION_SECONDS:
                        allowed, why = False, "Reserving the next opportunity for an older job"
                if token in admissions["grants"]:
                    allowed, why = False, "Admission is starting"
                if task_status(current["tasks"].get(str(task_id)))[0] not in ("Queued", "Stashed", "Running"):
                    allowed, why = False, "Job changed while resources were sampled"
                resources["decision"] = {"allowed": bool(allowed), "reason": why}
                if configuration["mode"] in ("shadow", "adaptive"):
                    audits.append((token, configuration["mode"], resources["prediction"], bool(allowed), why))
                if selected is None and allowed:
                    selected = (task_id, token, resources)
            # Publish the heartbeat before helpers may consume their grants.
            available = bool(host.get("complete")) and 0 <= time.time() - host["sampledAt"] <= FRESH_SECONDS
            if not available:
                reason = "Collecting a complete resource interval"
            value = {"mode": configuration["mode"], "generation": configuration["generation"],
                     "boot": self.boot, "sampledAt": time.time(), "updatedAt": datetime.now(UTC).isoformat(),
                     "available": available, "admissionReady": bool(admission_ready),
                     "reason": reason, "host": host, "jobs": jobs}
            write_private(gate.state / "resource-status.json", value)
            if configuration["mode"] == "adaptive":
                # Stash anything submitted just before a mode change. Helpers
                # already launched by Pueue still need a grant under the lease.
                for task_id, task in current["tasks"].items():
                    if command_token(task) and task_status(task)[0] == "Queued":
                        gate.pueue("stash", task_id, check=False)
                if selected and available:
                    task_id, token, resources = selected
                    admissions["grants"][token] = {"task_id": task_id, "manual": False,
                                                  "expires": time.time() + GRANT_SECONDS,
                                                  "prediction": resources["prediction"]}
                    write_private(gate.state / "resource-admissions.json", admissions)
                    if task_status(current["tasks"].get(str(task_id)))[0] in ("Queued", "Stashed"):
                        gate.pueue("start", str(task_id))
            write_private(gate.state / "resource-admissions.json", admissions)
        for audit in audits:
            self.store.record_decision(*audit, sampled_at=host["sampledAt"])
        # Bound cache files and in-memory terminal bookkeeping without touching
        # any task that still owns a receipt or is present in the active queue.
        retained = {command_token(task) for task in snapshot["tasks"].values() if task_status(task)[0] != "Done"}
        retained.update(read_private(path)["token"] for path in gate.receipts.glob("*.json"))
        for token in list(self.runs):
            if token not in retained and token not in terminal_tokens:
                self.runs.pop(token, None)
        for token, pgid in list(self.groups.items()):
            if token not in retained:
                self.sampler.forget_group(pgid)
                del self.groups[token]
        self.finalized.intersection_update(terminal_tokens)
        for directory in ("resource-runs", "workloads"):
            files = sorted((gate.state / directory).glob("*.json"), key=lambda path: path.stat().st_mtime)
            for path in files[:-1000]:
                if path.stem not in retained | terminal_tokens:
                    path.unlink()
        return value

    def close(self):
        self.store.close()

    def censor_runs(self):
        """A collector outage cannot become evidence of a cheap full run."""
        from agent_heavy import write_private
        for token, run in self.runs.items():
            run["complete"] = False
            run["latestComplete"] = False
            write_private(self.gate.state / "resource-runs" / f"{token}.json", run)


def serve(gate, *, sampler=None, once=False):
    from agent_heavy import GateError, private_lock, write_private
    from job_profiles import ProfileError
    from resource_monitor import SamplingError
    gate.prepare()
    stopped = False

    def stop(_signum, _frame):
        nonlocal stopped
        stopped = True

    with private_lock(gate, "resource-controller.lock"):
        controller = Controller(gate, sampler=sampler)
        originals = {sig: signal.signal(sig, stop) for sig in (signal.SIGTERM, signal.SIGINT)}
        try:
            while not stopped:
                started = time.monotonic()
                try:
                    controller.tick()
                except (GateError, SamplingError, ProfileError, SchedulingError, OSError, ValueError) as error:
                    controller.censor_runs()
                    configuration = settings(gate)
                    write_private(gate.state / "resource-status.json", {
                        "mode": configuration["mode"], "generation": configuration["generation"],
                        "boot": controller.boot, "sampledAt": time.time(), "updatedAt": datetime.now(UTC).isoformat(),
                        "available": False, "reason": f"Resource collection failed ({type(error).__name__}); admissions held",
                        "host": None, "jobs": {}})
                    if once:
                        raise
                if once:
                    return
                while not stopped and time.monotonic() - started < 2:
                    time.sleep(0.1)
        finally:
            for sig, handler in originals.items():
                signal.signal(sig, handler)
            controller.close()
