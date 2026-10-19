"""Private Unix-socket queue API. Caddy exclusively owns TLS and authentication."""
from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
from pathlib import Path

from flask import Flask, jsonify, request, send_from_directory
from flask_wtf.csrf import CSRFError, CSRFProtect, generate_csrf
from werkzeug.exceptions import HTTPException

CORE = Path(__file__).resolve().parents[1] / "agent_heavy.py"
STATIC = Path(__file__).resolve().parent / "static"
MAX_LOG = 65536


class BackendError(Exception):
    pass


def core_runner(arguments: list[str]) -> dict:
    try:
        result = subprocess.run([sys.executable, str(CORE), *arguments], stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=23, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BackendError("Queue controller is unavailable") from error
    if result.returncode:
        raise BackendError("Queue action failed; inspect the local Agent Queue")
    if arguments == ["status", "--json"]:
        try:
            value = json.loads(result.stdout)
        except ValueError as error:
            raise BackendError("Queue controller returned invalid status") from error
        if not isinstance(value, dict) or not isinstance(value.get("jobs"), list):
            raise BackendError("Queue controller returned invalid status")
        return value
    return {"ok": True}


def create_app(settings: dict, runner=None, *, test_mode: bool = False) -> Flask:
    injected = runner is not None
    run = runner or core_runner
    scheme = settings.get("scheme", "https")
    secure_cookie = settings.get("secure_cookie", True)
    if (scheme != "https" or secure_cookie is not True) and not (test_mode and injected):
        raise ValueError("Production remote access requires HTTPS and secure cookies")
    hosts = settings["hosts"]
    port = settings["port"]
    authorities = {f"{host.lower()}:{port}" for host in hosts}
    if port == (443 if scheme == "https" else 80):
        authorities.update(host.lower() for host in hosts)
    origins = {f"{scheme}://{host}" for host in authorities}
    app = Flask(__name__, static_folder=None)
    app.config.update(SECRET_KEY=settings["secret_key"], TRUSTED_HOSTS=hosts,
                      SESSION_COOKIE_NAME="agent_remote_csrf", SESSION_COOKIE_SECURE=secure_cookie,
                      SESSION_COOKIE_HTTPONLY=True, SESSION_COOKIE_SAMESITE="Strict",
                      MAX_CONTENT_LENGTH=4096, WTF_CSRF_TIME_LIMIT=3600,
                      WTF_CSRF_SSL_STRICT=True)

    @app.before_request
    def validate_request_boundary():
        if request.host.lower() not in authorities or request.scheme != scheme:
            return jsonify(error="Unrecognized request origin"), 400
        if request.method not in ("GET", "HEAD", "OPTIONS") and request.headers.get("Origin") not in origins:
            return jsonify(error="Cross-origin mutation rejected"), 403
        return None

    CSRFProtect(app)

    @app.after_request
    def security_headers(response):
        response.headers["Cache-Control"] = "no-store"
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["Referrer-Policy"] = "same-origin"
        response.headers["Content-Security-Policy"] = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
        return response

    @app.errorhandler(CSRFError)
    def csrf_error(_error):
        return jsonify(error="CSRF validation failed; refresh this page"), 400

    @app.errorhandler(BackendError)
    def backend_error(error):
        return jsonify(error=str(error)), 502

    @app.errorhandler(HTTPException)
    def http_error(error):
        return jsonify(error=error.name), error.code

    @app.get("/api/status")
    def queue_status():
        return jsonify(run(["status", "--json"]))

    @app.get("/api/csrf")
    def csrf_token():
        return jsonify(token=generate_csrf())

    @app.post("/api/limit")
    def limit():
        value = request.get_json()
        if not isinstance(value, dict) or set(value) != {"limit"} or type(value["limit"]) is not int or not 1 <= value["limit"] <= 8:
            return jsonify(error="Limit must be an integer from 1 to 8"), 400
        run(["limit", str(value["limit"])])
        return jsonify(ok=True)

    @app.post("/api/scheduling")
    def scheduling():
        value = request.get_json()
        if not isinstance(value, dict) or set(value) != {"mode"} or not isinstance(value["mode"], str) \
                or value["mode"] not in {"fixed", "shadow", "adaptive"}:
            return jsonify(error="Scheduling mode must be fixed, shadow, or adaptive"), 400
        run(["scheduling", value["mode"]])
        return jsonify(ok=True)

    @app.post("/api/jobs/<int:task_id>/<action>")
    def job_action(task_id, action):
        if action not in {"next", "last", "start", "cancel"} or task_id > 2**63 - 1:
            return jsonify(error="Unknown queue action"), 404
        value = request.get_json(silent=True)
        if value not in (None, {}):
            return jsonify(error="This action does not accept parameters"), 400
        run([action, str(task_id)])
        return jsonify(ok=True)

    @app.get("/api/jobs/<int:task_id>/log")
    def job_log(task_id):
        jobs = run(["status", "--json"]).get("jobs", [])
        job = next((candidate for candidate in jobs if candidate.get("id") == task_id), None)
        if job is None or not isinstance(job.get("logPath"), str):
            return jsonify(error="Job log is unavailable"), 404
        path = Path(job["logPath"])
        expected = Path.home() / ".local/state/agent-heavy/pueue/task_logs"
        if not test_mode and (path.name != f"{task_id}.log" or path.parent.resolve() != expected.resolve()):
            raise BackendError("Controller supplied an invalid log path")
        try:
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(descriptor, "rb") as handle:
                info = os.fstat(handle.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
                    raise BackendError("Job log ownership could not be verified")
                offset = max(0, info.st_size - MAX_LOG)
                handle.seek(offset)
                output = handle.read(MAX_LOG).decode("utf-8", errors="replace")
        except FileNotFoundError:
            return jsonify(output="", truncated=False)
        return jsonify(output=output, truncated=offset > 0)

    @app.get("/")
    def index():
        return send_from_directory(STATIC, "index.html")

    @app.get("/static/<path:filename>")
    def asset(filename):
        return send_from_directory(STATIC, filename)

    return app
