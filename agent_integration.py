#!/usr/bin/env python3
"""Opt-in, reversible global instructions for supported coding agents."""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import json
import os
import shlex
import stat
import sys
import uuid
from pathlib import Path

RUNNERS = {"codex": "Codex", "claude": "Claude Code"}
PREFIX = b"<!-- agent-job-queue:"
END = b"<!-- agent-job-queue:end -->\n"
MAX_BYTES = 1024 * 1024
CODEX_MAX_BYTES = 32768
# Rust str::trim uses Unicode White_Space, excluding Python's extra U+001C..001F.
# Match Codex's lossy UTF-8 read and trim when selecting its global override.
RUST_WHITE_SPACE = "\t\n\v\f\r \u0085\u00a0\u1680" + "".join(chr(point) for point in range(0x2000, 0x200B)) + "\u2028\u2029\u202f\u205f\u3000"


class IntegrationError(Exception):
    """Instructions could not be inspected or changed safely."""


def instruction() -> str:
    command = shlex.quote(str(Path.home() / ".local/bin/agent-heavy"))
    return (
        "## Agent Job Queue\n\n"
        "Use the shared queue across repositories and worktrees for heavy builds, broad test suites, "
        "full type checks, browser suites, and expensive generators:\n\n"
        f"```sh\n{command} run --label 'Brief task description' -- <command> [arguments...]\n```\n\n"
        "Run lightweight reads, edits, and small focused checks directly. Limit workers inside heavy commands "
        "conservatively. Keep persistent servers and watch mode outside the queue and budget their memory separately.\n\n"
        "Queue finite, noninteractive commands only; stdin receives EOF. Wrap an outer batch once and do not nest "
        "queue wrappers. Retain the job/session ID and inspect progress with agent-heavy status. Diagnose queue failures "
        "instead of bypassing the queue, clearing locks, or weakening checks. Do not change the operator's parallel "
        "job limit or force-start jobs without explicit permission. Cancel only your own jobs.\n"
    )


def _block(separator: int) -> bytes:
    return f"<!-- agent-job-queue:begin v1 separator={separator} -->\n".encode() + instruction().encode() + END


def _managed(data: bytes) -> tuple[int, int] | None:
    if PREFIX not in data:
        return None
    for separator in (0, 2):
        block = _block(separator)
        if data.count(PREFIX) != 2 or data.count(block) != 1:
            continue
        offset = data.index(block)
        if separator and (offset < separator or data[offset - separator:offset] != b"\n\n"):
            break
        return offset - separator, offset + len(block)
    raise IntegrationError("Managed instructions were edited or have duplicate/unbalanced markers; preserve your changes and resolve the block manually")


def _identity(info: os.stat_result) -> tuple:
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_nlink,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _directory_safe(info: os.stat_result, *, final: bool) -> None:
    if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.getuid()):
        raise IntegrationError("Instructions have a non-owned or non-directory parent")
    writable = info.st_mode & 0o022
    shared_temporary = info.st_uid == 0 and info.st_mode & stat.S_ISVTX
    if writable and not shared_temporary:
        raise IntegrationError("Instructions have a group/world-writable parent")
    if final and (info.st_uid != os.getuid() or writable):
        raise IntegrationError("Instructions directory must be owned by you and not group/world-writable")


@contextlib.contextmanager
def _parent(path: Path, *, create: bool = False):
    if not path.is_absolute() or ".." in path.parts:
        raise IntegrationError("Instruction paths must be absolute and must not contain '..'")
    descriptor = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        _directory_safe(os.fstat(descriptor), final=False)
        parts = path.parent.parts[1:]
        for index, part in enumerate(parts):
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            except FileNotFoundError:
                if not create:
                    raise
                try:
                    os.mkdir(part, 0o700, dir_fd=descriptor)
                except FileExistsError:
                    pass
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            try:
                _directory_safe(os.fstat(child), final=index == len(parts) - 1)
            except BaseException:
                os.close(child)
                raise
            os.close(descriptor)
            descriptor = child
        if not parts:
            _directory_safe(os.fstat(descriptor), final=True)
        yield descriptor
    finally:
        os.close(descriptor)


def _read(descriptor: int, name: str) -> tuple[bytes, os.stat_result | None]:
    try:
        source = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=descriptor)
    except FileNotFoundError:
        return b"", None
    try:
        info = os.fstat(source)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
            raise IntegrationError("Instructions must be a regular, single-link file owned by you")
        if info.st_mode & 0o022:
            raise IntegrationError("Instructions must not be group/world-writable")
        if info.st_size > MAX_BYTES:
            raise IntegrationError("Instructions exceed the 1 MiB safety limit")
        chunks = []
        size = 0
        while True:
            chunk = os.read(source, min(65536, MAX_BYTES + 1 - size))
            if not chunk:
                break
            size += len(chunk)
            if size > MAX_BYTES:
                raise IntegrationError("Instructions exceed the 1 MiB safety limit")
            chunks.append(chunk)
        if _identity(os.fstat(source)) != _identity(info):
            raise IntegrationError("Instructions changed during inspection; retry after the other editor finishes")
        return b"".join(chunks), info
    finally:
        os.close(source)


def _read_path(path: Path) -> tuple[bytes, os.stat_result | None]:
    try:
        with _parent(path) as descriptor:
            return _read(descriptor, path.name)
    except FileNotFoundError:
        return b"", None


