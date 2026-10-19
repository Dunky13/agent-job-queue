import importlib.util
import json
import os
import signal
import tempfile
import unittest
from pathlib import Path
from unittest.mock import call, patch

spec = importlib.util.spec_from_file_location("agent_heavy", Path(__file__).parents[1] / "agent_heavy.py")
gate_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate_module)


class GateTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        inherited = {key: value for key, value in os.environ.items()
                     if not key.startswith(("AGENT_HEAVY_", "PUEUE_"))}
        self.environment = patch.dict(os.environ, {**inherited,
            "AGENT_HEAVY_STATE": str(self.root / "state"),
            "AGENT_HEAVY_CONFIG": str(self.root / "config.yml"),
            "AGENT_HEAVY_LEGACY_SLOT": str(self.root / "slot"),
        }, clear=True)
        self.environment.start()
        self.gate = gate_module.Gate()
        self.gate.prepare()

    def tearDown(self):
        self.environment.stop()
        self.directory.cleanup()

    def test_default_lease_is_private_and_project_independent(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(Path, "home", return_value=self.root):
            gate = gate_module.Gate()
        self.assertEqual(gate.slot, self.root / ".local/state/agent-heavy/execution.slot")
        self.assertIn("/usr/local/bin", gate.control_env["PATH"].split(os.pathsep))

    def test_upgrade_retains_private_migrated_lease_path(self):
        retained = self.root / "prior-installation.slot"
        gate_module.write_private(self.gate.state / "legacy-slot.json", {"slot": str(retained)})
        os.environ.pop("AGENT_HEAVY_LEGACY_SLOT")
        migrated = gate_module.Gate()
        self.assertEqual(migrated.slot, retained)

    def test_malformed_or_unknown_migrated_lease_fails_closed(self):
        path = self.gate.state / "legacy-slot.json"
        os.environ.pop("AGENT_HEAVY_LEGACY_SLOT")
        for value in ({}, {"slot": "/fixture", "extra": True}, {"slot": "relative"},
                      {"slot": None}, {"slot": ""}, {"slot": "/fixture\x00"}):
            gate_module.write_private(path, value)
            with self.subTest(value=value), self.assertRaises(gate_module.GateError):
                gate_module.Gate()
        path.write_text("{invalid")
        with self.assertRaises(gate_module.GateError):
            gate_module.Gate()

    def test_migrated_lease_symlink_fails_closed_including_dangling_link(self):
        target = self.root / "target.json"
        gate_module.write_private(target, {"slot": str(self.root / "prior.slot")})
        path = self.gate.state / "legacy-slot.json"
        path.symlink_to(target)
        os.environ.pop("AGENT_HEAVY_LEGACY_SLOT")
        with self.assertRaises(gate_module.GateError):
            gate_module.Gate()
        target.unlink()
        with self.assertRaises(gate_module.GateError):
            gate_module.Gate()

    def test_migrated_lease_insecure_file_permissions_fail_closed(self):
        path = self.gate.state / "legacy-slot.json"
        gate_module.write_private(path, {"slot": str(self.root / "prior.slot")})
        path.chmod(0o644)
        os.environ.pop("AGENT_HEAVY_LEGACY_SLOT")
        with self.assertRaises(gate_module.GateError):
            gate_module.Gate()

    def test_explicit_legacy_override_wins_over_invalid_migration_file(self):
        (self.gate.state / "legacy-slot.json").write_text("invalid configuration")
        self.assertEqual(gate_module.Gate().slot, self.root / "slot")

    def test_pueue_discovery_includes_both_homebrew_architectures(self):
        self.gate.control_env["PATH"] = "/usr/bin:/bin"
        self.gate.control_env.pop("AGENT_HEAVY_PUEUE", None)
        for executable in ("/opt/homebrew/bin/pueue", "/usr/local/bin/pueue"):
            with self.subTest(executable=executable), patch.object(gate_module.shutil, "which", return_value=executable) as find:
                self.assertEqual(self.gate.pueue_executable(), executable)
                searched = find.call_args.kwargs["path"].split(os.pathsep)
                self.assertIn("/opt/homebrew/bin", searched)
                self.assertIn("/usr/local/bin", searched)

    def test_pueue_override_is_absolute_executable_and_never_falls_back(self):
        executable = self.root / "custom-pueue"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o700)
        self.gate.control_env["AGENT_HEAVY_PUEUE"] = str(executable)
        with patch.object(gate_module.shutil, "which") as find:
            self.assertEqual(self.gate.pueue_executable(), str(executable))
            for invalid in ("pueue", "", str(self.root / "missing-pueue")):
                self.gate.control_env["AGENT_HEAVY_PUEUE"] = invalid
                with self.subTest(invalid=invalid), self.assertRaises(gate_module.GateError):
                    self.gate.pueue_executable()
            find.assert_not_called()

    def test_missing_pueue_fails_without_running_unqueued_command(self):
        self.gate.control_env.pop("AGENT_HEAVY_PUEUE", None)
        with patch.object(gate_module.shutil, "which", return_value=None), \
                patch.object(gate_module.subprocess, "run") as run, self.assertRaises(gate_module.GateError):
            self.gate.pueue("status")
        run.assert_not_called()

    def marker(self, task_id=3, pgid=987654):
        self.gate.slot.mkdir()
        initial = [{"pid": 12345, "birth": "Wed Oct 7 23:57:18 2026"}]
        marker = {"task_id": task_id, "token": "a" * 32, "pgid": pgid, "pid": 12345,
                  "boot_uuid": "fixture-boot", "initial_members": initial, "members": initial}
        gate_module.write_private(self.gate.slot / "agent-heavy.json", marker)
        (self.gate.slot / "owner").write_text("agent-heavy\n")
        gate_module.write_private(self.gate.receipt_path(task_id), marker)
        return marker

    def test_result_codes(self):
        for result, code in (("Success", 0), ({"Failed": 7}, 7), ("Killed", 137), ({"FailedToSpawn": "missing"}, 125)):
            self.assertEqual(gate_module.result_code(result), code)
        with self.assertRaises(gate_module.GateError):
            gate_module.result_code({"Failed": True})

    def test_payload_privacy_and_symlink_rejection(self):
        path = self.gate.jobs / "private.json"
        gate_module.write_private(path, {"secret": "fixture"})
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        linked = self.gate.jobs / "linked.json"
        linked.symlink_to(path)
        with self.assertRaises(OSError):
            gate_module.read_private(linked)

    def test_unknown_legacy_owner_is_not_removed_or_signalled(self):
        self.gate.slot.mkdir()
        (self.gate.slot / "owner").write_text("legacy-worker")
        with patch.object(os, "killpg") as kill:
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()
        self.assertEqual((self.gate.slot / "owner").read_text(), "legacy-worker")

    def test_mismatched_receipt_is_not_signalled(self):
        marker = self.marker()
        gate_module.write_private(self.gate.receipt_path(3), {**marker, "token": "b" * 32})
        with patch.object(os, "killpg") as kill, self.assertRaises(gate_module.GateError):
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()
        self.assertTrue(self.gate.slot.exists())

    def test_own_group_is_never_signalled(self):
        self.marker(pgid=os.getpgrp())
        with patch.object(os, "killpg") as kill, self.assertRaises(gate_module.GateError):
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()

    def test_finished_owned_slot_removed(self):
        self.marker()
        with patch.object(gate_module, "group_exists", return_value=False):
            gate_module.cleanup(self.gate, 3)
        self.assertFalse(self.gate.slot.exists())
        self.assertFalse(self.gate.receipt_path(3).exists())

    def test_unknown_extra_metadata_keeps_owned_slot(self):
        self.marker()
        (self.gate.slot / "legacy-extra").write_text("retained")
        with patch.object(gate_module, "group_exists", return_value=False), self.assertRaises(gate_module.GateError):
            gate_module.cleanup(self.gate, 3)
        self.assertTrue((self.gate.slot / "owner").exists())
        self.assertTrue((self.gate.slot / "legacy-extra").exists())

    def test_cleanup_of_waiting_job_preserves_legacy_owner(self):
        self.gate.slot.mkdir()
        (self.gate.slot / "owner").write_text("legacy-worker")
        token = "a" * 32
        gate_module.write_private(self.gate.receipt_path(3), {"task_id": 3, "token": token})
        gate_module.write_private(self.gate.payload_path(token), {"fixture": "private"})
        with patch.object(os, "killpg") as kill:
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()
        self.assertFalse(self.gate.payload_path(token).exists())
        self.assertFalse(self.gate.receipt_path(3).exists())
        self.assertEqual((self.gate.slot / "owner").read_text(), "legacy-worker")

    def test_execute_preserves_literal_argv_cwd_environment_and_soft_limits(self):
        token = "a" * 32
        command = ["printf", "a b", "$(do-not-expand)", "it's literal"]
        payload = {"token": token, "task_id": 4, "argv": command, "cwd": str(self.root), "env": {"FIXTURE": "value", "GOMAXPROCS": "1"}}
        gate_module.write_private(self.gate.payload_path(token), payload)
        gate_module.write_private(self.gate.receipt_path(4), {"task_id": 4, "token": token})
        identities = [{"pid": os.getpid(), "birth": "helper-birth"}]
        with patch.object(gate_module.subprocess, "run") as spawn, \
                patch.object(gate_module, "boot_identity", return_value="fixture-boot"), \
                patch.object(gate_module, "process_group_members", return_value=identities), \
                patch.object(gate_module, "lease_capacity", return_value=1):
            spawn.return_value.returncode = 7
            self.assertEqual(gate_module.execute(self.gate, token), 7)
        args, kwargs = spawn.call_args
        self.assertEqual(args[0], command)
        self.assertEqual(kwargs["cwd"], str(self.root))
        self.assertEqual(kwargs["stdin"], gate_module.subprocess.DEVNULL)
        self.assertEqual(kwargs["env"]["FIXTURE"], "value")
        self.assertEqual(kwargs["env"]["GOMAXPROCS"], "1")
        self.assertEqual(kwargs["env"]["GOMEMLIMIT"], "512MiB")
        self.assertFalse(self.gate.payload_path(token).exists())
        self.assertTrue(self.gate.slot.exists(), "callback owns release")

    def test_nested_wrapper_fails_before_submission(self):
        with patch.dict(os.environ, {"AGENT_HEAVY_ACTIVE": "fixture"}), patch.object(self.gate, "pueue") as call, self.assertRaises(gate_module.GateError):
            gate_module.run(self.gate, ["true"])
        call.assert_not_called()

    def task(self, task_id, state="Queued", priority=0, token=None):
        details = {"start": "2026-10-18T20:00:00Z"} if state in ("Running", "Done") else {}
        if state == "Done":
            details.update({"end": "2026-10-18T20:01:00Z", "result": "Success"})
        return {"id": task_id, "status": {state: details}, "priority": priority, "path": str(self.root),
                "created_at": "2026-10-18T19:59:00Z", "label": None,
                "command": f"exec python helper execute {token or format(task_id, '032x')}"}

    def test_safe_summaries_never_expose_script_or_arguments(self):
        cases = [(["bash", "-lc", "private-script-body"], "Shell batch"),
                 (["python3", "-c", "private-script-body"], "Python batch"),
                 (["go", "test", "private-package-arg"], "Go tests"),
                 (["node", "--run", "typecheck", "private-arg"], "Typecheck"),
                 (["pnpm", "exec", "playwright", "test"], "Browser tests")]
        for argv, expected in cases:
            with self.subTest(argv=argv):
                title, executable = gate_module.safe_summary(argv)
                self.assertEqual(title, expected)
                self.assertNotIn("private", title + executable)

    def test_status_order_and_payload_metadata_remain_safe_and_cached(self):
        token = "f" * 32
        queued = self.task(4, token=token)
        gate_module.write_private(self.gate.payload_path(token), {"argv": ["bash", "-lc", "private-script-body"],
                                 "cwd": str(self.root), "env": {"SECRET": "private-env-value"}})
        snapshot = {"groups": {"default": {"status": "Running", "parallel_tasks": 1}},
                    "tasks": {"4": queued, "3": self.task(3, priority=5), "2": self.task(2, "Running"), "1": self.task(1, "Done")}}
        description = {"project": "fixture-project", "branch": "fixture-branch", "directory": str(self.root)}
        with patch.object(self.gate, "snapshot", return_value=snapshot), \
                patch.object(gate_module, "project_description", return_value=description):
            data = gate_module.status_data(self.gate)
        self.assertEqual([job["id"] for job in data["jobs"]], [2, 3, 4, 1])
        self.assertEqual([job["queuePosition"] for job in data["jobs"]], [None, 1, 2, None])
        self.assertEqual(data["queue"]["running"], 1)
        self.assertEqual(data["queue"]["waiting"], 2)
        self.assertNotIn("private-", json.dumps(data))
        queued_job = next(job for job in data["jobs"] if job["id"] == 4)
        self.assertEqual(queued_job["title"], "Shell batch")
        self.gate.payload_path(token).unlink()
        fresh_gate = gate_module.Gate()
        with patch.object(fresh_gate, "snapshot", return_value=snapshot), \
                patch.object(gate_module, "project_description") as lookup:
            self.assertEqual(gate_module.status_data(fresh_gate)["jobs"][2]["title"], "Shell batch")
        lookup.assert_not_called()

    def test_status_keeps_only_recent_thirty_terminal_jobs(self):
        tasks = {str(number): self.task(number, "Done") for number in range(40)}
        snapshot = {"groups": {}, "tasks": tasks}
        description = {"project": "fixture", "branch": "", "directory": str(self.root), "title": "Tests", "executable": "go"}
        with patch.object(self.gate, "snapshot", return_value=snapshot), \
                patch.object(gate_module, "describe_task", return_value=description):
            data = gate_module.status_data(self.gate)
        self.assertEqual(len(data["jobs"]), 30)
        self.assertEqual(data["jobs"][0]["id"], 39)

    def test_next_changes_priority_without_switching_or_renumbering(self):
        snapshot = {"tasks": {"3": self.task(3, priority=5), "4": self.task(4), "2": self.task(2, "Running", priority=99)}}
        with patch.object(self.gate, "snapshot", return_value=snapshot), \
                patch.object(self.gate, "pueue") as control:
            self.assertEqual(gate_module.reorder(self.gate, 4, "next"), 6)
        args, kwargs = control.call_args
        self.assertEqual(args, ("edit", "4"))
        self.assertIn("edit-priority 6", kwargs["env_extra"]["EDITOR"])
        self.assertEqual(snapshot["tasks"]["4"]["id"], 4)

    def test_running_job_cannot_be_reordered(self):
        with patch.object(self.gate, "snapshot", return_value={"tasks": {"3": self.task(3, "Running")}}), \
                patch.object(self.gate, "pueue") as control, \
                self.assertRaisesRegex(gate_module.GateError, "Only queued"):
            gate_module.reorder(self.gate, 3, "next")
        control.assert_not_called()

    def test_last_priority_overflow_is_rejected_before_edit(self):
        snapshot = {"tasks": {"3": self.task(3, priority=-(2**31))}}
        with patch.object(self.gate, "snapshot", return_value=snapshot), \
                patch.object(self.gate, "pueue") as control, \
                self.assertRaisesRegex(gate_module.GateError, "32-bit"):
            gate_module.reorder(self.gate, 3, "last")
        control.assert_not_called()

    def test_numeric_editor_touches_only_priority(self):
        editor_root = self.root / "pueue-edit"
        editor_root.mkdir(mode=0o700)
        task = editor_root / "3"
        task.mkdir()
        for name, value in {"priority": "0", "command": "literal-secret-command", "label": "Original label", "path": str(self.root)}.items():
            (task / name).write_text(value)
        gate_module.edit_priority(7, editor_root)
        self.assertEqual((task / "priority").read_text(), "7")
        self.assertEqual((task / "command").read_text(), "literal-secret-command")
        self.assertEqual((task / "label").read_text(), "Original label")

    def test_pueue_editor_inherits_private_umask(self):
        with patch.object(gate_module.subprocess, "run") as control:
            control.return_value.returncode = 0
            self.gate.pueue("edit", "3", env_extra={"EDITOR": "fixture-editor"})
        self.assertEqual(control.call_args.kwargs["umask"], 0o077)
        self.assertEqual(control.call_args.kwargs["env"]["EDITOR"], "fixture-editor")

    def test_editor_rejects_symlink_priority_without_touching_target(self):
        editor_root = self.root / "pueue-edit"
        editor_root.mkdir(mode=0o700)
        task = editor_root / "3"
        task.mkdir()
        target = self.root / "untouched"
        target.write_text("original")
        (task / "priority").symlink_to(target)
        with self.assertRaises(OSError):
            gate_module.edit_priority(7, editor_root)
        self.assertEqual(target.read_text(), "original")

    def lease_member(self, task_id):
        initial = [{"pid": 12000 + task_id, "birth": f"fixture-birth-{task_id}"}]
        return {"task_id": task_id, "token": format(task_id, "032x"), "pgid": 980000 + task_id,
                "pid": 12000 + task_id, "boot_uuid": "fixture-boot", "initial_members": initial, "members": initial}

    def test_shared_members_release_only_after_last_job(self):
        first, second = self.lease_member(3), self.lease_member(4)
        self.assertTrue(gate_module.acquire_lease(self.gate, first, 2))
        self.assertTrue(gate_module.acquire_lease(self.gate, second, 2))
        self.assertEqual(first["lease_id"], second["lease_id"])
        self.assertFalse(gate_module.legacy_occupied(self.gate))
        with patch.object(gate_module, "group_exists", return_value=False):
            gate_module.cleanup(self.gate, 3)
            self.assertTrue(self.gate.slot.exists())
            self.assertTrue(gate_module.job_marker_path(self.gate, second).exists())
            self.assertTrue(self.gate.receipt_path(4).exists())
            gate_module.cleanup(self.gate, 4)
        self.assertFalse(self.gate.slot.exists())

    def test_pending_cleanup_consumes_lease_capacity(self):
        self.assertTrue(gate_module.acquire_lease(self.gate, self.lease_member(3), 1))
        self.assertFalse(gate_module.acquire_lease(self.gate, self.lease_member(4), 1))
        self.assertTrue(gate_module.acquire_lease(self.gate, self.lease_member(4), 2))

    def test_old_singleton_remains_exclusive_and_is_legacy(self):
        old = self.marker()
        self.assertFalse(gate_module.acquire_lease(self.gate, self.lease_member(4), 2))
        self.assertEqual(gate_module.read_private(self.gate.slot / "agent-heavy.json"), old)
        self.assertTrue(gate_module.legacy_occupied(self.gate))

    def test_external_owner_blocks_shared_join_and_is_legacy(self):
        self.gate.slot.mkdir()
        (self.gate.slot / "owner").write_text("legacy-worker")
        self.assertFalse(gate_module.acquire_lease(self.gate, self.lease_member(3), 2))
        self.assertTrue(gate_module.legacy_occupied(self.gate))
        self.assertEqual((self.gate.slot / "owner").read_text(), "legacy-worker")

    def test_invalid_shared_member_retains_all_owners(self):
        first = self.lease_member(3)
        self.assertTrue(gate_module.acquire_lease(self.gate, first, 2))
        gate_module.write_private(self.gate.slot / "agent-heavy-jobs" / "unknown.json", {"unknown": "owner"})
        with self.assertRaises(gate_module.GateError):
            gate_module.acquire_lease(self.gate, self.lease_member(4), 2)
        with patch.object(gate_module, "group_exists", return_value=False), self.assertRaises(gate_module.GateError):
            gate_module.cleanup(self.gate, 3)
        self.assertTrue(gate_module.job_marker_path(self.gate, first).exists())

    def test_force_started_helpers_raise_effective_capacity(self):
        tasks = {str(number): self.task(number, "Running") for number in range(3)}
        with patch.object(self.gate, "snapshot", return_value={"groups": {"default": {"parallel_tasks": 1}}, "tasks": tasks}):
            self.assertEqual(gate_module.lease_capacity(self.gate), 3)

    def test_limit_persists_without_changing_paused_jobs_or_queue(self):
        snapshot = {"groups": {"default": {"status": "Paused", "parallel_tasks": 2}}, "tasks": {"3": self.task(3, "Paused")}}
        with patch.object(self.gate, "snapshot", return_value=snapshot), \
                patch.object(self.gate, "pueue") as control, \
                patch.object(gate_module, "persisted_limit", return_value=2):
            gate_module.set_limit(self.gate, 2)
        verbs = [args[0] for args, _ in control.call_args_list]
        self.assertEqual(verbs, ["parallel", "group", "group"])
        self.assertEqual(control.call_args_list[1].args[1], "add")
        self.assertEqual(control.call_args_list[2].args[1], "remove")
        self.assertEqual(snapshot["tasks"]["3"]["status"], {"Paused": {}})

    def test_invalid_limits_are_rejected_before_queue_mutation(self):
        for limit in (0, -1, 9, True):
            with patch.object(self.gate, "pueue") as control, self.assertRaises(gate_module.GateError):
                gate_module.set_limit(self.gate, limit)
            control.assert_not_called()

    def test_start_overrides_only_one_queued_owned_job(self):
        with patch.object(self.gate, "task", side_effect=[self.task(4), self.task(4, "Running")]), \
                patch.object(self.gate, "pueue") as control:
            gate_module.start_job(self.gate, 4)
        control.assert_called_once_with("start", "4")
        with patch.object(self.gate, "task", return_value=self.task(4, "Running")), \
                patch.object(self.gate, "pueue") as control, self.assertRaises(gate_module.GateError):
            gate_module.start_job(self.gate, 4)
        control.assert_not_called()

    def test_script_summaries_use_basename_without_script_arguments(self):
        self.assertEqual(gate_module.safe_summary(["python3", "/private/project/check_worker.py", "private-argument"]),
                         ("check_worker.py", "python3"))
        self.assertEqual(gate_module.safe_summary(["bash", "/private/project/validate.sh"]), ("validate.sh", "bash"))

    def test_legacy_generic_cache_upgrades_from_available_script_payload(self):
        token = "e" * 32
        description = {"project": "fixture", "branch": "main", "directory": str(self.root), "title": "Shell batch", "executable": "bash"}
        gate_module.cache_description(self.gate, token, description)
        gate_module.write_private(self.gate.payload_path(token), {"argv": ["bash", "/private/validate_branch.sh", "private-argument"],
                                 "cwd": str(self.root), "label": "Shell batch"})
        task = {**self.task(3, token=token), "label": "Shell batch"}
        self.assertEqual(gate_module.describe_task(self.gate, task, "queued")["title"], "validate_branch.sh")
        self.assertNotIn("private-argument", json.dumps(gate_module.read_private(self.gate.metadata / f"{token}.json")))

    def test_explicit_generic_label_is_preserved(self):
        token = "e" * 32
        description = {"project": "fixture", "branch": "main", "directory": str(self.root), "title": "Shell batch", "executable": "bash"}
        gate_module.cache_description(self.gate, token, description)
        gate_module.write_private(self.gate.payload_path(token), {"argv": ["bash", "/private/validate_branch.sh"],
                                 "cwd": str(self.root), "label": "Shell batch", "explicit_label": True})
        task = {**self.task(3, token=token), "label": "Shell batch"}
        self.assertEqual(gate_module.describe_task(self.gate, task, "queued")["title"], "Shell batch")

    def test_interrupt_waits_until_complete_lease_publication(self):
        marker = self.lease_member(3)
        original_write = gate_module.write_private
        receipt_observed = []

        def interrupt_after_member(path, value):
            original_write(path, value)
            if path.parent.name == "agent-heavy-jobs":
                os.kill(os.getpid(), signal.SIGINT)
            if path == self.gate.receipt_path(3):
                receipt_observed.append(cancelled[0])

        with gate_module.helper_interrupts() as cancelled, \
                patch.object(gate_module, "write_private", side_effect=interrupt_after_member):
            self.assertTrue(gate_module.acquire_lease(self.gate, marker))
            self.assertTrue(cancelled[0])
        self.assertEqual(receipt_observed, [False])
        self.assertEqual(self.gate.receipt(3), marker)

    def test_cancelled_helper_waiting_for_lease_never_launches_target(self):
        token = "d" * 32
        gate_module.write_private(self.gate.payload_path(token), {"task_id": 4, "token": token, "argv": ["true"],
                                 "cwd": str(self.root), "env": {}})
        gate_module.write_private(self.gate.receipt_path(4), {"task_id": 4, "token": token})

        def interrupt_waiter(*_args):
            signal.raise_signal(signal.SIGINT)
            return False

        with patch.object(gate_module, "lease_capacity", return_value=1), \
                patch.object(gate_module, "acquire_lease", side_effect=interrupt_waiter), \
                patch.object(gate_module, "boot_identity", return_value="fixture-boot"), \
                patch.object(gate_module, "process_group_members", return_value=[{"pid": os.getpid(), "birth": "helper"}]), \
                patch.object(gate_module, "project_description", return_value={"project": "fixture", "branch": "", "directory": str(self.root)}), \
                patch.object(gate_module.time, "sleep"), patch.object(gate_module.subprocess, "run") as spawn:
            self.assertEqual(gate_module.execute(self.gate, token), 130)
        spawn.assert_not_called()
        self.assertFalse(self.gate.slot.exists())

    def test_missing_acknowledgement_never_starts_command(self):
        token = "c" * 32
        gate_module.write_private(self.gate.payload_path(token), {"token": token, "argv": ["true"]})
        with patch.object(gate_module.time, "monotonic", side_effect=[0, 31]), \
                patch.object(gate_module.subprocess, "run") as spawn, \
                self.assertRaisesRegex(gate_module.GateError, "acknowledged job identity"):
            gate_module.execute(self.gate, token)
        spawn.assert_not_called()
        self.assertFalse(self.gate.slot.exists())
        self.assertFalse(self.gate.payload_path(token).exists())

    def test_cancel_completion_race_preserves_terminal_result(self):
        states = [{"status": {"Running": {}}}, {"status": {"Done": {"result": {"Failed": 7}}}}]
        with patch.object(self.gate, "task", side_effect=states), \
                patch.object(self.gate, "pueue") as control, \
                patch.object(gate_module.time, "sleep"):
            gate_module.cancel(self.gate, 8)
        self.assertFalse(any(call.args[0] == "remove" for call in control.call_args_list))
        result = gate_module.read_private(self.gate.cancelled / "8.json")
        self.assertEqual(result["exit_code"], 7)

    def test_nonempty_pipe_rejected_before_submission(self):
        reader, writer = os.pipe()
        os.write(writer, b"private-heredoc")
        os.close(writer)
        with os.fdopen(reader) as handle, patch.object(gate_module.sys, "stdin", handle), \
                patch.object(self.gate, "pueue") as call, \
                self.assertRaisesRegex(gate_module.GateError, "Piped/heredoc"):
            gate_module.run(self.gate, ["true"])
        call.assert_not_called()
        self.assertFalse(list(self.gate.jobs.iterdir()))

    def test_empty_pipe_and_open_empty_pipe_do_not_wait(self):
        for close_writer in (True, False):
            reader, writer = os.pipe()
            if close_writer:
                os.close(writer)
            try:
                with os.fdopen(reader) as handle, patch.object(gate_module.sys, "stdin", handle):
                    gate_module.reject_piped_input()
            finally:
                if not close_writer:
                    os.close(writer)

    def test_reused_group_with_different_boot_is_not_signalled(self):
        self.marker()
        with patch.object(gate_module, "group_exists", return_value=True), \
                patch.object(gate_module, "boot_identity", return_value="different-boot"), \
                patch.object(os, "killpg") as kill, \
                self.assertRaisesRegex(gate_module.GateError, "boot identity"):
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()
        self.assertTrue(self.gate.slot.exists())

    def test_reused_pid_with_different_birth_is_not_signalled(self):
        self.marker()
        with patch.object(gate_module, "group_exists", return_value=True), \
                patch.object(gate_module, "boot_identity", return_value="fixture-boot"), \
                patch.object(gate_module, "process_group_members", return_value=[{"pid": 12345, "birth": "reused"}]), \
                patch.object(os, "killpg") as kill, \
                self.assertRaisesRegex(gate_module.GateError, "ownership cannot"):
            gate_module.cleanup(self.gate, 3)
        kill.assert_not_called()
        self.assertTrue(self.gate.slot.exists())

    def test_dynamic_member_snapshot_preserves_immutable_receipt(self):
        marker = self.marker()
        marker["members"] = [*marker["members"], {"pid": 54321, "birth": "survivor-birth"}]
        gate_module.write_private(self.gate.slot / "agent-heavy.json", marker)
        self.assertEqual(gate_module.validate_marker(self.gate, 3), marker)
        with patch.object(gate_module, "group_exists", return_value=True), \
                patch.object(gate_module, "boot_identity", return_value="fixture-boot"), \
                patch.object(gate_module, "process_group_members", return_value=[{"pid": 54321, "birth": "survivor-birth"}]):
            self.assertTrue(gate_module.verify_group_identity(marker))

    def test_cancel_first_allows_interrupt_teardown(self):
        running = {"status": {"Running": {}}}
        with patch.object(self.gate, "task", side_effect=[running, None]), \
                patch.object(self.gate, "pueue") as control, patch.object(gate_module.time, "sleep"):
            gate_module.cancel(self.gate, 3)
        self.assertIn(call("kill", "--signal", "SIGINT", "3", check=False), control.call_args_list)
        self.assertNotIn(call("kill", "3", check=False), control.call_args_list)

    def test_cancel_escalates_when_interrupt_is_ignored(self):
        running = {"status": {"Running": {}}}
        with patch.object(self.gate, "task", side_effect=[running, running, running, None]), \
                patch.object(self.gate, "pueue") as control, \
                patch.object(gate_module.time, "monotonic", side_effect=range(20)), \
                patch.object(gate_module.time, "sleep"):
            gate_module.cancel(self.gate, 3)
        interrupt_call = call("kill", "--signal", "SIGINT", "3", check=False)
        hard_kill_call = call("kill", "3", check=False)
        self.assertLess(control.call_args_list.index(interrupt_call), control.call_args_list.index(hard_kill_call))


if __name__ == "__main__":
    unittest.main()
