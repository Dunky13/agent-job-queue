"""Ephemeral, loopback-only browser fixture with no access to the real queue."""
from __future__ import annotations

import copy
import json
import secrets
import signal
import sys
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path

from flask import jsonify, request
from waitress import create_server

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from server import BackendError, create_app


class FakeQueue:
    def __init__(self, root: Path):
        self.root = root
        self.lock = threading.RLock()
        self.reset()

    def reset(self):
        with self.lock:
            self.events = []
            self.offline = False
            self.limit = 1
            self.scheduler = None
            self.profile_revision = 4
            self.profiles = self.learning_profiles()
            self.profile_inputs = []
            self.jobs = [self.job(101, "running", "Build app"),
                         self.job(102, "queued", "Run browser tests"),
                         self.job(103, "queued", "Typecheck client")]

    def job(self, identifier: int, state: str, title: str):
        log = self.root / f"{identifier}.log"
        log.write_text(f"Starting {title}\nPassed fixture step\n")
        return {"id": identifier, "state": state, "title": title,
                "project": "example-project", "branch": "feature/queue",
                "directory": "/example/project", "executable": "fixture-command",
                "priority": 0, "queuePosition": None if state == "running" else identifier - 101,
                "submittedAt": "2026-01-01T10:00:00+00:00",
                "startedAt": "2026-01-01T10:01:00+00:00" if state == "running" else None,
                "finishedAt": None, "exitCode": None, "logPath": str(log),
                "canReorder": state == "queued", "canStart": state == "queued",
                "canCancel": True, "phase": state.capitalize()}

    def learning_profiles(self):
        def detail(identifier, label, families):
            members = [{"family": family, "label": f"{label} task {index + 1}", "project": "example-project", "executable": "python"} for index, family in enumerate(families)]
            profile = {"id": identifier, "label": label, "memberCount": len(members), "variantCount": 1,
                       "successCount": 5, "incompleteCount": 1, "peakMemoryBytes": 1610612736,
                       "peakCpuCores": 1.5, "lastRunAt": 1792404000, "memoryFloorBytes": None, "cpuFloorCores": None}
            return {"revision": 4, "profile": profile, "members": members,
                    "controllerAppliedRevision": 3, "controllerStatus": "Waiting for next fresh pass",
                    "variants": [{"id": "f" * 64, "family": families[0], "successCount": 5, "incompleteCount": 1,
                                  "lastRunAt": 1792404000, "peakMemoryBytes": 1610612736, "peakCpuCores": 1.5}],
                    "runs": [{"id": "9" * 64, "family": families[0], "variant": "f" * 64, "machine": "2" * 64,
                              "sampledAt": 1792404000, "memoryBytes": 1610612736, "cpuCores": 1.5,
                              "cpuSeconds": 20, "durationSeconds": 15, "success": False}],
                    "prediction": {"memoryBytes": 2147483648, "cpuCores": 2, "durationSeconds": 60,
                                   "samples": 0, "confidence": "unknown", "source": "family"},
                    "predictionMachine": "2" * 64, "truncated": {"members": False, "variants": False, "runs": False}}
        return {"a" * 64: detail("a" * 64, "Browser tests", ["c" * 64, "d" * 64]),
                "b" * 64: detail("b" * 64, "Client build", ["e" * 64])}

    def profile_call(self, arguments, input_data):
        action = arguments[1]
        if action == "list":
            offset, limit, query = int(arguments[3]), int(arguments[5]), arguments[7]
            profiles = [copy.deepcopy(detail["profile"]) for detail in self.profiles.values() if query.casefold() in detail["profile"]["label"].casefold()]
            return {"revision": self.profile_revision, "profiles": profiles[offset:offset + limit],
                    "total": len(profiles), "offset": offset, "limit": limit,
                    "controllerAppliedRevision": 3, "controllerStatus": "Waiting for next fresh pass"}
        identifier = arguments[2]
        if identifier not in self.profiles:
            raise BackendError("Workload profile no longer exists; refresh learning")
        detail = self.profiles[identifier]
        if action == "show":
            return copy.deepcopy({**detail, "revision": self.profile_revision})
        value = json.loads(input_data)
        if value["revision"] != self.profile_revision:
            raise BackendError("Learning changed; refresh before saving corrections")
        self.events.append(arguments)
        self.profile_inputs.append({"action": action, "id": identifier, "value": value})
        if action == "edit":
            detail["profile"].update({key: value[key] for key in ("label", "memoryFloorBytes", "cpuFloorCores")})
        elif action == "reset":
            detail["runs"], detail["variants"] = [], []
            detail["profile"].update(successCount=0, incompleteCount=0, variantCount=0, peakMemoryBytes=0, peakCpuCores=0, lastRunAt=None)
        elif action == "merge":
            target = self.profiles[value["targetId"]]
            target["members"].extend(detail["members"])
            target["profile"]["memberCount"] = len(target["members"])
            del self.profiles[identifier]
            detail = target
        elif action == "split":
            member = next(member for member in detail["members"] if member["family"] == value["family"])
            detail["members"].remove(member)
            detail["profile"]["memberCount"] = len(detail["members"])
            separate = copy.deepcopy(detail)
            separate["profile"].update(id="3" * 64, label=member["label"], memberCount=1)
            separate["members"] = [member]
            self.profiles["3" * 64] = separate
            detail = separate
        self.profile_revision += 1
        return copy.deepcopy({**detail, "revision": self.profile_revision})

    def __call__(self, arguments, input_data=None):
        with self.lock:
            if self.offline:
                raise BackendError("Fixture queue is offline")
            if arguments[0] == "profiles":
                return self.profile_call(arguments, input_data)
            if arguments == ["status", "--json"]:
                snapshot = {"updatedAt": datetime.now(timezone.utc).isoformat(),
                        "queue": {"status": "running", "limit": self.limit,
                                  "running": sum(job["state"] == "running" for job in self.jobs),
                                  "waiting": sum(job["state"] == "queued" for job in self.jobs),
                                  "legacyOccupied": False}, "jobs": copy.deepcopy(self.jobs)}
                if self.scheduler is not None:
                    snapshot["scheduler"] = copy.deepcopy(self.scheduler)
                return snapshot
            self.events.append(arguments)
            if arguments[0] == "limit":
                self.limit = int(arguments[1])
                return {"ok": True}
            if arguments[0] == "scheduling":
                if arguments[1] == "adaptive" and not self.scheduler["available"]:
                    raise BackendError("Resource sampler unavailable")
                self.scheduler["mode"] = arguments[1]
                return {"ok": True}
            job = next(job for job in self.jobs if job["id"] == int(arguments[1]))
            if arguments[0] == "start":
                job.update(state="running", phase="Running", queuePosition=None,
                           canStart=False, canReorder=False,
                           startedAt="2026-01-01T10:02:00+00:00")
            elif arguments[0] == "cancel":
                job.update(state="cancelled", phase="Cancelled", queuePosition=None,
                           canStart=False, canCancel=False, canReorder=False)
            elif arguments[0] in {"next", "last"}:
                waiting = [candidate for candidate in self.jobs
                           if candidate["state"] == "queued" and candidate is not job]
                ordered = [job, *waiting] if arguments[0] == "next" else [*waiting, job]
                for position, candidate in enumerate(ordered, 1):
                    candidate["queuePosition"] = position
            return {"ok": True}


