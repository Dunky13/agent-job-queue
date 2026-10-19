"""Private, worktree-aware resource estimates. These hashes are not build cache keys.

Only bounded command provenance is inspected. Source revisions, definitions, tool
selection and meaningful arguments separate variants; related variants borrow a
conservative family estimate until their own measurements become trustworthy.
"""
from __future__ import annotations

import contextlib
import errno
import hashlib
import json
import math
import os
import re
import shutil
import sqlite3
import stat
import subprocess
import time
from pathlib import Path

GIB = 1024 ** 3
DAY = 86400
MAX_RUNS = 4096
MAX_ENVELOPES = 2048
MAX_MACHINES = 64
MAX_DECISIONS = 4096
HASH = re.compile(r"[0-9a-f]{64}\Z")
DEFINITIONS = (
    "package.json", "package-lock.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "bun.lock",
    "pyproject.toml", "uv.lock", "requirements.txt", "requirements.lock", "go.mod", "go.sum", "go.work",
    "Cargo.toml", "Cargo.lock", "Makefile", "justfile", "tox.ini", "pytest.ini", "setup.cfg", "setup.py",
    "tsconfig.json", "vite.config.ts", "vitest.config.ts", ".nvmrc", ".python-version", "rust-toolchain.toml",
)
TRANSIENT_ENV = {"PWD", "OLDPWD", "SHLVL", "_", "TMPDIR", "TERM", "TERM_PROGRAM", "TERM_SESSION_ID"}
WORKER_FLAGS = {"--max-workers", "--maxWorkers", "--workers", "--jobs", "--threads", "--concurrency",
                "--test-concurrency"}


class ProfileError(Exception):
    """Unsafe or unavailable history must not silently become a cheap estimate."""


def _digest(value) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _git(cwd: Path, *args: str) -> str | None:
    try:
        result = subprocess.run(["git", "-C", str(cwd), *args], capture_output=True, timeout=2,
                                env={"PATH": os.environ.get("PATH", os.defpath), "LC_ALL": "C",
                                     "GIT_TERMINAL_PROMPT": "0"}, check=False)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout.decode(errors="replace").strip() if result.returncode == 0 else None


def _relative(value: str, root: Path, alias: Path) -> str:
    """Preserve unknown semantic arguments while relocating actual checkout paths."""
    for prefix in sorted({str(root), str(alias)}, key=len, reverse=True):
        if value == prefix:
            return "<repo>"
        value = value.replace(prefix + os.sep, "<repo>/")
        if value.endswith("=" + prefix):
            value = value[:-len(prefix)] + "<repo>"
    return value


def _fingerprint(path: Path):
    try:
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            return ["link", _digest(os.readlink(path))]
        if not stat.S_ISREG(info.st_mode):
            return ["other"]
        # Never load a dependency tree, huge fixture, or unbounded generated file.
        if info.st_size > 1024 * 1024:
            return ["large", info.st_size, info.st_mtime_ns]
        with path.open("rb") as handle:
            return ["file", hashlib.sha256(handle.read(1024 * 1024 + 1)).hexdigest()]
    except FileNotFoundError:
        return ["missing"]
    except OSError as error:
        raise ProfileError("Cannot read workload provenance") from error


def _toolchain(argv: list[str], directory: Path, env: dict[str, str]):
    selected = argv[0]
    if os.sep in selected:
        executable = directory / selected
    else:
        found = shutil.which(selected, path=env.get("PATH", os.environ.get("PATH", os.defpath)))
        if found is None:
            return ["unresolved", selected]
        executable = Path(found)
    try:
        resolved = executable.resolve()
        info = resolved.stat()
        # Metadata detects local toolchain replacement without running tools or
        # reading potentially enormous compiler/interpreter binaries.
        return [str(resolved), info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns]
    except FileNotFoundError:
        return ["missing", selected]
    except OSError as error:
        raise ProfileError("Cannot inspect the workload toolchain") from error


