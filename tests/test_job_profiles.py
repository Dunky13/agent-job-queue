import errno
import json
import os
import sqlite3
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import job_profiles as profiles


class WorkloadIdentityTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.repo = self.root / "project"
        self.repo.mkdir()
        self.git(self.repo, "init", "-q")
        (self.repo / "package.json").write_text('{"scripts":{"build":"compiler"}}')
        (self.repo / "source.py").write_text("print(1)\n")
        self.commit(self.repo)

    def tearDown(self):
        self.directory.cleanup()

    def git(self, root, *args):
        return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True,
                              env={**os.environ, "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})

    def commit(self, root):
        self.git(root, "add", ".")
        self.git(root, "-c", "commit.gpgsign=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test",
                 "commit", "-qm", "fixture")

    def identify(self, argv=None, cwd=None, env=None):
        return profiles.identify_workload(argv or ["npm", "run", "build"], str(cwd or self.repo), env or {})

    def test_linked_worktrees_share_logical_task_and_variant_without_paths(self):
        worktree = self.root / "different-name"
        self.git(self.repo, "worktree", "add", "-qb", "other", str(worktree))
        first = self.identify(["python3", str(self.repo / "source.py")], env={"PWD": str(self.repo)})
        second = self.identify(["python3", str(worktree / "source.py")], cwd=worktree, env={"PWD": str(worktree)})
        self.assertEqual(first, second)
        self.assertTrue(all(profiles.HASH.fullmatch(value) for value in first.values()))
        self.assertNotIn(str(self.root), json.dumps(first))

    def test_source_revision_and_definition_change_keep_family(self):
        before = self.identify()
        (self.repo / "source.py").write_text("print(2)\n")
        self.commit(self.repo)
        after = self.identify()
        self.assertEqual(before["family"], after["family"])
        self.assertNotEqual(before["variant"], after["variant"])
        (self.repo / "package.json").write_text('{"scripts":{"build":"different-compiler"}}')
        definition = self.identify()
        self.assertEqual(after["family"], definition["family"])
        self.assertNotEqual(after["variant"], definition["variant"])
        (self.repo / "pnpm-lock.yaml").write_text("dependencies: changed\n")
        lock = self.identify()
        self.assertEqual(lock["family"], definition["family"])
        self.assertNotEqual(lock["variant"], definition["variant"])

    def test_workers_share_family_but_targets_and_unknown_flags_do_not(self):
        before = self.identify(["go", "test", "-p", "2", "./store"])
        workers = self.identify(["go", "test", "-p8", "./store"])
        target = self.identify(["go", "test", "-p", "2", "./api"])
        unknown = self.identify(["go", "test", "-p", "2", "./store", "--different-mode"])
        self.assertEqual(before["family"], workers["family"])
        self.assertNotEqual(before["variant"], workers["variant"])
        self.assertNotEqual(before["family"], target["family"])
        self.assertNotEqual(before["family"], unknown["family"])
        first = self.identify(["custom-tool", "--workers", "2"])
        second = self.identify(["custom-tool", "--workers", "8"])
        self.assertNotEqual(first["family"], second["family"])

    def test_toolchain_replacement_changes_variant_and_external_paths_remain_significant(self):
        tool = self.repo / "local-tool"
        tool.write_text("#!/bin/sh\nexit 0\n")
        tool.chmod(0o700)
        before = self.identify([str(tool)])
        tool.write_text("#!/bin/sh\nexit 2\n")
        after = self.identify([str(tool)])
        self.assertEqual(before["family"], after["family"])
        self.assertNotEqual(before["variant"], after["variant"])
        first = self.identify(["npm", "run", "build", "--config=" + str(self.repo) + "-other"])
        second = self.identify(["npm", "run", "build", "--config=<repo>-other"])
        self.assertNotEqual(first["family"], second["family"])

    def test_package_scope_and_unrelated_same_basename_are_separate(self):
        package = self.repo / "packages" / "app"
        package.mkdir(parents=True)
        self.assertNotEqual(self.identify()["family"], self.identify(cwd=package)["family"])
        other = self.root / "other" / "project"
        other.mkdir(parents=True)
        self.git(other, "init", "-q")
        self.assertNotEqual(self.identify()["family"], self.identify(cwd=other)["family"])

    def test_non_git_and_opaque_shell_commands_do_not_accidentally_merge(self):
        other = self.root / "plain"
        other.mkdir()
        self.assertNotEqual(self.identify(cwd=other)["family"], self.identify()["family"])
        first = self.identify(["bash", "-c", "go test -p 2 ./store"])
        changed = self.identify(["bash", "-c", "go test -p 8 ./store"])
        self.assertNotEqual(first["family"], changed["family"])
        explicit = {"AGENT_HEAVY_PROFILE": "store-checks"}
        first = self.identify(["bash", "-c", "go test -p 2 ./store"], env=explicit)
        changed = self.identify(["bash", "-c", "go test -p 8 ./store"], env=explicit)
        self.assertEqual(first["family"], changed["family"])
        self.assertNotEqual(first["variant"], changed["variant"])

    def test_environment_and_cache_are_separate_from_family(self):
        before = self.identify(env={"GOMAXPROCS": "2", "SECRET_VALUE": "sensitive-fixture"})
        changed = self.identify(env={"GOMAXPROCS": "8", "SECRET_VALUE": "sensitive-fixture"})
        self.assertEqual(before["family"], changed["family"])
        self.assertNotEqual(before["variant"], changed["variant"])
        (self.repo / "node_modules").mkdir()
        cached = self.identify(env={"GOMAXPROCS": "2", "SECRET_VALUE": "sensitive-fixture"})
        self.assertEqual(before["variant"], cached["variant"])
        self.assertNotEqual(before["context"], cached["context"])
        self.assertNotIn("sensitive-fixture", json.dumps(before))

    def test_large_nested_inline_python_shell_and_eval_code_is_not_a_file(self):
        nested = "print(" + repr("inline_guard_" + "x" * 4096) + ")"
        python = "import subprocess; subprocess.run(['python3', '-c', " + repr(nested) + "])"
        shell = "python3 -c " + repr(python)
        commands = (["python3", "-c", python], ["python3.12", "-Ic", python],
                    ["bash", "-lc", shell], ["sh", "-c", shell], ["node", "--eval", python],
                    ["node", "--eval=" + python], ["node", "-e" + python],
                    ["uv", "run", "python3", "-c", python])
        original_resolve = Path.resolve
        original_stat = Path.stat

        def guard_resolve(path, *args, **kwargs):
            self.assertNotIn("inline_guard_", str(path), "Inline code was resolved as a path")
            return original_resolve(path, *args, **kwargs)

        def guard_stat(path, *args, **kwargs):
            self.assertNotIn("inline_guard_", str(path), "Inline code was inspected as a path")
            return original_stat(path, *args, **kwargs)

        with patch.object(Path, "resolve", guard_resolve), patch.object(Path, "stat", guard_stat):
            for argv in commands:
                with self.subTest(command=argv[:2]):
                    before = self.identify(argv)
                    changed = self.identify([*argv[:-1], argv[-1] + "\n# changed"])
                    self.assertNotEqual(before["family"], changed["family"])
                    self.assertNotEqual(before["variant"], changed["variant"])

    def test_inline_program_positional_data_is_not_probed_but_remains_significant(self):
        code = "print('fixture')"
        before = self.identify(["python3", "-c", code, "x" * 4096])
        after = self.identify(["python3", "-c", code, "y" * 4096])
        self.assertNotEqual(before["family"], after["family"])
        self.assertNotEqual(before["variant"], after["variant"])

    def test_unknown_nonpath_arguments_handle_long_names_and_nondirectory_components(self):
        for argument in ("x" * 4096, "source.py/child", "--input=" + "y" * 4096):
            with self.subTest(argument=argument[:32]):
                identified = self.identify(["custom-tool", argument])
                self.assertTrue(all(profiles.HASH.fullmatch(value) for value in identified.values()))

    def test_permission_errors_in_actual_argument_provenance_fail_loud(self):
        original_lstat = Path.lstat

        def denied(path, *args, **kwargs):
            if path.name == "source.py":
                raise PermissionError(errno.EACCES, "fixture access denied")
            return original_lstat(path, *args, **kwargs)

        with patch.object(Path, "lstat", denied), self.assertRaises(profiles.ProfileError):
            self.identify(["python3", "source.py"])


class ProfileStoreTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.store = profiles.ProfileStore(self.root / "state")
        self.identity = {"family": "a" * 64, "variant": "b" * 64, "context": "c" * 64}

    def tearDown(self):
        self.store.close()
        self.directory.cleanup()

    def record(self, token, memory=128 * 1024 ** 2, cpu=0.5, duration=10, complete=True, code=0, **options):
        metrics = {"memoryBytes": memory, "cpuCores": cpu, "cpuSeconds": cpu * duration,
                   "durationSeconds": duration}
        metrics.update(options.pop("metrics", {}))
        self.store.record(str(token), options.pop("identity", self.identity), options.pop("machine", "fixture-machine"),
                          metrics, complete=complete, exit_code=code)

    def prediction(self, identity=None, machine="fixture-machine"):
        return self.store.prediction(identity or self.identity, machine)

    def test_cold_prior_requires_repeat_success_before_lowering(self):
        cold = self.prediction()
        self.assertGreaterEqual(cold["memoryBytes"], profiles.GIB)
        self.assertGreaterEqual(cold["cpuCores"], 2)
        self.assertEqual((cold["samples"], cold["confidence"], cold["source"]), (0, "unknown", "prior"))
        for index in range(4):
            self.record(index)
        self.assertGreaterEqual(self.prediction()["memoryBytes"], profiles.GIB)
        self.record(4)
        estimate = self.prediction()
        self.assertLess(estimate["memoryBytes"], profiles.GIB)
        self.assertEqual(estimate["confidence"], "medium")
        for index in range(5, 12):
            self.record(index)
        self.assertEqual(self.prediction()["confidence"], "high")

    def test_severe_underprediction_immediately_raises_estimate_including_failures(self):
        for index in range(12):
            self.record(index)
        self.record("failed-large", memory=6 * profiles.GIB, cpu=7, complete=True, code=137)
        estimate = self.prediction()
        self.assertGreaterEqual(estimate["memoryBytes"], 6 * profiles.GIB)
        self.assertGreaterEqual(estimate["cpuCores"], 7)
        self.assertEqual(estimate["samples"], 12)
        self.assertEqual(estimate["confidence"], "low")
        self.record("unfinished", memory=9 * profiles.GIB, complete=False, code=None)
        self.assertGreaterEqual(self.prediction()["memoryBytes"], 9 * profiles.GIB)

    def test_partial_failed_and_zero_measurements_never_make_a_cheap_success(self):
        for index in range(12):
            self.record(index, complete=False, code=None)
            self.record("failure-" + str(index), code=1)
            self.record("empty-" + str(index), memory=0)
        estimate = self.prediction()
        self.assertEqual(estimate["samples"], 0)
        self.assertGreaterEqual(estimate["memoryBytes"], profiles.GIB)
        self.assertNotEqual(estimate["confidence"], "high")

    def test_idempotent_updates_keep_peaks_and_only_count_one_run(self):
        self.record("same", memory=3 * profiles.GIB, complete=False, code=None)
        self.record("same", memory=32 * 1024 ** 2)
        self.record("same", memory=16 * 1024 ** 2)
        estimate = self.prediction()
        self.assertEqual(estimate["samples"], 1)
        self.assertGreaterEqual(estimate["memoryBytes"], 3 * profiles.GIB)
        with self.assertRaises(profiles.ProfileError):
            self.record("same", machine="another-machine")

    def test_family_transfer_is_conservative_and_machine_specific(self):
        for index in range(12):
            self.record(index, memory=2 * profiles.GIB, cpu=3)
        changed = {**self.identity, "variant": "d" * 64}
        estimate = self.prediction(changed)
        self.assertEqual(estimate["source"], "family")
        self.assertEqual(estimate["confidence"], "low")
        self.assertGreater(estimate["memoryBytes"], self.prediction()["memoryBytes"])
        self.assertEqual(self.prediction(machine="other-machine")["source"], "prior")

    def test_borrowed_family_demand_survives_censored_exact_rows_until_repeat_success(self):
        for index in range(12):
            self.record(index, memory=8 * profiles.GIB, cpu=4, duration=60)
        changed = {**self.identity, "variant": "d" * 64}
        borrowed = self.prediction(changed)
        self.assertEqual(borrowed["source"], "family")
        self.record("partial-prefix", identity=changed, memory=64 * 1024 ** 2, cpu=0.1, duration=1,
                    complete=False, code=None)
        self.record("failed-prefix", identity=changed, memory=64 * 1024 ** 2, cpu=0.1, duration=1, code=1)
        for successes in range(5):
            estimated = self.prediction(changed)
            self.assertEqual(estimated["source"], "family")
            self.assertEqual(estimated["samples"], successes)
            self.assertEqual(estimated["confidence"], "low")
            for field in ("memoryBytes", "cpuCores", "durationSeconds"):
                self.assertGreaterEqual(estimated[field], borrowed[field])
            self.record("exact-success-" + str(successes), identity=changed,
                        memory=64 * 1024 ** 2, cpu=0.1, duration=2)
        learned = self.prediction(changed)
        self.assertEqual(learned["source"], "variant")
        self.assertEqual(learned["samples"], 5)
        self.assertEqual(learned["confidence"], "medium")
        for field in ("memoryBytes", "cpuCores", "durationSeconds"):
            self.assertLess(learned[field], borrowed[field])

    def test_failed_high_exact_peaks_immediately_raise_a_borrowed_family_envelope(self):
        for index in range(12):
            self.record(index, memory=8 * profiles.GIB, cpu=4, duration=60)
        changed = {**self.identity, "variant": "d" * 64}
        borrowed = self.prediction(changed)
        self.record("failed-high", identity=changed, memory=16 * profiles.GIB, cpu=8, duration=120, code=137)
        estimated = self.prediction(changed)
        self.assertEqual((estimated["source"], estimated["samples"]), ("family", 0))
        for field, peak in (("memoryBytes", 16 * profiles.GIB), ("cpuCores", 8), ("durationSeconds", 120)):
            self.assertGreaterEqual(estimated[field], peak)
            self.assertGreater(estimated[field], borrowed[field])
        self.record("partial-high", identity=changed, memory=24 * profiles.GIB, cpu=12, duration=180,
                    complete=False, code=None)
        partial = self.prediction(changed)
        self.assertEqual(partial["samples"], 0)
        for field in ("memoryBytes", "cpuCores", "durationSeconds"):
            self.assertGreater(partial[field], estimated[field])

    def test_aged_profiles_reduce_confidence_without_erasing_upper_evidence(self):
        for index in range(12):
            self.record(index, memory=4 * profiles.GIB)
        before = self.prediction()
        with patch.object(profiles.time, "time", return_value=time.time() + 120 * profiles.DAY):
            aged = self.prediction()
        self.assertEqual(aged["confidence"], "low")
        self.assertLess(aged["memoryBytes"], before["memoryBytes"])
        self.assertGreaterEqual(aged["memoryBytes"], 3 * profiles.GIB)
        self.assertEqual(aged["samples"], 0)

    def test_failed_prefix_cannot_refresh_stale_successes_or_drop_family_evidence(self):
        old = time.time() - 60 * profiles.DAY
        for index in range(12):
            self.record(index, metrics={"sampledAt": old})
        stale = self.prediction()
        self.assertGreaterEqual(stale["memoryBytes"], profiles.GIB)
        for token, complete, code in (("partial", False, None), ("failed", True, 1), (0, False, None)):
            self.record(token, memory=64 * 1024 ** 2, cpu=0.1, duration=1, complete=complete, code=code)
            estimate = self.prediction()
            self.assertGreaterEqual(estimate["memoryBytes"], stale["memoryBytes"])
            self.assertGreaterEqual(estimate["cpuCores"], stale["cpuCores"])
            self.assertEqual((estimate["samples"], estimate["confidence"]), (12, "low"))
        for index in range(4):
            self.record("fresh-" + str(index))
        self.assertGreaterEqual(self.prediction()["memoryBytes"], profiles.GIB)
        self.record("fresh-4")
        self.assertLess(self.prediction()["memoryBytes"], profiles.GIB)
        self.assertEqual(self.prediction()["confidence"], "medium")

        changed = {**self.identity, "variant": "d" * 64}
        for index in range(12):
            self.record("old-variant-" + str(index), identity=changed, metrics={"sampledAt": old})
        borrowed = self.prediction(changed)
        self.record("partial-variant", identity=changed, memory=64 * 1024 ** 2, cpu=0.1,
                    duration=1, complete=False, code=None)
        estimate = self.prediction(changed)
        self.assertEqual((estimate["source"], estimate["confidence"]), ("family", "low"))
        for field in ("memoryBytes", "cpuCores", "durationSeconds"):
            self.assertGreaterEqual(estimate[field], borrowed[field])

    def test_retention_is_bounded_and_preserves_old_upper_evidence(self):
        self.record("old-large", memory=8 * profiles.GIB)
        with patch.object(profiles, "MAX_RUNS", 3), patch.object(profiles, "MAX_ENVELOPES", 2):
            for index in range(8):
                self.record(index, identity={**self.identity, "variant": profiles._digest(index)})
        connection = sqlite3.connect(self.store.path)
        self.assertLessEqual(connection.execute("SELECT COUNT(*) FROM runs").fetchone()[0], 3)
        self.assertLessEqual(connection.execute("SELECT COUNT(*) FROM envelopes").fetchone()[0], 2)
        connection.close()
        self.assertGreaterEqual(self.prediction()["memoryBytes"], 8 * profiles.GIB)

    def test_private_storage_contains_no_command_environment_token_or_machine(self):
        self.record("sensitive-token", machine="private-hostname")
        self.assertEqual(self.store.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.store.state.stat().st_mode & 0o777, 0o700)
        contents = self.store.path.read_bytes()
        self.assertNotIn(b"sensitive-token", contents)
        self.assertNotIn(b"private-hostname", contents)
        readonly = profiles.ProfileStore(self.store.state, readonly=True)
        self.assertEqual(readonly.prediction(self.identity, "private-hostname")["samples"], 1)
        with self.assertRaises(profiles.ProfileError):
            readonly.record("blocked", self.identity, "host", {}, complete=False, exit_code=None)

    def test_symlinks_hardlinks_permissions_and_corrupt_storage_fail_closed(self):
        for case in ("symlink", "hardlink", "permissions", "corrupt", "journal-symlink"):
            with self.subTest(case=case):
                state = self.root / case
                state.mkdir(mode=0o700)
                path = state / "resource-history.sqlite3"
                if case == "symlink":
                    path.symlink_to(self.store.path)
                elif case == "hardlink":
                    os.link(self.store.path, path)
                elif case == "permissions":
                    path.write_text("fixture")
                    path.chmod(0o644)
                elif case == "corrupt":
                    path.write_text("corrupt-history")
                    path.chmod(0o600)
                else:
                    Path(str(path) + "-journal").symlink_to(self.root / "missing")
                with self.assertRaises(profiles.ProfileError):
                    profiles.ProfileStore(state)
                path.unlink(missing_ok=True)

    def test_invalid_metrics_are_rejected(self):
        for metrics in ({"cpuCores": float("nan")}, {"memoryBytes": -1}, {"durationSeconds": True},
                        {"memoryBytes": 2 ** 64}, {"sampledAt": time.time() + 1000}):
            with self.subTest(metrics=metrics), self.assertRaises(profiles.ProfileError):
                self.record("invalid", metrics=metrics)

    def decision_rows(self):
        with sqlite3.connect(self.store.path) as connection:
            connection.row_factory = sqlite3.Row
            return connection.execute("SELECT * FROM decisions ORDER BY id").fetchall()

    def test_decision_audit_coalesces_unchanged_events_without_refreshing_timestamps(self):
        prediction = self.prediction()
        reason = "Waiting for predicted peak memory headroom"
        self.store.record_decision("private-job-token", "shadow", prediction, False, reason, sampled_at=100)
        modified = self.store.path.stat().st_mtime_ns
        self.store.record_decision("private-job-token", "shadow", prediction, False, reason, sampled_at=200)
        rows = self.decision_rows()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["sampled"], 100)
        self.assertEqual(self.store.path.stat().st_mtime_ns, modified)
        self.assertEqual(rows[0]["token"], profiles._digest(["run-v1", "private-job-token"]))
        self.assertNotIn(b"private-job-token", self.store.path.read_bytes())

    def test_decision_changes_and_modes_create_distinct_audit_events(self):
        prediction = self.prediction()
        reason = "Predicted resource demand fits the current headroom"
        self.store.record_decision("job", "shadow", prediction, True, reason, sampled_at=100)
        self.store.record_decision("job", "adaptive", prediction, True, reason, sampled_at=101)
        self.store.record_decision("job", "adaptive", prediction, False, "Queue is paused", sampled_at=102)
        changed = {**prediction, "memoryBytes": prediction["memoryBytes"] * 2}
        self.store.record_decision("job", "adaptive", changed, False, "Queue is paused", sampled_at=103)
        self.store.record_decision("job", "adaptive", {**changed, "confidence": "low"}, False,
                                   "Queue is paused", sampled_at=104)
        rows = self.decision_rows()
        self.assertEqual(len(rows), 5)
        self.assertEqual([row["sampled"] for row in rows], [100, 101, 102, 103, 104])
        self.assertEqual(rows[0]["memory"], prediction["memoryBytes"])
        self.assertEqual(rows[-1]["confidence"], "low")

    def test_decision_retention_is_bounded_and_ignores_extra_private_prediction_fields(self):
        prediction = {**self.prediction(), "argv": ["never-retain-this-command"],
                      "env": {"secret": "never-retain-this-environment"}}
        with patch.object(profiles, "MAX_DECISIONS", 3):
            for index in range(8):
                self.store.record_decision(str(index), "shadow", prediction, False, "Queue is paused", sampled_at=index)
        rows = self.decision_rows()
        self.assertEqual(len(rows), 3)
        self.assertEqual([row["sampled"] for row in rows], [5, 6, 7])
        self.assertNotIn(b"never-retain-this-command", self.store.path.read_bytes())
        self.assertNotIn(b"never-retain-this-environment", self.store.path.read_bytes())

    def test_decision_table_is_additive_for_existing_v1_and_optional_to_readers(self):
        self.record("measured-job", memory=2 * profiles.GIB)
        before = self.prediction()
        with sqlite3.connect(self.store.path) as connection:
            connection.execute("DROP TABLE decisions")
        readonly = profiles.ProfileStore(self.store.state, readonly=True)
        self.assertEqual(readonly.prediction(self.identity, "fixture-machine"), before)
        with self.assertRaises(profiles.ProfileError):
            readonly.record_decision("job", "shadow", before, True, "Ready")
        reopened = profiles.ProfileStore(self.store.state)
        self.assertEqual(reopened.prediction(self.identity, "fixture-machine"), before)
        reopened.record_decision("job", "adaptive", before, True, "Ready")
        self.assertEqual(len(self.decision_rows()), 1)
        with sqlite3.connect(self.store.path) as connection:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 1)
            self.assertEqual(connection.execute("SELECT COUNT(*) FROM runs").fetchone()[0], 1)

    def test_invalid_decisions_do_not_persist_uncontrolled_values(self):
        valid = {"token": "job", "mode": "shadow", "prediction": self.prediction(), "allowed": False,
                 "reason": "Queue is paused"}
        for changes in ({"token": ""}, {"mode": "fixed"}, {"mode": []}, {"allowed": 1}, {"reason": "x" * 257},
                        {"reason": "line\nbreak"}, {"reason": "control\x00"}, {"reason": " padded "},
                        {"sampled_at": float("nan")}, {"sampled_at": time.time() + 1000},
                        {"prediction": {**valid["prediction"], "cpuCores": float("inf")}},
                        {"prediction": {**valid["prediction"], "confidence": "uncontrolled text"}},
                        {"prediction": {**valid["prediction"], "memoryBytes": True}}):
            with self.subTest(changes=changes), self.assertRaises(profiles.ProfileError):
                self.store.record_decision(**{**valid, **changes})
        self.assertEqual(self.decision_rows(), [])


if __name__ == "__main__":
    unittest.main()
