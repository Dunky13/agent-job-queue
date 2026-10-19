#!/usr/bin/env python3
"""Host-wide foreground command queue backed by a dedicated Pueue daemon."""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import gzip
import json
import os
import re
import select
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path

import resource_scheduler
from job_profiles import ProfileError

TOOL_PATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
MODULE = str(Path(__file__).resolve())
OVERRIDES = ("AGENT_HEAVY_CONFIG", "AGENT_HEAVY_STATE", "AGENT_HEAVY_LEGACY_SLOT", "AGENT_HEAVY_PUEUE")
MINIMUM_ENV = ("HOME", "PATH", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL")
TOKEN = re.compile(r"[0-9a-f]{32}\Z")
MAX_PARALLEL = 8
GENERIC_TITLES = {"Shell batch", "Python batch", "Validation batch", "Active validation batch"}


class GateError(Exception):
    """An unsafe or unavailable queue must never run a command directly."""


class Interrupted(Exception):
    def __init__(self, signum: int):
        self.signum = signum


class Gate:
    def __init__(self):
        self.config = Path(os.environ.get(OVERRIDES[0], Path.home() / ".config/agent-heavy/pueue.yml"))
        self.state = Path(os.environ.get(OVERRIDES[1], Path.home() / ".local/state/agent-heavy"))
        self.slot = lease_slot(self.state)
        self.jobs = self.state / "jobs"
        self.receipts = self.state / "receipts"
        self.cancelled = self.state / "cancelled"
        self.metadata = self.state / "metadata"
        self.description_cache = {}
        self.project_cache = {}
        self.control_deadline = None
        self.control_env = {key: os.environ[key] for key in MINIMUM_ENV + OVERRIDES if key in os.environ}
        self.control_env.setdefault("PATH", TOOL_PATH)

    def pueue_executable(self) -> str:
        override = self.control_env.get("AGENT_HEAVY_PUEUE")
        if override is not None:
            if not Path(override).is_absolute() or not Path(override).is_file() or not os.access(override, os.X_OK):
                raise GateError("AGENT_HEAVY_PUEUE must name an absolute executable file")
            return override
        executable = shutil.which("pueue", path=self.control_env["PATH"] + os.pathsep + TOOL_PATH)
        if executable is None:
            raise GateError("Pueue is not installed; install pueue before using the queue")
        return executable

    def prepare(self):
        for directory in (self.state, self.jobs, self.receipts, self.cancelled, self.metadata,
                          self.state / "workloads", self.state / "resource-runs"):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            info = directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
                raise GateError(f"Unsafe private directory: {directory}")
            directory.chmod(0o700)

    def pueue(self, *args: str, check: bool = True, env_extra: dict | None = None) -> subprocess.CompletedProcess:
        timeout = 15 if self.control_deadline is None else min(15, self.control_deadline - time.monotonic())
        if timeout <= 0:
            raise GateError("Queue control deadline expired; inspect the current queue state")
        try:
            result = subprocess.run([self.pueue_executable(), "-c", str(self.config), "--color", "never", *args],
                                    env={**self.control_env, **(env_extra or {})}, capture_output=True, timeout=timeout,
                                    umask=0o077, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise GateError(f"Pueue unavailable ({type(error).__name__}); command was not run directly") from error
        if check and result.returncode:
            # Pueue's own errors may include command strings. Keep control errors private.
            raise GateError(f"Pueue {args[0]} failed (exit {result.returncode}); check the dedicated daemon")
        return result

    def snapshot(self) -> dict:
        try:
            data = json.loads(self.pueue("status", "--json").stdout)
        except (ValueError, TypeError) as error:
            raise GateError("Pueue returned invalid status JSON") from error
        if not isinstance(data, dict) or not isinstance(data.get("tasks"), dict):
            raise GateError("Pueue returned an invalid task map")
        return data

    def task(self, task_id: int) -> dict | None:
        task = self.snapshot()["tasks"].get(str(task_id))
        if task is not None and not isinstance(task, dict):
            raise GateError("Pueue returned an invalid task")
        return task

    def receipt_path(self, task_id: int) -> Path:
        return self.receipts / f"{task_id}.json"

    def payload_path(self, token: str) -> Path:
        if not TOKEN.fullmatch(token):
            raise GateError("Invalid private job token")
        return self.jobs / f"{token}.json"

    def receipt(self, task_id: int) -> dict | None:
        path = self.receipt_path(task_id)
        try:
            return read_private(path)
        except FileNotFoundError:
            return None


def write_private(path: Path, value: dict):
    temporary = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w") as handle:
            json.dump(value, handle)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def read_private(path: Path) -> dict:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise GateError(f"Unsafe private job file: {path}")
        value = json.load(handle)
    if not isinstance(value, dict):
        raise GateError("Private job file must contain an object")
    return value


def lease_slot(state: Path) -> Path:
    """Retain an upgraded installation's lease without embedding its old path."""
    if "AGENT_HEAVY_LEGACY_SLOT" in os.environ:
        value = os.environ["AGENT_HEAVY_LEGACY_SLOT"]
    else:
        try:
            settings = read_private(state / "legacy-slot.json")
        except FileNotFoundError:
            return state / "execution.slot"
        except (OSError, ValueError) as error:
            raise GateError("Cannot safely read the retained legacy lease configuration") from error
        if set(settings) != {"slot"}:
            raise GateError("Invalid retained legacy lease configuration fields")
        value = settings["slot"]
    if not isinstance(value, str) or not value or "\x00" in value or not Path(value).is_absolute():
        raise GateError("Legacy lease configuration must name an absolute path")
    return Path(value)


def task_status(task: dict | None) -> tuple[str, dict]:
    if task is None:
        return "Missing", {}
    status_value = task.get("status")
    if not isinstance(status_value, dict) or len(status_value) != 1:
        raise GateError("Pueue returned invalid task status")
    name, value = next(iter(status_value.items()))
    if not isinstance(value, dict):
        raise GateError("Pueue returned invalid task status details")
    return name, value


def result_code(result) -> int:
    if result == "Success":
        return 0
    if isinstance(result, dict) and set(result) == {"Failed"}:
        code = result["Failed"]
        if type(code) is int and 0 < code <= 255:
            return code
    if result == "Killed":
        return 137
    if result in ("Errored", "DependencyFailed") or isinstance(result, dict) and "FailedToSpawn" in result:
        return 125
    raise GateError("Pueue returned an unknown task result")


def interrupt(signum, _frame):
    raise Interrupted(signum)


@contextlib.contextmanager
def signal_handlers():
    originals = {sig: signal.signal(sig, interrupt) for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    try:
        yield
    finally:
        for sig, handler in originals.items():
            signal.signal(sig, handler)


def drain(path: Path, offset: int) -> int:
    try:
        with path.open("rb") as handle:
            handle.seek(offset)
            while chunk := handle.read(65536):
                sys.stdout.buffer.write(chunk)
                sys.stdout.buffer.flush()
            return handle.tell()
    except FileNotFoundError:
        return offset


def follow(gate: Gate, task_id: int) -> int:
    offset = 0
    last_notice = time.monotonic()
    path = gate.state / "pueue/task_logs" / f"{task_id}.log"
    while True:
        offset = drain(path, offset)
        name, status = task_status(gate.task(task_id))
        if name == "Missing":
            if (gate.cancelled / f"{task_id}.json").exists():
                wait_cleanup(gate, task_id)
                drain(path, offset)
                cancellation = read_private(gate.cancelled / f"{task_id}.json")
                return result_code({"Failed": cancellation["exit_code"]}) if cancellation["exit_code"] else 0
            raise GateError(f"Job {task_id} disappeared before its result could be read")
        if name == "Done":
            offset = drain(path, offset)
            code = result_code(status.get("result"))
            wait_cleanup(gate, task_id)
            drain(path, offset)
            return code
        if time.monotonic() - last_notice >= 15:
            receipt = gate.receipt(task_id)
            waiting_slot = name == "Running" and receipt is not None and "pgid" not in receipt
            phase = "waiting for shared validation slot" if waiting_slot else name.lower()
            print(f"agent-heavy: job {task_id} {phase}", file=sys.stderr, flush=True)
            last_notice = time.monotonic()
        time.sleep(0.4)


def wait_cleanup(gate: Gate, task_id: int):
    deadline = time.monotonic() + 12
    while gate.receipt(task_id) is not None:
        if time.monotonic() >= deadline:
            raise GateError(f"Job {task_id} finished but cleanup did not complete; shared slot retained")
        time.sleep(0.1)


def cancel(gate: Gate, task_id: int):
    gate.prepare()
    cancelled_path = gate.cancelled / f"{task_id}.json"
    write_private(cancelled_path, {"task_id": task_id, "exit_code": 130})
    deadline = time.monotonic() + 12
    graceful_until = None
    while time.monotonic() < deadline:
        name, details = task_status(gate.task(task_id))
        if name == "Done":
            write_private(cancelled_path, {"task_id": task_id,
                                          "exit_code": result_code(details.get("result"))})
            # Keep terminal results/logs: removing after an earlier Running
            # snapshot races completion and can discard the real exit code.
            wait_cleanup(gate, task_id)
            return
        if name == "Missing":
            receipt = gate.receipt(task_id)
            if receipt is not None:
                gate.payload_path(receipt["token"]).unlink(missing_ok=True)
                if "pgid" not in receipt:
                    gate.receipt_path(task_id).unlink(missing_ok=True)
            wait_cleanup(gate, task_id)
            return
        if name == "Queued":
            # Stash is conditional on still being queued. If execution won the
            # race, inspect its new state rather than deleting a terminal result.
            gate.pueue("stash", str(task_id), check=False)
        elif name == "Stashed":
            gate.pueue("remove", str(task_id), check=False)
        elif name in ("Running", "Paused"):
            if graceful_until is None:
                gate.pueue("kill", "--signal", "SIGINT", str(task_id), check=False)
                graceful_until = time.monotonic() + 3
            elif time.monotonic() >= graceful_until:
                gate.pueue("kill", str(task_id), check=False)
        else:
            raise GateError(f"Job {task_id} has unexpected state {name}; inspect the queue before recovery")
        time.sleep(0.2)
    raise GateError(f"Cancellation of job {task_id} did not finish; its exclusion lock is retained")


def reject_piped_input():
    """Do not block waiting for input: queued commands are explicitly argv-only."""
    if sys.stdin.isatty():
        return
    descriptor = sys.stdin.fileno()
    ready, _, _ = select.select([descriptor], [], [], 0)
    if ready:
        previous = os.get_blocking(descriptor)
        try:
            os.set_blocking(descriptor, False)
            try:
                data = os.read(descriptor, 1)
            except BlockingIOError:
                data = b""
        finally:
            os.set_blocking(descriptor, previous)
        if data:
            raise GateError("Piped/heredoc input is unsupported; use argv or an explicit input file. Queued commands receive stdin EOF")


def run(gate: Gate, argv: list[str], label: str | None = None) -> int:
    if os.environ.get("AGENT_HEAVY_ACTIVE"):
        raise GateError("Nested agent-heavy would deadlock; wrap the outer command once")
    if not argv:
        raise GateError("Specify a command after --")
    reject_piped_input()
    gate.prepare()
    data = gate.snapshot()
    group = data.get("groups", {}).get("default", {})
    validate_limit(group.get("parallel_tasks"))
    if group.get("status") != "Running":
        raise GateError("Dedicated default queue must be Running; no command was submitted")
    token = uuid.uuid4().hex
    payload = gate.payload_path(token)
    description = make_description(gate, argv, os.getcwd(), label)
    cache_description(gate, token, description)
    from job_profiles import identify_workload
    write_private(gate.state / "workloads" / f"{token}.json", identify_workload(argv, os.getcwd(), dict(os.environ)))
    forwarded = {key: value for key, value in os.environ.items() if not key.startswith(("AGENT_HEAVY_", "PUEUE_"))}
    content = {"argv": argv, "cwd": os.getcwd(), "env": forwarded, "token": token, "label": description["title"], "explicit_label": label is not None}
    write_private(payload, content)
    task_id = None
    try:
        with signal_handlers():
            blocked = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM, signal.SIGHUP})
            try:
                # add persists Queued state; Pueue 4.0.4 enqueue does not save it.
                # The helper waits for the acknowledged identity before execution.
                with private_lock(gate, "scheduling.lock"):
                    managed = resource_scheduler.settings(gate)["mode"] == "adaptive"
                    options = ["--stashed"] if managed else []
                    added = gate.pueue("add", *options, "--escape", "--print-task-id", "--label", description["title"], "--",
                                       "exec", sys.executable, MODULE, "execute", token)
                    if not added.stdout.strip().isdigit():
                        raise GateError("Pueue did not acknowledge a numeric task ID")
                    task_id = int(added.stdout.strip())
                    content.update(task_id=task_id, resource_managed=managed)
                    write_private(payload, content)
                    write_private(gate.receipt_path(task_id), {"task_id": task_id, "token": token})
                print(f"agent-heavy: job {task_id} queued", file=sys.stderr, flush=True)
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, blocked)
            return follow(gate, task_id)
    except Interrupted as error:
        if task_id is not None:
            with signal_handlers_ignored():
                cancel(gate, task_id)
                payload.unlink(missing_ok=True)
        return 128 + error.signum
    except BaseException:
        if task_id is not None:
            with signal_handlers_ignored():
                cancel(gate, task_id)
                payload.unlink(missing_ok=True)
        raise
    finally:
        if task_id is None:
            payload.unlink(missing_ok=True)