def _provenance_arguments(argv: list[str]):
    """Yield possible file arguments, never inline programs or their input data.

    Interpreter tokens are also recognized inside launchers such as ``uv run``
    and ``env``. The full original argument vector still participates in both
    hashes; this only controls optional filesystem inspection.
    """
    interpreter = None
    for index, arg in enumerate(argv):
        name = Path(arg).name
        if re.fullmatch(r"(?:python|pypy)(?:\d+(?:\.\d+)*)?", name):
            interpreter = "python"
        elif name in {"sh", "bash", "zsh", "dash", "ksh", "fish"}:
            interpreter = "shell"
        elif name in {"node", "nodejs", "bun", "deno"}:
            interpreter = "javascript"
        elif interpreter is not None:
            inline = (interpreter == "python" and (arg == "-c" or arg.startswith("-c")
                                                       or re.fullmatch(r"-[BEIOPqsuUvWx]+c", arg)))
            inline = inline or (interpreter == "shell" and (arg == "--command"
                               or arg.startswith("--command=") or re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", arg)))
            inline = inline or (interpreter == "javascript" and (arg in {"-e", "--eval", "-p", "--print", "eval"}
                               or arg.startswith(("--eval=", "--print=", "-e"))))
            if inline:
                return
        if index and (not arg.startswith("-") or "=" in arg):
            yield arg.split("=", 1)[-1]


def _argument_provenance(argv: list[str], directory: Path, root: Path) -> list:
    provenance = []
    for value in _provenance_arguments(argv[:33]):
        path = directory / value
        try:
            relative = path.resolve().relative_to(root)
            info = path.lstat()
        except ValueError:
            continue
        except OSError as error:
            if error.errno in (errno.ENOENT, errno.ENOTDIR, errno.ENAMETOOLONG):
                # Optional opaque data is often not a valid filesystem path.
                # Permission and I/O errors for real provenance remain errors.
                continue
            raise ProfileError("Cannot inspect workload argument provenance") from error
        if stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
            provenance.append([str(relative), _fingerprint(path)])
    return provenance


def identify_workload(argv: list[str], cwd: str, env: dict[str, str]) -> dict:
    """Return opaque family, variant and context identifiers, without retaining inputs.

    The common Git directory identifies linked worktrees without using their
    checkout names. Unknown arguments remain significant. Explicit profile names
    permit an operator to relate opaque shell or non-Git commands intentionally.
    """
    if not argv or not all(isinstance(arg, str) and "\0" not in arg for arg in argv):
        raise ProfileError("A workload needs a valid argument vector")
    directory = Path(cwd).resolve()
    if not directory.is_dir():
        raise ProfileError("Workload directory does not exist")
    location = _git(directory, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
    root = directory
    revision = None
    repository = None
    if location and len(location.splitlines()) == 2:
        root = Path(location.splitlines()[0]).resolve()
        common = Path(location.splitlines()[1]).resolve()
        try:
            info = common.stat()
            repository = ["git", info.st_dev, info.st_ino]
        except OSError as error:
            raise ProfileError("Cannot identify the Git repository") from error
        revision = _git(directory, "rev-parse", "HEAD")
    if repository is None:
        repository = ["directory", str(directory)]
    try:
        relative_cwd = str(directory.relative_to(root))
    except ValueError as error:
        raise ProfileError("Workload is outside its Git checkout") from error
    root_alias = Path(os.path.abspath(cwd))
    for _ in directory.relative_to(root).parts:
        root_alias = root_alias.parent
    command = [_relative(arg, root, root_alias) for arg in argv]
    executable = Path(argv[0]).name
    if executable in {"go", "cargo", "make", "gmake", "npm", "pnpm", "yarn", "npx", "node",
                      "python", "python3", "pytest", "uv", "bash", "sh", "zsh"}:
        command[0] = executable
    worker_flags = set(WORKER_FLAGS) if executable in {
        "go", "cargo", "make", "gmake", "npm", "pnpm", "yarn", "npx", "node", "python", "python3", "pytest", "uv"
    } else set()
    if executable in {"go"}:
        worker_flags.update({"-p", "-parallel"})
    if executable in {"make", "gmake", "cargo"}:
        worker_flags.add("-j")
    if executable == "pytest":
        worker_flags.add("-n")
    family_command = []
    skip = False
    opaque = executable in {"bash", "sh", "zsh"} and any("c" in arg[1:] for arg in argv[1:] if arg.startswith("-"))
    for arg in command:
        if skip:
            skip = False
            continue
        flag = arg.split("=", 1)[0]
        if worker_flags and arg in {"--runInBand", "--run-in-band", "--no-file-parallelism"}:
            continue
        attached_worker = any(arg.startswith(short) and arg[len(short):].isdigit()
                              for short in ("-j", "-p", "-n") if short in worker_flags)
        if not opaque and (flag in worker_flags or attached_worker):
            skip = "=" not in arg and not attached_worker
        else:
            family_command.append(arg)
    explicit = env.get("AGENT_HEAVY_PROFILE")
    if explicit is not None and (not explicit.strip() or len(explicit) > 256):
        raise ProfileError("Explicit workload profile must contain 1 to 256 characters")
    family = _digest(["family-v1", repository, relative_cwd, ["explicit", explicit] if explicit else family_command])
    provenance = []
    if repository[0] == "git":
        parents = [directory, *directory.parents]
        for parent in parents:
            if parent != root and root not in parent.parents:
                break
            for name in DEFINITIONS:
                path = parent / name
                if path.exists() or path.is_symlink():
                    provenance.append([str(path.relative_to(root)), _fingerprint(path)])
            if parent == root:
                break
    provenance.extend(_argument_provenance(argv, directory, root))
    environment = {key: _relative(value, root, root_alias) for key, value in env.items()
                   if key not in TRANSIENT_ENV and not key.startswith(("AGENT_HEAVY_", "PUEUE_"))}
    toolchain = _toolchain(argv, directory, env)
    if isinstance(toolchain[0], str):
        toolchain[0] = _relative(toolchain[0], root, root_alias)
    variant = _digest(["variant-v1", family, command, revision, provenance, environment, toolchain])
    caches = []
    for name in ("node_modules", ".venv", "target", "build", ".pytest_cache", ".next", ".turbo"):
        try:
            info = (directory / name).stat()
            caches.append([name, info.st_mtime_ns, info.st_size])
        except FileNotFoundError:
            caches.append([name, None])
        except OSError as error:
            raise ProfileError("Cannot inspect workload cache context") from error
    return {"family": family, "variant": variant, "context": _digest(["context-v1", caches])}


def _identity(identity: dict) -> tuple[str, str, str]:
    if not isinstance(identity, dict) or any(not isinstance(identity.get(key), str) or not HASH.fullmatch(identity[key])
                                             for key in ("family", "variant", "context")):
        raise ProfileError("Invalid workload identity")
    return identity["family"], identity["variant"], identity["context"]


def _number(value, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
        raise ProfileError(f"Invalid resource metric: {name}")
    return float(value)


class ProfileStore:
    """Single-writer SQLite history with a separate, genuinely read-only mode."""

    def __init__(self, state: Path, *, readonly: bool = False):
        self.state = Path(state)
        self.path = self.state / "resource-history.sqlite3"
        self.readonly = readonly
        try:
            if not readonly:
                self.state.mkdir(parents=True, mode=0o700, exist_ok=True)
            info = self.state.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise ProfileError("Unsafe resource history directory")
            if not readonly:
                try:
                    fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
                except FileExistsError:
                    pass
                else:
                    os.close(fd)
            with self._connection(write=not readonly) as connection:
                version = connection.execute("PRAGMA user_version").fetchone()[0]
                if version == 0 and not readonly:
                    self._schema(connection)
                elif version == 1 and not readonly:
                    self._decision_schema(connection)
                elif version != 1:
                    raise ProfileError("Unsupported resource history schema")
                if connection.execute("PRAGMA quick_check").fetchone()[0] != "ok":
                    raise ProfileError("Resource history integrity check failed")
        except (OSError, sqlite3.Error) as error:
            raise ProfileError("Cannot safely open resource history") from error

    def _check_files(self):
        for suffix in ("", "-journal", "-wal", "-shm"):
            path = Path(str(self.path) + suffix)
            try:
                info = path.lstat()
            except FileNotFoundError:
                if suffix:
                    continue
                raise ProfileError("Resource history is missing")
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_mode & 0o077 or info.st_nlink != 1):
                raise ProfileError("Unsafe resource history file")

    @contextlib.contextmanager
    def _connection(self, *, write=False):
        if write and self.readonly:
            raise ProfileError("Resource history was opened read-only")
        connection = None
        try:
            self._check_files()
            mode = "rw" if write else "ro"
            connection = sqlite3.connect(self.path.as_uri() + "?mode=" + mode, uri=True, timeout=0.25)
            connection.row_factory = sqlite3.Row
            if write:
                connection.execute("PRAGMA journal_mode=DELETE")
                connection.execute("PRAGMA synchronous=FULL")
            else:
                connection.execute("PRAGMA query_only=ON")
            yield connection
            if write:
                connection.commit()
        except (sqlite3.Error, OSError) as error:
            if connection is not None:
                connection.rollback()
            raise ProfileError("Cannot safely access resource history") from error
        finally:
            if connection is not None:
                connection.close()

    @staticmethod
    def _schema(connection):
        connection.executescript("""
            CREATE TABLE runs (
                token TEXT PRIMARY KEY, family TEXT NOT NULL, variant TEXT NOT NULL, context TEXT NOT NULL,
                machine TEXT NOT NULL, memory INTEGER NOT NULL, cpu REAL NOT NULL, cpu_seconds REAL NOT NULL,
                duration REAL NOT NULL, sampled REAL NOT NULL, success INTEGER NOT NULL
            );
            CREATE INDEX runs_profile ON runs(machine, family, variant, sampled);
            CREATE TABLE envelopes (
                machine TEXT NOT NULL, family TEXT NOT NULL, variant TEXT NOT NULL,
                memory INTEGER NOT NULL, cpu REAL NOT NULL, duration REAL NOT NULL, sampled REAL NOT NULL,
                PRIMARY KEY(machine, family, variant)
            );
            CREATE TABLE floors (
                machine TEXT PRIMARY KEY, memory INTEGER NOT NULL, cpu REAL NOT NULL, sampled REAL NOT NULL
            );
            PRAGMA user_version=1;
        """)
        ProfileStore._decision_schema(connection)

    @staticmethod
    def _decision_schema(connection):
        # An additive v1 migration leaves existing measurement tables intact.
        # Read-only consumers do not need this optional audit table.
        connection.executescript("""
            CREATE TABLE IF NOT EXISTS decisions (
                id INTEGER PRIMARY KEY, token TEXT NOT NULL, mode TEXT NOT NULL,
                memory INTEGER NOT NULL, cpu REAL NOT NULL, duration REAL,
                samples INTEGER NOT NULL, confidence TEXT NOT NULL, source TEXT NOT NULL,
                allowed INTEGER NOT NULL, reason TEXT NOT NULL, sampled REAL NOT NULL
            );
            CREATE INDEX IF NOT EXISTS decisions_token ON decisions(token, mode, id DESC);
        """)

    def record_decision(self, token: str, mode: str, prediction: dict, allowed: bool, reason: str, *,
                        sampled_at: float | None = None) -> None:
        """Retain changed shadow/adaptive decisions without private command data.

        Reasons must be controlled human-readable text supplied by the caller,
        never command output. Repeated unchanged observations retain the original
        event timestamp and do not issue a database mutation.
        """
        if (not isinstance(token, str) or not token or not isinstance(mode, str)
                or mode not in {"shadow", "adaptive"}):
            raise ProfileError("Invalid resource decision identity or mode")
        if not isinstance(allowed, bool):
            raise ProfileError("Invalid resource admission decision")
        if (not isinstance(reason, str) or not 1 <= len(reason) <= 256
                or not reason.isprintable() or reason.strip() != reason):
            raise ProfileError("Invalid resource decision reason")
        if not isinstance(prediction, dict):
            raise ProfileError("Invalid resource decision prediction")
        memory = prediction.get("memoryBytes")
        samples = prediction.get("samples", 0)
        if (type(memory) is not int or not 0 <= memory <= 2 ** 63 - 1
                or type(samples) is not int or not 0 <= samples <= 2 ** 63 - 1):
            raise ProfileError("Invalid resource decision memory or sample count")
        cpu = _number(prediction.get("cpuCores"), "cpuCores")
        duration = prediction.get("durationSeconds")
        if duration is not None:
            duration = _number(duration, "durationSeconds")
        confidence = prediction.get("confidence", "unknown")
        source = prediction.get("source", "prior")
        if (not isinstance(confidence, str) or confidence not in {"unknown", "low", "medium", "high"}
                or not isinstance(source, str) or source not in {"prior", "family", "variant"}):
            raise ProfileError("Invalid resource decision confidence or source")
        sampled = _number(time.time() if sampled_at is None else sampled_at, "sampledAt")
        if sampled > time.time() + 300:
            raise ProfileError("Resource decision timestamp is out of range")
        token = _digest(["run-v1", token])
        values = (memory, cpu, duration, samples, confidence, source, int(allowed), reason)
        with self._connection(write=True) as connection:
            previous = connection.execute("""SELECT memory,cpu,duration,samples,confidence,source,allowed,reason
                FROM decisions WHERE token=? AND mode=? ORDER BY id DESC LIMIT 1""", (token, mode)).fetchone()
            if previous is not None and tuple(previous) == values:
                return
            connection.execute("""INSERT INTO decisions
                (token,mode,memory,cpu,duration,samples,confidence,source,allowed,reason,sampled)
                VALUES (?,?,?,?,?,?,?,?,?,?,?)""", (token, mode, *values, sampled))
            connection.execute("""DELETE FROM decisions WHERE id IN
                (SELECT id FROM decisions ORDER BY sampled DESC,id DESC LIMIT -1 OFFSET ?)""", (MAX_DECISIONS,))

    def prediction(self, identity: dict, machine: str) -> dict:
        family, variant, _ = _identity(identity)
        if not isinstance(machine, str) or not machine:
            raise ProfileError("A prediction needs a machine identity")
        machine = _digest(["machine-v1", machine])
        now = time.time()
        with self._connection() as connection:
            envelopes = connection.execute("SELECT * FROM envelopes WHERE machine=? AND family=?",
                                           (machine, family)).fetchall()
            exact = [row for row in envelopes if row["variant"] == variant]
            runs = connection.execute("SELECT * FROM runs WHERE machine=? AND family=? AND sampled>=?",
                                      (machine, family, now - 90 * DAY)).fetchall()
            if exact:
                runs = [row for row in runs if row["variant"] == variant]
            floors = connection.execute("SELECT memory,cpu FROM floors WHERE machine IN (?, '*')", (machine,)).fetchall()
        successes = [row for row in runs if row["success"]]
        samples = len(successes)
        # A censored/failed observation is only a lower bound. It cannot replace
        # the family prior with its small observed prefix. Require repeat exact
        # successes before dropping sibling upper evidence and uncertainty.
        borrowing = bool(exact) and samples < 5 and any(row["variant"] != variant for row in envelopes)
        use_exact = bool(exact) and not borrowing
        selected = envelopes if borrowing else exact or envelopes
        source = "variant" if use_exact else "family" if envelopes else "prior"
        age = max(0, now - max((row["sampled"] for row in selected), default=now))
        memory_floor = max([GIB, *[row["memory"] for row in floors]])
        cpu_floor = max([2.0, *[row["cpu"] for row in floors]])
        if not selected:
            return {"memoryBytes": memory_floor, "cpuCores": cpu_floor, "durationSeconds": None,
                    "samples": 0, "confidence": "unknown", "source": source}
        margin = 1.25 if use_exact else 1.5
        # Keep upper evidence across retention pruning. Very old evidence eases
        # only to 75 percent, and stale profiles regain the cold-start floor.
        age_factor = max(0.75, 1 - max(0, age - 30 * DAY) / (240 * DAY))
        memory = max(row["memory"] for row in selected) * margin * age_factor
        cpu = max(row["cpu"] for row in selected) * margin * age_factor
        if not use_exact or samples < 5 or age > 30 * DAY:
            memory = max(memory, memory_floor)
            cpu = max(cpu, cpu_floor)
        else:
            memory = max(memory, 64 * 1024 ** 2, *[row["memory"] for row in floors])
            cpu = max(cpu, 0.1, *[row["cpu"] for row in floors])
        durations = [row["duration"] for row in selected if row["duration"] > 0]
        confidence = ("high" if use_exact and samples >= 12 and age <= 30 * DAY
                      else "medium" if use_exact and samples >= 5 else "low")
        if age > 30 * DAY or (runs and not max(runs, key=lambda row: row["sampled"])["success"]):
            confidence = "low"
        return {"memoryBytes": math.ceil(memory), "cpuCores": round(cpu, 3),
                "durationSeconds": max(durations) * margin if durations else None,
                "samples": samples, "confidence": confidence, "source": source}

    def record(self, token: str, identity: dict, machine: str, metrics: dict, *, complete: bool,
               exit_code: int | None) -> None:
        family, variant, context = _identity(identity)
        if not isinstance(token, str) or not token or not isinstance(machine, str) or not machine:
            raise ProfileError("A resource sample needs a token and machine identity")
        if not isinstance(complete, bool) or (exit_code is not None and (isinstance(exit_code, bool)
                                                                       or not isinstance(exit_code, int))):
            raise ProfileError("Invalid resource completion state")
        if not isinstance(metrics, dict):
            raise ProfileError("Invalid resource metrics")
        memory = math.ceil(_number(metrics.get("memoryBytes", 0), "memoryBytes"))
        cpu_seconds = _number(metrics.get("cpuSeconds", 0), "cpuSeconds")
        duration = _number(metrics.get("durationSeconds", 0), "durationSeconds")
        cpu = max(_number(metrics.get("cpuCores", 0), "cpuCores"), cpu_seconds / duration if duration else 0)
        sampled = _number(metrics.get("sampledAt", time.time()), "sampledAt")
        if memory > 2 ** 63 - 1 or sampled > time.time() + 300:
            raise ProfileError("Resource metric is out of range")
        token = _digest(["run-v1", token])
        machine = _digest(["machine-v1", machine])
        success = int(complete and exit_code == 0 and memory > 0 and duration > 0)
        with self._connection(write=True) as connection:
            prior = connection.execute("SELECT family,variant,machine FROM runs WHERE token=?", (token,)).fetchone()
            if prior and (prior["family"], prior["variant"], prior["machine"]) != (family, variant, machine):
                raise ProfileError("A resource token cannot change workload identity")
            connection.execute("""INSERT INTO runs VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(token) DO UPDATE SET
                memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),
                cpu_seconds=MAX(cpu_seconds,excluded.cpu_seconds),duration=MAX(duration,excluded.duration),
                sampled=MAX(sampled,excluded.sampled),success=MAX(success,excluded.success)""",
                               (token, family, variant, context, machine, memory, cpu, cpu_seconds, duration, sampled, success))
            connection.execute("""INSERT INTO envelopes VALUES (?,?,?,?,?,?,?)
                ON CONFLICT(machine,family,variant) DO UPDATE SET memory=MAX(memory,excluded.memory),
                cpu=MAX(cpu,excluded.cpu),duration=MAX(duration,excluded.duration),sampled=MAX(sampled,excluded.sampled)""",
                               (machine, family, variant, memory, cpu, duration, sampled))
            connection.execute("DELETE FROM runs WHERE token IN (SELECT token FROM runs ORDER BY sampled DESC LIMIT -1 OFFSET ?)",
                               (MAX_RUNS,))
            overflow = connection.execute("SELECT * FROM envelopes ORDER BY sampled DESC LIMIT -1 OFFSET ?",
                                          (MAX_ENVELOPES,)).fetchall()
            for row in overflow:
                connection.execute("""INSERT INTO floors VALUES (?,?,?,?) ON CONFLICT(machine) DO UPDATE SET
                    memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),sampled=MAX(sampled,excluded.sampled)""",
                                   (row["machine"], row["memory"], row["cpu"], row["sampled"]))
                connection.execute("DELETE FROM envelopes WHERE machine=? AND family=? AND variant=?",
                                   (row["machine"], row["family"], row["variant"]))
            overflow_machines = connection.execute("SELECT * FROM floors WHERE machine!='*' ORDER BY sampled DESC LIMIT -1 OFFSET ?",
                                                   (MAX_MACHINES,)).fetchall()
            for row in overflow_machines:
                connection.execute("""INSERT INTO floors VALUES ('*',?,?,?) ON CONFLICT(machine) DO UPDATE SET
                    memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),sampled=MAX(sampled,excluded.sampled)""",
                                   (row["memory"], row["cpu"], row["sampled"]))
                connection.execute("DELETE FROM floors WHERE machine=?", (row["machine"],))

    def close(self):
        """Connections are short-lived; this method permits conventional ownership."""

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()
