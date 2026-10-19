#!/usr/bin/env python3
"""Preserve a prior queue lease without executing installed Python source."""
from __future__ import annotations

import ast
import json
import os
import stat
import sys
from pathlib import Path


def read_owned(path: Path, *, private: bool) -> bytes:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as handle:
        info = os.fstat(handle.fileno())
        forbidden = 0o077 if private else 0o022
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & forbidden:
            raise ValueError("Unsafe installed queue file ownership or permissions")
        data = handle.read(4_000_001)
        if len(data) > 4_000_000:
            raise ValueError("Installed queue source exceeds the migration limit")
        return data


def validate_slot(data):
    if not isinstance(data, dict) or set(data) != {"slot"} or not isinstance(data["slot"], str) \
            or "\0" in data["slot"] or not Path(data["slot"]).is_absolute():
        raise ValueError("Invalid persisted queue lease setting")
    return data["slot"]


def is_node(node, source: str) -> bool:
    return ast.dump(node, include_attributes=False) == ast.dump(ast.parse(source, mode="eval").body,
                                                               include_attributes=False)


def migrate(root: Path, state: Path) -> None:
    setting = state / "legacy-slot.json"
    if setting.exists() or setting.is_symlink():
        validate_slot(json.loads(read_owned(setting, private=True)))
        return
    previous = root / "agent_heavy.py"
    if not previous.exists() and not previous.is_symlink():
        return
    tree = ast.parse(read_owned(previous, private=False))
    assignments = [node.value for cls in tree.body if isinstance(cls, ast.ClassDef) and cls.name == "Gate"
                   for method in cls.body if isinstance(method, ast.FunctionDef) and method.name == "__init__"
                   for node in ast.walk(method) if isinstance(node, ast.Assign)
                   and any(isinstance(target, ast.Attribute) and target.attr == "slot"
                           and is_node(target.value, "self") for target in node.targets)]
    if len(assignments) != 1:
        raise ValueError("Cannot safely identify the installed queue lease configuration")
    value = assignments[0]
    if is_node(value, "lease_slot(self.state)"):
        return
    if not isinstance(value, ast.Call) or not is_node(value.func, "Path") or len(value.args) != 1:
        raise ValueError("Cannot safely identify the installed queue lease configuration")
    lookup = value.args[0]
    if not isinstance(lookup, ast.Call) or not is_node(lookup.func, "os.environ.get") or len(lookup.args) != 2 \
            or not (is_node(lookup.args[0], "OVERRIDES[2]")
                    or is_node(lookup.args[0], repr("AGENT_HEAVY_LEGACY_SLOT"))):
        raise ValueError("Cannot safely identify the installed queue lease configuration")
    default = lookup.args[1]
    if is_node(default, 'self.state / "execution.slot"'):
        return
    if not isinstance(default, ast.Constant) or not isinstance(default.value, str):
        raise ValueError("Cannot safely identify the installed queue lease configuration")
    slot = validate_slot({"slot": default.value})
    descriptor = os.open(setting, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as handle:
        json.dump({"slot": slot}, handle)
        handle.flush()
        os.fsync(handle.fileno())
    print("Preserved the existing queue execution lease for active jobs.")


def main():
    try:
        migrate(Path(sys.argv[1]), Path(sys.argv[2]))
    except (OSError, ValueError, SyntaxError, IndexError) as error:
        print(f"Upgrade stopped before replacing installed source: {error}.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
