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

    def __call__(self, arguments):
        with self.lock:
            if self.offline:
                raise BackendError("Fixture queue is offline")
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
