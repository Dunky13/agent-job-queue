"""Private correction control, controller revision races and admission safety."""
from __future__ import annotations

import io
import fcntl
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

import agent_heavy
from job_profiles import ProfileError, ProfileStore
import learning_control as learning
import resource_scheduler as scheduler
from test_resource_scheduler import FixtureSampler, NOW, TemporaryGate, host, prediction


def workload(number=1):
    return {"family": format(number, "064x"), "variant": format(number + 100, "064x"), "context": "c" * 64}


class LearningControlTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.gate = TemporaryGate(self.directory.name)
        self.store = ProfileStore(self.gate.state)
        self.addCleanup(self.store.close)
        self.store.observe(workload(), {"title": "Tests", "project": "example", "executable": "python"})
        self.identity = workload()["family"]
        self.configuration = {"mode": "adaptive", "generation": "before-edit"}
        agent_heavy.write_private(self.gate.state / "scheduling.json", self.configuration)
        self.clock = patch.object(scheduler.time, "time", return_value=NOW)
        self.clock.start()
        self.addCleanup(self.clock.stop)
        self.boot = patch.object(agent_heavy, "boot_identity", return_value="test-boot")
        self.boot.start()
        self.addCleanup(self.boot.stop)
        self.publish(learningRevision=0)

    def edit(self, revision=0, **changes):
        return {"revision": revision, "label": "Unit tests", "memoryFloorBytes": 4 * 1024 ** 3,
                "cpuFloorCores": 3.5, **changes}

    def publish(self, **changes):
        agent_heavy.write_private(self.gate.state / "resource-status.json", {
            "generation": scheduler.settings(self.gate)["generation"], "boot": "test-boot",
            "sampledAt": NOW, "updatedAt": "fixture", "host": host(), "jobs": {},
            "available": True, "admissionReady": True, **changes,
        })

    def grants(self):
        grants = {format(number, "032x"): {"task_id": number, "manual": number == 2,
                  "expires": NOW + 5, "prediction": prediction()} for number in (1, 2, 3)}
        agent_heavy.write_private(self.gate.receipts / "3.json", {"pgid": 123, "task_id": 3})
        agent_heavy.write_private(self.gate.state / "resource-admissions.json", {
            "generation": self.configuration["generation"], "boot": "test-boot", "grants": grants,
        })
        return grants

    def test_inspection_of_absent_state_does_not_create_files(self):
        self.gate.state = Path(self.directory.name) / "absent"
        result = learning.inspect(self.gate)
        self.assertEqual(result["profiles"], [])
        self.assertEqual(result["revision"], 0)
        self.assertIsNone(result["controllerAppliedRevision"])
        self.assertFalse(self.gate.state.exists())

    def test_history_remains_visible_without_host_and_prediction_uses_current_machine(self):
        (self.gate.state / "resource-status.json").unlink()
        result = learning.inspect(self.gate, identity=self.identity)
        self.assertEqual(result["profile"]["label"], "Tests")
        self.assertIsNone(result["prediction"])
        self.publish(learningRevision=0)
        result = learning.inspect(self.gate, identity=self.identity)
        self.assertEqual(result["prediction"]["source"], "prior")
        self.assertEqual(result["controllerAppliedRevision"], 0)
        self.assertEqual(len(result["predictionMachine"]), 64)
        self.assertNotIn("test-capacity", json.dumps(result))

    def test_edit_invalidates_pending_automatic_grants_before_store_mutation(self):
        self.grants()
        self.publish(learningRevision=0)
        original = ProfileStore.edit

        def checked(store, *args, **kwargs):
            self.assertNotEqual(scheduler.settings(self.gate)["generation"], "before-edit")
            current = scheduler.ledger(self.gate, scheduler.settings(self.gate))["grants"]
            self.assertEqual(set(current), {format(2, "032x"), format(3, "032x")})
            self.assertFalse(scheduler.view(self.gate)["available"])
            return original(store, *args, **kwargs)

        with patch.object(ProfileStore, "edit", checked):
            result = learning.correct(self.gate, "edit", self.identity, self.edit())
        self.assertEqual(result["revision"], 1)
        self.assertEqual(result["profile"]["label"], "Unit tests")
        self.assertIsNone(result["controllerAppliedRevision"])
        self.assertEqual(self.gate.calls, [])
        demand = self.store.prediction(workload(), "test-capacity")
        self.assertEqual(demand["memoryBytes"], 4 * 1024 ** 3)
        self.assertEqual(demand["cpuCores"], 3.5)

    def test_save_response_is_captured_before_another_operator_can_change_its_profile(self):
        original = learning.inspect

        def inspect_under_correction_lock(*args, **kwargs):
            with (self.gate.state / "scheduling.lock").open("r") as competing_operator:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(competing_operator, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return original(*args, **kwargs)

        with patch.object(learning, "inspect", side_effect=inspect_under_correction_lock):
            result = learning.correct(self.gate, "edit", self.identity, self.edit())
        self.assertEqual(result["revision"], 1)

    def test_stale_revision_and_invalid_fields_leave_settings_and_learning_unchanged(self):
        for payload in (self.edit(revision=1), self.edit(revision=True), self.edit(cpuFloorCores=float("nan")),
                        self.edit(memoryFloorBytes=0), self.edit(extra="field"), self.edit(label="\n")):
            with self.subTest(payload=payload), self.assertRaises(ProfileError):
                learning.correct(self.gate, "edit", self.identity, payload)
            self.assertEqual(scheduler.settings(self.gate), self.configuration)
            self.assertEqual(self.store.revision(), 0)

    def test_failed_mutation_cannot_leave_old_grant_usable(self):
        self.grants()
        self.publish(learningRevision=0)
        with patch.object(ProfileStore, "edit", side_effect=ProfileError("simulated storage failure")), \
                self.assertRaises(ProfileError):
            learning.correct(self.gate, "edit", self.identity, self.edit())
        self.assertEqual(self.store.revision(), 0)
        self.assertFalse(scheduler.view(self.gate)["available"])
        grants = scheduler.ledger(self.gate, scheduler.settings(self.gate))["grants"]
        self.assertNotIn(format(1, "032x"), grants)

    def test_old_controller_cannot_republish_usable_estimates_after_correction(self):
        learning.correct(self.gate, "edit", self.identity, self.edit())
        for changes in ({}, {"learningRevision": 0}):
            self.publish(**changes)
            self.assertFalse(scheduler.view(self.gate)["available"])
        self.publish(learningRevision=1)
        self.assertTrue(scheduler.view(self.gate)["available"])
        self.assertEqual(learning.inspect(self.gate)["controllerAppliedRevision"], 1)

    def test_automatic_permit_requires_applied_revision_but_manual_remains_explicit(self):
        learning.correct(self.gate, "edit", self.identity, self.edit())
        grants = self.grants()
        configuration = scheduler.settings(self.gate)
        agent_heavy.write_private(self.gate.state / "resource-admissions.json", {
            "generation": configuration["generation"], "boot": "test-boot", "grants": grants,
        })
        self.publish()
        self.assertFalse(scheduler.permit(self.gate, {"task_id": 1, "token": format(1, "032x")}, 0))
        self.assertTrue(scheduler.permit(self.gate, {"task_id": 2, "token": format(2, "032x")}, 0))

    def test_merge_split_reset_return_the_profile_that_survives_and_keep_floors(self):
        second = workload(2)
        self.store.observe(second, {"title": "Browser tests"})
        learning.correct(self.gate, "edit", self.identity, self.edit())
        self.publish(learningRevision=1)
        result = learning.correct(self.gate, "merge", self.identity, {"revision": 1, "targetId": second["family"]})
        self.assertEqual(result["profile"]["id"], second["family"])
        self.assertEqual(result["profile"]["memberCount"], 2)
        self.publish(learningRevision=2)
        result = learning.correct(self.gate, "split", second["family"], {"revision": 2, "family": self.identity})
        self.assertEqual(result["profile"]["id"], self.identity)
        self.assertEqual(result["profile"]["memberCount"], 1)
        with self.assertRaises(ProfileError):
            learning.correct(self.gate, "reset", self.identity, {"revision": 3, "confirm": False})
        self.publish(learningRevision=3)
        result = learning.correct(self.gate, "reset", self.identity, {"revision": 3, "confirm": True})
        self.assertEqual(result["revision"], 4)
        self.assertEqual(result["profile"]["memoryFloorBytes"], 4 * 1024 ** 3)

    def test_controller_discards_raced_edit_then_recomputes_group_reference_and_floors(self):
        token = "a" * 32
        agent_heavy.write_private(self.gate.state / "workloads" / f"{token}.json", workload())
        self.gate.data["tasks"]["1"] = {"command": "agent-heavy execute " + token, "status": {"Stashed": {}}, "priority": 0}
        controller = scheduler.Controller(self.gate, sampler=FixtureSampler(), store=self.store)
        original = self.store.prediction

        def edit_during_prediction(*args):
            demand = original(*args)
            learning.correct(self.gate, "edit", self.identity, self.edit())
            return demand

        with patch.object(self.store, "prediction", side_effect=edit_during_prediction):
            result = controller.tick()
        self.assertFalse(result["available"])
        self.assertEqual(self.gate.calls, [])
        result = controller.tick()
        self.assertEqual(result["learningRevision"], 1)
        self.assertEqual(result["jobs"][token]["learning"]["label"], "Unit tests")
        self.assertEqual(result["jobs"][token]["prediction"]["cpuCores"], 3.5)
        self.assertEqual(learning.inspect(self.gate)["controllerAppliedRevision"], 1)

    def test_cli_private_stdin_and_bounded_json_errors(self):
        output = io.StringIO()
        with patch.object(learning, "Gate", return_value=self.gate), \
                patch.object(learning.sys, "stdin", io.TextIOWrapper(io.BytesIO(json.dumps(self.edit()).encode()))), \
                redirect_stdout(output):
            self.assertEqual(agent_heavy.main(["profiles", "edit", self.identity, "--json"]), 0)
        self.assertEqual(json.loads(output.getvalue())["revision"], 1)
        for body in (b'{"revision":0,"revision":1}', b"x" * (learning.MAX_INPUT + 1), b'[]'):
            output = io.StringIO()
            with patch.object(learning, "Gate", return_value=self.gate), \
                    patch.object(learning.sys, "stdin", io.TextIOWrapper(io.BytesIO(body))), redirect_stdout(output):
                self.assertEqual(agent_heavy.main(["profiles", "edit", self.identity, "--json"]), 125)
            self.assertEqual(set(json.loads(output.getvalue())), {"error"})


if __name__ == "__main__":
    unittest.main()