@contextlib.contextmanager
def signal_handlers_ignored():
    originals = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    try:
        yield
    finally:
        for sig, handler in originals.items():
            signal.signal(sig, handler)


def execute(gate: Gate, token: str) -> int:
    gate.prepare()
    payload_path = gate.payload_path(token)
    deadline = time.monotonic() + 30
    while True:
        content = read_private(payload_path)
        task_id = content.get("task_id")
        if content.get("token") != token:
            raise GateError("Invalid job identity")
        if task_id is not None:
            if type(task_id) is not int or task_id < 0:
                raise GateError("Invalid job identity")
            receipt = gate.receipt(task_id)
            if receipt == {"task_id": task_id, "token": token}:
                break
            if receipt is not None:
                raise GateError("Job receipt does not match its private payload")
        if time.monotonic() >= deadline:
            payload_path.unlink(missing_ok=True)
            raise GateError("Caller did not publish an acknowledged job identity; command was not started")
        time.sleep(0.05)
    argv, cwd, env = content.get("argv"), content.get("cwd"), content.get("env")
    if not isinstance(argv, list) or not argv or not all(isinstance(arg, str) and "\x00" not in arg for arg in argv):
        raise GateError("Invalid command argv")
    if not isinstance(cwd, str) or not isinstance(env, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in env.items()):
        raise GateError("Invalid job cwd/environment")
    if not (gate.metadata / f"{token}.json").exists():
        cache_description(gate, token, make_description(gate, argv, cwd, payload_label(content)))
    payload_path.unlink()
    pgid = os.getpgrp()
    if pgid <= 1:
        raise GateError("Refusing an unsafe process group")
    identities = process_group_members(pgid)
    initial = [member for member in identities if member["pid"] in (pgid, os.getpid())]
    if not any(member["pid"] == os.getpid() for member in initial):
        raise GateError("Cannot establish the helper process birth identity")
    marker = {"task_id": task_id, "token": token, "pgid": pgid, "pid": os.getpid(),
              "boot_uuid": boot_identity(), "initial_members": initial, "members": initial}
    if content.get("resource_managed"):
        marker["resourceManaged"] = True
    with helper_interrupts() as cancelled:
        while True:
            if cancelled[0]:
                return 130
            if acquire_lease(gate, marker, lease_capacity(gate)):
                break
            time.sleep(0.5)
        if cancelled[0]:
            return 130
        env["AGENT_HEAVY_ACTIVE"] = token
        env.setdefault("GOMAXPROCS", "2")
        env.setdefault("GOMEMLIMIT", "512MiB")
        env.setdefault("GOGC", "50")
        try:
            result = subprocess.run(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL, check=False)
        except FileNotFoundError:
            print("agent-heavy: command executable or working directory does not exist", file=sys.stderr)
            return 127
        except PermissionError:
            print("agent-heavy: command executable or working directory is inaccessible", file=sys.stderr)
            return 126
        # Record surviving same-group children before this known helper exits.
        known = {(member["pid"], member["birth"]): member for member in marker["members"]}
        for member in process_group_members(pgid):
            known[(member["pid"], member["birth"])] = member
        marker["members"] = list(known.values())
        with private_lock(gate), publication_signals():
            write_private(job_marker_path(gate, marker), marker)
        # Only the independent daemon callback may release this lease member.
        return result.returncode if result.returncode >= 0 else 128 - result.returncode


