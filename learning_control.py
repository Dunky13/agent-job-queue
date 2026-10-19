"""Inspect and correct workload learning through one private control boundary."""
from __future__ import annotations

import argparse
import json
import math
import re
import sys
import uuid

from agent_heavy import Gate, GateError, control_budget, private_lock, write_private
from job_profiles import ProfileError, ProfileStore
import resource_scheduler as scheduler

PROFILE_ID = re.compile(r"[0-9a-f]{64}\Z")
MAX_INPUT = 16_384


def profile_id(value):
    if not isinstance(value, str) or not PROFILE_ID.fullmatch(value):
        raise ProfileError("Invalid workload profile identity")
    return value


def request(operation, value):
    """Reject malformed edits before opening a writable database or lock."""
    fields = {"edit": {"revision", "label", "memoryFloorBytes", "cpuFloorCores"},
              "merge": {"revision", "targetId"}, "split": {"revision", "family"},
              "reset": {"revision", "confirm"}}
    if not isinstance(value, dict) or set(value) != fields[operation]:
        raise ProfileError("Invalid workload correction fields")
    if type(value["revision"]) is not int or not 0 <= value["revision"] <= 2 ** 63 - 2:
        raise ProfileError("Invalid expected learning revision")
    if operation == "edit":
        label = value["label"]
        if not isinstance(label, str) or not 1 <= len(label.strip()) <= 120 or not label.strip().isprintable():
            raise ProfileError("A profile name must contain 1 to 120 printable characters")
        value = {**value, "label": label.strip()}
        memory, cpu = value["memoryFloorBytes"], value["cpuFloorCores"]
        if memory is not None and (type(memory) is not int or not 0 < memory <= 2 ** 50):
            raise ProfileError("Minimum memory must be a positive integer no greater than 2^50 bytes")
        if cpu is not None and (type(cpu) not in (int, float) or not 0 < cpu <= 1024 or not math.isfinite(cpu)):
            raise ProfileError("Minimum CPU must be greater than zero and no greater than 1024 cores")
    elif operation == "merge":
        profile_id(value["targetId"])
    elif operation == "split":
        profile_id(value["family"])
    elif value["confirm"] is not True:
        raise ProfileError("Reset requires explicit confirmation")
    return value


def _controller(gate):
    try:
        state = scheduler.view(gate)
    except (GateError, ProfileError, scheduler.SchedulingError, OSError, ValueError):
        return None, None, "Controller status is unavailable; saved learning remains visible"
    applied = state.get("learningRevision")
    applied = applied if type(applied) is int and applied >= 0 else None
    host = state.get("host")
    machine = host["machine"] if isinstance(host, dict) else None
    reason = ("Current controller predictions include these corrections" if state.get("available")
              else state.get("reason", "Waiting for fresh controller predictions"))
    return machine, applied, reason


def _decorate(value, applied, status):
    if applied is None or applied != value["revision"]:
        status = "Saved learning is awaiting fresh controller predictions. " + status
    return {**value, "controllerAppliedRevision": applied, "controllerStatus": status}


def inspect(gate, *, identity=None, offset=0, limit=100, query=""):
    machine, applied, status = _controller(gate)
    store = ProfileStore(gate.state, readonly=True)
    try:
        value = (store.detail(profile_id(identity), machine=machine) if identity is not None
                 else store.catalog(offset=offset, limit=limit, query=query))
        return _decorate(value, applied, status)
    finally:
        store.close()