def _target(runner: str) -> Path:
    if runner not in RUNNERS:
        raise IntegrationError("Unknown agent runner")
    variable, default, filename = ("CODEX_HOME", ".codex", "AGENTS.md") if runner == "codex" else (
        "CLAUDE_CONFIG_DIR", ".claude", "CLAUDE.md")
    configured = os.environ.get(variable)
    home = Path(configured) if configured is not None else Path.home() / default
    if not home.is_absolute() or ".." in home.parts:
        raise IntegrationError(f"{variable} must be an absolute directory without '..'")
    if runner == "codex":
        override = home / "AGENTS.override.md"
        content, info = _read_path(override)
        if info is not None and content.decode("utf-8", errors="replace").strip(RUST_WHITE_SPACE):
            return override
    return home / filename


def snapshot() -> dict:
    rows = []
    for runner, name in RUNNERS.items():
        path = None
        configured = False
        error = None
        try:
            target = _target(runner)
            path = str(target)
            content, _ = _read_path(target)
            configured = _managed(content) is not None
            if runner == "codex" and configured and len(content) > CODEX_MAX_BYTES:
                configured = False
                raise IntegrationError("Codex instructions exceed its default 32 KiB instruction budget")
        except (IntegrationError, OSError) as exc:
            error = str(exc)
        rows.append({"id": runner, "name": name, "path": path, "configured": configured, "error": error})
    return {"runners": rows, "instruction": instruction()}


def _same_file(descriptor: int, path: Path, data: bytes, info: os.stat_result | None) -> None:
    actual, current = _read(descriptor, path.name)
    same_info = (current is None and info is None) or (
        current is not None and info is not None and _identity(current) == _identity(info))
    if not same_info or actual != data:
        raise IntegrationError("Instructions changed concurrently; no replacement was made")
    # An editor may replace a parent directory while we hold its old descriptor.
    with _parent(path) as reopened:
        if _identity(os.fstat(reopened))[:2] != _identity(os.fstat(descriptor))[:2]:
            raise IntegrationError("Instructions directory changed concurrently; no replacement was made")


def _private_write(descriptor: int, name: str, data: bytes, mode: int = 0o600) -> None:
    output = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode, dir_fd=descriptor)
    try:
        with os.fdopen(output, "wb", closefd=False) as stream:
            stream.write(data)
            stream.flush()
            os.fsync(output)
    finally:
        os.close(output)


def change(runner: str, action: str) -> None:
    if action not in ("install", "remove"):
        raise IntegrationError("Unknown integration action")
    path = _target(runner)
    # Removal of an absent integration is read-only, including absent directories.
    if action == "remove":
        data, _ = _read_path(path)
        if _managed(data) is None:
            return
    with _parent(path, create=action == "install") as descriptor:
        lock_name = ".agent-job-queue.lock"
        lock = os.open(lock_name, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=descriptor)
        try:
            info = os.fstat(lock)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_mode & 0o077:
                raise IntegrationError("Unsafe agent integration lock")
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise IntegrationError("Another agent integration update is running; try again when it finishes") from exc
            data, before = _read(descriptor, path.name)
            managed = _managed(data)
            if action == "install":
                if managed is not None:
                    return
                separator = 2 if data else 0
                replacement = data + b"\n" * separator + _block(separator)
            else:
                if managed is None:
                    return
                replacement = data[:managed[0]] + data[managed[1]:]
            if len(replacement) > MAX_BYTES:
                raise IntegrationError("Resulting instructions exceed the 1 MiB safety limit")
            if action == "install" and runner == "codex" and len(replacement) > CODEX_MAX_BYTES:
                raise IntegrationError("Codex instructions would exceed its default 32 KiB instruction budget")
            token = uuid.uuid4().hex
            temporary = f".agent-job-queue-{token}.tmp"
            try:
                _same_file(descriptor, path, data, before)
                if before is not None:
                    backup = f"{path.name}.agent-job-queue-backup-{token}"
                    _private_write(descriptor, backup, data)
                    os.fsync(descriptor)
                _private_write(descriptor, temporary, replacement)
                if before is not None:
                    os.chmod(temporary, stat.S_IMODE(before.st_mode), dir_fd=descriptor, follow_symlinks=False)
                _same_file(descriptor, path, data, before)
                # Codex may have gained a higher-priority override during this update.
                if _target(runner) != path:
                    raise IntegrationError("Agent instruction precedence changed concurrently; retry")
                os.replace(temporary, path.name, src_dir_fd=descriptor, dst_dir_fd=descriptor)
                os.fsync(descriptor)
            finally:
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(temporary, dir_fd=descriptor)
        finally:
            os.close(lock)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("status", "install", "remove"))
    parser.add_argument("--runner", choices=tuple(RUNNERS))
    parser.add_argument("--json", action="store_true", dest="json_output")
    args = parser.parse_args(argv)
    if args.action != "status" and args.runner is None:
        parser.error("--runner is required for install/remove")
    try:
        if args.action != "status":
            change(args.runner, args.action)
        state = snapshot()
        if args.json_output:
            print(json.dumps(state))
        else:
            for row in state["runners"]:
                status = row["error"] or ("Configured by this app" if row["configured"] else "Not configured by this app")
                print(f"{row['name']}: {status}\n  {row['path'] or 'Invalid instruction directory'}")
        return 0 if args.json_output else int(any(row["error"] for row in state["runners"]))
    except (IntegrationError, OSError) as exc:
        print(f"Agent integration: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