@contextlib.contextmanager
def helper_interrupts():
    cancelled = [False]

    def remember_interrupt(_signum, _frame):
        cancelled[0] = True

    # Caught handlers reset in exec'd targets, which still receive group SIGINT.
    previous = signal.signal(signal.SIGINT, remember_interrupt)
    try:
        yield cancelled
    finally:
        signal.signal(signal.SIGINT, previous)


@contextlib.contextmanager
def publication_signals():
    previous = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM, signal.SIGHUP})
    try:
        yield
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)


def identity_command(argv: list[str]) -> str:
    try:
        completed = subprocess.run(argv, env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL": "C"},
                                   stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=5, check=True)
    except (OSError, subprocess.SubprocessError) as error:
        raise GateError("Cannot establish process identity; shared slot retained") from error
    return completed.stdout


def boot_identity() -> str:
    value = identity_command(["/usr/sbin/sysctl", "-n", "kern.bootsessionuuid"]).strip()
    try:
        return str(uuid.UUID(value))
    except ValueError as error:
        raise GateError("Invalid boot identity; shared slot retained") from error


def process_group_members(pgid: int) -> list[dict]:
    output = identity_command(["/bin/ps", "-axo", "pid=,pgid=,lstart="])
    members = []
    for line in output.splitlines():
        fields = line.split(None, 2)
        if len(fields) != 3 or not fields[0].isdigit() or not fields[1].isdigit():
            raise GateError("Invalid process identity listing; shared slot retained")
        if int(fields[1]) == pgid:
            members.append({"pid": int(fields[0]), "birth": fields[2].strip()})
    return members