def main():
    def stop(_signum, _frame):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    with tempfile.TemporaryDirectory(prefix="agent-queue-browser-") as directory:
        queue = FakeQueue(Path(directory))
        # The HTTP exception requires BOTH explicit test_mode and an injected runner.
        settings = {"hosts": ["127.0.0.1"], "scheme": "http",
                    "secure_cookie": False, "secret_key": secrets.token_hex(32)}
        # Bind first so the real request boundary uses the actual ephemeral port.
        # The WSGI closure is invoked only after app setup and listener.run().
        def application(environ, start_response):
            return app(environ, start_response)

        listener = create_server(application, host="127.0.0.1", port=0, threads=2)
        settings["port"] = listener.effective_port
        app = create_app(settings, runner=queue, test_mode=True)

        @app.get("/__fixture/events")
        def events():
            with queue.lock:
                return jsonify(events=queue.events)

        @app.post("/__fixture/reset")
        def reset():
            queue.reset()
            return jsonify(ok=True)

        @app.post("/__fixture/state")
        def update():
            values = request.get_json()
            with queue.lock:
                if "profileFloors" in values:
                    queue.profiles["a" * 64]["profile"].update(values["profileFloors"])
                if "profileRevision" in values:
                    queue.profile_revision = values["profileRevision"]
                if "profileCount" in values:
                    template = queue.learning_profiles()["a" * 64]
                    queue.profiles = {}
                    for index in range(values["profileCount"]):
                        identifier = f"{index:064x}"
                        detail = copy.deepcopy(template)
                        detail["profile"].update(id=identifier, label=f"Workload {index}")
                        queue.profiles[identifier] = detail
                if "profiles" in values:
                    queue.profiles = values["profiles"]
                if "offline" in values:
                    queue.offline = values["offline"]
                if "title" in values:
                    queue.jobs[1]["title"] = values["title"]
                if "scheduler" in values:
                    queue.scheduler = values["scheduler"]
                if "resources" in values:
                    queue.jobs[1]["resources"] = values["resources"]
            return jsonify(ok=True)

        origin = f"http://127.0.0.1:{listener.effective_port}"
        print(json.dumps({"url": origin}), flush=True)
        listener.run()


if __name__ == "__main__":
    main()
