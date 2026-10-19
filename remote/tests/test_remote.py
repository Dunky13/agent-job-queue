from __future__ import annotations

import http.cookiejar
import json
import os
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import control
import server


class DependencyTests(unittest.TestCase):
    def test_caddy_discovery_searches_both_homebrew_architectures(self):
        with patch.dict(os.environ, {}, clear=True):
            for executable in ("/opt/homebrew/bin/caddy", "/usr/local/bin/caddy"):
                with self.subTest(executable=executable), patch.object(control.shutil, "which", return_value=executable) as find:
                    self.assertEqual(control.caddy_executable(), executable)
                    searched = find.call_args.kwargs["path"].split(os.pathsep)
                    self.assertIn("/opt/homebrew/bin", searched)
                    self.assertIn("/usr/local/bin", searched)

    def test_custom_caddy_is_validated_and_preserved_in_service_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "caddy"
            executable.write_text("#!/bin/sh\nexit 0\n")
            executable.chmod(0o700)
            with patch.dict(os.environ, {"AGENT_REMOTE_CADDY": str(executable)}), patch.object(control.shutil, "which") as find:
                self.assertEqual(control.caddy_executable(), str(executable))
                self.assertEqual(control.control_environment()["AGENT_REMOTE_CADDY"], str(executable))
                for invalid in ("caddy", "", str(executable.parent / "missing")):
                    os.environ["AGENT_REMOTE_CADDY"] = invalid
                    with self.subTest(invalid=invalid), self.assertRaises(control.RemoteError):
                        control.caddy_executable()
                find.assert_not_called()

    def test_missing_caddy_fails_before_enabling_remote_access(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(control.shutil, "which", return_value=None), \
                self.assertRaises(control.RemoteError):
            control.caddy_executable()


class ApiTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ar-api-", dir="/tmp")
        self.root = Path(self.temp.name)
        self.calls = []
        self.log = self.root / "log"
        self.log.write_bytes(b"x" * 70000)

        def runner(arguments):
            self.calls.append(arguments)
            return {"jobs": [{"id": 4, "logPath": str(self.log)}]} if arguments == ["status", "--json"] else {"ok": True}

        self.app = server.create_app({"hosts": ["queue.local"], "port": 8743, "secret_key": "fixture" * 8}, runner, test_mode=True)
        self.client = self.app.test_client()
        self.origin = "https://queue.local:8743"
        response = self.client.get("/api/csrf", base_url=self.origin)
        self.token = response.json["token"]
        self.headers = {"Origin": self.origin, "Referer": self.origin + "/", "X-CSRFToken": self.token}
        self.cookie = response.headers["Set-Cookie"]

    def tearDown(self):
        self.temp.cleanup()

    def test_secure_cookie_and_missing_token_rejection(self):
        for attribute in ("Secure", "HttpOnly", "SameSite=Strict"):
            self.assertIn(attribute, self.cookie)
        result = self.client.post("/api/limit", base_url=self.origin, json={"limit": 2}, headers={"Origin": self.origin})
        self.assertEqual(result.status_code, 400)
        self.assertEqual(self.calls, [])

    def test_host_origin_and_https_referrer_rejected(self):
        for headers in ({**self.headers, "Origin": "https://evil.test"}, {**self.headers, "Referer": "https://evil.test/"}):
            result = self.client.post("/api/limit", base_url=self.origin, json={"limit": 2}, headers=headers)
            self.assertIn(result.status_code, (400, 403))
        self.assertEqual(self.client.get("/api/status", base_url="https://evil.test:8743").status_code, 400)
        self.assertEqual(self.calls, [])

    def test_strict_action_input_and_mutation_whitelist(self):
        for invalid in (0, 9, True, "2"):
            result = self.client.post("/api/limit", base_url=self.origin, json={"limit": invalid}, headers=self.headers)
            self.assertEqual(result.status_code, 400)
        for action in ("next", "last", "start", "cancel"):
            result = self.client.post(f"/api/jobs/4/{action}", base_url=self.origin, json={}, headers=self.headers)
            self.assertEqual(result.json, {"ok": True})
        self.assertEqual(self.calls, [[action, "4"] for action in ("next", "last", "start", "cancel")])
        result = self.client.post("/api/jobs/4/run", base_url=self.origin, json={"command": "forbidden"}, headers=self.headers)
        self.assertEqual(result.status_code, 404)

    def test_logs_are_bounded_and_status_get_is_read_only(self):
        result = self.client.get("/api/jobs/4/log", base_url=self.origin)
        self.assertEqual(len(result.json["output"]), 65536)
        self.assertTrue(result.json["truncated"])
        self.assertEqual(self.calls, [["status", "--json"]])

    def test_static_asset_routes_and_mime_types(self):
        (self.root / "vendor").mkdir()
        for name in ("app.css", "app.js", "vendor/module.js"):
            (self.root / name).write_text("/* fixture */")
        with patch.object(server, "STATIC", self.root):
            for name in ("app.css", "app.js", "vendor/module.js"):
                response = self.client.get("/static/" + name, base_url=self.origin)
                self.assertEqual(response.status_code, 200)
                self.assertIn("css" if name.endswith("css") else "javascript", response.content_type)
                response.close()
        self.assertEqual(self.client.get("/static/../control.py", base_url=self.origin).status_code, 404)

    def test_http_seam_requires_explicit_fixture_injection(self):
        settings = {"hosts": ["queue.local"], "port": 8750, "secret_key": "fixture" * 8, "scheme": "http", "secure_cookie": False}
        with self.assertRaises(ValueError):
            server.create_app(settings)


class PasswordTests(unittest.TestCase):
    def test_rejects_short_nonprintable_and_unsupported_length_before_writing(self):
        paths = control.Paths()
        with patch.object(paths, "prepare", side_effect=AssertionError("Invalid password reached configuration writes")):
            for password in (None, 1234, "", "a", "abc", "word\n", "word\t", "word\x00", "é" * 37):
                with self.subTest(password=password), self.assertRaises(control.RemoteError):
                    control.configure(paths, {"password": password})


class GatewayTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="ar-tls-", dir="/tmp")
        cls.root = Path(cls.temp.name)
        cls.state = cls.root / "state"
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            cls.port = listener.getsockname()[1]
        cls.password = "2468"  # Isolated fixture: prove a simple PIN works through real Caddy auth.
        cls.record = cls.root / "calls.jsonl"
        fixture = cls.root / "controller.py"
        fixture.write_text("import json,sys\nfrom pathlib import Path\n"
                           f"with Path({str(cls.record)!r}).open('a') as f:f.write(json.dumps(sys.argv[1:])+'\\n')\n"
                           "print(json.dumps({'jobs': [], 'queue': {'limit': 1}} if sys.argv[1:] == ['status','--json'] else {'ok':True}))\n")
        cls.env = {**os.environ, "AGENT_REMOTE_TEST_MODE": "1", "AGENT_REMOTE_STATE": str(cls.state),
                   "AGENT_REMOTE_CONFIG": str(cls.root / "config.json"), "AGENT_REMOTE_BIND": "127.0.0.1",
                   "AGENT_REMOTE_TEST_CONTROLLER": str(fixture)}
        result = cls.cli("configure", input_data=json.dumps({"password": cls.password, "port": cls.port}))
        if result.returncode:
            raise AssertionError(result.stdout)
        cls.cli("enable")
        cls.process = subprocess.Popen([sys.executable, str(control.ROOT / "control.py"), "serve"], env=cls.env,
                                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            result = cls.cli("status", "--json")
            cls.info = json.loads(result.stdout)
            if cls.info["running"]:
                break
            if cls.process.poll() is not None:
                raise AssertionError(cls.process.communicate())
            time.sleep(0.1)
        else:
            raise AssertionError("Private HTTPS fixture did not become ready")
        cls.url = cls.info["url"]
        cls.context = ssl.create_default_context(cafile=cls.info["certificatePath"])

    @classmethod
    def cli(cls, *arguments, input_data=None):
        return subprocess.run([sys.executable, str(control.ROOT / "control.py"), *arguments], env=cls.env,
                              input=input_data, capture_output=True, text=True, timeout=25, check=False)

    @classmethod
    def tearDownClass(cls):
        cls.cli("disable")
        cls.process.communicate(timeout=8)
        cls.temp.cleanup()

    def opener(self, password=None):
        handlers = [urllib.request.HTTPSHandler(context=self.context), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())]
        if password:
            manager = urllib.request.HTTPPasswordMgrWithDefaultRealm()
            manager.add_password(None, self.url, "agent", password)
            handlers.append(urllib.request.HTTPBasicAuthHandler(manager))
        return urllib.request.build_opener(*handlers)

    def test_01_tls_rejects_untrusted_ca_and_requires_caddy_credentials(self):
        with self.assertRaises(urllib.error.URLError):
            urllib.request.urlopen(self.url + "api/csrf", timeout=3)
        for password in (None, "wrong-fixture-password"):
            with self.assertRaises(urllib.error.HTTPError) as failure:
                self.opener(password).open(self.url + "api/csrf", timeout=5)
            self.assertEqual(failure.exception.code, 401)

    def test_02_real_https_unix_proxy_cookie_csrf_and_action(self):
        opener = self.opener(self.password)
        response = opener.open(self.url + "api/csrf", timeout=5)
        token = json.load(response)["token"]
        self.assertIn("Secure", response.headers["Set-Cookie"])
        request = urllib.request.Request(self.url + "api/limit", data=b'{"limit":2}', method="POST",
                                         headers={"Content-Type": "application/json", "Origin": self.url.rstrip("/"),
                                                  "Referer": self.url, "X-CSRFToken": token})
        self.assertEqual(json.load(opener.open(request, timeout=5)), {"ok": True})
        self.assertIn('["limit", "2"]', self.record.read_text())
        request.headers["Origin"] = "https://evil.test"
        with self.assertRaises(urllib.error.HTTPError) as failure:
            opener.open(request, timeout=5)
        self.assertEqual(failure.exception.code, 403)

    def test_03_private_config_contains_hash_only_and_service_controls_are_isolated(self):
        content = (self.root / "config.json").read_text()
        self.assertNotIn(self.password, content)
        self.assertIn("$2a$14$", content)
        self.assertEqual((self.root / "config.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.state / "api.sock").stat().st_mode & 0o777, 0o600)
        self.assertIn("admin off", (self.state / "Caddyfile").read_text())
        self.assertIn("skip_install_trust", (self.state / "Caddyfile").read_text())
        self.assertTrue(self.cli("status", "--json").returncode == 0)

    def wait_ready(self):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            if json.loads(self.cli("status", "--json").stdout)["running"]:
                return
            if self.process.poll() is not None:
                self.fail(self.process.communicate())
            time.sleep(0.05)
        self.fail("Restarted HTTPS fixture did not become ready")

    def restart(self, env=None):
        type(self).process = subprocess.Popen([sys.executable, str(control.ROOT / "control.py"), "serve"],
                                             env=env or self.env, stdin=subprocess.DEVNULL,
                                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def port_open(self):
        try:
            with socket.create_connection(("127.0.0.1", self.port), timeout=0.2):
                return True
        except OSError:
            return False

    def test_04_supervisor_sigkill_restart_reconciles_registered_gateway(self):
        old = [json.loads(path.read_text()) for path in (self.state / "children").glob("*.json")]
        self.process.kill()
        self.process.communicate(timeout=3)
        self.assertFalse(json.loads(self.cli("status", "--json").stdout)["running"])
        self.restart()
        self.wait_ready()
        for record in old:
            self.assertNotEqual(control.birth(record["pid"]), record["birth"])
        self.assertEqual(json.load(self.opener(self.password).open(self.url + "api/csrf", timeout=5)).keys(), {"token"})

    def test_05_sigkill_before_gateway_ack_never_publishes_untracked_listener(self):
        self.assertEqual(self.cli("disable").returncode, 0)
        self.process.communicate(timeout=3)
        self.assertFalse(self.port_open())
        self.assertEqual(self.cli("enable").returncode, 0)
        gate = self.root / "hold-caddy-ack"
        self.restart({**self.env, "AGENT_REMOTE_TEST_ACK_GATE": str(gate)})
        deadline = time.monotonic() + 8
        caddy_records = []
        while time.monotonic() < deadline:
            caddy_records = list((self.state / "children").glob("*-caddy.json"))
            if caddy_records:
                break
            time.sleep(0.03)
        self.assertTrue(caddy_records)
        self.assertFalse(json.loads(caddy_records[0].read_text())["ack"])
        self.assertFalse(self.port_open())
        self.process.send_signal(signal.SIGKILL)
        self.process.communicate(timeout=3)
        self.assertEqual(self.cli("disable").returncode, 0)
        gate.touch()
        time.sleep(0.2)
        self.assertFalse(self.port_open())
        self.assertFalse(list((self.state / "children").glob("*.json")))
        self.assertEqual(self.cli("enable").returncode, 0)
        self.restart()
        self.wait_ready()

    def test_06_reconfigure_enabled_service_rotates_caddy_credentials(self):
        new_password = "replacement-fixture-password"
        result = self.cli("configure", input_data=json.dumps({"password": new_password, "port": self.port}))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.process.communicate(timeout=3)
        self.assertFalse(self.port_open())
        self.restart()
        self.wait_ready()
        with self.assertRaises(urllib.error.HTTPError) as failure:
            self.opener(self.password).open(self.url + "api/csrf", timeout=5)
        self.assertEqual(failure.exception.code, 401)
        self.assertIn("token", json.load(self.opener(new_password).open(self.url + "api/csrf", timeout=5)))
        type(self).password = new_password

    def test_07_stop_tool_timeout_still_closes_owned_gateway(self):
        with patch.dict(os.environ, self.env):
            paths = control.Paths()
        with patch.object(control, "service_action", side_effect=subprocess.TimeoutExpired(["launchctl", "bootout"], 5)), \
                self.assertRaisesRegex(control.RemoteError, "shutdown reported errors"):
            control.disable(paths)
        self.process.communicate(timeout=3)
        self.assertFalse(self.port_open())
        self.assertFalse(paths.pid.exists())
        self.assertFalse(list(paths.children.glob("*.json")))
        reopened = json.loads(self.cli("status", "--json").stdout)
        self.assertFalse(reopened["enabled"])
        self.assertIsNotNone(reopened["error"])
        self.assertEqual(self.cli("disable").returncode, 0)
        self.assertIsNone(json.loads(self.cli("status", "--json").stdout)["error"])
        self.assertEqual(self.cli("enable").returncode, 0)
        self.restart()
        self.wait_ready()

    def test_08_incomplete_stop_stays_visible_until_positive_recovery(self):
        self.process.kill()
        self.process.communicate(timeout=3)
        with patch.dict(os.environ, self.env):
            paths = control.Paths()
        with patch.object(control, "service_action", side_effect=control.RemoteError("Injected stop tool failure")), \
                patch.object(control, "reconcile_children", side_effect=control.RemoteError("Injected owned cleanup failure")), \
                self.assertRaisesRegex(control.RemoteError, "owned cleanup failure"):
            control.disable(paths)
        self.assertTrue(self.port_open())
        reopened = json.loads(self.cli("status", "--json").stdout)
        self.assertFalse(reopened["enabled"])
        self.assertFalse(reopened["running"])
        self.assertIn("owned cleanup failure", reopened["error"])
        self.assertEqual(self.cli("disable").returncode, 0)
        self.assertFalse(self.port_open())
        self.assertIsNone(json.loads(self.cli("status", "--json").stdout)["error"])


if __name__ == "__main__":
    unittest.main()
