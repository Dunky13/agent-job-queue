"""Explicit, bounded release checks and verified installer handoff.

The public GitHub release is the distribution trust boundary. Checking never
installs, and preparing an update opens the existing installer for confirmation.
No queue process, settings, credentials or Gatekeeper policy is changed here.
"""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
import plistlib
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unicodedata
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path, PurePosixPath

from release_metadata import REPOSITORY, validate_build_info, validate_manifest

API = f"https://api.github.com/repos/{REPOSITORY}/releases?per_page=100"
TAG = re.compile(r"build-([1-9][0-9]{0,8})-([0-9a-f]{7})\Z")
MAX_ARCHIVE = 128 * 1024 ** 2
MAX_EXPANDED = 512 * 1024 ** 2
MAX_RELEASE_PAGES = 10
INSTALLER = "Agent Job Queue Installer.app"
ALLOWED_HOSTS = {"api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"}


class UpdateError(Exception):
    """A failed check or verification must never become permission to install."""


def _read_owned(path: Path, limit: int = 16_384) -> bytes:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as handle:
        info = os.fstat(handle.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o022 or info.st_nlink != 1):
            raise UpdateError("Installed version metadata has unsafe ownership or permissions")
        data = handle.read(limit + 1)
        if len(data) > limit:
            raise UpdateError("Installed version metadata exceeds its size limit")
        return data


def _require_completed_installation(root: Path):
    pending = root / "install-in-progress.json"
    if pending.exists() or pending.is_symlink():
        validate_build_info(json.loads(_read_owned(pending)))
        raise UpdateError("Installation is incomplete. Finish or retry the installer before checking versions or updates")


def installed_version(root: Path | None = None, app: Path | None = None) -> dict:
    root = root if root is not None else Path.home() / ".local/share/agent-heavy"
    _require_completed_installation(root)
    result = _installed_version(root, app)
    _require_completed_installation(root)
    return result


def _installed_version(root: Path, app: Path | None) -> dict:
    root = root if root is not None else Path.home() / ".local/share/agent-heavy"
    app = app if app is not None else Path.home() / "Applications/Agent Queue.app"
    marker = root / "build-info.json"
    bundled = app / "Contents/Resources/build-info.json"
    try:
        if marker.exists() or marker.is_symlink():
            data = validate_build_info(json.loads(_read_owned(marker)))
            if data != validate_build_info(json.loads(_read_owned(bundled))):
                raise UpdateError("App and controller versions differ. Complete installation before checking for updates")
            info = plistlib.loads(_read_owned(app / "Contents/Info.plist"))
            if (info.get("CFBundleIdentifier") != "io.went.agent-queue"
                    or info.get("CFBundleShortVersionString") != data["version"]
                    or info.get("CFBundleVersion") != str(data["build"] or 0)):
                raise UpdateError("Installed app version does not match its release metadata")
            build = data["build"]
            display = f"{data['version']} (build {build})" if build else f"{data['version']} (untracked build)"
            return {"version": data["version"], "build": build, "bundleBuild": info["CFBundleVersion"],
                    "commit": data["commit"], "tracked": build is not None, "displayVersion": display}
        if bundled.exists() or bundled.is_symlink():
            raise UpdateError("Installed controller version is missing. Complete installation before checking for updates")
        plist = app / "Contents/Info.plist"
        if not plist.exists() and not plist.is_symlink():
            return {"version": None, "build": None, "bundleBuild": None, "commit": None,
                    "tracked": False, "displayVersion": "Not installed"}
        info = plistlib.loads(_read_owned(plist))
        version, build = info.get("CFBundleShortVersionString"), info.get("CFBundleVersion")
        if (info.get("CFBundleIdentifier") != "io.went.agent-queue" or not isinstance(version, str)
                or not 1 <= len(version) <= 80 or not version.isprintable()
                or not isinstance(build, str) or not 1 <= len(build) <= 80 or not build.isprintable()):
            raise UpdateError("Installed app version is invalid")
        return {"version": version, "build": None, "bundleBuild": build, "commit": None,
                "tracked": False, "displayVersion": f"{version} (build {build}, legacy installation)"}
    except (ValueError, plistlib.InvalidFileException) as error:
        raise UpdateError("Installed version metadata is invalid") from error


def _trusted_url(url: str) -> bool:
    try:
        value = urllib.parse.urlsplit(url)
        return (value.scheme == "https" and value.hostname in ALLOWED_HOSTS and value.port in (None, 443)
                and value.username is None and value.password is None and not value.fragment)
    except ValueError:
        return False


class _ReleaseRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, newurl):
        if not _trusted_url(newurl):
            raise UpdateError("Release download redirected outside the trusted HTTPS hosts")
        return super().redirect_request(request, response, code, message, headers, newurl)


