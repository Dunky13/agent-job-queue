"""Bounded real-daemon acceptance probes, isolated from the installed queue."""

from __future__ import annotations

import contextlib
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

MODULE = Path(__file__).resolve().parents[1] / "agent_heavy.py"
TOOL_PATH = os.environ.get("PATH", "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
PUEUE = os.environ.get("AGENT_HEAVY_PUEUE") or shutil.which("pueue", path=TOOL_PATH)
PUEUED = os.environ.get("AGENT_HEAVY_PUEUED") or shutil.which("pueued", path=TOOL_PATH)
PYTHON = sys.executable


def fixture_environment(overrides):
    # This batch is already admitted by any outer queue. Its private daemon must
    # not inherit the outer queue's active marker, socket, or state overrides.
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith(("AGENT_HEAVY_", "PUEUE_"))}
    environment.update(overrides)
    return environment


class EnvironmentTests(unittest.TestCase):
    def test_fixture_drops_outer_queue_overrides_and_retains_its_own(self):
        inherited = {"AGENT_HEAVY_ACTIVE": "parent", "AGENT_HEAVY_STATE": "/parent/state",
                     "PUEUE_CONFIG_PATH": "/parent/config", "FIXTURE_VALUE": "retained"}
        owned = {"AGENT_HEAVY_CONFIG": "/fixture/config", "AGENT_HEAVY_STATE": "/fixture/state",
                 "AGENT_HEAVY_LEGACY_SLOT": "/fixture/slot", "AGENT_HEAVY_PUEUE": "/fixture/pueue"}
        with patch.dict(os.environ, inherited, clear=True):
            environment = fixture_environment(owned)
        self.assertEqual(environment, {"FIXTURE_VALUE": "retained", **owned})


def until(predicate, timeout=12):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if value := predicate():
            return value
        time.sleep(0.08)
    raise AssertionError("Acceptance condition did not become true before deadline")


