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
MAX_REGISTERED_FAMILIES = 8192
MAX_RETAINED_ENVELOPES = 8192
MAX_DETAIL_MEMBERS = 256
MAX_DETAIL_VARIANTS = 100
MAX_DETAIL_RUNS = 30
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
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        raise ProfileError(f"Invalid resource metric: {name}")
    try:
        converted = float(value)
    except OverflowError as error:
        raise ProfileError(f"Invalid resource metric: {name}") from error
    if not math.isfinite(converted):
        raise ProfileError(f"Invalid resource metric: {name}")
    return converted


class ProfileStore:
    """Single-writer SQLite history with a separate, genuinely read-only mode."""

    def __init__(self, state: Path, *, readonly: bool = False):
        self.state = Path(state)
        self.path = self.state / "resource-history.sqlite3"
        self.readonly = readonly
        self._empty = False
        try:
            if not readonly:
                self.state.mkdir(parents=True, mode=0o700, exist_ok=True)
            elif not self.state.exists() and not self.state.is_symlink():
                self._empty = True
                return
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
            elif not self.path.exists() and not self.path.is_symlink():
                self._empty = True
                return
            with self._connection(write=not readonly) as connection:
                version = connection.execute("PRAGMA user_version").fetchone()[0]
                if version == 0 and not readonly:
                    self._schema(connection)
                elif version == 1 and not readonly:
                    self._decision_schema(connection)
                    self._learning_schema(connection)
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
                connection.execute("BEGIN")
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
        ProfileStore._learning_schema(connection)

    @staticmethod
    def _learning_schema(connection):
        # Optional additive tables keep v1 numeric observations readable by old
        # clients. Opening a reader never creates these tables or a database.
        connection.executescript("""
            CREATE TABLE IF NOT EXISTS learning_state (
                id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL
            );
            INSERT OR IGNORE INTO learning_state VALUES (1,0);
            CREATE TABLE IF NOT EXISTS learning_profiles (
                id TEXT PRIMARY KEY, label TEXT, memory_floor INTEGER, cpu_floor REAL,
                pinned INTEGER NOT NULL, ignore_legacy INTEGER NOT NULL, updated REAL NOT NULL
            );
            CREATE TABLE IF NOT EXISTS learning_members (
                family TEXT PRIMARY KEY, profile TEXT NOT NULL, title TEXT, project TEXT,
                executable TEXT, updated REAL NOT NULL
            );
            CREATE INDEX IF NOT EXISTS learning_member_profile ON learning_members(profile);
            CREATE TABLE IF NOT EXISTS learning_resets (
                family TEXT PRIMARY KEY, sampled REAL NOT NULL
            );
            CREATE TABLE IF NOT EXISTS learning_retained (
                machine TEXT NOT NULL, family TEXT NOT NULL, variant TEXT NOT NULL,
                memory INTEGER NOT NULL, cpu REAL NOT NULL, duration REAL NOT NULL, sampled REAL NOT NULL,
                PRIMARY KEY(machine, family, variant)
            );
        """)

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

    @staticmethod
    def _has_learning(connection) -> bool:
        return connection.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='learning_state'").fetchone() is not None

    @staticmethod
    def _revision(connection) -> int:
        if not ProfileStore._has_learning(connection):
            return 0
        row = connection.execute("SELECT revision FROM learning_state WHERE id=1").fetchone()
        if row is None or type(row[0]) is not int or row[0] < 0:
            raise ProfileError("Invalid learning revision")
        return row[0]

    def revision(self) -> int:
        if self._empty:
            return 0
        with self._connection() as connection:
            return self._revision(connection)

    @staticmethod
    def _label(family: str, member=None) -> str:
        if member:
            return member.get("title") or member.get("project") or member.get("executable") or f"Workload {family[:12]}"
        return f"Workload {family[:12]}"

    @staticmethod
    def _group_label(profile, members) -> str:
        if profile["label"]:
            return profile["label"]
        representative = min(members, key=lambda member: (member["family"] != profile["id"], member["family"]))
        return ProfileStore._label(representative["family"], representative)

    @staticmethod
    def _text(value, name: str, *, optional=False, maximum=120, basename=False):
        if optional and value is None:
            return None
        if (not isinstance(value, str) or not 1 <= len(value) <= maximum
                or value.strip() != value or not value.isprintable()
                or (basename and ("/" in value or "\\" in value or value in {".", ".."}))):
            raise ProfileError(f"Invalid workload {name}")
        return value

    @staticmethod
    def _profile_id(value):
        if not isinstance(value, str) or not HASH.fullmatch(value):
            raise ProfileError("Invalid workload profile identity")
        return value

    @staticmethod
    def _prediction_group(connection, family):
        if not ProfileStore._has_learning(connection):
            return [family], {}
        member = connection.execute("SELECT profile FROM learning_members WHERE family=?", (family,)).fetchone()
        if member is None:
            return [family], {}
        profile = connection.execute("SELECT * FROM learning_profiles WHERE id=?", (member[0],)).fetchone()
        if profile is None:
            raise ProfileError("Workload grouping is inconsistent")
        families = [row[0] for row in connection.execute(
            "SELECT family FROM learning_members WHERE profile=? ORDER BY family", (member[0],))]
        return families, dict(profile)

    @staticmethod
    def _registry_slot(connection) -> bool:
        count = connection.execute("SELECT COUNT(*) FROM learning_members").fetchone()[0]
        if count < MAX_REGISTERED_FAMILIES:
            return True
        # Explicit corrections are never evicted. Only automatic metadata with
        # no remaining numeric history can be reclaimed without losing learning.
        candidate = connection.execute("""SELECT m.family,m.profile FROM learning_members m
            JOIN learning_profiles p ON p.id=m.profile WHERE p.pinned=0
            AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.family=m.family)
            AND NOT EXISTS(SELECT 1 FROM envelopes e WHERE e.family=m.family)
            AND NOT EXISTS(SELECT 1 FROM learning_retained e WHERE e.family=m.family)
            ORDER BY m.updated,m.family LIMIT 1""").fetchone()
        if candidate is None:
            return False
        connection.execute("DELETE FROM learning_members WHERE family=?", (candidate["family"],))
        connection.execute("DELETE FROM learning_profiles WHERE id=?", (candidate["profile"],))
        return True

    @staticmethod
    def _register(connection, family, description=None, *, required=False):
        member = connection.execute("SELECT * FROM learning_members WHERE family=?", (family,)).fetchone()
        if member is not None:
            return dict(member)
        if not ProfileStore._registry_slot(connection):
            if required:
                raise ProfileError("Learning registry is full; existing operator corrections were preserved")
            return None
        now = time.time()
        description = description or {}
        profile_id = family
        # A root family's original identifier can remain the identifier of a
        # group after that family has been separated into a new profile.
        if connection.execute("SELECT 1 FROM learning_profiles WHERE id=?", (profile_id,)).fetchone():
            profile_id = _digest(["profile-v1", family, os.urandom(16).hex()])
        connection.execute("INSERT INTO learning_profiles VALUES (?,?,NULL,NULL,0,0,?)", (profile_id, None, now))
        connection.execute("INSERT INTO learning_members VALUES (?,?,?,?,?,?)",
                           (family, profile_id, description.get("title"), description.get("project"),
                            description.get("executable"), now))
        return {"family": family, "profile": profile_id, "title": description.get("title"),
                "project": description.get("project"), "executable": description.get("executable"), "updated": now}

    def observe(self, identity: dict, description: dict) -> None:
        family, _, _ = _identity(identity)
        if not isinstance(description, dict) or set(description) - {"title", "project", "executable"}:
            raise ProfileError("Invalid safe workload description")
        description = {key: self._text(description.get(key), key, optional=True,
                                       maximum=120 if key == "title" else 80,
                                       basename=key != "title") for key in ("title", "project", "executable")}
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            previous = connection.execute("SELECT * FROM learning_members WHERE family=?", (family,)).fetchone()
            if previous is None:
                self._register(connection, family, description)
            else:
                # Preserve known metadata when an old or minimal caller has no
                # description. Avoid writes on every controller heartbeat.
                values = tuple(description[key] or previous[key] for key in ("title", "project", "executable"))
                if values != (previous["title"], previous["project"], previous["executable"]):
                    connection.execute("UPDATE learning_members SET title=?,project=?,executable=?,updated=? WHERE family=?",
                                       (*values, time.time(), family))

    def match(self, identity: dict) -> dict:
        family, _, _ = _identity(identity)
        if self._empty:
            return {"id": family, "label": self._label(family), "reason": "Automatic workload family"}
        with self._connection() as connection:
            if self._has_learning(connection):
                row = connection.execute("""SELECT m.*,p.label FROM learning_members m
                    JOIN learning_profiles p ON p.id=m.profile WHERE family=?""", (family,)).fetchone()
                if row:
                    members = [dict(member) for member in connection.execute(
                        "SELECT * FROM learning_members WHERE profile=?", (row["profile"],))]
                    return {"id": row["profile"],
                            "label": self._group_label({"id": row["profile"], "label": row["label"]}, members),
                            "reason": "Operator grouping" if len(members) > 1 or row["profile"] != family else "Automatic workload family"}
        return {"id": family, "label": self._label(family), "reason": "Automatic workload family"}

    @staticmethod
    def _dataset(connection):
        profiles = {}
        members = {}
        if ProfileStore._has_learning(connection):
            profiles = {row["id"]: dict(row) for row in connection.execute("SELECT * FROM learning_profiles")}
            members = {row["family"]: dict(row) for row in connection.execute("SELECT * FROM learning_members")}
        runs = [dict(row) for row in connection.execute("SELECT * FROM runs")]
        envelopes = [dict(row) for row in connection.execute("SELECT * FROM envelopes")]
        if ProfileStore._has_learning(connection):
            envelopes.extend(dict(row) for row in connection.execute("SELECT * FROM learning_retained"))
        for row in [*runs, *envelopes]:
            family = row["family"]
            if family not in members:
                if family in profiles:
                    raise ProfileError("Workload grouping is inconsistent")
                profiles[family] = {"id": family, "label": None, "memory_floor": None, "cpu_floor": None,
                                    "pinned": 0, "ignore_legacy": 0, "updated": row["sampled"]}
                members[family] = {"family": family, "profile": family, "title": None, "project": None, "executable": None}
        groups = {profile_id: {"members": [], "runs": [], "envelopes": []} for profile_id in profiles}
        for member in members.values():
            if member["profile"] not in groups:
                raise ProfileError("Workload grouping is inconsistent")
            groups[member["profile"]]["members"].append(member)
        for kind, observations in (("runs", runs), ("envelopes", envelopes)):
            for row in observations:
                groups[members[row["family"]]["profile"]][kind].append(row)
        return profiles, groups

    @staticmethod
    def _summary(profile, group):
        members = sorted(group["members"], key=lambda member: member["family"])
        if not members:
            raise ProfileError("Workload profile has no members")
        observations = [*group["runs"], *group["envelopes"]]
        return {"id": profile["id"], "label": ProfileStore._group_label(profile, members),
                "memberCount": len(members),
                "variantCount": len({(row["family"], row["variant"]) for row in observations}),
                "successCount": sum(row["success"] == 1 for row in group["runs"]),
                "incompleteCount": sum(row["success"] != 1 for row in group["runs"]),
                "peakMemoryBytes": max((row["memory"] for row in observations), default=0),
                "peakCpuCores": max((row["cpu"] for row in observations), default=0.0),
                "lastRunAt": max((row["sampled"] for row in observations), default=None),
                "memoryFloorBytes": profile["memory_floor"], "cpuFloorCores": profile["cpu_floor"]}

    def catalog(self, *, offset=0, limit=100, query="") -> dict:
        if type(offset) is not int or offset < 0 or type(limit) is not int or not 1 <= limit <= 100:
            raise ProfileError("Invalid workload profile page")
        if not isinstance(query, str) or len(query) > 120 or (query and not query.isprintable()):
            raise ProfileError("Invalid workload profile search")
        if self._empty:
            return {"revision": 0, "profiles": [], "total": 0, "offset": offset, "limit": limit}
        with self._connection() as connection:
            revision = self._revision(connection)
            profiles, groups = self._dataset(connection)
            entries = [self._summary(profile, groups[profile_id]) for profile_id, profile in profiles.items()]
            needle = query.casefold().strip()
            if needle:
                entries = [entry for entry in entries if needle in entry["label"].casefold() or needle in entry["id"]]
            entries.sort(key=lambda entry: (entry["label"].casefold(), entry["id"]))
            return {"revision": revision, "profiles": entries[offset:offset + limit], "total": len(entries),
                    "offset": offset, "limit": limit}

    def detail(self, profile_id, *, machine=None) -> dict:
        self._profile_id(profile_id)
        if machine is not None and (not isinstance(machine, str) or not machine):
            raise ProfileError("Invalid workload prediction machine")
        if self._empty:
            raise ProfileError("Workload profile was not found")
        with self._connection() as connection:
            profiles, groups = self._dataset(connection)
            if profile_id not in profiles:
                raise ProfileError("Workload profile was not found; refresh the list")
            profile, group = profiles[profile_id], groups[profile_id]
            members = sorted(group["members"], key=lambda member: member["family"])
            variants = {}
            for row in [*group["envelopes"], *group["runs"]]:
                key = (row["family"], row["variant"])
                item = variants.setdefault(key, {"id": row["variant"], "family": row["family"], "successCount": 0,
                                                 "incompleteCount": 0, "lastRunAt": 0,
                                                 "peakMemoryBytes": 0, "peakCpuCores": 0.0})
                item["lastRunAt"] = max(item["lastRunAt"], row["sampled"])
                item["peakMemoryBytes"] = max(item["peakMemoryBytes"], row["memory"])
                item["peakCpuCores"] = max(item["peakCpuCores"], row["cpu"])
                if "success" in row:
                    item["successCount"] += row["success"] == 1
                    item["incompleteCount"] += row["success"] != 1
            variants = sorted(variants.values(), key=lambda row: (-row["lastRunAt"], row["family"], row["id"]))
            runs = sorted(group["runs"], key=lambda row: (-row["sampled"], row["token"]))
            prediction = None
            hashed_machine = None
            if machine is not None:
                hashed_machine = _digest(["machine-v1", machine])
                # All grouped families contribute, with no fabricated exact
                # variant successes. This is explicitly a group estimate.
                representative = members[0]["family"]
                synthetic = _digest(["group-estimate-v1", profile_id])
                prediction = self._prediction(connection, representative, synthetic, hashed_machine)
            return {"revision": self._revision(connection), "profile": self._summary(profile, group),
                    "members": [{"family": member["family"], "label": self._label(member["family"], member),
                                 "project": member["project"] or "", "executable": member["executable"] or ""}
                                for member in members[:MAX_DETAIL_MEMBERS]],
                    "variants": variants[:MAX_DETAIL_VARIANTS],
                    "runs": [{"id": row["token"], "family": row["family"], "variant": row["variant"],
                              "machine": row["machine"], "sampledAt": row["sampled"], "memoryBytes": row["memory"],
                              "cpuCores": row["cpu"], "cpuSeconds": row["cpu_seconds"],
                              "durationSeconds": row["duration"], "success": bool(row["success"])}
                             for row in runs[:MAX_DETAIL_RUNS]],
                    "prediction": prediction, "predictionMachine": hashed_machine,
                    "truncated": {"members": len(members) > MAX_DETAIL_MEMBERS,
                                  "variants": len(variants) > MAX_DETAIL_VARIANTS, "runs": len(runs) > MAX_DETAIL_RUNS}}

    @staticmethod
    def _expected_revision(connection, revision):
        if type(revision) is not int or revision < 0:
            raise ProfileError("Invalid expected learning revision")
        if ProfileStore._revision(connection) != revision:
            raise ProfileError("Learning changed since you opened it; refresh before saving")

    @staticmethod
    def _changed(connection):
        connection.execute("UPDATE learning_state SET revision=revision+1 WHERE id=1")

    def _editable(self, connection, profile_id):
        self._profile_id(profile_id)
        profiles, groups = self._dataset(connection)
        if profile_id not in profiles:
            raise ProfileError("Workload profile was not found; refresh the list")
        for member in groups[profile_id]["members"]:
            self._register(connection, member["family"], required=True)
        return profiles[profile_id], groups[profile_id]

    def edit(self, profile_id, *, revision, label, memory_floor_bytes, cpu_floor_cores) -> None:
        label = self._text(label, "profile label")
        if memory_floor_bytes is not None and (type(memory_floor_bytes) is not int or not 0 < memory_floor_bytes <= 2 ** 50):
            raise ProfileError("Minimum memory must be a positive integer no greater than 2^50 bytes")
        if cpu_floor_cores is not None:
            cpu_floor_cores = _number(cpu_floor_cores, "minimumCpuCores")
            if not 0 < cpu_floor_cores <= 1024:
                raise ProfileError("Minimum CPU must be greater than zero and no greater than 1024 cores")
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            self._expected_revision(connection, revision)
            self._editable(connection, profile_id)
            connection.execute("UPDATE learning_profiles SET label=?,memory_floor=?,cpu_floor=?,pinned=1,updated=? WHERE id=?",
                               (label, memory_floor_bytes, cpu_floor_cores, time.time(), profile_id))
            self._changed(connection)

    def merge(self, profile_id, target_id, *, revision) -> None:
        self._profile_id(profile_id)
        self._profile_id(target_id)
        if profile_id == target_id:
            raise ProfileError("Choose a different profile to share learning with")
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            self._expected_revision(connection, revision)
            source, _ = self._editable(connection, profile_id)
            target, target_group = self._editable(connection, target_id)
            connection.execute("UPDATE learning_members SET profile=? WHERE profile=?", (target_id, profile_id))
            connection.execute("""UPDATE learning_profiles SET label=?,memory_floor=?,cpu_floor=?,ignore_legacy=?,pinned=1,updated=?
                WHERE id=?""", (self._group_label(target, target_group["members"]),
                               max(source["memory_floor"] or 0, target["memory_floor"] or 0) or None,
                               max(source["cpu_floor"] or 0, target["cpu_floor"] or 0) or None,
                               int(bool(source["ignore_legacy"] and target["ignore_legacy"])), time.time(), target_id))
            connection.execute("DELETE FROM learning_profiles WHERE id=?", (profile_id,))
            self._changed(connection)

    def split(self, profile_id, family, *, revision) -> str:
        self._profile_id(family)
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            self._expected_revision(connection, revision)
            source, group = self._editable(connection, profile_id)
            if not any(member["family"] == family for member in group["members"]):
                raise ProfileError("That job family does not belong to this profile")
            if len(group["members"]) < 2:
                raise ProfileError("This job family already has an independent profile")
            new_id = family
            if connection.execute("SELECT 1 FROM learning_profiles WHERE id=?", (new_id,)).fetchone():
                new_id = _digest(["profile-v1", family, os.urandom(16).hex()])
            connection.execute("INSERT INTO learning_profiles VALUES (?,?,?, ?,1,?,?)",
                               (new_id, None, source["memory_floor"], source["cpu_floor"], source["ignore_legacy"], time.time()))
            connection.execute("UPDATE learning_members SET profile=? WHERE family=?", (new_id, family))
            connection.execute("UPDATE learning_profiles SET pinned=1,updated=? WHERE id=?", (time.time(), profile_id))
            self._changed(connection)
            return new_id

    def reset(self, profile_id, *, revision) -> None:
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            self._expected_revision(connection, revision)
            _, group = self._editable(connection, profile_id)
            for member in group["members"]:
                connection.execute("DELETE FROM runs WHERE family=?", (member["family"],))
                connection.execute("DELETE FROM envelopes WHERE family=?", (member["family"],))
                connection.execute("DELETE FROM learning_retained WHERE family=?", (member["family"],))
                connection.execute("""INSERT INTO learning_resets VALUES (?,?) ON CONFLICT(family)
                    DO UPDATE SET sampled=MAX(sampled,excluded.sampled)""", (member["family"], time.time()))
            # The legacy retention floor has no family attribution. An explicit
            # reset opts this group out of that old floor, leaving other groups
            # untouched and retaining all explicit operator reservations.
            connection.execute("UPDATE learning_profiles SET ignore_legacy=1,pinned=1,updated=? WHERE id=?",
                               (time.time(), profile_id))
            self._changed(connection)

    def prediction(self, identity: dict, machine: str) -> dict:
        family, variant, _ = _identity(identity)
        if not isinstance(machine, str) or not machine:
            raise ProfileError("A prediction needs a machine identity")
        if self._empty:
            return {"memoryBytes": GIB, "cpuCores": 2.0, "durationSeconds": None,
                    "samples": 0, "confidence": "unknown", "source": "prior"}
        machine = _digest(["machine-v1", machine])
        with self._connection() as connection:
            return self._prediction(connection, family, variant, machine)

    def _prediction(self, connection, family: str, variant: str, machine: str) -> dict:
        now = time.time()
        families, operator = self._prediction_group(connection, family)
        placeholders = ",".join("?" for _ in families)
        envelopes = connection.execute(
            f"SELECT * FROM envelopes WHERE machine=? AND family IN ({placeholders})",
            (machine, *families)).fetchall()
        if self._has_learning(connection):
            envelopes.extend(connection.execute(
                f"SELECT * FROM learning_retained WHERE machine=? AND family IN ({placeholders})",
                (machine, *families)).fetchall())
        exact = [row for row in envelopes if row["family"] == family and row["variant"] == variant]
        # Exact successes never combine across grouped families, including when
        # there is no exact envelope yet for a newly changed source variant.
        runs = connection.execute("SELECT * FROM runs WHERE machine=? AND family=? AND variant=? AND sampled>=?",
                                  (machine, family, variant, now - 90 * DAY)).fetchall()
        floors = ([] if operator.get("ignore_legacy") else connection.execute(
            "SELECT memory,cpu FROM floors WHERE machine IN (?, '*')", (machine,)).fetchall())
        successes = [row for row in runs if row["success"]]
        samples = len(successes)
        recent_successes = sum(row["sampled"] >= now - 30 * DAY for row in successes)
        # A censored/failed observation is only a lower bound. It cannot replace
        # the family prior with its small observed prefix. Require repeat exact
        # successes before dropping sibling upper evidence and uncertainty.
        borrowing = bool(exact) and recent_successes < 5 and any(
            (row["family"], row["variant"]) != (family, variant) for row in envelopes)
        use_exact = bool(exact) and not borrowing
        selected = envelopes if borrowing else exact or envelopes
        source = "variant" if use_exact else "family" if envelopes else "prior"
        age = max(0, now - max((row["sampled"] for row in selected), default=now))
        minimum_memory = operator.get("memory_floor") or 0
        minimum_cpu = operator.get("cpu_floor") or 0
        memory_floor = max([GIB, minimum_memory, *[row["memory"] for row in floors]])
        cpu_floor = max([2.0, minimum_cpu, *[row["cpu"] for row in floors]])
        if not selected:
            return {"memoryBytes": memory_floor, "cpuCores": cpu_floor, "durationSeconds": None,
                    "samples": 0, "confidence": "unknown", "source": source}
        margin = 1.25 if use_exact else 1.5
        # Keep upper evidence across retention pruning. Very old evidence eases
        # only to 75 percent, and stale profiles regain the cold-start floor.
        age_factor = max(0.75, 1 - max(0, age - 30 * DAY) / (240 * DAY))
        memory = max(row["memory"] for row in selected) * margin * age_factor
        cpu = max(row["cpu"] for row in selected) * margin * age_factor
        # Envelope timestamps include censored peaks. Only recent successful
        # runs can establish that an exact variant is still inexpensive.
        if not use_exact or recent_successes < 5:
            memory = max(memory, memory_floor)
            cpu = max(cpu, cpu_floor)
        else:
            memory = max(memory, 64 * 1024 ** 2, minimum_memory, *[row["memory"] for row in floors])
            cpu = max(cpu, 0.1, minimum_cpu, *[row["cpu"] for row in floors])
        durations = [row["duration"] for row in selected if row["duration"] > 0]
        confidence = ("high" if use_exact and recent_successes >= 12
                      else "medium" if use_exact and recent_successes >= 5 else "low")
        if age > 30 * DAY or (runs and not max(runs, key=lambda row: row["sampled"])["success"]):
            confidence = "low"
        return {"memoryBytes": math.ceil(memory), "cpuCores": max(round(cpu, 3), minimum_cpu),
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
        cpu = _number(max(_number(metrics.get("cpuCores", 0), "cpuCores"),
                          cpu_seconds / duration if duration else 0), "cpuCores")
        sampled = _number(metrics.get("sampledAt", time.time()), "sampledAt")
        if memory > 2 ** 63 - 1 or sampled > time.time() + 300:
            raise ProfileError("Resource metric is out of range")
        token = _digest(["run-v1", token])
        machine = _digest(["machine-v1", machine])
        success = int(complete and exit_code == 0 and memory > 0 and duration > 0)
        with self._connection(write=True) as connection:
            connection.execute("BEGIN IMMEDIATE")
            reset = connection.execute("SELECT sampled FROM learning_resets WHERE family=?", (family,)).fetchone()
            if reset is not None and sampled <= reset[0]:
                return
            prior = connection.execute("SELECT family,variant,machine FROM runs WHERE token=?", (token,)).fetchone()
            if prior and (prior["family"], prior["variant"], prior["machine"]) != (family, variant, machine):
                raise ProfileError("A resource token cannot change workload identity")
            connection.execute("""INSERT INTO runs VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(token) DO UPDATE SET
                memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),
                cpu_seconds=MAX(cpu_seconds,excluded.cpu_seconds),duration=MAX(duration,excluded.duration),
                sampled=CASE WHEN success=1 AND excluded.success=0 THEN sampled
                    ELSE MAX(sampled,excluded.sampled) END,success=MAX(success,excluded.success)""",
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
                connection.execute("""INSERT INTO learning_retained VALUES (?,?,?,?,?,?,?)
                    ON CONFLICT(machine,family,variant) DO UPDATE SET memory=MAX(memory,excluded.memory),
                    cpu=MAX(cpu,excluded.cpu),duration=MAX(duration,excluded.duration),sampled=MAX(sampled,excluded.sampled)""",
                                   (row["machine"], row["family"], row["variant"], row["memory"], row["cpu"],
                                    row["duration"], row["sampled"]))
                connection.execute("DELETE FROM envelopes WHERE machine=? AND family=? AND variant=?",
                                   (row["machine"], row["family"], row["variant"]))
            overflow_retained = connection.execute("SELECT * FROM learning_retained ORDER BY sampled DESC LIMIT -1 OFFSET ?",
                                                   (MAX_RETAINED_ENVELOPES,)).fetchall()
            for row in overflow_retained:
                # At the outer bound retain a conservative machine floor, as
                # previous versions did. If a reset group reaches this bound,
                # restore its use of that floor rather than discard a peak.
                connection.execute("""INSERT INTO floors VALUES (?,?,?,?) ON CONFLICT(machine) DO UPDATE SET
                    memory=MAX(memory,excluded.memory),cpu=MAX(cpu,excluded.cpu),sampled=MAX(sampled,excluded.sampled)""",
                                   (row["machine"], row["memory"], row["cpu"], row["sampled"]))
                connection.execute("""UPDATE learning_profiles SET ignore_legacy=0 WHERE id IN
                    (SELECT profile FROM learning_members WHERE family=?)""", (row["family"],))
                connection.execute("DELETE FROM learning_retained WHERE machine=? AND family=? AND variant=?",
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