def _remaining(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise UpdateError("Update operation timed out. Check your connection and try again")
    return remaining


@contextlib.contextmanager
def _network_deadline(deadline: float):
    """Interrupt DNS, TLS, headers and trickling bodies at the CLI deadline.

    A socket's timeout is an idle timeout and cannot bound a slow stream. This
    module's network entry point runs in the main thread of its own CLI process.
    An existing process alarm is never silently replaced.
    """
    remaining = _remaining(deadline)
    if signal.getitimer(signal.ITIMER_REAL)[0]:
        raise UpdateError("Update networking requires an independent CLI process")
    def expired(_signal, _frame):
        raise UpdateError("Update operation timed out. Check your connection and try again")
    try:
        previous = signal.signal(signal.SIGALRM, expired)
    except ValueError:
        raise UpdateError("Update networking requires the CLI main thread") from None
    try:
        signal.setitimer(signal.ITIMER_REAL, remaining)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def _download(url: str, limit: int, deadline: float, destination: Path | None = None) -> bytes:
    with _network_deadline(deadline):
        return _download_response(url, limit, deadline, destination)


def _download_response(url: str, limit: int, deadline: float, destination: Path | None) -> bytes:
    if not _trusted_url(url):
        raise UpdateError("Release download URL is not trusted")
    request = urllib.request.Request(url, headers={"User-Agent": "Agent-Job-Queue-Updater",
        "Accept": "application/vnd.github+json" if urllib.parse.urlsplit(url).hostname == "api.github.com" else "application/octet-stream",
        "X-GitHub-Api-Version": "2022-11-28"})
    try:
        opener = urllib.request.build_opener(_ReleaseRedirect())
        with opener.open(request, timeout=min(15, _remaining(deadline))) as response:
            if not _trusted_url(response.geturl()):
                raise UpdateError("Release response is not from a trusted HTTPS host")
            declared = response.headers.get("Content-Length")
            if declared is not None and (not declared.isdigit() or int(declared) > limit):
                raise UpdateError("Release response exceeds its size limit")
            with destination.open("xb") if destination is not None else contextlib.nullcontext() as handle:
                data = bytearray()
                size = 0
                while True:
                    _remaining(deadline)
                    chunk = response.read1(min(256 * 1024, limit + 1 - size))
                    _remaining(deadline)
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > limit:
                        raise UpdateError("Release response exceeds its size limit")
                    if handle is not None:
                        handle.write(chunk)
                    else:
                        data.extend(chunk)
                if declared is not None and size != int(declared):
                    raise UpdateError("Release download was incomplete")
                return bytes(data)
    except urllib.error.HTTPError as error:
        if error.code in (403, 429):
            raise UpdateError("GitHub limited the update request. Try again later") from None
        raise UpdateError("GitHub could not provide the release. Try again later") from None
    except TimeoutError as error:
        raise UpdateError("Update operation timed out. Check your connection and try again") from error
    except urllib.error.URLError as error:
        if isinstance(error.reason, TimeoutError):
            raise UpdateError("Update operation timed out. Check your connection and try again") from error
        raise UpdateError("Cannot reach the release service. Check your connection and try again") from error
    except ConnectionError as error:
        raise UpdateError("Cannot reach the release service. Check your connection and try again") from error


def _asset(release: dict, name: str) -> dict:
    matches = [asset for asset in release["assets"] if isinstance(asset, dict) and asset.get("name") == name]
    if len(matches) != 1:
        raise UpdateError("Release assets are missing or ambiguous")
    asset = matches[0]
    expected = f"https://github.com/{REPOSITORY}/releases/download/{release['tag_name']}/{name}"
    if asset.get("browser_download_url") != expected:
        raise UpdateError("Release asset does not belong to the expected published build")
    digest = asset.get("digest")
    if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest)):
        raise UpdateError("Release asset digest is invalid")
    return asset


def _verify_digest(data: bytes, asset: dict):
    if asset.get("digest") is not None and "sha256:" + hashlib.sha256(data).hexdigest() != asset["digest"]:
        raise UpdateError("Release asset checksum does not match GitHub's digest")


def _private_directory(path: Path):
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise UpdateError("Update storage has unsafe ownership or permissions")


