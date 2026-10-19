"""Release trust, installed identity and inert installer-handoff regressions."""
from __future__ import annotations

import contextlib
import hashlib
import http.client
import io
import json
import os
import plistlib
import signal
import socket
import stat
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
import zipfile
from pathlib import Path
from unittest.mock import patch

import app_updates as updates


class UpdateTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.root = self.directory / "installed"
        self.app = self.directory / "Agent Queue.app"
        self.state = self.directory / "state"
        self.root.mkdir(mode=0o700)
        self.commands = []
        self.manifest = {"schemaVersion": 1, "repository": updates.REPOSITORY, "version": "0.1.0",
                         "build": 20, "commit": "a" * 40, "tag": "build-20-aaaaaaa",
                         "asset": "Agent-Job-Queue-Installer-0.1.0-universal.zip", "sha256": "0" * 64}
        self.raw_archive = self.archive()
        self.manifest["sha256"] = hashlib.sha256(self.raw_archive).hexdigest()
        self.release = {"draft": False, "prerelease": True, "tag_name": self.manifest["tag"],
                        "target_commitish": self.manifest["commit"], "assets": []}
        for name, data in ((self.manifest["asset"], self.raw_archive), ("agent-job-queue.json", self.raw_manifest())):
            self.release["assets"].append({"name": name, "browser_download_url": self.url(name),
                                          "digest": "sha256:" + hashlib.sha256(data).hexdigest()})
        self.client = updates.UpdateClient(root=self.root, app=self.app, state=self.state)

    def url(self, name):
        return f"https://github.com/{updates.REPOSITORY}/releases/download/{self.manifest['tag']}/{name}"

    def info(self, build=10, commit="b" * 40):
        return {key: value for key, value in {**self.manifest, "build": build, "commit": commit}.items()
                if key in ("schemaVersion", "repository", "version", "build", "commit")}

    def bundle(self, directory, identifier, metadata):
        contents = directory / "Contents"
        (contents / "Resources").mkdir(parents=True, exist_ok=True)
        (contents / "Resources/build-info.json").write_text(json.dumps(metadata))
        (contents / "Info.plist").write_bytes(plistlib.dumps({
            "CFBundleIdentifier": identifier, "CFBundleShortVersionString": metadata["version"],
            "CFBundleVersion": str(metadata["build"] or 0)}))

    def install(self, build=10, commit="b" * 40):
        info = self.info(build, commit)
        (self.root / "build-info.json").write_text(json.dumps(info))
        self.bundle(self.app, "io.went.agent-queue", info)

    def archive(self, *, mismatch=False):
        metadata = self.info(20, "a" * 40)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            installer = root / updates.INSTALLER
            self.bundle(installer, "io.went.agent-queue.installer", metadata)
            payload = installer / "Contents/Resources/payload"
            self.bundle(payload / "Agent Queue.app", "io.went.agent-queue", metadata)
            (payload / "source").mkdir()
            (payload / "source/build-info.json").write_text(json.dumps({**metadata, "build": 19} if mismatch else metadata))
            output = io.BytesIO()
            with zipfile.ZipFile(output, "w") as archive:
                for path in root.rglob("*"):
                    if path.is_file():
                        archive.write(path, str(path.relative_to(root)))
            return output.getvalue()

    def raw_manifest(self):
        return json.dumps(self.manifest).encode()

    def download(self, url, limit, deadline, destination=None):
        if url == updates.API:
            data = json.dumps([self.release]).encode()
        elif url == self.url("agent-job-queue.json"):
            data = self.raw_manifest()
        elif url == self.url(self.manifest["asset"]):
            data = self.raw_archive
        else:
            self.fail(f"Unexpected update request: {url}")
        self.assertLessEqual(len(data), limit)
        if destination is not None:
            destination.write_bytes(data)
            return b""
        return data

    def command(self, arguments, deadline):
        self.commands.append(arguments)
        if arguments[0] == "/usr/bin/ditto":
            with zipfile.ZipFile(arguments[-2]) as archive:
                archive.extractall(arguments[-1])

    @contextlib.contextmanager
    def transport(self):
        with patch.object(updates, "_download", side_effect=self.download), \
                patch.object(updates, "_command", side_effect=self.command):
            yield

    def test_not_installed_and_legacy_versions_are_truthful_and_offline(self):
        with patch.object(updates, "_download", side_effect=AssertionError("Version must be offline")):
            self.assertEqual(updates.installed_version(self.root, self.app)["displayVersion"], "Not installed")
            contents = self.app / "Contents"
            contents.mkdir(parents=True)
            (contents / "Info.plist").write_bytes(plistlib.dumps({"CFBundleIdentifier": "io.went.agent-queue",
                "CFBundleShortVersionString": "1.0", "CFBundleVersion": "1"}))
            installed = updates.installed_version(self.root, self.app)
            self.assertEqual(installed["version"], "1.0")
            self.assertIsNone(installed["build"])
            self.assertFalse(installed["tracked"])
            self.assertIn("build 1, legacy", installed["displayVersion"])
        self.assertFalse(self.state.exists())

    def test_installed_metadata_and_bundle_must_match(self):
        self.install()
        value = updates.installed_version(self.root, self.app)
        self.assertEqual((value["build"], value["tracked"], value["displayVersion"]), (10, True, "0.1.0 (build 10)"))
        (self.app / "Contents/Resources/build-info.json").write_text(json.dumps(self.info(11)))
        with self.assertRaisesRegex(updates.UpdateError, "versions differ"):
            updates.installed_version(self.root, self.app)

    def test_untracked_local_build_does_not_invent_a_release_number(self):
        self.install(None, None)
        value = updates.installed_version(self.root, self.app)
        self.assertEqual((value["build"], value["bundleBuild"], value["tracked"]), (None, "0", False))

    def test_incomplete_installation_cannot_report_an_old_completed_controller_version(self):
        self.install()
        pending = self.root / "install-in-progress.json"
        pending.write_text(json.dumps(self.info(20, "a" * 40)))
        with self.assertRaisesRegex(updates.UpdateError, "Installation is incomplete"):
            updates.installed_version(self.root, self.app)
        pending.unlink()
        original = updates._read_owned
        def read(path, limit=16_384):
            value = original(path, limit)
            if path == self.app / "Contents/Info.plist":
                pending.write_text(json.dumps(self.info(20, "a" * 40)))
            return value
        with patch.object(updates, "_read_owned", side_effect=read), \
                self.assertRaisesRegex(updates.UpdateError, "Installation is incomplete"):
            updates.installed_version(self.root, self.app)

    def test_corrupt_missing_symlink_hardlink_and_writable_metadata_fail_closed(self):
        for kind in ("corrupt", "missing-app", "symlink", "hardlink", "writable", "plist"):
            with self.subTest(kind=kind):
                self.install()
                marker = self.root / "build-info.json"
                if kind == "corrupt":
                    marker.write_text("{invalid")
                elif kind == "missing-app":
                    (self.app / "Contents/Resources/build-info.json").unlink()
                elif kind in ("symlink", "hardlink"):
                    other = self.directory / "other"
                    other.unlink(missing_ok=True)
                    marker.rename(other)
                    marker.symlink_to(other) if kind == "symlink" else os.link(other, marker)
                elif kind == "writable":
                    marker.chmod(0o666)
                else:
                    (self.app / "Contents/Info.plist").write_bytes(b"invalid plist")
                with self.assertRaises((updates.UpdateError, OSError)):
                    updates.installed_version(self.root, self.app)
                marker.unlink()

    def test_read_only_check_selects_published_prerelease_and_compares_build_not_semver(self):
        self.install()
        with self.transport():
            result = self.client.check()
        self.assertEqual((result["comparison"], result["canInstall"], result["latest"]["build"]), ("available", True, 20))
        self.assertFalse(self.state.exists())
        self.assertEqual(self.commands, [])

    def test_same_newer_untracked_and_missing_release_states(self):
        for build, commit, comparison, can_install in ((20, "a" * 40, "current", False),
                (21, "c" * 40, "newer", False), (None, None, "untracked", True)):
            with self.subTest(build=build), self.transport():
                self.install(build, commit)
                result = self.client.check()
                self.assertEqual((result["comparison"], result["canInstall"]), (comparison, can_install))
        with patch.object(updates, "_download", return_value=b"[]"):
            self.assertEqual(self.client.check()["comparison"], "unavailable")

    def test_reused_build_with_different_identity_is_rejected(self):
        self.install(20)
        with self.transport(), self.assertRaisesRegex(updates.UpdateError, "conflicts"):
            self.client.check()

    def test_drafts_unknown_tags_are_ignored_but_ambiguous_builds_rejected(self):
        with patch.object(updates, "_download", return_value=json.dumps([
                {**self.release, "draft": True}, {**self.release, "tag_name": "untested"}]).encode()):
            self.assertIsNone(self.client.check()["latest"])
        with patch.object(updates, "_download", return_value=json.dumps([self.release, self.release]).encode()), \
                self.assertRaisesRegex(updates.UpdateError, "ambiguous"):
            self.client.check()

    def test_release_search_paginates_and_never_claims_latest_after_truncation(self):
        original = self.download
        requested = []
        def download(url, limit, deadline, destination=None):
            requested.append(url)
            if url == updates.API:
                return json.dumps([{"tag_name": "unrelated", "draft": False}] * 100).encode()
            if url == updates.API + "&page=2":
                return json.dumps([self.release]).encode()
            return original(url, limit, deadline, destination)
        with patch.object(updates, "_download", side_effect=download):
            self.assertEqual(self.client.check()["latest"]["build"], 20)
        self.assertIn(updates.API + "&page=2", requested)
        with patch.object(updates, "MAX_RELEASE_PAGES", 2), \
                patch.object(updates, "_download", return_value=json.dumps([self.release] * 100).encode()), \
                self.assertRaisesRegex(updates.UpdateError, "search exceeded"):
            self.client.check()

    def test_release_manifest_identity_digest_and_source_are_verified(self):
        for kind in ("commit", "tag", "asset-url", "manifest-digest", "duplicate-asset"):
            with self.subTest(kind=kind):
                original = json.loads(json.dumps(self.release))
                if kind == "commit":
                    self.release["target_commitish"] = "b" * 40
                elif kind == "tag":
                    self.release["tag_name"] = "build-21-aaaaaaa"
                elif kind == "asset-url":
                    self.release["assets"][0]["browser_download_url"] = "https://example.invalid/install.zip"
                elif kind == "manifest-digest":
                    self.release["assets"][1]["digest"] = "sha256:" + "0" * 64
                else:
                    self.release["assets"].append(self.release["assets"][1])
                with self.transport(), self.assertRaises(updates.UpdateError):
                    self.client.check()
                self.release = original

    def test_open_installer_verifies_and_hands_off_without_running_installer_script(self):
        self.install()
        with self.transport():
            result = self.client.open_installer(20)
        self.assertTrue(result["installerOpened"])
        self.assertEqual([command[0] for command in self.commands],
                         ["/usr/bin/ditto", "/usr/bin/codesign", "/usr/bin/codesign", "/usr/bin/open"])
        self.assertEqual(self.commands[-1][1], "-n")
        self.assertEqual(updates.installed_version(self.root, self.app)["build"], 10)
        ready = list((self.state / "updates").glob("ready-*"))
        self.assertEqual(len(ready), 1)
        self.assertTrue((ready[0] / "release.json").is_file())

    def test_stale_requested_build_current_and_newer_installs_never_download(self):
        for installed, requested in ((10, 19), (20, 20), (21, 20)):
            self.install(installed, "a" * 40 if installed == 20 else "b" * 40)
            with self.transport(), self.assertRaisesRegex(updates.UpdateError, "availability changed"):
                self.client.open_installer(requested)
            self.assertEqual(self.commands, [])
        for build in (True, -1, 0, "20", 1_000_000_000):
            with self.assertRaises(updates.UpdateError):
                self.client.open_installer(build)

    def test_changed_installation_during_download_prevents_open_and_cleans_stage(self):
        self.install()
        def command(arguments, deadline):
            self.command(arguments, deadline)
            if arguments[0] == "/usr/bin/codesign":
                self.install(21, "c" * 40)
        with self.transport(), patch.object(updates, "_command", side_effect=command), \
                self.assertRaisesRegex(updates.UpdateError, "version changed"):
            self.client.open_installer(20)
        self.assertNotIn("/usr/bin/open", [item[0] for item in self.commands])
        self.assertEqual(list((self.state / "updates").iterdir()), [])

    def test_archive_checksum_metadata_and_signature_failures_do_not_open(self):
        self.install()
        original = self.raw_archive
        for kind in ("checksum", "identity", "signature"):
            with self.subTest(kind=kind), self.transport():
                self.raw_archive = self.archive(mismatch=True) if kind == "identity" else original
                old_checksum = self.manifest["sha256"]
                if kind == "identity":
                    self.manifest["sha256"] = hashlib.sha256(self.raw_archive).hexdigest()
                    self.release["assets"][0]["digest"] = "sha256:" + self.manifest["sha256"]
                    self.release["assets"][1]["digest"] = "sha256:" + hashlib.sha256(self.raw_manifest()).hexdigest()
                if kind == "checksum":
                    self.raw_archive += b"tampered"
                def command(arguments, deadline):
                    if kind == "signature" and arguments[0] == "/usr/bin/codesign":
                        raise updates.UpdateError("Invalid signature")
                    self.command(arguments, deadline)
                with patch.object(updates, "_command", side_effect=command), self.assertRaises(updates.UpdateError):
                    self.client.open_installer(20)
                self.assertNotIn("/usr/bin/open", [item[0] for item in self.commands])
                self.assertEqual(list((self.state / "updates").iterdir()), [])
                self.manifest["sha256"] = old_checksum
                self.release["assets"][0]["digest"] = "sha256:" + old_checksum
                self.release["assets"][1]["digest"] = "sha256:" + hashlib.sha256(self.raw_manifest()).hexdigest()

    def test_archive_rejects_escape_symlinks_special_files_and_collisions(self):
        cases = [("../outside", stat.S_IFREG), ("/absolute", stat.S_IFREG),
                 (updates.INSTALLER + "/link", stat.S_IFLNK), (updates.INSTALLER + "/pipe", stat.S_IFIFO),
                 (updates.INSTALLER + "/../outside", stat.S_IFREG), (updates.INSTALLER + "/./file", stat.S_IFREG)]
        for name, kind in cases:
            with self.subTest(name=name):
                archive = self.directory / "bad.zip"
                with zipfile.ZipFile(archive, "w") as package:
                    item = zipfile.ZipInfo(name)
                    item.external_attr = (kind | 0o600) << 16
                    package.writestr(item, "data")
                with self.assertRaises(updates.UpdateError):
                    updates._inspect_archive(archive)
        with zipfile.ZipFile(archive, "w") as package:
            package.writestr(updates.INSTALLER + "/FILE", "one")
            package.writestr(updates.INSTALLER + "/file", "two")
        with self.assertRaises(updates.UpdateError):
            updates._inspect_archive(archive)

    def test_private_lock_and_bounded_prepared_installer_storage(self):
        self.install()
        with updates._update_lock(self.state), self.transport(), self.assertRaisesRegex(updates.UpdateError, "Another update"):
            self.client.open_installer(20)
        self.assertEqual((self.state / "updates.lock").stat().st_mode & 0o777, 0o600)
        with self.transport():
            for _ in range(3):
                self.client.open_installer(20)
            with self.assertRaisesRegex(updates.UpdateError, "previously opened"):
                self.client.open_installer(20)
        self.assertEqual(len(list((self.state / "updates").glob("ready-*"))), 3)

    def test_unsafe_storage_and_lock_fail_before_network_or_mutation(self):
        self.state.mkdir(mode=0o700)
        for kind in ("permissions", "symlink", "hardlink"):
            with self.subTest(kind=kind):
                lock = self.state / "updates.lock"
                lock.unlink(missing_ok=True)
                if kind == "permissions":
                    lock.write_text("")
                    lock.chmod(0o666)
                else:
                    other = self.directory / "other-lock"
                    other.touch(mode=0o600, exist_ok=True)
                    lock.symlink_to(other) if kind == "symlink" else os.link(other, lock)
                with self.transport(), self.assertRaises((updates.UpdateError, OSError)):
                    self.client.open_installer(20)
        self.assertFalse((self.state / "updates").exists())

    def test_transport_rejects_unsafe_urls_and_redirects(self):
        for url in ("http://github.com/file", "https://evil.invalid/file", "https://github.com:8080/file",
                    "https://person@github.com/file", "file:///installer", "https://github.com/file#fragment"):
            with self.subTest(url=url), self.assertRaises(updates.UpdateError):
                updates._download(url, 1024, time.monotonic() + 1)
            with self.assertRaises(updates.UpdateError):
                updates._ReleaseRedirect().redirect_request(urllib.request.Request(updates.API), None,
                                                            302, "Found", {}, url)

    def test_transport_bounds_length_truncation_and_network_errors(self):
        class Response(io.BytesIO):
            def __init__(self, data, declared):
                super().__init__(data)
                self.headers = {"Content-Length": declared} if declared is not None else {}
            def geturl(self):
                return updates.API
        for data, declared in ((b"large", "5"), (b"large", None), (b"x", "2"), (b"x", "invalid")):
            with patch.object(urllib.request.OpenerDirector, "open", return_value=Response(data, declared)), \
                    self.assertRaises(updates.UpdateError):
                updates._download(updates.API, 3, time.monotonic() + 10)
        for error in (urllib.error.URLError("offline"), urllib.error.HTTPError(updates.API, 429, "Limited", {}, None)):
            with patch.object(urllib.request.OpenerDirector, "open", side_effect=error), \
                    self.assertRaises(updates.UpdateError):
                updates._download(updates.API, 3, time.monotonic() + 10)
        with self.assertRaisesRegex(updates.UpdateError, "timed out"):
            updates._remaining(time.monotonic() - 1)

    def test_transport_rejects_eof_that_arrives_after_deadline(self):
        clock = [0.0]
        class Response(io.BytesIO):
            headers = {}
            def geturl(self):
                return updates.API
            def read1(self, size):
                clock[0] = 100.0
                return b""
        with patch.object(urllib.request.OpenerDirector, "open", return_value=Response()), \
                patch.object(updates.time, "monotonic", side_effect=lambda: clock[0]), \
                self.assertRaisesRegex(updates.UpdateError, "timed out"):
            updates._download(updates.API, 1024, 40.0)

    def test_hard_deadline_interrupts_trickled_headers_and_body_and_restores_alarm(self):
        for phase in ("headers", "body"):
            with self.subTest(phase=phase):
                incoming, outgoing = socket.socketpair()
                # No competing socket-idle timer: this must prove the absolute
                # alarm interrupts blocking I/O even if the sender is delayed.
                incoming.settimeout(None)
                response = http.client.HTTPResponse(incoming)
                stopped = threading.Event()
                def send():
                    try:
                        data = b"HTTP/1.1 200 OK\r\nContent-Length: 200\r\n\r\n" if phase == "headers" else b"x" * 200
                        if phase == "body":
                            outgoing.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 200\r\n\r\n")
                        for byte in data:
                            if stopped.wait(0.02):
                                break
                            outgoing.sendall(bytes([byte]))
                    except OSError:
                        pass
                    finally:
                        outgoing.close()
                worker = threading.Thread(target=send)
                worker.start()
                def open_response(*_args, **_kwargs):
                    response.begin()
                    response.geturl = lambda: updates.API
                    return response
                original = signal.getsignal(signal.SIGALRM)
                started = time.monotonic()
                try:
                    with patch.object(urllib.request.OpenerDirector, "open", side_effect=open_response), \
                            self.assertRaisesRegex(updates.UpdateError, "timed out"):
                        updates._download(updates.API, 1024, started + 0.12)
                    self.assertLess(time.monotonic() - started, 0.8)
                    self.assertEqual(signal.getsignal(signal.SIGALRM), original)
                    self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))
                finally:
                    stopped.set()
                    incoming.close()
                    outgoing.close()
                    worker.join(timeout=1)
                    response.close()

    def test_socket_timeouts_are_reported_as_timeout_for_direct_and_wrapped_errors(self):
        for error in (TimeoutError("socket idle"), urllib.error.URLError(TimeoutError("socket idle"))):
            with self.subTest(error=error), patch.object(urllib.request.OpenerDirector, "open", side_effect=error), \
                    self.assertRaisesRegex(updates.UpdateError, "timed out"):
                updates._download(updates.API, 1024, time.monotonic() + 1)

    def test_cli_version_is_offline_and_update_install_requires_pinned_build(self):
        import agent_heavy
        self.install()
        installed = updates.installed_version(self.root, self.app)
        with patch.object(updates, "installed_version", return_value=installed), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(agent_heavy.main(["--version", "--json"]), 0)
        self.assertEqual(json.loads(output.getvalue())["build"], 10)
        for arguments in (["updates", "install"], ["updates", "check", "--build", "20"]):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                agent_heavy.main(arguments)


if __name__ == "__main__":
    unittest.main()
