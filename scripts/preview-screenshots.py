#!/usr/bin/env python3
"""Preview the real phone dashboard with public, simulated screenshot data.

Run from the repository with its Python dependencies installed:
    python scripts/preview-screenshots.py

The printed URL uses an ephemeral loopback port. To preview on a LAN device,
pass --bind with the development machine's LAN IP. Stop with Ctrl+C.
This uses the browser-test fake runner, never the installed queue or settings.
"""
from __future__ import annotations

import argparse
import ipaddress
import runpy
import secrets
import signal
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

from waitress import create_server


def private_address(value: str) -> str:
    address = ipaddress.ip_address(value)
    if address.version != 4 or not (address.is_private or address.is_loopback) or address.is_unspecified:
        raise argparse.ArgumentTypeError("Use an explicit loopback or private IPv4 address")
    return str(address)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bind", type=private_address, default="127.0.0.1")
    arguments = parser.parse_args()

    def stop(_signum, _frame):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    fixture = runpy.run_path(str(Path(__file__).resolve().parents[1] / "remote/tests/browser/server.py"))
    with tempfile.TemporaryDirectory(prefix="agent-queue-screenshots-") as directory:
        queue = fixture["FakeQueue"](Path(directory))
        now = datetime.now(timezone.utc).replace(second=0, microsecond=0)
        examples = [("Build desktop app", "desktop-app", "feat/menu-bar"),
                    ("Run browser tests", "web-dashboard", "fix/queue-refresh"),
                    ("Typecheck API", "api-service", "feat/job-controls")]
        for index, (job, (title, project, branch)) in enumerate(zip(queue.jobs, examples, strict=True)):
            job.update(title=title, project=project, branch=branch,
                       submittedAt=(now - timedelta(minutes=5 - index)).isoformat(),
                       startedAt=(now - timedelta(minutes=2)).isoformat() if index == 0 else None)
            Path(job["logPath"]).write_text(f"Starting {title}\nExample validation step passed\n")

        def application(environ, start_response):
            return app(environ, start_response)

        listener = create_server(application, host=arguments.bind, port=0, threads=2)
        app = fixture["create_app"]({"hosts": [arguments.bind], "port": listener.effective_port,
                                     "scheme": "http", "secure_cookie": False,
                                     "secret_key": secrets.token_hex(32)}, runner=queue, test_mode=True)
        print(f"Simulated dashboard: http://{arguments.bind}:{listener.effective_port}/", flush=True)
        print("All projects and jobs are artificial. Ctrl+C stops this preview.", flush=True)
        listener.run()


if __name__ == "__main__":
    main()