@contextlib.contextmanager
def _update_lock(state: Path):
    _private_directory(state)
    descriptor = os.open(state / "updates.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o077 or info.st_nlink != 1):
            raise UpdateError("Update lock has unsafe ownership or permissions")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise UpdateError("Another update is being prepared. Wait for it to finish") from None
        yield
    finally:
        os.close(descriptor)


def _inspect_archive(archive: Path):
    try:
        with zipfile.ZipFile(archive) as package:
            items = package.infolist()
            if not items or len(items) > 5000 or sum(item.file_size for item in items) > MAX_EXPANDED:
                raise UpdateError("Installer archive exceeds its extraction limits")
            names = set()
            for item in items:
                name = item.filename
                path = PurePosixPath(name)
                kind = stat.S_IFMT(item.external_attr >> 16)
                if (path.is_absolute() or ".." in path.parts or "\\" in name or ":" in name
                        or not path.parts or path.parts[0] not in (INSTALLER, "__MACOSX")
                        or kind not in (0, stat.S_IFREG, stat.S_IFDIR) or item.flag_bits & 1
                        or unicodedata.normalize("NFC", str(path)).casefold() in names
                        or item.orig_filename != name or any(part == "." for part in name.split("/"))):
                    raise UpdateError("Installer archive contains unsafe paths or entries")
                names.add(unicodedata.normalize("NFC", str(path)).casefold())
    except (zipfile.BadZipFile, RuntimeError) as error:
        raise UpdateError("Installer archive is invalid") from error


def _command(arguments: list[str], deadline: float):
    try:
        result = subprocess.run(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=min(30, _remaining(deadline)), check=False)
        if result.returncode:
            raise UpdateError("Installer verification or opening failed. Download the installer from the release page")
    except subprocess.TimeoutExpired:
        raise UpdateError("Installer verification timed out. Try again") from None


def _verify_package(directory: Path, manifest: dict, deadline: float) -> Path:
    installer = directory / INSTALLER
    payload = installer / "Contents/Resources/payload"
    app = payload / "Agent Queue.app"
    expected = {key: manifest[key] for key in ("schemaVersion", "repository", "version", "commit", "build")}
    for path in (app / "Contents/Resources/build-info.json", payload / "source/build-info.json"):
        try:
            if validate_build_info(json.loads(_read_owned(path))) != expected:
                raise UpdateError("Installer build identity does not match the published release")
        except (ValueError, FileNotFoundError) as error:
            raise UpdateError("Installer lacks matching build metadata. Use the release page for this older build") from error
    for bundle, identifier in ((installer, "io.went.agent-queue.installer"), (app, "io.went.agent-queue")):
        info = plistlib.loads(_read_owned(bundle / "Contents/Info.plist"))
        if (info.get("CFBundleIdentifier") != identifier or info.get("CFBundleShortVersionString") != manifest["version"]
                or info.get("CFBundleVersion") != str(manifest["build"])):
            raise UpdateError("Installer bundle version does not match the published release")
        _command(["/usr/bin/codesign", "--verify", "--strict", "--deep", str(bundle)], deadline)
    return installer


class UpdateClient:
    def __init__(self, *, root: Path | None = None, app: Path | None = None, state: Path | None = None):
        self.root, self.app = root, app
        self.state = state if state is not None else Path.home() / ".local/state/agent-heavy"

    def _release(self, deadline: float) -> tuple[dict, dict] | None:
        try:
            releases = []
            for page in range(1, MAX_RELEASE_PAGES + 1):
                url = API if page == 1 else f"{API}&page={page}"
                items = json.loads(_download(url, 4 * 1024 ** 2, deadline))
                _remaining(deadline)
                if not isinstance(items, list) or len(items) > 100:
                    raise UpdateError("Release service returned an invalid response")
                releases.extend(items)
                if len(items) < 100:
                    break
            else:
                raise UpdateError("Release search exceeded its limit. Open the project release page to check for updates")
            candidates = [item for item in releases if isinstance(item, dict) and item.get("draft") is False
                          and isinstance(item.get("tag_name"), str) and TAG.fullmatch(item["tag_name"])
                          and isinstance(item.get("assets"), list)
                          and any(isinstance(asset, dict) and asset.get("name") == "agent-job-queue.json"
                                  for asset in item["assets"])]
            if not candidates:
                return None
            number = max(int(TAG.fullmatch(item["tag_name"])[1]) for item in candidates)
            newest = [item for item in candidates if int(TAG.fullmatch(item["tag_name"])[1]) == number]
            if len(newest) != 1:
                raise UpdateError("Published build identity is ambiguous")
            release = newest[0]
            asset = _asset(release, "agent-job-queue.json")
            raw = _download(asset["browser_download_url"], 16_384, deadline)
            _verify_digest(raw, asset)
            manifest = validate_manifest(json.loads(raw))
            if manifest["tag"] != release["tag_name"] or manifest["commit"] != release.get("target_commitish"):
                raise UpdateError("Release identity does not match its manifest")
            _asset(release, manifest["asset"])
            return manifest, release
        except (ValueError, KeyError, TypeError) as error:
            raise UpdateError("Published release metadata is invalid") from error

    def _check(self, deadline: float) -> tuple[dict, tuple[dict, dict] | None]:
        installed = installed_version(self.root, self.app)
        release = self._release(deadline)
        latest = None
        comparison = "unavailable"
        message = "No published build is available"
        if release is not None:
            manifest, _ = release
            latest = {key: manifest[key] for key in ("version", "build", "commit", "tag")}
            latest["url"] = f"https://github.com/{REPOSITORY}/releases/tag/{manifest['tag']}"
            if installed["build"] is None:
                comparison, message = "untracked", "Installed build is untracked. A published release is available"
            elif installed["build"] == manifest["build"]:
                if installed["commit"] != manifest["commit"] or installed["version"] != manifest["version"]:
                    raise UpdateError("Published build number conflicts with the installed release identity")
                comparison, message = "current", "You have the latest published build"
            elif installed["build"] > manifest["build"]:
                comparison, message = "newer", "Installed build is newer than the latest published release"
            else:
                comparison, message = "available", "An update is available"
        return {"installed": installed, "latest": latest, "comparison": comparison,
                "canInstall": comparison in ("available", "untracked"), "message": message}, release

    def check(self) -> dict:
        return self._check(time.monotonic() + 40)[0]

    def open_installer(self, build: int) -> dict:
        if type(build) is not int or not 1 <= build <= 999_999_999:
            raise UpdateError("Select a valid published build before downloading")
        deadline = time.monotonic() + 120
        with _update_lock(self.state):
            status, selected = self._check(deadline)
            if not status["canInstall"] or selected is None or selected[0]["build"] != build:
                raise UpdateError("Update availability changed. Check for updates again before downloading")
            manifest, release = selected
            updates = self.state / "updates"
            _private_directory(updates)
            for path in updates.glob("preparing-*"):
                _private_directory(path)
                shutil.rmtree(path)
            prepared = list(updates.glob("ready-*"))
            for path in list(prepared):
                _private_directory(path)
                prior = validate_manifest(json.loads(_read_owned(path / "release.json")))
                if status["installed"]["build"] is not None and prior["build"] <= status["installed"]["build"]:
                    shutil.rmtree(path)
                    prepared.remove(path)
            if len(prepared) >= 3:
                raise UpdateError("Finish a previously opened installer before preparing another update")
            temporary = Path(tempfile.mkdtemp(prefix="preparing-", dir=updates))
            try:
                archive = temporary / manifest["asset"]
                asset = _asset(release, manifest["asset"])
                _download(asset["browser_download_url"], MAX_ARCHIVE, deadline, archive)
                with archive.open("rb") as handle:
                    digest = hashlib.file_digest(handle, "sha256").hexdigest()
                if digest != manifest["sha256"] or asset.get("digest") not in (None, "sha256:" + digest):
                    raise UpdateError("Installer checksum does not match the published release")
                _inspect_archive(archive)
                expanded = temporary / "expanded"
                expanded.mkdir(mode=0o700)
                _command(["/usr/bin/ditto", "-x", "-k", str(archive), str(expanded)], deadline)
                _verify_package(expanded, manifest, deadline)
                (temporary / "release.json").write_text(json.dumps(manifest))
                ready = temporary.with_name("ready-" + temporary.name.removeprefix("preparing-"))
                temporary.rename(ready)
                temporary = ready
                installer = ready / "expanded" / INSTALLER
                if installed_version(self.root, self.app) != status["installed"]:
                    raise UpdateError("Installed version changed while downloading. Check for updates again")
                _command(["/usr/bin/open", "-n", str(installer)], deadline)
                temporary = None
                return {"installerOpened": True, "build": build,
                        "message": "Installer opened. Choose Install to finish. Queued and running jobs are preserved"}
            finally:
                if temporary is not None:
                    shutil.rmtree(temporary)


def main(arguments: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="agent-heavy", description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    version = commands.add_parser("version", help="show the installed app version without a network request")
    version.add_argument("--json", action="store_true")
    updates = commands.add_parser("updates", help="check published builds or open a verified installer")
    updates.add_argument("operation", choices=("check", "install"))
    updates.add_argument("--build", type=int)
    updates.add_argument("--json", action="store_true")
    args = parser.parse_args(arguments)
    if args.action == "updates" and ((args.operation == "install") != (args.build is not None)):
        parser.error("updates install requires --build; updates check does not accept --build")
    try:
        if args.action == "version":
            result = installed_version()
            display = result["displayVersion"]
        else:
            client = UpdateClient()
            result = client.check() if args.operation == "check" else client.open_installer(args.build)
            display = result["message"]
            if args.operation == "check" and result["latest"]:
                latest = result["latest"]
                display += f"\nInstalled: {result['installed']['displayVersion']}\nLatest: {latest['version']} (build {latest['build']})"
        print(json.dumps(result) if args.json else display)
        return 0
    except (UpdateError, OSError, ValueError, plistlib.InvalidFileException) as error:
        print(f"agent-heavy: {error}", file=sys.stderr)
        return 125
