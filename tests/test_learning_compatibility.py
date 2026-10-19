"""Retained pre-learning processes must not bypass new operator corrections.

These fixtures preserve the old wire protocol without loading Git history or
calling the current view/permit functions. An already running Python helper
keeps its imported code; adding a revision check to a new module cannot change
that helper. Numeric host admission is represented only for this idle fixture.
"""
from __future__ import annotations

import fcntl
import os
import tempfile
import unittest
from unittest.mock import patch

import agent_heavy
from job_profiles import GIB, ProfileError, ProfileStore
import learning_control as learning
import resource_scheduler as scheduler
from test_resource_scheduler import FixtureSampler, NOW, TemporaryGate, host, prediction


class LegacyController:
    """A surviving controller reads generations, but knows no learning tables."""

    def __init__(self, gate, token):
        self.gate = gate
        self.token = token
        self.owner = None

    def __enter__(self):
        self.owner = agent_heavy.private_lock(self.gate, "resource-controller.lock")
        self.owner.__enter__()
        return self

    def __exit__(self, *arguments):
        owner, self.owner = self.owner, None
        owner.__exit__(*arguments)

    def tick(self):
        if self.owner is None:
            raise RuntimeError("A retired controller cannot publish admission")
        with agent_heavy.private_lock(self.gate, "scheduling.lock"), agent_heavy.private_lock(self.gate):
            configuration = agent_heavy.read_private(self.gate.state / "scheduling.json")
            demand = prediction(memoryBytes=GIB, cpuCores=2.0, samples=0, confidence="unknown", source="prior")
            # Pre-learning code never consults operator floors or emits an
            # applied learning revision, even after reading a new generation.
            agent_heavy.write_private(self.gate.state / "resource-status.json", {
                "mode": configuration["mode"], "generation": configuration["generation"],
                "boot": "fixture-boot", "sampledAt": NOW, "updatedAt": "fixture",
                "available": True, "admissionReady": True, "host": host(),
                "reason": "Resource predictions are ready", "jobs": {
                    self.token: {"prediction": demand, "observed": None, "decision": None},
                },
            })
            agent_heavy.write_private(self.gate.state / "resource-admissions.json", {
                "boot": "fixture-boot", "generation": configuration["generation"], "grants": {
                    self.token: {"task_id": 1, "manual": False, "expires": NOW + 10, "prediction": demand},
                },
            })


class LegacyWaitingHelper:
    """Freeze the old helper's revision-blind, automatic grant consumption."""

    def __init__(self, gate, token):
        self.gate = gate
        self.token = token

    def permit(self):
        with agent_heavy.private_lock(self.gate):
            configuration = agent_heavy.read_private(self.gate.state / "scheduling.json")
            ledger = agent_heavy.read_private(self.gate.state / "resource-admissions.json")
            status = agent_heavy.read_private(self.gate.state / "resource-status.json")
            grant = ledger["grants"].get(self.token)
            if configuration["mode"] != "adaptive" or grant is None:
                return False
            if (ledger["boot"] != "fixture-boot" or status["boot"] != "fixture-boot"
                    or ledger["generation"] != configuration["generation"]
                    or status["generation"] != configuration["generation"]
                    or grant["task_id"] != 1 or grant["manual"] or grant["expires"] < NOW
                    or not status["available"] or not status["admissionReady"]
                    or not 0 <= NOW - status["sampledAt"] <= 10):
                return False
            group = self.gate.snapshot()["groups"]["default"]
            sample, demand = status["host"], grant["prediction"]
            reserve = max(GIB, int(sample["totalMemoryBytes"] * 0.15))
            return (group["status"] == "Running" and group["parallel_tasks"] > 0
                    and sample["complete"] and sample["memoryPressure"] == "normal"
                    and demand["memoryBytes"] + reserve <= sample["availableMemoryBytes"]
                    and sample["cpuBusyCores"] + demand["cpuCores"] <= sample["cpuCores"] * 0.8)


class LearningCompatibilityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.gate = TemporaryGate(temporary.name)
        self.store = ProfileStore(self.gate.state)
        self.addCleanup(self.store.close)
        self.workload = {"family": "1" * 64, "variant": "2" * 64, "context": "3" * 64}
        self.token = "a" * 32
        self.store.observe(self.workload, {"title": "Fixture workload"})
        agent_heavy.write_private(self.gate.state / "workloads" / f"{self.token}.json", self.workload)
        agent_heavy.write_private(self.gate.state / "scheduling.json", {"mode": "adaptive", "generation": "before-edit"})
        self.gate.data["tasks"]["1"] = {
            "command": "agent-heavy execute " + self.token, "status": {"Running": {}}, "priority": 0,
        }
        for replacement in (patch.object(agent_heavy, "boot_identity", return_value="fixture-boot"),
                            patch.object(scheduler.time, "time", return_value=NOW)):
            replacement.start()
            self.addCleanup(replacement.stop)
        self.helper = LegacyWaitingHelper(self.gate, self.token)

    def correction(self):
        return {"revision": 0, "label": "Needs more memory than this Mac has",
                "memoryFloorBytes": 20 * GIB, "cpuFloorCores": None}

    def test_legacy_fixture_remains_revision_blind_after_new_code_is_loaded(self):
        with LegacyController(self.gate, self.token) as legacy:
            # Characterize the retained binary's unsafe boundary. Only the
            # public correction API may enforce compatibility before this edit.
            self.store.edit(self.workload["family"], revision=0, label="Direct fixture edit",
                            memory_floor_bytes=20 * GIB, cpu_floor_cores=None)
            legacy.tick()
            self.assertFalse(scheduler.view(self.gate)["available"])
            self.assertTrue(self.helper.permit())

    def test_adaptive_correction_is_rejected_while_legacy_controller_and_helper_survive(self):
        with LegacyController(self.gate, self.token) as legacy:
            legacy.tick()
            before = scheduler.settings(self.gate)
            with self.assertRaises(ProfileError):
                learning.correct(self.gate, "edit", self.workload["family"], self.correction())
            self.assertEqual(self.store.revision(), 0)
            self.assertEqual(scheduler.settings(self.gate), before)
            self.assertIsNone(self.store.detail(self.workload["family"])["profile"]["memoryFloorBytes"])
            legacy.tick()
            # Admission still follows the unchanged old policy; the requested
            # floor was rejected, rather than being reported saved and ignored.
            self.assertTrue(self.helper.permit())

    def test_replacement_controller_acknowledges_then_enforces_correction_for_old_helper(self):
        with LegacyController(self.gate, self.token) as legacy:
            legacy.tick()
            descriptor = os.open(self.gate.state / "resource-controller.lock", os.O_RDWR)
            try:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally:
                os.close(descriptor)

        # The old owner has exited. Its helper can remain, but only the current
        # exclusive controller can now publish a new generation's reservations.
        with agent_heavy.private_lock(self.gate, "resource-controller.lock"):
            controller = scheduler.Controller(self.gate, sampler=FixtureSampler(), store=self.store)
            self.assertEqual(controller.tick()["learningRevision"], 0)
            saved = learning.correct(self.gate, "edit", self.workload["family"], self.correction())
            self.assertEqual(saved["profile"]["memoryFloorBytes"], 20 * GIB)
            status = controller.tick()
            self.assertEqual(status["learningRevision"], 1)
            demand = status["jobs"][self.token]["prediction"]["memoryBytes"]
            self.assertEqual(demand, 20 * GIB)
            self.assertFalse(self.helper.permit())


if __name__ == "__main__":
    unittest.main()
