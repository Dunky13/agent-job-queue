#!/usr/bin/env python3
"""Opt-in local HTTPS service control; credentials are owned by Caddy."""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import ipaddress
import json
import os
import plistlib
import re
import secrets
import select
import shutil
import signal
import socket
import stat
import subprocess
import sys
import time
import uuid
from pathlib import Path

TOOL_PATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
ROOT = Path(__file__).resolve().parent
LABEL = "io.went.agent-remote"
USERNAME = "agent"
DEFAULT_PORT = 8743
CONTROL_DEADLINE = None


class RemoteError(Exception):
    pass


class Paths:
    def __init__(self):
        self.testing = os.environ.get("AGENT_REMOTE_TEST_MODE") == "1"
        self.state = Path(os.environ.get("AGENT_REMOTE_STATE", Path.home() / ".local/state/agent-heavy/remote")) if self.testing else Path.home() / ".local/state/agent-heavy/remote"
        self.config = Path(os.environ.get("AGENT_REMOTE_CONFIG", self.state / "config.json")) if self.testing else Path.home() / ".config/agent-heavy/remote.json"
        self.plist = self.state / f"{LABEL}.plist" if self.testing else Path.home() / "Library/LaunchAgents" / f"{LABEL}.plist"
        self.socket = self.state / "api.sock"
        self.pid = self.state / "processes.json"
        self.caddyfile = self.state / "Caddyfile"
        self.certificate = self.state / "caddy/pki/authorities/local/root.crt"
        self.log = self.state / "service.log"
        self.error = self.state / "error.json"
        self.children = self.state / "children"

    def prepare(self):
        for directory in (self.state, self.config.parent, self.state / "caddy", self.children):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            info = directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
                raise RemoteError("Remote access requires private directories owned by the current user")
            directory.chmod(0o700)


def save_private(path: Path, value):
    temporary = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            data = json.dumps(value).encode() if isinstance(value, dict) else value
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def load_private(path: Path) -> dict:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise RemoteError("Unsafe remote settings ownership or permissions")
        data = json.load(handle)
    if not isinstance(data, dict):
        raise RemoteError("Invalid remote settings")
    return data


def control_environment() -> dict[str, str]:
    environment = {"HOME": str(Path.home()), "PATH": TOOL_PATH, "GOMAXPROCS": "2"}
    if "AGENT_REMOTE_CADDY" in os.environ:
        environment["AGENT_REMOTE_CADDY"] = os.environ["AGENT_REMOTE_CADDY"]
    return environment


def caddy_executable() -> str:
    override = os.environ.get("AGENT_REMOTE_CADDY")
    if override is not None:
        if not Path(override).is_absolute() or not Path(override).is_file() or not os.access(override, os.X_OK):
            raise RemoteError("AGENT_REMOTE_CADDY must name an absolute executable file")
        return override
    executable = shutil.which("caddy", path=TOOL_PATH)
    if executable is None:
        raise RemoteError("Caddy is not installed; install caddy before enabling remote access")
    return executable


