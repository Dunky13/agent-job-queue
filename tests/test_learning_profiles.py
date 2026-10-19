"""Private, reversible operator corrections to statistical workload learning."""
import json
import sqlite3
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from unittest.mock import patch

import job_profiles as profiles


class LearningProfileTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.store = profiles.ProfileStore(self.root / "state")
        self.first = self.identity("build")
        self.second = self.identity("test")

    @staticmethod
    def identity(name, variant="initial"):
        return {"family": profiles._digest(["family", name]), "variant": profiles._digest(["variant", name, variant]),
                "context": profiles._digest(["context", name])}

    def observe(self, identity=None, title="Build app", project="app", executable="python"):
        self.store.observe(identity or self.first, {"title": title, "project": project, "executable": executable})

    def record(self, token, identity=None, *, memory=2 * profiles.GIB, cpu=3, complete=True, code=0, sampled=None):
        self.store.record(token, identity or self.first, "fixture-machine", {
            "memoryBytes": memory, "cpuCores": cpu, "durationSeconds": 10, "cpuSeconds": cpu * 5,
            "sampledAt": sampled if sampled is not None else time.time(),
        }, complete=complete, exit_code=code)

    def edit(self, profile_id=None, *, label="Build app", memory=None, cpu=None, revision=None):
        self.store.edit(profile_id or self.first["family"], revision=self.store.revision() if revision is None else revision,
                        label=label, memory_floor_bytes=memory, cpu_floor_cores=cpu)

    def test_readonly_empty_does_not_create_state_or_database(self):
        state = self.root / "missing" / "state"
        store = profiles.ProfileStore(state, readonly=True)
        self.assertEqual(store.catalog(), {"revision": 0, "profiles": [], "total": 0, "offset": 0, "limit": 100})
        self.assertEqual(store.revision(), 0)
        self.assertEqual(store.match(self.first)["id"], self.first["family"])
        self.assertFalse(state.parent.exists())
        with self.assertRaises(profiles.ProfileError):
            store.observe(self.first, {})

        existing = self.root / "empty"
        existing.mkdir(mode=0o700)
        before = existing.stat().st_mtime_ns
        self.assertEqual(profiles.ProfileStore(existing, readonly=True).catalog()["total"], 0)
        self.assertEqual(existing.stat().st_mtime_ns, before)
        self.assertEqual(list(existing.iterdir()), [])

    def test_observation_registers_safe_description_without_operator_revision(self):
        self.observe()
        self.assertEqual(self.store.revision(), 0)
        matched = self.store.match(self.first)
        self.assertEqual(matched, {"id": self.first["family"], "label": "Build app", "reason": "Automatic workload family"})
        catalog = self.store.catalog()
        self.assertEqual(catalog["total"], 1)
        summary = catalog["profiles"][0]
        self.assertEqual((summary["memberCount"], summary["variantCount"], summary["successCount"]), (1, 0, 0))
        self.assertIsNone(summary["lastRunAt"])
        self.assertIsNone(self.store.detail(matched["id"])["prediction"])
        before = self.store.path.stat().st_mtime_ns
        self.observe()
        self.assertEqual(self.store.path.stat().st_mtime_ns, before)

    def test_unknown_description_and_legacy_details_use_client_compatible_strings(self):
        self.store.observe(self.first, {"title": None, "project": None, "executable": None})
        empty = self.store.detail(self.first["family"])
        self.assertEqual(empty["members"], [{"family": self.first["family"],
                                          "label": f"Workload {self.first['family'][:12]}",
                                          "project": "", "executable": ""}])
        self.assertIsNone(empty["profile"]["lastRunAt"])
        self.assertEqual(empty["profile"]["variantCount"], 0)
        for key in ("memberCount", "variantCount", "successCount", "incompleteCount", "peakMemoryBytes"):
            self.assertIs(type(empty["profile"][key]), int)
        self.record("legacy", self.second)
        legacy = self.store.detail(self.second["family"])
        self.assertEqual(legacy["members"][0]["project"], "")
        self.assertEqual(legacy["members"][0]["executable"], "")
        self.assertIs(type(legacy["profile"]["lastRunAt"]), float)
        self.assertEqual(legacy["profile"]["successCount"], 1)

    def test_catalog_detail_keeps_censored_peaks_separate_from_success_counts(self):
        self.observe()
        self.record("success")
        self.record("incomplete", memory=8 * profiles.GIB, complete=False, code=None)
        self.record("failed", memory=4 * profiles.GIB, code=1)
        detail = self.store.detail(self.first["family"], machine="fixture-machine")
        self.assertEqual((detail["profile"]["successCount"], detail["profile"]["incompleteCount"]), (1, 2))
        self.assertEqual(detail["profile"]["peakMemoryBytes"], 8 * profiles.GIB)
        self.assertEqual(detail["variants"][0]["incompleteCount"], 2)
        self.assertEqual(sum(row["success"] for row in detail["runs"]), 1)
        self.assertEqual(detail["prediction"]["source"], "family")
        self.assertEqual(detail["prediction"]["samples"], 0)
        self.assertEqual(detail["predictionMachine"], profiles._digest(["machine-v1", "fixture-machine"]))
        self.assertNotIn("fixture-machine", json.dumps(detail))

    def test_merge_applies_to_future_variants_without_combining_exact_successes(self):
        self.observe()
        self.observe(self.second, title="Test app")
        for index in range(12):
            self.record(str(index), memory=8 * profiles.GIB, cpu=6)
        before = self.store.prediction(self.second, "fixture-machine")
        self.store.merge(self.first["family"], self.second["family"], revision=0)
        changed = self.identity("test", "new-worktree-source-revision")
        estimate = self.store.prediction(changed, "fixture-machine")
        self.assertGreater(estimate["memoryBytes"], before["memoryBytes"])
        self.assertEqual((estimate["source"], estimate["samples"], estimate["confidence"]), ("family", 0, "low"))
        self.assertEqual(self.store.match(changed)["id"], self.second["family"])
        self.assertEqual(self.store.match(self.first)["reason"], "Operator grouping")
        self.assertEqual(self.store.match(self.first)["label"], "Test app")
        self.assertEqual(self.store.match(self.second)["label"], "Test app")
        self.assertEqual(self.store.catalog()["profiles"][0]["label"], "Test app")
        self.assertEqual(self.store.catalog()["total"], 1)
        for index in range(4):
            self.record(f"small-{index}", changed, memory=64 * 1024 ** 2, cpu=0.1)
            self.assertGreaterEqual(self.store.prediction(changed, "fixture-machine")["memoryBytes"], 8 * profiles.GIB)
        self.record("small-4", changed, memory=64 * 1024 ** 2, cpu=0.1)
        exact = self.store.prediction(changed, "fixture-machine")
        self.assertEqual((exact["samples"], exact["confidence"]), (5, "medium"))
        self.assertLess(exact["memoryBytes"], profiles.GIB)
        other_machine = self.store.prediction(changed, "other-machine")
        self.assertEqual(other_machine["source"], "prior")

    def test_split_undoes_history_grouping_and_keeps_original_numeric_identities(self):
        self.observe()
        self.observe(self.second, title="Test app")
        self.record("large", memory=8 * profiles.GIB)
        self.record("small", self.second, memory=128 * 1024 ** 2)
        before = self.store.prediction(self.second, "fixture-machine")
        self.store.merge(self.first["family"], self.second["family"], revision=0)
        self.assertGreater(self.store.prediction(self.second, "fixture-machine")["memoryBytes"], before["memoryBytes"])
        new_id = self.store.split(self.second["family"], self.first["family"], revision=1)
        self.assertEqual(self.store.match(self.first)["id"], new_id)
        self.assertEqual(self.store.prediction(self.second, "fixture-machine"), before)
        first = self.store.detail(new_id)
        self.assertEqual(first["profile"]["peakMemoryBytes"], 8 * profiles.GIB)
        self.assertEqual(first["runs"][0]["family"], self.first["family"])
        self.assertEqual(first["runs"][0]["variant"], self.first["variant"])

    def test_splitting_group_root_gets_distinct_stable_profile_identity(self):
        self.observe()
        self.observe(self.second, title="Test app")
        self.store.merge(self.second["family"], self.first["family"], revision=0)
        new_id = self.store.split(self.first["family"], self.first["family"], revision=1)
        self.assertNotEqual(new_id, self.first["family"])
        self.assertEqual(self.store.match(self.first)["id"], new_id)
        self.assertEqual(self.store.match(self.second)["id"], self.first["family"])
        self.assertEqual(self.store.detail(new_id)["profile"]["memberCount"], 1)
        self.assertEqual(self.store.catalog()["total"], 2)

    def test_operator_floors_only_raise_estimates_and_can_be_cleared(self):
        self.observe()
        for index in range(12):
            self.record(str(index), memory=64 * 1024 ** 2, cpu=0.1)
        before = self.store.prediction(self.first, "fixture-machine")
        self.edit(memory=4 * profiles.GIB, cpu=5)
        raised = self.store.prediction(self.first, "fixture-machine")
        self.assertEqual(raised["memoryBytes"], 4 * profiles.GIB)
        self.assertEqual(raised["cpuCores"], 5)
        self.assertEqual(raised["samples"], before["samples"])
        self.edit(memory=1, cpu=0.01)
        self.assertEqual(self.store.prediction(self.first, "fixture-machine"), before)
        self.edit()
        self.assertEqual(self.store.prediction(self.first, "fixture-machine"), before)
        self.edit(cpu=0.1004)
        self.assertGreaterEqual(self.store.prediction(self.first, "fixture-machine")["cpuCores"], 0.1004)

    def test_merge_preserves_higher_floors_and_split_keeps_reservations(self):
        self.observe()
        self.observe(self.second, title="Test app")
        self.edit(memory=4 * profiles.GIB, cpu=2)
        self.edit(self.second["family"], label="Testing", memory=2 * profiles.GIB, cpu=5)
        self.store.merge(self.first["family"], self.second["family"], revision=2)
        merged = self.store.detail(self.second["family"])["profile"]
        self.assertEqual((merged["memoryFloorBytes"], merged["cpuFloorCores"]), (4 * profiles.GIB, 5))
        new_id = self.store.split(self.second["family"], self.first["family"], revision=3)
        split = self.store.detail(new_id)["profile"]
        self.assertEqual((split["memoryFloorBytes"], split["cpuFloorCores"]), (4 * profiles.GIB, 5))

    def test_reset_preserves_corrections_returns_cold_and_ignores_old_completion_replay(self):
        self.observe()
        self.observe(self.second, title="Test app")
        old = time.time() - 60
        self.record("old", memory=8 * profiles.GIB, sampled=old)
        self.store.merge(self.first["family"], self.second["family"], revision=0)
        self.edit(self.second["family"], label="Shared build", cpu=4)
        self.store.reset(self.second["family"], revision=2)
        store = profiles.ProfileStore(self.store.state)
        store.record("old", self.first, "fixture-machine", {"memoryBytes": 8 * profiles.GIB, "cpuCores": 6,
                     "durationSeconds": 10, "sampledAt": old}, complete=True, exit_code=0)
        detail = store.detail(self.second["family"], machine="fixture-machine")
        self.assertEqual(detail["profile"]["label"], "Shared build")
        self.assertEqual(detail["profile"]["memberCount"], 2)
        self.assertEqual(detail["runs"], [])
        self.assertEqual(detail["prediction"]["source"], "prior")
        self.assertEqual((detail["prediction"]["memoryBytes"], detail["prediction"]["cpuCores"]), (profiles.GIB, 4))
        self.record("in-flight-completes-after-reset", memory=2 * profiles.GIB)
        self.assertEqual(store.detail(self.second["family"])["profile"]["successCount"], 1)

    def test_reset_watermark_survives_merge_and_split(self):
        self.observe()
        self.observe(self.second, title="Test app")
        old = time.time() - 60
        self.record("old", sampled=old)
        self.store.reset(self.first["family"], revision=0)
        self.store.merge(self.first["family"], self.second["family"], revision=1)
        new_id = self.store.split(self.second["family"], self.first["family"], revision=2)
        self.record("old", sampled=old)
        self.assertEqual(self.store.detail(new_id)["runs"], [])

    def test_post_reset_upper_evidence_keeps_family_attribution_after_retention(self):
        self.observe()
        self.record("old-before-reset", memory=16 * profiles.GIB)
        self.store.reset(self.first["family"], revision=0)
        self.record("new-large", memory=8 * profiles.GIB)
        with patch.object(profiles, "MAX_ENVELOPES", 1), patch.object(profiles, "MAX_RUNS", 1):
            for index in range(4):
                self.record(f"other-{index}", self.identity("other", str(index)))
        estimate = self.store.prediction(self.first, "fixture-machine")
        self.assertGreaterEqual(estimate["memoryBytes"], 8 * profiles.GIB)
        self.assertLess(estimate["memoryBytes"], 16 * profiles.GIB)
        self.assertEqual(self.store.detail(self.first["family"])["profile"]["peakMemoryBytes"], 8 * profiles.GIB)
        self.store.reset(self.first["family"], revision=1)
        self.assertEqual(self.store.prediction(self.first, "fixture-machine")["source"], "prior")

    def test_outer_retention_bound_falls_back_conservatively_instead_of_losing_peak(self):
        self.observe()
        self.store.reset(self.first["family"], revision=0)
        self.record("new-large", memory=8 * profiles.GIB)
        with patch.object(profiles, "MAX_ENVELOPES", 1), patch.object(profiles, "MAX_RUNS", 1), \
                patch.object(profiles, "MAX_RETAINED_ENVELOPES", 1):
            for index in range(4):
                self.record(f"other-{index}", self.identity("other", str(index)))
        self.assertGreaterEqual(self.store.prediction(self.first, "fixture-machine")["memoryBytes"], 8 * profiles.GIB)
        with sqlite3.connect(self.store.path) as connection:
            self.assertLessEqual(connection.execute("SELECT COUNT(*) FROM learning_retained").fetchone()[0], 1)

    def test_reset_can_discard_unattributed_legacy_retention_floor_only_for_target(self):
        self.observe()
        self.observe(self.second, title="Test app")
        with sqlite3.connect(self.store.path) as connection:
            connection.execute("INSERT INTO floors VALUES ('*',?,?,?)", (16 * profiles.GIB, 8, time.time()))
        self.store.reset(self.first["family"], revision=0)
        self.assertEqual(self.store.prediction(self.first, "fixture-machine")["memoryBytes"], profiles.GIB)
        self.assertEqual(self.store.prediction(self.second, "fixture-machine")["memoryBytes"], 16 * profiles.GIB)

    def test_stale_revision_rejects_all_changes_without_partial_writes(self):
        self.observe()
        self.observe(self.second, title="Test app")
        self.record("keep")
        self.edit(label="First edit")
        before = self.store.path.read_bytes()
        actions = [lambda: self.edit(label="stale", revision=0),
                   lambda: self.store.merge(self.first["family"], self.second["family"], revision=0),
                   lambda: self.store.split(self.first["family"], self.first["family"], revision=0),
                   lambda: self.store.reset(self.first["family"], revision=0)]
        for action in actions:
            with self.assertRaisesRegex(profiles.ProfileError, "refresh"):
                action()
            self.assertEqual(self.store.path.read_bytes(), before)

    def test_concurrent_expected_revision_changes_have_only_one_winner(self):
        self.observe()
        barrier = threading.Barrier(2)
        stores = [profiles.ProfileStore(self.store.state), profiles.ProfileStore(self.store.state)]

        def edit(entry):
            store, label = entry
            barrier.wait()
            try:
                store.edit(self.first["family"], revision=0, label=label,
                           memory_floor_bytes=None, cpu_floor_cores=None)
                return "saved"
            except profiles.ProfileError as error:
                return str(error)

        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(edit, zip(stores, ["First concurrent edit", "Second concurrent edit"])))
        self.assertEqual(results.count("saved"), 1)
        self.assertEqual(sum("refresh" in result for result in results), 1)
        self.assertEqual(self.store.revision(), 1)

    def test_legacy_readonly_catalog_and_prediction_never_migrate_or_touch_files(self):
        self.record("legacy")
        with sqlite3.connect(self.store.path) as connection:
            for name in ("learning_members", "learning_profiles", "learning_state", "learning_resets"):
                connection.execute(f"DROP TABLE {name}")
        before = {path.name: (path.stat().st_mtime_ns, path.read_bytes()) for path in self.store.state.iterdir()}
        readonly = profiles.ProfileStore(self.store.state, readonly=True)
        catalog = readonly.catalog()
        self.assertEqual(catalog["revision"], 0)
        self.assertEqual(catalog["profiles"][0]["label"], f"Workload {self.first['family'][:12]}")
        self.assertEqual(readonly.detail(self.first["family"], machine="fixture-machine")["profile"]["successCount"], 1)
        self.assertEqual(readonly.prediction(self.first, "fixture-machine")["samples"], 1)
        after = {path.name: (path.stat().st_mtime_ns, path.read_bytes()) for path in self.store.state.iterdir()}
        self.assertEqual(after, before)
        reopened = profiles.ProfileStore(self.store.state)
        self.assertEqual(reopened.catalog()["profiles"][0]["successCount"], 1)
        reopened.edit(self.first["family"], revision=0, label="Recovered", memory_floor_bytes=None, cpu_floor_cores=None)
        self.assertEqual(reopened.match(self.first)["label"], "Recovered")

    def test_registry_retention_never_discards_explicit_corrections(self):
        with patch.object(profiles, "MAX_REGISTERED_FAMILIES", 2):
            self.observe()
            self.edit(label="Keep my correction")
            self.observe(self.second, title="Reclaimable")
            third = self.identity("third")
            self.observe(third, title="Replacement")
            self.assertEqual(self.store.match(self.first)["label"], "Keep my correction")
            self.assertEqual(self.store.catalog()["total"], 2)
            self.edit(third["family"], label="Also pinned")
            fourth = self.identity("fourth")
            self.observe(fourth, title="Cannot register")
            self.assertEqual(self.store.match(fourth)["label"], f"Workload {fourth['family'][:12]}")
            self.record("unregistered", fourth)
            with self.assertRaisesRegex(profiles.ProfileError, "registry is full"):
                self.edit(fourth["family"])
            self.assertEqual(self.store.catalog()["total"], 3)
            self.assertEqual(self.store.revision(), 2)

    def test_bounded_details_catalog_search_and_opaque_privacy(self):
        self.observe()
        self.observe(self.second, title="Searchable test")
        for index in range(8):
            self.record(f"secret-private-token-{index}", self.identity("build", str(index)))
        self.store.merge(self.first["family"], self.second["family"], revision=0)
        with patch.object(profiles, "MAX_DETAIL_MEMBERS", 1), patch.object(profiles, "MAX_DETAIL_VARIANTS", 3), \
                patch.object(profiles, "MAX_DETAIL_RUNS", 4):
            detail = self.store.detail(self.second["family"])
            self.assertEqual(detail["truncated"], {"members": True, "variants": True, "runs": True})
            self.assertEqual((len(detail["members"]), len(detail["variants"]), len(detail["runs"])), (1, 3, 4))
        self.assertEqual(self.store.catalog(query="SEARCHABLE", limit=1)["total"], 1)
        self.assertEqual(self.store.catalog(offset=1)["profiles"], [])
        self.assertNotIn(b"secret-private-token", self.store.path.read_bytes())
        self.assertNotIn("secret-private-token", json.dumps(detail))
        self.assertEqual(self.store.path.stat().st_mode & 0o777, 0o600)

    def test_malformed_metadata_pages_and_changes_are_rejected(self):
        self.observe()
        actions = [lambda: self.store.observe(self.first, {"argv": ["secret"]}),
                   lambda: self.observe(project="some/path"), lambda: self.observe(executable="../python"),
                   lambda: self.observe(title="line\nbreak"), lambda: self.observe(title="x" * 121),
                   lambda: self.edit(label=" padded "), lambda: self.edit(label=""),
                   lambda: self.edit(memory=True), lambda: self.edit(memory=-1), lambda: self.edit(memory=2 ** 51),
                   lambda: self.edit(cpu=float("nan")), lambda: self.edit(cpu=1025), lambda: self.edit(cpu=0),
                   lambda: self.edit(cpu=2 ** 10000),
                   lambda: self.edit(revision=True), lambda: self.store.catalog(limit=101),
                   lambda: self.store.catalog(offset=True), lambda: self.store.catalog(query="line\nbreak"),
                   lambda: self.store.detail("not-a-hash"), lambda: self.store.detail(self.first["family"], machine=""),
                   lambda: self.store.merge(self.first["family"], self.first["family"], revision=0),
                   lambda: self.store.split(self.first["family"], self.first["family"], revision=0)]
        for action in actions:
            with self.assertRaises(profiles.ProfileError):
                action()
        self.assertEqual(self.store.revision(), 0)


if __name__ == "__main__":
    unittest.main()