def correct(gate, operation, identity, value):
    identity = profile_id(identity)
    value = request(operation, value)
    gate.prepare()
    with control_budget(gate), private_lock(gate, "scheduling.lock"), private_lock(gate):
        store = ProfileStore(gate.state)
        try:
            if store.revision() != value["revision"]:
                raise ProfileError("Learning changed since you opened it; refresh before saving")
            # Reject missing profiles and invalid membership before changing
            # admission state. SQLite repeats the CAS inside its transaction.
            detail = store.detail(identity)
            if operation == "merge":
                if identity == value["targetId"]:
                    raise ProfileError("Choose a different profile to share learning with")
                store.detail(value["targetId"])
            if operation == "split" and (detail["profile"]["memberCount"] < 2 or (
                not detail["truncated"]["members"] and not any(
                    member["family"] == value["family"] for member in detail["members"]
                )
            )):
                raise ProfileError("Choose a member family from a shared profile to separate")
            previous = scheduler.settings(gate)
            if previous["mode"] == "adaptive":
                # Waiting helpers may still have pre-upgrade Python code in
                # memory. Only a fresh learning-aware controller can retire
                # those old automatic decisions safely. Its exclusive service
                # lock rules out an old controller publishing in parallel.
                state = scheduler.view(gate)
                if type(state.get("learningRevision")) is not int:
                    raise ProfileError("Wait for the updated resource controller before correcting adaptive learning, or select Fixed scheduling")
            admissions = scheduler.ledger(gate, previous)
            configuration = {"mode": previous["mode"], "generation": uuid.uuid4().hex}
            admissions["generation"] = configuration["generation"]
            admissions["grants"] = {token: grant for token, grant in admissions["grants"].items()
                                    if grant["manual"] or "pgid" in (gate.receipt(grant["task_id"]) or {})}
            # Invalidate first. A failed write or process crash must never leave
            # an old automatic grant usable with changed learning.
            write_private(gate.state / "resource-admissions.json", admissions)
            write_private(gate.state / "scheduling.json", configuration)
            if operation == "edit":
                store.edit(identity, revision=value["revision"], label=value["label"],
                           memory_floor_bytes=value["memoryFloorBytes"], cpu_floor_cores=value["cpuFloorCores"])
            elif operation == "merge":
                store.merge(identity, value["targetId"], revision=value["revision"])
                identity = value["targetId"]
            elif operation == "split":
                identity = store.split(identity, value["family"], revision=value["revision"])
            else:
                store.reset(identity, revision=value["revision"])
            # Capture the committed result before another correction can merge
            # or remove this profile. A successful save must return its own
            # revision rather than a later operator's edits or a false failure.
            result = inspect(gate, identity=identity)
        finally:
            store.close()
    return result


def _input():
    if sys.stdin.isatty():
        raise ProfileError("Supply the correction JSON through private standard input")
    encoded = sys.stdin.buffer.read(MAX_INPUT + 1)
    if len(encoded) > MAX_INPUT:
        raise ProfileError("Workload correction exceeds its size limit")
    try:
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError("duplicate field")
                result[key] = value
            return result
        return json.loads(encoded, object_pairs_hook=unique)
    except (UnicodeError, ValueError, RecursionError) as error:
        raise ProfileError("Workload correction must be a JSON object with unique fields") from error


def main(arguments):
    parser = argparse.ArgumentParser(prog="agent-heavy profiles", description=__doc__)
    commands = parser.add_subparsers(dest="operation", required=True)
    listing = commands.add_parser("list", help="inspect safe learned-workload summaries")
    listing.add_argument("--offset", type=int, default=0)
    listing.add_argument("--limit", type=int, default=100)
    listing.add_argument("--query", default="")
    listing.add_argument("--json", action="store_true")
    for operation in ("show", "edit", "merge", "split", "reset"):
        child = commands.add_parser(operation)
        child.add_argument("identity")
        child.add_argument("--json", action="store_true")
    args = parser.parse_args(arguments)
    try:
        value = _input() if args.operation not in ("list", "show") else None
        gate = Gate()
        if args.operation == "list":
            result = inspect(gate, offset=args.offset, limit=args.limit, query=args.query)
        elif args.operation == "show":
            result = inspect(gate, identity=args.identity)
        else:
            result = correct(gate, args.operation, args.identity, value)
        # JSON is also the readable CLI format, keeping the inspect/edit
        # contract identical across the app, dashboard and terminal.
        print(json.dumps(result, indent=None if args.json else 2, allow_nan=False))
        return 0
    except (GateError, ProfileError, scheduler.SchedulingError, OSError, ValueError) as error:
        message = str(error) if isinstance(error, (GateError, ProfileError, scheduler.SchedulingError)) else "Workload learning could not be safely accessed"
        if args.json:
            print(json.dumps({"error": message}))
        else:
            print(f"agent-heavy: {message}", file=sys.stderr)
        return 125