def alive(pid):
    result = subprocess.run(
        ["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True, check=False
    )
    return bool(result.stdout.strip()) and not result.stdout.strip().startswith("Z")


class Acceptance(unittest.TestCase):
    def setUp(self):
        self.clients = []
        self.assertEqual(self.pueue("parallel", "1").returncode, 0)

    @classmethod
    def setUpClass(cls):
        if not PUEUE or not PUEUED:
            raise RuntimeError("Acceptance tests require installed pueue and pueued executables")
        cls.temp = tempfile.TemporaryDirectory(prefix="ah-test-", dir="/tmp")
        cls.root = Path(cls.temp.name)
        cls.state = cls.root / "state"
        cls.config = cls.root / "config.yml"
        cls.legacy = cls.root / "legacy.slot"
        for p in [cls.state, cls.state / "pueue", cls.state / "run", cls.state / "jobs"]:
            p.mkdir(mode=0o700, parents=True, exist_ok=True)
        cls.env = fixture_environment(dict(
            AGENT_HEAVY_CONFIG=str(cls.config),
            AGENT_HEAVY_STATE=str(cls.state),
            AGENT_HEAVY_LEGACY_SLOT=str(cls.legacy),
            AGENT_HEAVY_PUEUE=PUEUE,
        ))
        callback = shlex.join(
            ["/usr/bin/env"]
            + [f"{key}={cls.env[key]}" for key in [
                "AGENT_HEAVY_CONFIG", "AGENT_HEAVY_STATE", "AGENT_HEAVY_LEGACY_SLOT", "AGENT_HEAVY_PUEUE"
            ]]
            + [PYTHON, str(MODULE), "cleanup"]
        ) + " {{ id }}"
        settings = {
            "client": {"edit_mode": "files"},
            "shared": {
                "pueue_directory": str(cls.state / "pueue"),
                "runtime_directory": str(cls.state / "run"),
                "use_unix_socket": True,
                "unix_socket_permissions": 384,
            },
            "daemon": {
                "callback": callback,
                "pause_group_on_failure": False,
                "pause_all_on_failure": False,
                "shell_command": ["/bin/sh", "-c", "{{ pueue_command_string }}"],
            },
        }
        cls.config.write_text(json.dumps(settings))
        cls.config.chmod(0o600)
        cls.daemon_log = (cls.root / "daemon.log").open("w+")
        cls.daemon = subprocess.Popen(
            [PUEUED, "-c", str(cls.config)],
            stdout=cls.daemon_log, stderr=subprocess.STDOUT,
            env={"HOME": str(Path.home()), "PATH": TOOL_PATH},
            start_new_session=True,
        )
        until(lambda: cls.pueue("status", "--json").returncode == 0)
        cls.pueue("parallel", "1")

    @classmethod
    def pueue(cls, *args):
        return subprocess.run(
            [PUEUE, "-c", str(cls.config), *args],
            capture_output=True, text=True, timeout=8, check=False,
        )

    @classmethod
    def tearDownClass(cls):
        cls.pueue("kill", "--all")
        cls.pueue("shutdown")
        try:
            cls.daemon.wait(timeout=8)
        except subprocess.TimeoutExpired:
            cls.daemon.kill()
            cls.daemon.wait(timeout=3)
        cls.daemon_log.close()
        cls.temp.cleanup()

    def tearDown(self):
        self.pueue("kill", "--all")
        # Reconcile only this isolated fixture's receipts if its daemon crashed.
        for receipt in (self.state / "receipts").glob("*.json"):
            subprocess.run(
                [PYTHON, str(MODULE), "cleanup", receipt.stem], env=self.env,
                capture_output=True, timeout=12, check=False,
            )
        self.pueue("start")
        for client in self.clients:
            try:
                client.communicate(timeout=15)
            except subprocess.TimeoutExpired:
                client.terminate()
                client.communicate(timeout=15)
        until(lambda: not self.legacy.exists())

    def launch(self, code, *args, cwd=None, env=None, label=None):
        invocation = [PYTHON, str(MODULE)]
        invocation += ["run", "--label", label, "--"] if label else ["--"]
        process = subprocess.Popen(
            [*invocation, PYTHON, "-c", code, *map(str, args)],
            cwd=cwd, env=self.env if env is None else env,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        self.clients.append(process)
        return process

    def finish(self, process, code=0, timeout=20):
        out, err = process.communicate(timeout=timeout)
        self.assertEqual(process.returncode, code, (out, err))
        return out, err

    def queued_id(self, process):
        line = process.stderr.readline()
        match = re.search(r"job\s+(\d+)", line)
        self.assertIsNotNone(match, line)
        return match[1]

    def control(self, *args, expected=0):
        result = subprocess.run(
            [PYTHON, str(MODULE), *map(str, args)], capture_output=True,
            text=True, env=self.env, timeout=24, check=False,
        )
        self.assertEqual(result.returncode, expected, result.stderr)
        return result

    def holding(self, name):
        started = self.root / f"{name}.started"
        release = self.root / f"{name}.release"
        code = """import pathlib,sys,time
pathlib.Path(sys.argv[1]).touch()
deadline=time.monotonic()+25
while not pathlib.Path(sys.argv[2]).exists():
 if time.monotonic()>deadline:raise RuntimeError('Fixture release deadline exceeded')
 time.sleep(.04)
"""
        client = self.launch(code, started, release, label=name)
        task_id = self.queued_id(client)
        return client, task_id, started, release

    def task_state(self, task_id):
        return json.loads(self.pueue("status", "--json").stdout)["tasks"][str(task_id)]["status"]

    @contextlib.contextmanager
    def resource_controller(self, *, pressure="normal"):
        """Real daemon and ownership, deterministic resource demand, no host load."""
        host_file = self.root / "resource-host.json"
        host_file.write_text(json.dumps({"memoryPressure": pressure}))
        script = self.root / "resource-controller.py"
        script.write_text('''import json,sys,time
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import agent_heavy,resource_scheduler
class Sampler:
 def __init__(self): self.groups={}
 def host(self):
  return dict(sampledAt=time.time(),totalMemoryBytes=16*1024**3,availableMemoryBytes=12*1024**3,
   cpuCores=12,cpuBusyCores=.2,memoryPressure="normal",swapUsedBytes=0,
   swapInBytesPerSecond=0.,swapOutBytesPerSecond=0.,diskFreeBytes=100*1024**3,
   machine="acceptance-machine",complete=True,**{}) | json.loads(Path(sys.argv[2]).read_text())
 def group(self,pgid,expected_members):
  count=self.groups.get(pgid,0);self.groups[pgid]=count+1
  return dict(memoryBytes=64*1024**2,cpuSeconds=count*.1,cpuCores=.05,
   processCount=len(expected_members),complete=count>0,baseline=count==0)
 def forget_group(self,pgid): self.groups.pop(pgid,None)
resource_scheduler.serve(agent_heavy.Gate(),sampler=Sampler())
''')
        log = (self.root / "resource-controller.log").open("w+")
        controller = subprocess.Popen([PYTHON, str(script), str(MODULE.parent), str(host_file)],
                                      env=self.env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            until(lambda: json.loads(self.control("status", "--json").stdout)["scheduler"]["available"])
            yield host_file, controller
        finally:
            controller.terminate()
            controller.wait(timeout=8)
            for task_id, task in json.loads(self.pueue("status", "--json").stdout)["tasks"].items():
                if "Done" not in task["status"]:
                    self.control("cancel", task_id)
            self.control("scheduling", "fixed")
            log.close()

    def seed_resource_profile(self, commands):
        from job_profiles import ProfileStore, identify_workload
        environment = dict(self.env, AGENT_HEAVY_PROFILE="adaptive-acceptance")
        with ProfileStore(self.state) as store:
            for command in commands:
                # Known overlap requires successful evidence for the actual
                # variant. A different inline program only shares its family;
                # that evidence must not certify a newly changed job as cheap.
                workload = identify_workload(command, str(Path.cwd()), environment)
                for number in range(5):
                    store.record(f"acceptance-{time.time_ns()}-{number}", workload, "acceptance-machine",
                                 {"memoryBytes": 256 * 1024 ** 2, "cpuSeconds": .1,
                                  "cpuCores": .2, "durationSeconds": 1}, complete=True, exit_code=0)
        return environment

    def test_01_cwd_environment_arguments_and_exact_failure(self):
        work = self.root / "working directory"
        work.mkdir()
        env = dict(self.env, AH_PROBE_SECRET="fake-test-value-not-a-credential")
        process = self.launch(
            "import os,sys,json; print(json.dumps([os.getcwd(),sys.argv[1:],os.environ['AH_PROBE_SECRET']])); sys.exit(42)",
            "hello world", "$(echo SHOULD_NOT_RUN)", "a'b", cwd=work, env=env,
        )
        out, _ = self.finish(process, 42)
        value = json.loads(out.splitlines()[-1])
        self.assertEqual(Path(value[0]).resolve(), work.resolve())
        self.assertEqual(value[1], ["hello world", "$(echo SHOULD_NOT_RUN)", "a'b"])
        self.assertEqual(value[2], env["AH_PROBE_SECRET"])
        state_files = list((self.state / "pueue").glob("*json*"))
        for path in state_files:
            self.assertNotIn(env["AH_PROBE_SECRET"].encode(), path.read_bytes())
        until(lambda: not self.legacy.exists())

    def test_02_eight_concurrent_submitters_have_one_active_job(self):
        events = self.root / "events.jsonl"
        code = """import json,os,sys,time
data=bytearray(16*1024*1024)
def event(kind):
 fd=os.open(sys.argv[1],os.O_CREAT|os.O_APPEND|os.O_WRONLY,0o600)
 os.write(fd,(json.dumps([time.monotonic_ns(),sys.argv[2],kind])+"\\n").encode());os.close(fd)
event('start');time.sleep(.18);event('end')
"""
        processes = [self.launch(code, events, n) for n in range(8)]
        for process in processes:
            self.finish(process, timeout=35)
        active = peak = 0
        for _, _, kind in sorted(json.loads(row) for row in events.read_text().splitlines()):
            active += 1 if kind == "start" else -1
            peak = max(peak, active)
            self.assertIn(active, [0, 1])
        self.assertEqual(peak, 1)
        self.assertEqual(active, 0)

    def test_03_cancel_waiting_client_never_runs_its_command(self):
        started = self.root / "blocker.started"
        blocker = self.launch("import pathlib,sys,time;pathlib.Path(sys.argv[1]).touch();time.sleep(2)", started)
        until(started.exists)
        forbidden = self.root / "cancelled-queued-command"
        pending = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", forbidden)
        self.queued_id(pending)
        pending.send_signal(signal.SIGTERM)
        self.finish(pending, 143)
        self.finish(blocker)
        self.assertFalse(forbidden.exists())

    def test_04_cancel_running_job_removes_child_and_grandchild(self):
        pidfile = self.root / "tree.json"
        code = """import json,os,pathlib,subprocess,sys,time
child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'])
pathlib.Path(sys.argv[1]).write_text(json.dumps([os.getpid(),child.pid]))
time.sleep(30)
"""
        running = self.launch(code, pidfile)
        until(pidfile.exists)
        pids = json.loads(pidfile.read_text())
        running.send_signal(signal.SIGTERM)
        self.finish(running, 143)
        until(lambda: all(not alive(pid) for pid in pids))

    def test_05_client_sigkill_does_not_release_a_running_job(self):
        events = self.root / "orphan-events.jsonl"
        code = """import os,sys,time
def event(kind):
 with open(sys.argv[1],'a') as f:f.write(sys.argv[2]+kind+'\\n')
event('-start');time.sleep(float(sys.argv[3]));event('-end')
"""
        first = self.launch(code, events, "A", 1.5)
        until(lambda: events.exists() and "A-start" in events.read_text())
        first.kill()
        first.communicate(timeout=3)
        second = self.launch(code, events, "B", 0.1)
        self.finish(second)
        self.assertEqual(events.read_text().splitlines(), ["A-start", "A-end", "B-start", "B-end"])

    def test_06_legacy_owner_blocks_execution_until_release(self):
        self.legacy.mkdir()
        (self.legacy / "owner").write_text("test-owned legacy job\n")
        marker = self.root / "after-legacy"
        pending = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", marker)
        self.queued_id(pending)
        time.sleep(.6)
        self.assertFalse(marker.exists())
        self.assertEqual((self.legacy / "owner").read_text(), "test-owned legacy job\n")
        (self.legacy / "owner").unlink()
        self.legacy.rmdir()
        self.finish(pending)
        self.assertTrue(marker.exists())

    def test_07_final_output_is_not_truncated(self):
        process = self.launch("print('x'*65536);print('FINAL-BYTES')")
        out, _ = self.finish(process)
        self.assertTrue(out.endswith("FINAL-BYTES\n"))
        self.assertIn("x" * 65536, out)

    def test_08_background_descendant_cannot_outlive_released_slot(self):
        pidfile = self.root / "background.pid"
        code = """import pathlib,subprocess,sys
child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pathlib.Path(sys.argv[1]).write_text(str(child.pid))
"""
        process = self.launch(code, pidfile)
        self.finish(process)
        pid = int(pidfile.read_text())
        until(lambda: not alive(pid))
        until(lambda: not self.legacy.exists())

    def test_09_pipe_input_is_rejected_before_submission(self):
        before = set(json.loads(self.pueue("status", "--json").stdout)["tasks"])
        result = subprocess.run(
            [PYTHON, str(MODULE), "--", PYTHON, "-"],
            input="print('HEREDOC-SHOULD-NOT-RUN')\n", capture_output=True,
            text=True, env=self.env, timeout=5, check=False,
        )
        self.assertEqual(result.returncode, 125, result.stderr)
        self.assertIn("stdin", result.stderr.lower())
        self.assertEqual(result.stdout, "")
        after = set(json.loads(self.pueue("status", "--json").stdout)["tasks"])
        self.assertEqual(before, after)

    def test_10_target_stdin_receives_eof(self):
        process = self.launch("import sys;print(len(sys.stdin.read()))")
        out, _ = self.finish(process, timeout=5)
        self.assertEqual(out, "0\n")

    def test_11_cancel_by_id_reports_killed_to_waiting_caller(self):
        started = self.root / "external-cancel.started"
        process = self.launch("import pathlib,sys,time;pathlib.Path(sys.argv[1]).touch();time.sleep(30)", started)
        task_id = self.queued_id(process)
        until(started.exists)
        result = subprocess.run(
            [PYTHON, str(MODULE), "cancel", task_id], capture_output=True,
            text=True, env=self.env, timeout=15, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.finish(process, 130)
        until(lambda: not self.legacy.exists())

    def test_12_runtime_defaults_preserve_explicit_overrides(self):
        env = dict(self.env)
        for key in ["GOMAXPROCS", "GOMEMLIMIT", "GOGC"]:
            env.pop(key, None)
        code = "import os,json;print(json.dumps([os.environ[k] for k in ['GOMAXPROCS','GOMEMLIMIT','GOGC']]))"
        process = self.launch(code, env=env)
        out, _ = self.finish(process)
        self.assertEqual(json.loads(out), ["2", "512MiB", "50"])
        env.update(GOMAXPROCS="1", GOMEMLIMIT="256MiB", GOGC="40")
        process = self.launch(code, env=env)
        out, _ = self.finish(process)
        self.assertEqual(json.loads(out), ["1", "256MiB", "40"])

    def test_13_priority_reorders_waiters_without_changing_job_ids(self):
        events = self.root / "priority-events.txt"
        code = """import pathlib,sys,time
def event(kind):
 with open(sys.argv[1],'a') as f:f.write(sys.argv[2]+kind+'\\n')
event('-start')
if sys.argv[3]=='hold':
 deadline=time.monotonic()+25
 while not pathlib.Path(sys.argv[4]).exists():
  if time.monotonic()>deadline:raise RuntimeError('Fixture release deadline exceeded')
  time.sleep(.05)
else:time.sleep(float(sys.argv[3]))
event('-end')
"""
        release = self.root / "release-priority-fixture"
        active = self.launch(code, events, "A", "hold", release, label="Active fixture")
        active_id = self.queued_id(active)
        until(lambda: events.exists() and "A-start" in events.read_text())
        b = self.launch(code, events, "B", .1, label="Batch B")
        b_id = self.queued_id(b)
        c = self.launch(code, events, "C", .1, label="Batch C")
        c_id = self.queued_id(c)
        d = self.launch(code, events, "D", .1, label="Batch D")
        d_id = self.queued_id(d)
        for action, task in [("next", d_id), ("last", b_id)]:
            result = subprocess.run(
                [PYTHON, str(MODULE), action, task], capture_output=True,
                text=True, env=self.env, timeout=10, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
        view = subprocess.run(
            [PYTHON, str(MODULE), "status", "--json"], capture_output=True,
            text=True, env=self.env, timeout=8, check=False,
        )
        self.assertEqual(view.returncode, 0, view.stderr)
        snapshot = json.loads(view.stdout)
        waiting = [job for job in snapshot["jobs"] if job["state"] == "queued"]
        self.assertEqual([str(job["id"]) for job in waiting], [d_id, c_id, b_id])
        self.assertEqual([job["title"] for job in waiting], ["Batch D", "Batch C", "Batch B"])
        unchanged = next(job for job in snapshot["jobs"] if str(job["id"]) == active_id)
        self.assertFalse(unchanged["canReorder"])
        self.assertEqual(unchanged["state"], "running")
        rejected = subprocess.run(
            [PYTHON, str(MODULE), "next", active_id], capture_output=True,
            text=True, env=self.env, timeout=8, check=False,
        )
        self.assertEqual(rejected.returncode, 125)
        release.touch()
        for client in [active, b, c, d]:
            self.finish(client)
        self.assertEqual(events.read_text().splitlines(),
                         ["A-start", "A-end", "D-start", "D-end", "C-start", "C-end", "B-start", "B-end"])

    def test_14_snapshot_has_metadata_without_caller_secret(self):
        process = self.launch("print('VISIBLE')", label="Readable validation batch")
        task_id = self.queued_id(process)
        self.finish(process)
        result = subprocess.run(
            [PYTHON, str(MODULE), "status", "--json"], capture_output=True,
            text=True, env=self.env, timeout=8, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("fake-test-value-not-a-credential", result.stdout)
        snapshot = json.loads(result.stdout)
        job = next(job for job in snapshot["jobs"] if str(job["id"]) == task_id)
        self.assertEqual(job["title"], "Readable validation batch")
        self.assertIn("project", job)
        self.assertIn("branch", job)
        self.assertIn("logPath", job)
        self.assertEqual(job["exitCode"], 0)

    def test_15_limit_two_overlaps_and_lowering_waits_for_active_jobs(self):
        self.control("limit", 2)
        a, a_id, a_started, a_release = self.holding("limit-A")
        b, b_id, b_started, b_release = self.holding("limit-B")
        until(lambda: a_started.exists() and b_started.exists())
        c, c_id, c_started, c_release = self.holding("limit-C")
        self.assertIn("Queued", self.task_state(c_id))
        self.control("limit", 1)
        for task_id in [a_id, b_id]:
            self.assertIn("Running", self.task_state(task_id))
        saved = json.loads((self.state / "pueue/state.json").read_text())
        self.assertEqual(saved["groups"]["default"]["parallel_tasks"], 1)
        self.assertEqual(set(saved["groups"]), {"default"})
        a_release.touch()
        self.finish(a)
        self.assertIn("Running", self.task_state(b_id))
        self.assertIn("Queued", self.task_state(c_id))
        self.assertFalse(c_started.exists())
        b_release.touch()
        self.finish(b)
        until(c_started.exists)
        c_release.touch()
        self.finish(c)

    def test_16_force_start_overlaps_without_changing_limit_and_cancel_isolated(self):
        a, _, a_started, a_release = self.holding("override-A")
        until(a_started.exists)
        b, b_id, b_started, _ = self.holding("override-B")
        c, c_id, c_started, c_release = self.holding("override-C")
        self.control("start", b_id)
        until(b_started.exists)
        snapshot = json.loads(self.control("status", "--json").stdout)
        self.assertEqual(snapshot["queue"]["limit"], 1)
        self.assertEqual(snapshot["queue"]["running"], 2)
        self.assertFalse(snapshot["queue"]["legacyOccupied"])
        self.assertFalse(c_started.exists())
        self.control("cancel", b_id)
        self.finish(b, 130)
        self.assertIsNone(a.poll())
        self.assertTrue(self.legacy.exists())
        self.assertIn("Queued", self.task_state(c_id))
        self.assertFalse(c_started.exists())
        a_release.touch()
        self.finish(a)
        until(c_started.exists)
        c_release.touch()
        self.finish(c)
        until(lambda: not self.legacy.exists())

    def test_17_limit_change_preserves_paused_job_and_paused_queue(self):
        a, a_id, a_started, a_release = self.holding("paused-A")
        until(a_started.exists)
        self.assertEqual(self.pueue("pause", a_id).returncode, 0)
        until(lambda: "Paused" in self.task_state(a_id))
        self.assertEqual(self.pueue("pause", "--wait", "--group", "default").returncode, 0)
        self.control("limit", 2)
        snapshot = json.loads(self.pueue("status", "--json").stdout)
        self.assertEqual(snapshot["groups"]["default"]["status"], "Paused")
        self.assertIn("Paused", snapshot["tasks"][a_id]["status"])
        saved = json.loads((self.state / "pueue/state.json").read_text())
        self.assertEqual(saved["groups"]["default"]["parallel_tasks"], 2)
        self.assertIn("Paused", saved["tasks"][a_id]["status"])
        self.assertEqual(saved["groups"]["default"]["status"], "Paused")
        self.assertEqual(self.pueue("start", a_id).returncode, 0)
        a_release.touch()
        self.finish(a)
        self.assertEqual(self.pueue("start", "--group", "default").returncode, 0)
        self.control("limit", 1)

    def test_18_parallel_helpers_respect_external_legacy_owner(self):
        self.control("limit", 2)
        self.legacy.mkdir()
        (self.legacy / "owner").write_text("isolated external legacy fixture\n")
        a, _, a_started, a_release = self.holding("legacy-parallel-A")
        b, _, b_started, b_release = self.holding("legacy-parallel-B")
        time.sleep(.7)
        self.assertFalse(a_started.exists())
        self.assertFalse(b_started.exists())
        snapshot = json.loads(self.control("status", "--json").stdout)
        self.assertTrue(snapshot["queue"]["legacyOccupied"])
        self.assertEqual((self.legacy / "owner").read_text(), "isolated external legacy fixture\n")
        (self.legacy / "owner").unlink()
        self.legacy.rmdir()
        until(lambda: a_started.exists() and b_started.exists())
        a_release.touch()
        self.finish(a)
        self.assertTrue(self.legacy.exists())
        b_release.touch()
        self.finish(b)
        until(lambda: not self.legacy.exists())
        self.control("limit", 1)

    def test_19_following_target_waits_for_background_child_teardown(self):
        pidfile = self.root / "teardown-child.pid"
        ready = self.root / "teardown-child.ready"
        release = self.root / "teardown-parent.release"
        child_code = """import pathlib,signal,sys,time
def stop(*_):time.sleep(.4);sys.exit(0)
signal.signal(signal.SIGTERM,stop)
pathlib.Path(sys.argv[1]).touch()
time.sleep(25)
"""
        parent_code = """import pathlib,subprocess,sys,time
child=subprocess.Popen([sys.executable,'-c',sys.argv[1],sys.argv[3]],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pathlib.Path(sys.argv[2]).write_text(str(child.pid))
deadline=time.monotonic()+25
while not pathlib.Path(sys.argv[4]).exists():
 if time.monotonic()>deadline:raise RuntimeError('Fixture release deadline exceeded')
 time.sleep(.04)
"""
        first = self.launch(parent_code, child_code, pidfile, ready, release)
        until(ready.exists)
        next_code = """import pathlib,subprocess,sys
pid=pathlib.Path(sys.argv[1]).read_text().strip()
state=subprocess.run(['ps','-o','stat=','-p',pid],capture_output=True,text=True,check=False).stdout.strip()
if state and not state.startswith('Z'):raise RuntimeError('Previous target child still active')
print('PREVIOUS-CHILD-GONE')
"""
        second = self.launch(next_code, pidfile)
        second_id = self.queued_id(second)
        self.assertIn("Queued", self.task_state(second_id))
        release.touch()
        self.finish(first)
        out, _ = self.finish(second)
        self.assertIn("PREVIOUS-CHILD-GONE", out)

    def test_20_invalid_limit_is_rejected_without_changing_queue(self):
        for limit in [0, -1, 9]:
            self.control("limit", limit, expected=125)
        snapshot = json.loads(self.control("status", "--json").stdout)
        self.assertEqual(snapshot["queue"]["limit"], 1)

    def test_21_adaptive_admission_overlaps_known_jobs_and_respects_lowered_cap(self):
        started = [self.root / f"adaptive-{index}.started" for index in range(3)]
        release = [self.root / f"adaptive-{index}.release" for index in range(3)]
        code = "import pathlib,sys,time;pathlib.Path(sys.argv[1]).touch()\nwhile not pathlib.Path(sys.argv[2]).exists():time.sleep(.05)"
        environment = self.seed_resource_profile([
            [PYTHON, "-c", code, str(start), str(end)] for start, end in zip(started, release, strict=True)
        ])
        with self.resource_controller():
            self.control("limit", 2)
            self.control("scheduling", "adaptive")
            first = self.launch(code, started[0], release[0], env=environment)
            self.queued_id(first)
            until(started[0].exists)
            second = self.launch(code, started[1], release[1], env=environment)
            self.queued_id(second)
            until(started[1].exists)
            self.control("limit", 1)
            third = self.launch(code, started[2], release[2], env=environment)
            third_id = self.queued_id(third)
            until(lambda: "Stashed" in self.task_state(third_id))
            release[0].touch()
            self.finish(first)
            self.assertFalse(started[2].exists())
            release[1].touch()
            self.finish(second)
            until(started[2].exists)
            release[2].touch()
            self.finish(third)

    def test_22_pressure_holds_jobs_and_recovers_without_changing_limit(self):
        with self.resource_controller(pressure="warning") as (host_file, _controller):
            self.control("scheduling", "adaptive")
            started = self.root / "adaptive-pressure.started"
            client = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", started)
            task_id = self.queued_id(client)
            until(lambda: "Stashed" in self.task_state(task_id))
            status = json.loads(self.control("status", "--json").stdout)
            self.assertEqual(status["queue"]["limit"], 1)
            self.assertFalse(started.exists())
            temporary = host_file.with_suffix(".tmp")
            temporary.write_text(json.dumps({"memoryPressure": "normal"}))
            temporary.replace(host_file)
            until(started.exists, timeout=25)
            self.finish(client)

    def test_23_controller_loss_holds_new_jobs_and_manual_start_is_explicit(self):
        with self.resource_controller() as (_host_file, controller):
            self.control("scheduling", "adaptive")
            controller.terminate()
            controller.wait(timeout=8)
            started = self.root / "adaptive-manual.started"
            client = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", started)
            task_id = self.queued_id(client)
            self.assertIn("Stashed", self.task_state(task_id))
            self.assertFalse(started.exists())
            self.control("start", task_id)
            until(started.exists)
            self.finish(client)

    def test_24_shadow_never_controls_and_adaptive_migrates_existing_jobs(self):
        with self.resource_controller(pressure="critical"):
            self.control("scheduling", "shadow")
            until(lambda: json.loads(self.control("status", "--json").stdout)["scheduler"]["available"])
            client = self.launch("print('SHADOW')")
            out, _err = self.finish(client)
            self.assertEqual(out, "SHADOW\n")
            blocker, _blocker_id, blocker_started, blocker_release = self.holding("migration-blocker")
            until(blocker_started.exists)
            started = self.root / "adaptive-migration.started"
            client = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", started)
            task_id = self.queued_id(client)
            self.pueue("pause", "--wait")
            receipt = json.loads((self.state / "receipts" / f"{task_id}.json").read_text())
            workload_path = self.state / "workloads" / f"{receipt['token']}.json"
            workload_path.unlink()
            self.control("scheduling", "adaptive")
            until(workload_path.exists)
            self.assertIn("Stashed", self.task_state(task_id))
            self.assertFalse(started.exists())
            # Returning to fixed scheduling releases managed jobs through Pueue,
            # while preserving its deliberately paused queue state.
            self.control("scheduling", "fixed")
            self.assertIn("Queued", self.task_state(task_id))
            blocker_release.touch()
            self.finish(blocker)
            self.pueue("start")
            until(started.exists)
            self.finish(client)

    def test_25_profile_corrections_hold_waiting_work_and_preserve_active_job(self):
        from job_profiles import identify_workload
        started = [self.root / f"learning-{index}.started" for index in range(2)]
        release = [self.root / f"learning-{index}.release" for index in range(2)]
        code = "import pathlib,sys,time;pathlib.Path(sys.argv[1]).touch()\nwhile not pathlib.Path(sys.argv[2]).exists():time.sleep(.05)"
        commands = [[PYTHON, "-c", code, str(start), str(end)] for start, end in zip(started, release, strict=True)]
        environment = self.seed_resource_profile(commands)
        identity = identify_workload(commands[0], str(Path.cwd()), environment)["family"]

        def edit(revision, minimum):
            result = subprocess.run(
                [PYTHON, str(MODULE), "profiles", "edit", identity, "--json"], env=self.env,
                input=json.dumps({"revision": revision, "label": "Operator-corrected tests",
                                  "memoryFloorBytes": minimum, "cpuFloorCores": None}),
                capture_output=True, text=True, timeout=24, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            return json.loads(result.stdout)

        with self.resource_controller():
            self.control("scheduling", "adaptive")
            first = self.launch(code, started[0], release[0], env=environment)
            self.queued_id(first)
            until(started[0].exists)
            second = self.launch(code, started[1], release[1], env=environment)
            second_id = self.queued_id(second)
            until(lambda: "Stashed" in self.task_state(second_id))
            catalog = json.loads(self.control("profiles", "list", "--json").stdout)
            self.assertIn(identity, {profile["id"] for profile in catalog["profiles"]})
            detail = json.loads(self.control("profiles", "show", identity, "--json").stdout)
            saved = edit(detail["revision"], 20 * 1024 ** 3)
            self.assertEqual(saved["profile"]["label"], "Operator-corrected tests")
            self.control("limit", 2)
            def applied_wait():
                status = json.loads(self.control("status", "--json").stdout)
                waiting = next(job for job in status["jobs"] if job["id"] == int(second_id))
                decision = waiting.get("resources", {}).get("decision")
                applied = json.loads(self.control("profiles", "show", identity, "--json").stdout)
                return applied["controllerAppliedRevision"] == saved["revision"] and decision and "exceeds" in decision["reason"]

            until(applied_wait)
            self.assertFalse(started[1].exists())
            self.assertIsNone(first.poll())
            cleared = edit(saved["revision"], None)
            self.assertIsNone(cleared["profile"]["memoryFloorBytes"])
            until(started[1].exists)
            self.assertIsNone(first.poll())
            for path in release:
                path.touch()
            self.finish(first)
            self.finish(second)

    def test_99_daemon_crash_pauses_queue_until_owned_work_is_reconciled(self):
        started = self.root / "crash.started"
        first = self.launch("import pathlib,sys,time;pathlib.Path(sys.argv[1]).touch();time.sleep(30)", started)
        first_id = self.queued_id(first)
        until(started.exists)
        forbidden = self.root / "must-not-start-after-crash"
        second = self.launch("import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", forbidden)
        second_id = self.queued_id(second)
        until(lambda: "Queued" in json.loads(self.pueue("status", "--json").stdout)["tasks"][second_id]["status"])
        def durable_queue():
            try:
                saved = json.loads((self.state / "pueue/state.json").read_text())
            except (FileNotFoundError, ValueError):
                return False
            return "Queued" in saved.get("tasks", {}).get(second_id, {}).get("status", {})

        until(durable_queue)
        type(self).daemon.kill()
        type(self).daemon.wait(timeout=5)
        type(self).daemon = subprocess.Popen(
            [PUEUED, "-c", str(self.config)],
            stdout=self.daemon_log, stderr=subprocess.STDOUT,
            env={"HOME": str(Path.home()), "PATH": TOOL_PATH},
            start_new_session=True,
        )
        until(lambda: self.pueue("status", "--json").returncode == 0)
        snapshot = json.loads(self.pueue("status", "--json").stdout)
        self.assertEqual(snapshot["groups"]["default"]["status"], "Paused")
        self.assertFalse(forbidden.exists())
        # Recovery applies only to this fixture's positively identified process group.
        cleanup = subprocess.run(
            [PYTHON, str(MODULE), "cleanup", first_id], capture_output=True,
            text=True, env=self.env, timeout=12, check=False,
        )
        self.assertEqual(cleanup.returncode, 0, cleanup.stderr)
        cancel = subprocess.run(
            [PYTHON, str(MODULE), "cancel", second_id], capture_output=True,
            text=True, env=self.env, timeout=15, check=False,
        )
        self.assertEqual(cancel.returncode, 0, cancel.stderr)
        first.communicate(timeout=15)
        second.communicate(timeout=15)
        self.assertFalse(forbidden.exists())
        self.assertEqual(self.pueue("start").returncode, 0)
        recovered = self.launch("print('RECOVERED')")
        out, _ = self.finish(recovered)
        self.assertEqual(out, "RECOVERED\n")


if __name__ == "__main__":
    unittest.main(verbosity=2)