def command(arguments: list[str], *, input_data: bytes | None = None, timeout=5, check=True):
    if CONTROL_DEADLINE is not None:
        timeout = min(timeout, CONTROL_DEADLINE - time.monotonic())
        if timeout <= 0:
            raise RemoteError("Remote settings operation exceeded its deadline; inspect service status")
    try:
        result = subprocess.run(arguments, input=input_data, capture_output=True, timeout=timeout,
                                env=control_environment(),
                                umask=0o077, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RemoteError("A required remote-access tool is unavailable") from error
    if check and result.returncode:
        raise RemoteError("Remote-access tool failed; inspect the private service log")
    return result


def lan_address(paths: Paths) -> str:
    if paths.testing and os.environ.get("AGENT_REMOTE_BIND"):
        return str(ipaddress.IPv4Address(os.environ["AGENT_REMOTE_BIND"]))
    route = command(["/sbin/route", "-n", "get", "default"], check=False).stdout.decode()
    match = re.search(r"interface:\s+(\S+)", route)
    interfaces = ([match[1]] if match else []) + command(["/sbin/ifconfig", "-l"]).stdout.decode().split()
    for interface in dict.fromkeys(interfaces):
        result = command(["/usr/sbin/ipconfig", "getifaddr", interface], check=False)
        try:
            address = ipaddress.IPv4Address(result.stdout.decode().strip())
        except ipaddress.AddressValueError:
            continue
        if address.is_private and not address.is_loopback and not address.is_link_local:
            return str(address)
    raise RemoteError("No private LAN IPv4 address is available")


def allowed_hosts(paths: Paths) -> list[str]:
    hosts = [lan_address(paths)]
    if not paths.testing:
        name = command(["/usr/sbin/scutil", "--get", "LocalHostName"], check=False).stdout.decode().strip()
        if re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", name):
            hosts.append(name.lower() + ".local")
    return hosts


def load_settings(paths: Paths) -> dict | None:
    try:
        data = load_private(paths.config)
    except FileNotFoundError:
        return None
    if set(data) != {"enabled", "username", "port", "hosts", "password_hash", "secret_key"}:
        raise RemoteError("Invalid remote settings fields")
    if type(data["enabled"]) is not bool or data["username"] != USERNAME or type(data["port"]) is not int or not 1024 <= data["port"] <= 65535:
        raise RemoteError("Invalid remote settings")
    if not isinstance(data["password_hash"], str) or not isinstance(data["secret_key"], str) \
            or not re.fullmatch(r"\$2[aby]\$14\$[./A-Za-z0-9]{53}", data["password_hash"]) or len(data["secret_key"]) < 32:
        raise RemoteError("Invalid remote credential configuration")
    if not isinstance(data["hosts"], list) or not data["hosts"]:
        raise RemoteError("Invalid remote host configuration")
    for host in data["hosts"]:
        if not isinstance(host, str) or not re.fullmatch(r"[A-Za-z0-9.-]{1,253}", host):
            raise RemoteError("Invalid remote host configuration")
    return data


def birth(pid: int) -> str | None:
    if type(pid) is not int or pid <= 1:
        return None
    result = command(["/bin/ps", "-p", str(pid), "-o", "lstart=,stat="], check=False)
    fields = result.stdout.decode().strip().rsplit(None, 1)
    return fields[0].strip() if len(fields) == 2 and not fields[1].startswith("Z") else None


def process_record(pid: int) -> dict:
    started = birth(pid)
    if not started:
        raise RemoteError("Could not establish service process identity")
    return {"pid": pid, "birth": started}


def boot_identity() -> str:
    value = command(["/usr/sbin/sysctl", "-n", "kern.bootsessionuuid"]).stdout.decode().strip()
    return str(uuid.UUID(value))


def revision(settings: dict) -> str:
    return hashlib.sha256(json.dumps(settings, sort_keys=True).encode()).hexdigest()


@contextlib.contextmanager
def registry_lock(paths: Paths, name: str = "registry.lock"):
    descriptor = os.open(paths.state / name, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "a") as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise RemoteError("Unsafe remote coordination lock")
        deadline = min(time.monotonic() + 5, CONTROL_DEADLINE or float("inf"))
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RemoteError("Remote service registry is busy")
                time.sleep(0.02)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def generation(paths: Paths) -> dict | None:
    try:
        return load_private(paths.pid)
    except FileNotFoundError:
        return None


def child_path(paths: Paths, token: str, role: str) -> Path:
    if not re.fullmatch(r"[0-9a-f]{32}", token) or role not in ("api", "caddy"):
        raise RemoteError("Invalid service child identity")
    return paths.children / f"{token}-{role}.json"


def is_running(paths: Paths) -> bool:
    try:
        record = generation(paths)
        settings = load_settings(paths)
        if not record or not settings or not record.get("active") or not record.get("ready") \
                or record.get("revision") != revision(settings) or record.get("boot_uuid") != boot_identity():
            return False
        supervisor = record.get("supervisor", {})
        if not supervisor.get("birth") or birth(supervisor.get("pid")) != supervisor["birth"]:
            return False
        for role in ("api", "caddy"):
            child = load_private(child_path(paths, record["token"], role))
            if not child.get("ack") or not child.get("birth") or birth(child.get("pid")) != child["birth"]:
                return False
        return paths.socket.is_socket()
    except (FileNotFoundError, RemoteError, OSError):
        return False


def status(paths: Paths) -> dict:
    settings = load_settings(paths)
    enabled = bool(settings and settings["enabled"])
    running = is_running(paths)
    try:
        error = load_private(paths.error).get("error")
    except FileNotFoundError:
        error = None
    if not enabled and unresolved_children(paths):
        error = error or "Remote access is disabled, but owned service cleanup is incomplete"
    elif enabled and not running:
        error = error or "Remote service is not running"
    return {"enabled": enabled, "configured": settings is not None, "running": bool(running),
            "url": f"https://{settings['hosts'][0]}:{settings['port']}/" if settings else "",
            "port": settings["port"] if settings else DEFAULT_PORT, "username": USERNAME,
            "certificatePath": str(paths.certificate) if paths.certificate.is_file() else "", "error": error}


def read_configuration_input() -> dict:
    descriptor = sys.stdin.fileno()
    deadline = time.monotonic() + 5
    data = bytearray()
    while time.monotonic() < deadline:
        ready, _, _ = select.select([descriptor], [], [], max(0, deadline - time.monotonic()))
        if not ready:
            break
        chunk = os.read(descriptor, 4096)
        if not chunk:
            try:
                value = json.loads(data)
            except (ValueError, UnicodeDecodeError) as error:
                raise RemoteError("Configure expects JSON on stdin") from error
            if not isinstance(value, dict) or not {"password"} <= set(value) <= {"password", "port"}:
                raise RemoteError("Configure accepts only password and optional port")
            return value
        data.extend(chunk)
        if len(data) > 4096:
            raise RemoteError("Configure input is too large")
    raise RemoteError("Configure stdin did not finish before its deadline")


def configure(paths: Paths, value: dict):
    password = value.get("password")
    port = value.get("port", DEFAULT_PORT)
    if not isinstance(password, str) or len(password) < 4 or not all(character.isprintable() for character in password):
        raise RemoteError("Use at least 4 characters. No special characters required.")
    if len(password.encode()) > 72:
        raise RemoteError("Password must be at most 72 UTF-8 bytes for Caddy bcrypt")
    if type(port) is not int or not 1024 <= port <= 65535:
        raise RemoteError("Port must be from 1024 to 65535")
    paths.prepare()
    previous = load_settings(paths)
    hashed = command([caddy_executable(), "hash-password", "--algorithm", "bcrypt", "--bcrypt-cost", "14"],
                     input_data=password.encode() + b"\n", timeout=8).stdout.decode().strip()
    settings = {"enabled": bool(previous and previous["enabled"]), "username": USERNAME, "port": port,
                "hosts": allowed_hosts(paths), "password_hash": hashed, "secret_key": secrets.token_hex(32)}
    # Validate generated Caddy configuration before changing durable credentials.
    candidate = paths.state / "Caddyfile.pending"
    save_private(candidate, caddy_configuration(paths, settings).encode())
    try:
        command([caddy_executable(), "adapt", "--config", str(candidate), "--adapter", "caddyfile", "--validate"], timeout=8)
    finally:
        candidate.unlink(missing_ok=True)
    save_private(paths.config, settings)
    if settings["enabled"]:
        enable(paths)
    elif paths.error.exists() or unresolved_children(paths):
        stop_owned_service(paths)
        paths.error.unlink(missing_ok=True)


def caddy_configuration(paths: Paths, settings: dict) -> str:
    sites = ", ".join(f"https://{host}:{settings['port']}" for host in settings["hosts"])
    # Configured hosts/port/hash are validated; paths are JSON quoted for Caddy.
    return "\n".join(["{", " admin off", " persist_config off", " auto_https disable_redirects", " skip_install_trust",
                       " storage file_system {", f"  root {json.dumps(str(paths.state / 'caddy'))}", " }",
                       " servers {", "  protocols h1 h2", " }", "}", sites + " {",
                       f" bind {settings['hosts'][0]}", " tls internal", " basic_auth {",
                       f"  {USERNAME} {settings['password_hash']}", " }", " header -Server",
                       f" reverse_proxy {json.dumps('unix/' + str(paths.socket))} {{",
                       "  header_up -Authorization", "  header_up -Forwarded", "  header_up -X-Forwarded-Host",
                       "  header_up X-Forwarded-Proto https", " }", "}", ""])


def launch_agent(paths: Paths):
    paths.plist.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    environment = control_environment()
    settings = {"Label": LABEL, "ProgramArguments": [sys.executable, str(ROOT / "control.py"), "serve"],
                "RunAtLoad": True, "KeepAlive": {"SuccessfulExit": False}, "ThrottleInterval": 10,
                "ProcessType": "Background", "Umask": 63, "EnvironmentVariables": environment,
                "StandardOutPath": str(paths.log), "StandardErrorPath": str(paths.log)}
    save_private(paths.plist, plistlib.dumps(settings))


def service_action(paths: Paths, action: str):
    if paths.testing:
        if action == "stop":
            try:
                record = load_private(paths.pid)
                supervisor = record.get("supervisor", {})
                if record.get("boot_uuid") == boot_identity() and supervisor.get("pid") != os.getpid() \
                        and supervisor.get("birth") and birth(supervisor.get("pid")) == supervisor["birth"]:
                    os.kill(supervisor["pid"], signal.SIGTERM)
            except FileNotFoundError:
                pass
        return
    domain = f"gui/{os.getuid()}"
    if action == "stop":
        command(["/bin/launchctl", "bootout", f"{domain}/{LABEL}"], check=False)
    else:
        command(["/bin/launchctl", "bootstrap", domain, str(paths.plist)])


def enable(paths: Paths):
    paths.prepare()
    settings = load_settings(paths)
    if settings is None:
        raise RemoteError("Configure a password before enabling remote access")
    settings.update(enabled=True, hosts=allowed_hosts(paths))
    save_private(paths.config, settings)
    stop_owned_service(paths)
    launch_agent(paths)
    service_action(paths, "start")
    if not paths.testing:
        deadline = time.monotonic() + 15
        while not is_running(paths) and time.monotonic() < deadline:
            time.sleep(0.15)
        if not is_running(paths):
            raise RemoteError("Remote access was enabled but its service did not become ready")
    paths.error.unlink(missing_ok=True)


def disable(paths: Paths):
    paths.prepare()
    settings = load_settings(paths)
    if settings is not None:
        settings["enabled"] = False
        save_private(paths.config, settings)
    stop_owned_service(paths)
    paths.error.unlink(missing_ok=True)


def deactivate(paths: Paths, token: str | None = None):
    with registry_lock(paths):
        record = generation(paths)
        if record and (token is None or record.get("token") == token):
            record["active"] = False
            record["ready"] = False
            save_private(paths.pid, record)


def reconcile_children(paths: Paths, token: str | None = None):
    current_boot = boot_identity()
    for path in paths.children.glob("*.json"):
        record = load_private(path)
        if token is not None and record.get("generation") != token:
            continue
        expected = child_path(paths, record.get("generation", ""), record.get("role", ""))
        if path != expected or type(record.get("pid")) is not int or not isinstance(record.get("birth"), str):
            raise RemoteError("Unknown service child record; refusing to signal it")
        # A different boot or birth proves that the recorded process departed.
        if record.get("boot_uuid") == current_boot and birth(record["pid"]) == record["birth"]:
            for signum, timeout in ((signal.SIGTERM, 2), (signal.SIGKILL, 2)):
                if birth(record["pid"]) != record["birth"]:
                    break
                if record["pid"] == os.getpid() or os.getpgid(record["pid"]) != record["pid"]:
                    raise RemoteError("Owned service group identity could not be verified")
                try:
                    os.killpg(record["pid"], signum)
                except ProcessLookupError:
                    break
                deadline = time.monotonic() + timeout
                while birth(record["pid"]) == record["birth"] and time.monotonic() < deadline:
                    time.sleep(0.05)
            if birth(record["pid"]) == record["birth"]:
                raise RemoteError("An owned remote service process did not stop")
        path.unlink(missing_ok=True)


def unresolved_children(paths: Paths) -> bool:
    paths_list = list(paths.children.glob("*.json"))
    if not paths_list:
        return False
    try:
        current_boot = boot_identity()
        for path in paths_list:
            record = load_private(path)
            if path != child_path(paths, record.get("generation", ""), record.get("role", "")) \
                    or type(record.get("pid")) is not int or not isinstance(record.get("birth"), str):
                return True
            if record.get("boot_uuid") == current_boot and birth(record["pid"]) == record["birth"]:
                return True
    except (RemoteError, OSError, ValueError):
        return True
    return False


def stop_supervisor(paths: Paths):
    deadline = time.monotonic() + 3
    record = generation(paths)
    supervisor = record.get("supervisor", {}) if record else {}
    if record and record.get("boot_uuid") == boot_identity() and supervisor.get("birth") \
            and birth(supervisor.get("pid")) == supervisor["birth"]:
        if supervisor["pid"] == os.getpid():
            raise RemoteError("Refusing to stop the current control process")
        os.kill(supervisor["pid"], signal.SIGTERM)
    while supervisor.get("birth") and birth(supervisor.get("pid")) == supervisor["birth"] and time.monotonic() < deadline:
        time.sleep(0.05)
    if record and record.get("boot_uuid") == boot_identity() and supervisor.get("birth") \
            and birth(supervisor.get("pid")) == supervisor["birth"]:
        if supervisor["pid"] == os.getpid():
            raise RemoteError("Refusing to stop the current control process")
        os.kill(supervisor["pid"], signal.SIGKILL)


def stop_owned_service(paths: Paths):
    failures = []
    # Every stage is attempted even if launchctl is unavailable or times out.
    for action in (lambda: deactivate(paths), lambda: service_action(paths, "stop"), lambda: stop_supervisor(paths)):
        try:
            action()
        except (RemoteError, OSError, subprocess.SubprocessError) as error:
            failures.append(str(error))
    try:
        with registry_lock(paths):
            reconcile_children(paths)
            current = generation(paths)
            if current and not current.get("active"):
                paths.pid.unlink(missing_ok=True)
    except (RemoteError, OSError, subprocess.SubprocessError) as error:
        failures.append(str(error))
    if failures:
        message = "Remote shutdown reported errors: " + "; ".join(failures)
        save_private(paths.error, {"error": message})
        raise RemoteError(message)


def child_main(paths: Paths, role: str, token: str):
    path = child_path(paths, token, role)
    own = {**process_record(os.getpid()), "generation": token, "role": role, "boot_uuid": boot_identity(), "ack": False}
    with registry_lock(paths):
        parent = generation(paths)
        settings = load_settings(paths)
        if not parent or parent.get("token") != token or not parent.get("active") or not settings or not settings["enabled"]:
            return
        if birth(parent["supervisor"]["pid"]) != parent["supervisor"]["birth"]:
            return
        save_private(path, own)
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        parent = generation(paths)
        settings = load_settings(paths)
        if not parent or parent.get("token") != token or not parent.get("active") or not settings or not settings["enabled"] \
                or parent.get("revision") != revision(settings) or birth(parent["supervisor"]["pid"]) != parent["supervisor"]["birth"]:
            return
        if load_private(path).get("ack"):
            break
        time.sleep(0.03)
    else:
        return
    if role == "api":
        api_process(paths)
    else:
        executable = caddy_executable()
        os.execve(executable, [executable, "run", "--config", str(paths.caddyfile), "--adapter", "caddyfile"],
                  control_environment())


def api_app(settings: dict, runner, testing: bool):
    from server import create_app

    return create_app(settings, runner=runner, test_mode=testing)


def api_process(paths: Paths):
    from waitress import serve

    settings = load_settings(paths)
    if not settings or not settings["enabled"]:
        raise RemoteError("Remote access is disabled")
    runner = None
    if paths.testing and os.environ.get("AGENT_REMOTE_TEST_CONTROLLER"):
        fixture = os.environ["AGENT_REMOTE_TEST_CONTROLLER"]

        def fixture_runner(arguments, input_data=None):
            payload = None if input_data is None else input_data.encode()
            return json.loads(command([sys.executable, fixture, *arguments], input_data=payload, timeout=23).stdout)

        runner = fixture_runner
    serve(api_app(settings, runner, paths.testing), unix_socket=str(paths.socket), unix_socket_perms="600", threads=2,
          url_scheme="https", trusted_proxy="localhost", trusted_proxy_headers={"x-forwarded-proto"},
          clear_untrusted_proxy_headers=True, connection_limit=32, max_request_body_size=4096,
          max_request_header_size=16384, channel_timeout=30, expose_tracebacks=False)


def supervisor(paths: Paths) -> int:
    paths.prepare()
    settings = load_settings(paths)
    if not settings or not settings["enabled"]:
        return 0
    lock_descriptor = os.open(paths.state / "supervisor.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    children = []
    stopping = [False]
    token = uuid.uuid4().hex

    def stop(_signum, _frame):
        stopping[0] = True

    with os.fdopen(lock_descriptor, "a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RemoteError("Remote supervisor is already running") from error
        for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            signal.signal(signum, stop)
        deactivate(paths)
        with registry_lock(paths):
            reconcile_children(paths)
            save_private(paths.pid, {"token": token, "active": True, "ready": False,
                                    "boot_uuid": boot_identity(), "revision": revision(settings),
                                    "supervisor": process_record(os.getpid())})
        try:
            if paths.socket.exists():
                info = paths.socket.lstat()
                if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                    raise RemoteError("Refusing to remove an unknown API socket")
                paths.socket.unlink()
            save_private(paths.caddyfile, caddy_configuration(paths, settings).encode())
            descriptor = os.open(paths.log, os.O_CREAT | os.O_APPEND | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, "ab", buffering=0) as log:
                environment = control_environment()
                if paths.testing:
                    environment.update({key: value for key, value in os.environ.items() if key.startswith("AGENT_REMOTE_")})

                def spawn(role):
                    child = subprocess.Popen([sys.executable, str(ROOT / "control.py"), "child", role, token],
                                             stdout=log, stderr=log, stdin=subprocess.DEVNULL,
                                             start_new_session=True, umask=0o077, env=environment)
                    children.append(child)
                    path = child_path(paths, token, role)
                    deadline = time.monotonic() + 8
                    while not path.exists() and child.poll() is None and time.monotonic() < deadline and not stopping[0]:
                        time.sleep(0.03)
                    gate = os.environ.get("AGENT_REMOTE_TEST_ACK_GATE") if paths.testing and role == "caddy" else None
                    while gate and not Path(gate).exists() and time.monotonic() < deadline and not stopping[0]:
                        time.sleep(0.03)
                    with registry_lock(paths):
                        record = load_private(path)
                        current = generation(paths)
                        if not current or current.get("token") != token or not current.get("active") \
                                or record.get("pid") != child.pid or birth(child.pid) != record.get("birth"):
                            raise RemoteError("Service child registration was not acknowledged")
                        record["ack"] = True
                        save_private(path, record)
                    return child

                api = spawn("api")
                deadline = time.monotonic() + 8
                while not paths.socket.exists() and api.poll() is None and time.monotonic() < deadline and not stopping[0]:
                    time.sleep(0.05)
                if not paths.socket.is_socket():
                    raise RemoteError("Private queue API did not start")
                gateway = spawn("caddy")
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and gateway.poll() is None and not stopping[0]:
                    try:
                        with socket.create_connection((settings["hosts"][0], settings["port"]), timeout=0.2):
                            break
                    except OSError:
                        time.sleep(0.1)
                else:
                    raise RemoteError("HTTPS gateway did not start")
                with registry_lock(paths):
                    record = generation(paths)
                    if not record or record.get("token") != token or not record.get("active"):
                        raise RemoteError("Service generation was superseded during startup")
                    record["ready"] = True
                    save_private(paths.pid, record)
                paths.error.unlink(missing_ok=True)
                while not stopping[0]:
                    current = generation(paths)
                    if not current or current.get("token") != token or not current.get("active"):
                        break
                    if any(child.poll() is not None for child in children):
                        raise RemoteError("A remote service child exited unexpectedly")
                    time.sleep(0.2)
                return 0
        except RemoteError as error:
            save_private(paths.error, {"error": str(error)})
            raise
        finally:
            deactivate(paths, token)
            with registry_lock(paths):
                reconcile_children(paths, token)
                current = generation(paths)
                if current and current.get("token") == token:
                    paths.pid.unlink(missing_ok=True)
            for child in children:
                child.wait(timeout=3)
            if paths.socket.is_socket():
                paths.socket.unlink()


def main() -> int:
    global CONTROL_DEADLINE
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    for action in ("status", "configure", "enable", "disable", "serve", "api"):
        child = commands.add_parser(action)
        if action == "status":
            child.add_argument("--json", action="store_true")
    child_parser = commands.add_parser("child")
    child_parser.add_argument("role", choices=("api", "caddy"))
    child_parser.add_argument("token")
    args = parser.parse_args()
    paths = Paths()
    try:
        if args.action in ("configure", "enable", "disable"):
            CONTROL_DEADLINE = time.monotonic() + 30
            paths.prepare()
            with registry_lock(paths, "control.lock"):
                try:
                    if args.action == "configure":
                        configure(paths, read_configuration_input())
                    elif args.action == "enable":
                        enable(paths)
                    else:
                        disable(paths)
                except (RemoteError, OSError, ValueError) as error:
                    try:
                        save_private(paths.error, {"error": str(error)})
                    except OSError as save_error:
                        raise RemoteError("Remote control failed and its failure state could not be saved") from save_error
                    raise
        elif args.action == "serve":
            return supervisor(paths)
        elif args.action == "api":
            api_process(paths)
            return 0
        elif args.action == "child":
            child_main(paths, args.role, args.token)
            return 0
        print(json.dumps(status(paths)))
        return 0
    except (RemoteError, OSError, ValueError) as error:
        print(json.dumps({"error": str(error)}))
        return 125


if __name__ == "__main__":
    sys.exit(main())