def group_exists(pgid: int) -> bool:
    """Whether a group still has potentially running members.

    Darwin's killpg filters zombies and returns EPERM when none are signalable.
    Distinguish that exited-group state from a live inaccessible group without
    ever sending a nonzero signal to establish existence.
    """
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError as error:
        if sys.platform == "darwin":
            initial = process_group_states(pgid)
            if not any(not state.startswith("Z") for _, _, state in initial):
                try:
                    os.killpg(pgid, 0)
                except ProcessLookupError:
                    return False
                except PermissionError:
                    # Empty or changing listings are not proof of a dead group.
                    if initial:
                        if process_group_states(pgid) == initial:
                            return False
                        # Reaping can remove a positively observed zombie
                        # between snapshots. Only kernel absence settles that.
                        try:
                            os.killpg(pgid, 0)
                        except ProcessLookupError:
                            return False
                        except PermissionError:
                            pass
                else:
                    return True
        raise GateError("Cannot verify the owned process group; retaining its lock") from error


def process_group_states(pgid: int) -> list[tuple[int, str, str]]:
    output = identity_command(["/bin/ps", "-axo", "pid=,pgid=,stat=,lstart="])
    observer_seen = False
    members = []
    for line in output.splitlines():
        fields = line.split(None, 3)
        if len(fields) != 4 or not fields[0].isdigit() or not fields[1].isdigit():
            raise GateError("Invalid process state listing; shared slot retained")
        pid, group = int(fields[0]), int(fields[1])
        if pid == os.getpid() and group == os.getpgrp() and not fields[2].startswith("Z"):
            observer_seen = True
        if group == pgid:
            members.append((pid, fields[3].strip(), fields[2]))
    if not observer_seen:
        raise GateError("Incomplete process state listing; shared slot retained")
    return sorted(members)


@contextlib.contextmanager
def private_lock(gate: Gate, name: str = "lease.lock"):
    descriptor = os.open(gate.state / name, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "a") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_nlink != 1:
            raise GateError("Unsafe queue coordination lock")
        deadline = time.monotonic() + 10
        while True:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise GateError("Queue coordination lock did not become available")
                time.sleep(0.05)
        try:
            yield
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


def shared_manifest(gate: Gate) -> dict | None:
    path = gate.slot / "agent-heavy-lease.json"
    try:
        manifest = read_private(path)
    except FileNotFoundError:
        return None
    if set(manifest) != {"version", "lease_id", "uid", "boot_uuid"} or manifest.get("version") != 1 \
            or manifest.get("uid") != os.getuid() or not isinstance(manifest.get("lease_id"), str) \
            or not TOKEN.fullmatch(manifest["lease_id"]) or not isinstance(manifest.get("boot_uuid"), str):
        raise GateError("Invalid shared lease ownership; retaining the validation slot")
    for directory in (gate.slot, gate.slot / "agent-heavy-jobs"):
        info = directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise GateError("Unsafe shared lease directory; retaining the validation slot")
    if {entry.name for entry in gate.slot.iterdir()} != {"owner", "agent-heavy-lease.json", "agent-heavy-jobs"}:
        raise GateError("Unexpected shared lease metadata; retaining the validation slot")
    for path in (gate.slot / "agent-heavy-jobs").iterdir():
        member = read_private(path)
        if type(member.get("task_id")) is not int or not isinstance(member.get("token"), str) \
                or not TOKEN.fullmatch(member["token"]) or path.name != f"{member['task_id']}-{member['token']}.json" \
                or member.get("lease_id") != manifest["lease_id"] or member.get("boot_uuid") != manifest["boot_uuid"]:
            raise GateError("Invalid shared lease member; retaining the validation slot")
    return manifest


def job_marker_path(gate: Gate, marker: dict) -> Path:
    if "lease_id" in marker:
        return gate.slot / "agent-heavy-jobs" / f"{marker['task_id']}-{marker['token']}.json"
    return gate.slot / "agent-heavy.json"


def lease_capacity(gate: Gate) -> int:
    snapshot = gate.snapshot()
    limit = snapshot.get("groups", {}).get("default", {}).get("parallel_tasks")
    validate_limit(limit)
    admitted = sum(task_status(task)[0] in ("Running", "Paused")
                   for task in snapshot["tasks"].values() if task.get("group", "default") == "default")
    return max(limit, admitted)


def acquire_lease(gate: Gate, marker: dict, capacity: int = 1) -> bool:
    """Join our shared lease, or wait behind an external/old singleton owner."""
    with private_lock(gate), publication_signals():
        count = 0
        if gate.slot.exists():
            manifest = shared_manifest(gate)
            if manifest is None:
                return False
            count = len(list((gate.slot / "agent-heavy-jobs").iterdir()))
        if not resource_scheduler.permit(gate, marker, count):
            return False
        try:
            gate.slot.mkdir(mode=0o700)
        except FileExistsError:
            manifest = shared_manifest(gate)
            if manifest is None:
                return False
        else:
            manifest = {"version": 1, "lease_id": uuid.uuid4().hex, "uid": os.getuid(), "boot_uuid": marker["boot_uuid"]}
            (gate.slot / "agent-heavy-jobs").mkdir(mode=0o700)
            owner = gate.slot / "owner"
            descriptor = os.open(owner, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(descriptor, "w") as handle:
                handle.write("agent-heavy shared validation lease\n")
            write_private(gate.slot / "agent-heavy-lease.json", manifest)
        if manifest["boot_uuid"] != marker["boot_uuid"]:
            raise GateError("Shared lease belongs to another boot; inspect retained owners before recovery")
        if len(list((gate.slot / "agent-heavy-jobs").iterdir())) >= capacity:
            return False
        marker["lease_id"] = manifest["lease_id"]
        path = job_marker_path(gate, marker)
        if path.exists():
            raise GateError("Job already owns a validation lease member")
        write_private(path, marker)
        write_private(gate.receipt_path(marker["task_id"]), marker)
        return True


def validate_marker(gate: Gate, task_id: int) -> dict | None:
    receipt = gate.receipt(task_id)
    if receipt is None or "pgid" not in receipt:
        return None
    if "lease_id" in receipt:
        manifest = shared_manifest(gate)
        if manifest is None or manifest["lease_id"] != receipt["lease_id"]:
            raise GateError("Shared lease identity changed; refusing cleanup")
    marker_path = job_marker_path(gate, receipt)
    if not marker_path.exists():
        raise GateError("Owned job marker disappeared; refusing cleanup")
    marker = read_private(marker_path)
    pgid = marker.get("pgid")
    immutable_marker = {key: value for key, value in marker.items() if key != "members"}
    immutable_receipt = {key: value for key, value in receipt.items() if key != "members"}
    if immutable_marker != immutable_receipt or marker.get("task_id") != task_id or not isinstance(marker.get("token"), str) or not TOKEN.fullmatch(marker["token"]):
        raise GateError("Shared slot belongs to another owner; refusing cleanup")
    if type(pgid) is not int or pgid <= 1 or pgid == os.getpgrp():
        raise GateError("Unsafe cleanup process group; retaining the shared slot")
    members = marker.get("members")
    if not isinstance(marker.get("boot_uuid"), str) or not isinstance(members, list) or not members:
        raise GateError("Missing process birth identities; retaining the shared slot")
    if not all(isinstance(member, dict) and type(member.get("pid")) is int and member["pid"] > 1
               and isinstance(member.get("birth"), str) and member["birth"] for member in members):
        raise GateError("Invalid process birth identities; retaining the shared slot")
    return marker


def verify_group_identity(marker: dict) -> bool:
    """Return False for a departed group; refuse to signal a reused group ID."""
    pgid = marker["pgid"]
    if not group_exists(pgid):
        return False
    if boot_identity() != marker["boot_uuid"]:
        raise GateError("Process group boot identity changed; shared slot retained without signalling")
    current = process_group_members(pgid)
    known = {(member["pid"], member["birth"]) for member in marker["members"]}
    if not any((member["pid"], member["birth"]) in known for member in current):
        if not group_exists(pgid):
            return False
        raise GateError("Process group ownership cannot be verified; shared slot retained without signalling")
    return True


def cleanup(gate: Gate, task_id: int):
    gate.prepare()
    with private_lock(gate):
        marker = validate_marker(gate, task_id)
    if marker is None:
        receipt = gate.receipt(task_id)
        if receipt is not None:
            gate.payload_path(receipt["token"]).unlink(missing_ok=True)
            gate.receipt_path(task_id).unlink(missing_ok=True)
        return
    pgid = marker["pgid"]
    for sig, timeout in ((signal.SIGTERM, 2.0), (signal.SIGKILL, 4.0)):
        if not verify_group_identity(marker):
            break
        try:
            os.killpg(pgid, sig)
        except ProcessLookupError:
            break
        except PermissionError as error:
            # The verified group can exit between identity check and signal.
            # Do not retry a signal when it still exists or cannot be verified.
            if group_exists(pgid):
                raise GateError("Cannot signal the owned process group; retaining its lock") from error
            break
        deadline = time.monotonic() + timeout
        while group_exists(pgid) and time.monotonic() < deadline:
            time.sleep(0.1)
    if group_exists(pgid):
        raise GateError(f"Job {task_id} process group {pgid} remains (possibly zombie); shared slot retained")
    with private_lock(gate):
        if validate_marker(gate, task_id) != marker:
            raise GateError("Slot ownership changed during cleanup; shared slot retained")
        if "lease_id" in marker:
            job_marker_path(gate, marker).unlink()
            members = gate.slot / "agent-heavy-jobs"
            if not any(members.iterdir()):
                members.rmdir()
                (gate.slot / "owner").unlink()
                (gate.slot / "agent-heavy-lease.json").unlink()
                gate.slot.rmdir()
        else:
            # Existing singleton jobs remain exclusive during the migration.
            if {entry.name for entry in gate.slot.iterdir()} != {"owner", "agent-heavy.json"}:
                raise GateError("Unexpected shared slot metadata; shared slot retained")
            (gate.slot / "owner").unlink()
            (gate.slot / "agent-heavy.json").unlink()
            gate.slot.rmdir()
        gate.payload_path(marker["token"]).unlink(missing_ok=True)
        gate.receipt_path(task_id).unlink(missing_ok=True)


def display_text(value: str, limit: int = 120) -> str:
    return " ".join("".join(character if character.isprintable() else " " for character in value).split())[:limit]


def safe_summary(argv: list[str]) -> tuple[str, str]:
    """Only executable names and known verbs are displayable, never script text."""
    executable = display_text(Path(argv[0]).name, 80) if argv else ""
    base = executable.lower()
    if len(argv) > 1 and not argv[1].startswith("-") and Path(argv[1]).suffix.lower() in (".sh", ".py", ".js", ".mjs", ".cjs"):
        return display_text(Path(argv[1]).name), executable
    if base in ("sh", "bash", "zsh", "fish", "dash"):
        return "Shell batch", executable
    if base.startswith("python"):
        return "Python batch", executable
    verbs = {"typecheck": "Typecheck", "type-check": "Typecheck", "tsc": "Typecheck", "tsgo": "Typecheck",
             "test": "Tests", "tests": "Tests", "vitest": "Tests", "pytest": "Tests", "playwright": "Browser tests",
             "build": "Build", "lint": "Lint", "check": "Checks", "vet": "Go vet"}
    if base == "go" and len(argv) > 1:
        return {"test": "Go tests", "build": "Go build", "vet": "Go vet"}.get(argv[1], "Go batch"), executable
    if base in ("npm", "pnpm", "yarn", "bun", "node", "npx", "uv", "cargo", "make"):
        for argument in argv[1:4]:
            if argument in verbs:
                return verbs[argument], executable
        if len(argv) > 1 and not argv[1].startswith("-") and Path(argv[1]).suffix in (".js", ".mjs", ".cjs", ".py", ".sh"):
            return display_text(Path(argv[1]).name), executable
        return f"{executable} batch", executable
    return verbs.get(base, executable or "Validation batch"), executable


def project_description(gate: Gate, directory: str) -> dict:
    if directory in gate.project_cache:
        return gate.project_cache[directory]
    result = {"project": Path(directory).name or "Unknown project", "branch": "", "directory": directory}
    try:
        info = subprocess.run(["/usr/bin/git", "-C", directory, "rev-parse", "--path-format=absolute",
                               "--git-common-dir", "--show-toplevel", "--abbrev-ref", "HEAD"],
                              env=gate.control_env, stdin=subprocess.DEVNULL, capture_output=True,
                              text=True, timeout=2, check=False)
        lines = info.stdout.splitlines()
        if info.returncode == 0 and len(lines) == 3:
            common, root, branch = lines
            result["project"] = Path(common).parent.name if Path(common).name == ".git" else Path(root).name
            result["branch"] = display_text(branch)
    except (OSError, subprocess.TimeoutExpired):
        pass  # Non-Git directories still have useful directory-based metadata.
    result["project"] = display_text(result["project"])
    gate.project_cache[directory] = result
    return result


def make_description(gate: Gate, argv: list[str], directory: str, label: str | None = None) -> dict:
    title, executable = safe_summary(argv)
    if label is not None:
        if not isinstance(label, str) or not label.strip() or len(label) > 120 or any(not character.isprintable() for character in label):
            raise GateError("Label must be 1 to 120 printable characters")
        title = label.strip()
    return {**project_description(gate, directory), "title": title, "executable": executable}


def payload_label(payload: dict) -> str | None:
    label = payload.get("label")
    return label if payload.get("explicit_label") or label not in GENERIC_TITLES else None


def cache_description(gate: Gate, token: str, description: dict):
    gate.payload_path(token)  # Validate the same immutable token used by payloads.
    gate.description_cache[token] = description
    write_private(gate.metadata / f"{token}.json", description)


def command_token(task: dict) -> str | None:
    command = task.get("command", "")
    if not isinstance(command, str):
        return None
    try:
        fields = shlex.split(command)
    except ValueError:
        return None
    if len(fields) >= 2 and fields[-2] == "execute" and TOKEN.fullmatch(fields[-1]):
        return fields[-1]
    return None


def describe_task(gate: Gate, task: dict, state_name: str) -> dict:
    token = command_token(task)
    description = gate.description_cache.get(token)
    if token and description is None:
        try:
            description = read_private(gate.metadata / f"{token}.json")
            gate.description_cache[token] = description
        except FileNotFoundError:
            try:
                payload = read_private(gate.payload_path(token))
            except FileNotFoundError:
                payload = None
            if payload is not None:
                description = make_description(gate, payload.get("argv", []), payload.get("cwd", ""), payload_label(payload))
    if description is None:
        directory = task.get("path", "")
        directory = directory if isinstance(directory, str) else ""
        description = {**project_description(gate, directory), "title": "Active validation batch" if state_name == "running" else "Validation batch", "executable": ""}
    required = ("project", "branch", "directory", "title", "executable")
    if not all(isinstance(description.get(key), str) for key in required):
        raise GateError("Invalid cached job description")
    description = {key: description[key] for key in required}
    explicit_generic = False
    if token and description["title"] in GENERIC_TITLES:
        try:
            payload = read_private(gate.payload_path(token))
        except FileNotFoundError:
            payload = None
        if payload is not None:
            explicit_generic = bool(payload.get("explicit_label"))
            title, executable = safe_summary(payload.get("argv", []))
            if not explicit_generic and title not in GENERIC_TITLES:
                description.update(title=title, executable=executable)
    label = task.get("label")
    if isinstance(label, str) and label.strip() and (label not in GENERIC_TITLES or explicit_generic or description["title"] in GENERIC_TITLES):
        description["title"] = display_text(label)
    if token:
        if gate.description_cache.get(token) != description:
            cache_description(gate, token, description)
        gate.description_cache[token] = description
    return description


def status_data(gate: Gate) -> dict:
    gate.prepare()
    snapshot = gate.snapshot()
    scheduling = resource_scheduler.view(gate)
    jobs = []
    entries = list(snapshot["tasks"].items())
    recent_done = sorted((entry for entry in entries if task_status(entry[1])[0] == "Done"),
                         key=lambda entry: (task_status(entry[1])[1].get("end", ""), int(entry[0])), reverse=True)[:30]
    selected = [entry for entry in entries if task_status(entry[1])[0] != "Done"] + recent_done
    for key, task in selected:
        task_id = int(key)
        name, details = task_status(task)
        state_name = name.lower()
        receipt = gate.receipt(task_id)
        token = command_token(task)
        resources = scheduling["jobs"].get(token)
        owned_waiting = name == "Stashed" and token is not None
        if owned_waiting:
            state_name = "queued"
        phase = "Waiting for shared validation slot" if name == "Running" and receipt is not None and "pgid" not in receipt else name
        if (owned_waiting or name == "Queued") and scheduling["mode"] == "adaptive":
            phase = ((resources or {}).get("decision") or {}).get("reason", "Waiting for resource admission")
        if name == "Done" and receipt is not None:
            phase = "Finishing cleanup"
        priority = task.get("priority", 0)
        if type(priority) is not int:
            raise GateError("Pueue returned an invalid priority")
        jobs.append({"id": task_id, "state": state_name, **describe_task(gate, task, state_name),
                     "priority": priority, "queuePosition": None, "submittedAt": task.get("created_at"),
                     "startedAt": details.get("start"), "finishedAt": details.get("end"),
                     "exitCode": result_code(details.get("result")) if name == "Done" else None,
                     "logPath": str(gate.state / "pueue/task_logs" / f"{task_id}.log"),
                     "canReorder": name == "Queued" or owned_waiting, "canStart": name == "Queued" or owned_waiting,
                     "canCancel": name in ("Queued", "Stashed", "Running", "Paused"), "phase": phase,
                     **({"resources": resources} if resources is not None else {})})
    active = sorted((job for job in jobs if job["state"] in ("running", "paused")), key=lambda job: job["id"])
    waiting = sorted((job for job in jobs if job["state"] not in ("running", "paused", "done")), key=lambda job: (-job["priority"], job["id"]))
    queued = [job for job in waiting if job["state"] == "queued"]
    for position, job in enumerate(queued, 1):
        job["queuePosition"] = position
    done = sorted((job for job in jobs if job["state"] == "done"), key=lambda job: (job["finishedAt"] or "", job["id"]), reverse=True)[:30]
    group = snapshot.get("groups", {}).get("default", {})
    return {"updatedAt": datetime.now(UTC).isoformat(),
            "queue": {"status": str(group.get("status", "unknown")).lower(), "limit": group.get("parallel_tasks", 1),
                      "running": len(active), "waiting": len(waiting), "legacyOccupied": legacy_occupied(gate)},
            "jobs": [*active, *waiting, *done],
            "scheduler": {key: scheduling[key] for key in ("mode", "available", "reason", "updatedAt", "host")}}


def status(gate: Gate, as_json: bool = False):
    data = status_data(gate)
    if as_json:
        print(json.dumps(data))
        return
    queue = data["queue"]
    print(f"Queue {queue['status']}: {queue['running']} running, {queue['waiting']} waiting; limit {queue['limit']}")
    print(f"Scheduling {data['scheduler']['mode']}: {data['scheduler']['reason']}")
    for job in data["jobs"]:
        context = f"{job['project']} / {job['branch']}" if job["branch"] else job["project"]
        suffix = f"; exit={job['exitCode']}" if job["exitCode"] is not None else ""
        position = f"; queue #{job['queuePosition']}" if job["queuePosition"] else ""
        print(f"  job {job['id']}: {job['phase']} | {job['title']} | {context}{position}{suffix}")
    print(f"Exclusive legacy owner: {'yes' if queue['legacyOccupied'] else 'no'}")


def validate_priority(priority: int):
    if type(priority) is not int or not -(2**31) <= priority <= 2**31 - 1:
        raise GateError("Priority must be a signed 32-bit integer")


def legacy_occupied(gate: Gate) -> bool:
    if not gate.slot.exists():
        return False
    try:
        with private_lock(gate):
            # Old singleton jobs remain exclusive during migration.
            return gate.slot.exists() and shared_manifest(gate) is None
    except (OSError, GateError, KeyError, ValueError):
        return True


def validate_limit(limit: int):
    if type(limit) is not int or not 1 <= limit <= MAX_PARALLEL:
        raise GateError(f"Parallel limit must be from 1 to {MAX_PARALLEL}; zero means unlimited and is forbidden")


@contextlib.contextmanager
def control_budget(gate: Gate):
    previous = gate.control_deadline
    gate.control_deadline = time.monotonic() + 20
    try:
        yield
    finally:
        gate.control_deadline = previous


def persisted_limit(gate: Gate) -> int:
    directory = gate.state / "pueue"
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise GateError("Cannot verify private persisted queue state")
    path = directory / "state.json"
    if not path.exists():
        path = directory / "state.json.gz"
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
            raise GateError("Cannot verify persisted queue state owner")
        if path.suffix == ".gz":
            with gzip.GzipFile(fileobj=handle) as compressed:
                saved = json.load(compressed)
        else:
            saved = json.load(handle)
    return saved.get("groups", {}).get("default", {}).get("parallel_tasks")


def set_limit(gate: Gate, limit: int):
    validate_limit(limit)
    gate.prepare()
    with control_budget(gate), private_lock(gate, "settings.lock"):
        gate.pueue("parallel", str(limit), "--group", "default")
        save_barrier(gate)
        current = gate.snapshot().get("groups", {}).get("default", {}).get("parallel_tasks")
        if current != limit or persisted_limit(gate) != limit:
            raise GateError(f"Limit {limit} could not be verified in memory and persisted state; inspect queue settings")


def save_barrier(gate: Gate):
    # Pueue 4.0.4 parallel/enqueue changes are not saved immediately. A supported
    # group mutation persists them without editing daemon state on disk.
    barrier = f"agent-heavy-save-{uuid.uuid4().hex}"
    gate.pueue("group", "add", barrier, "--parallel", "1")
    gate.pueue("group", "remove", barrier)


def start_job(gate: Gate, task_id: int):
    if task_id < 0:
        raise GateError("Task ID must be nonnegative")
    with control_budget(gate), private_lock(gate, "scheduling.lock"):
        task = gate.task(task_id)
        token = command_token(task)
        if task_status(task)[0] not in ("Queued", "Stashed") or token is None:
            raise GateError(f"Only a queued agent-heavy job can be started; inspect job {task_id}")
        resource_scheduler.manual_grant(gate, task_id, token)
        gate.pueue("start", str(task_id))
        name, _ = task_status(gate.task(task_id))
        if name not in ("Running", "Done"):
            raise GateError(f"Job {task_id} start could not be verified")


def edit_priority(priority: int, directory: Path):
    """Pueue files-mode editor: touch only the one locked task's priority."""
    validate_priority(priority)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise GateError("Priority editor requires Pueue's private files-mode directory")
    children = list(directory.iterdir())
    if len(children) != 1 or not children[0].name.isdigit():
        raise GateError("Priority editor requires exactly one task")
    child_info = children[0].lstat()
    if not stat.S_ISDIR(child_info.st_mode) or child_info.st_uid != os.getuid():
        raise GateError("Unsafe priority editor task directory")
    descriptor = os.open(children[0] / "priority", os.O_WRONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "w") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
            raise GateError("Unsafe priority editor file")
        os.ftruncate(handle.fileno(), 0)
        handle.write(str(priority))
        handle.flush()
        os.fsync(handle.fileno())


def reorder(gate: Gate, task_id: int, action: str, priority: int | None = None) -> int:
    if task_id < 0:
        raise GateError("Task ID must be nonnegative")
    snapshot = gate.snapshot()
    task = snapshot["tasks"].get(str(task_id))
    name, _ = task_status(task)
    if name not in ("Queued", "Stashed"):
        raise GateError(f"Only queued jobs can be reordered; job {task_id} is {name.lower()}")
    priorities = [candidate.get("priority", 0) for candidate in snapshot["tasks"].values()
                  if task_status(candidate)[0] in ("Queued", "Stashed")]
    if not all(type(value) is int for value in priorities):
        raise GateError("Pueue returned an invalid queue priority")
    if action == "next":
        priority = max(priorities) + 1
    elif action == "last":
        priority = min(priorities) - 1
    validate_priority(priority)
    editor = shlex.join([sys.executable, MODULE, "edit-priority", str(priority)])
    try:
        gate.pueue("edit", str(task_id), env_extra={"EDITOR": editor})
    except GateError as error:
        raise GateError(f"Job {task_id} could not be reordered; it may have started, or Pueue client.edit_mode is not files") from error
    return priority


def main(argv: list[str] | None = None) -> int:
    arguments = sys.argv[1:] if argv is None else argv
    if arguments and arguments[0] == "--":
        arguments = ["run", *arguments[1:]]
    elif arguments and arguments[0] == "--label":
        arguments = ["run", *arguments]
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="action", required=True)
    run_parser = subparsers.add_parser("run", help="queue and wait for one foreground command")
    run_parser.add_argument("--label", help="short readable name for this validation batch")
    run_parser.add_argument("command", nargs=argparse.REMAINDER)
    status_parser = subparsers.add_parser("status", help="show readable jobs without command or environment values")
    status_parser.add_argument("--json", action="store_true", help="print the safe menu-bar inspection API")
    for action in ("cancel", "logs", "cleanup"):
        child = subparsers.add_parser(action)
        child.add_argument("task_id", type=int)
    execute_parser = subparsers.add_parser("execute")
    execute_parser.add_argument("token")
    for action in ("next", "last", "priority"):
        reorder_parser = subparsers.add_parser(action, help="reorder one waiting job without changing its ID")
        reorder_parser.add_argument("task_id", type=int)
        if action == "priority":
            reorder_parser.add_argument("priority", type=int)
    priority_editor = subparsers.add_parser("edit-priority")
    priority_editor.add_argument("priority", type=int)
    priority_editor.add_argument("directory", type=Path)
    limit_parser = subparsers.add_parser("limit", help="set and persist the parallel job limit without stopping active jobs")
    limit_parser.add_argument("count", type=int)
    start_parser = subparsers.add_parser("start", help="start one queued job now, bypassing the parallel limit")
    start_parser.add_argument("task_id", type=int)
    scheduling_parser = subparsers.add_parser("scheduling", help="choose fixed, shadow, or adaptive resource admission")
    scheduling_parser.add_argument("mode", choices=resource_scheduler.MODES)
    controller_parser = subparsers.add_parser("scheduler", help="run the independent resource controller")
    controller_parser.add_argument("--once", action="store_true", help="sample and reconcile once")
    agents_parser = subparsers.add_parser("agents", help="manage optional coding-agent queue instructions")
    agents_parser.add_argument("operation", choices=("status", "install", "remove"))
    agents_parser.add_argument("--runner", choices=("codex", "claude"))
    agents_parser.add_argument("--json", action="store_true")
    args = parser.parse_args(arguments)
    if args.action == "agents":
        import agent_integration
        options = [args.operation]
        if args.runner:
            options.extend(["--runner", args.runner])
        if args.json:
            options.append("--json")
        return agent_integration.main(options)
    try:
        gate = Gate()
        if args.action == "run":
            command = args.command[1:] if args.command[:1] == ["--"] else args.command
            return run(gate, command, args.label)
        if args.action == "status":
            status(gate, args.json)
        elif args.action == "cancel":
            if args.task_id < 0:
                raise GateError("Task ID must be nonnegative")
            cancel(gate, args.task_id)
            print(f"agent-heavy: job {args.task_id} cancelled", file=sys.stderr)
        elif args.action == "logs":
            drain(gate.state / "pueue/task_logs" / f"{args.task_id}.log", 0)
        elif args.action == "execute":
            return execute(gate, args.token)
        elif args.action == "cleanup":
            cleanup(gate, args.task_id)
        elif args.action in ("next", "last", "priority"):
            priority = reorder(gate, args.task_id, args.action, getattr(args, "priority", None))
            print(f"agent-heavy: job {args.task_id} priority {priority}")
        elif args.action == "edit-priority":
            edit_priority(args.priority, args.directory)
        elif args.action == "limit":
            set_limit(gate, args.count)
            print(f"agent-heavy: parallel limit {args.count} saved")
        elif args.action == "start":
            start_job(gate, args.task_id)
            print(f"agent-heavy: job {args.task_id} started now")
        elif args.action == "scheduling":
            resource_scheduler.set_mode(gate, args.mode)
            print(f"agent-heavy: scheduling mode {args.mode} saved")
        elif args.action == "scheduler":
            resource_scheduler.serve(gate, once=args.once)
        return 0
    except (GateError, ProfileError, resource_scheduler.SchedulingError, OSError, ValueError, KeyError) as error:
        print(f"agent-heavy: {error}", file=sys.stderr)
        return 125


if __name__ == "__main__":
    sys.modules["agent_heavy"] = sys.modules[__name__]
    sys.exit(main())
