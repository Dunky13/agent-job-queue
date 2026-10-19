import copy
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import patch

import agent_heavy
import job_profiles
import resource_scheduler as scheduler
from resource_monitor import SamplingError

GIB = 1024**3
NOW = 1_800_000_000.0


def host(**changes):
    return {"sampledAt": NOW, "totalMemoryBytes": 16 * GIB, "availableMemoryBytes": 12 * GIB,
            "cpuCores": 8, "cpuBusyCores": 0.5, "memoryPressure": "normal", "swapUsedBytes": 4 * GIB,
            "swapInBytesPerSecond": 0.0, "swapOutBytesPerSecond": 0.0, "diskFreeBytes": 50 * GIB,
            "machine": "test-capacity", "complete": True, **changes}


def prediction(**changes):
    return {"memoryBytes": GIB, "cpuCores": 1.0, "durationSeconds": 10.0,
            "samples": 8, "confidence": "medium", "source": "variant", **changes}


def measured(**changes):
    return {"memoryBytes": GIB, "cpuSeconds": 1.0, "cpuCores": 1.0,
            "processCount": 2, "complete": True, "baseline": False, **changes}


def active(*, demand=None, observed=None):
    return {"prediction": demand or prediction(), "observed": observed or measured()}


class DecisionTests(unittest.TestCase):
    def test_host_memory_is_not_charged_again_for_current_job_footprints(self):
        sample = host(availableMemoryBytes=4 * GIB)
        current = active(demand=prediction(memoryBytes=10 * GIB), observed=measured(memoryBytes=10 * GIB))
        allowed, _ = scheduler.decision(sample, prediction(), [current], 2)
        self.assertTrue(allowed)

    def test_future_growth_of_running_jobs_is_reserved(self):
        sample = host(availableMemoryBytes=4 * GIB)
        current = active(demand=prediction(memoryBytes=3 * GIB), observed=measured(memoryBytes=GIB))
        allowed, reason = scheduler.decision(sample, prediction(), [current], 2)
        self.assertFalse(allowed)
        self.assertIn("memory headroom", reason)

    def test_current_overprediction_never_creates_extra_headroom(self):
        sample = host(availableMemoryBytes=3 * GIB)
        current = active(demand=prediction(memoryBytes=GIB), observed=measured(memoryBytes=4 * GIB))
        self.assertFalse(scheduler.decision(sample, prediction(), [current], 2)[0])

    def test_cpu_current_use_is_counted_once_but_future_growth_is_reserved(self):
        sample = host(cpuBusyCores=4.0)
        running = active(demand=prediction(cpuCores=3.0), observed=measured(cpuCores=3.0))
        self.assertTrue(scheduler.decision(sample, prediction(cpuCores=2.0), [running], 2)[0])
        running["prediction"]["cpuCores"] = 5.0
        self.assertFalse(scheduler.decision(sample, prediction(cpuCores=2.0), [running], 2)[0])

    def test_first_job_still_reserves_predicted_cpu_above_background_usage(self):
        allowed, reason = scheduler.decision(host(cpuBusyCores=4.0), prediction(cpuCores=6.0), [], 2)
        self.assertFalse(allowed)
        self.assertIn("CPU headroom", reason)
        self.assertTrue(scheduler.decision(host(cpuBusyCores=1.0), prediction(cpuCores=5.0), [], 2)[0])
        allowed, reason = scheduler.decision(host(), prediction(cpuCores=8.0), [], 2)
        self.assertFalse(allowed)
        self.assertIn("machine's budget", reason)

    def test_memory_pressure_warning_and_critical_hold_admission(self):
        for pressure in ("warning", "critical"):
            with self.subTest(pressure=pressure):
                allowed, reason = scheduler.decision(host(memoryPressure=pressure), prediction(), [], 2)
                self.assertFalse(allowed)
                self.assertIn("memory pressure", reason)

    def test_active_swap_out_holds_admission_even_with_plenty_of_free_memory(self):
        allowed, reason = scheduler.decision(host(swapOutBytesPerSecond=1024**2 + 1), prediction(), [], 2)
        self.assertFalse(allowed)
        self.assertIn("swapping", reason)

    def test_historical_swap_allocation_alone_does_not_block_work(self):
        self.assertTrue(scheduler.decision(host(swapUsedBytes=100 * GIB), prediction(), [], 2)[0])

    def test_insufficient_disk_and_unrunnable_memory_estimates_explain_the_delay(self):
        allowed, reason = scheduler.decision(host(diskFreeBytes=GIB - 1), prediction(), [], 2)
        self.assertFalse(allowed)
        self.assertIn("disk space", reason)
        allowed, reason = scheduler.decision(host(), prediction(memoryBytes=15 * GIB), [], 2)
        self.assertFalse(allowed)
        self.assertIn("machine's budget", reason)

    def test_background_cpu_gate_applies_even_to_an_otherwise_empty_queue(self):
        allowed, reason = scheduler.decision(host(cpuBusyCores=5.0), prediction(), [], 2)
        self.assertFalse(allowed)
        self.assertIn("background CPU", reason)
        self.assertTrue(scheduler.decision(host(cpuBusyCores=4.0), prediction(), [], 2)[0])

    def test_unknown_workload_can_learn_alone_but_not_overlap_existing_work(self):
        unknown = prediction(source="prior", confidence="unknown", samples=0)
        self.assertTrue(scheduler.decision(host(), unknown, [], 2)[0])
        allowed, reason = scheduler.decision(host(), unknown, [active()], 2)
        self.assertFalse(allowed)
        self.assertIn("unfamiliar workload", reason)

    def test_running_unfamiliar_workload_retains_its_solo_learning_window(self):
        unknown = active(demand=prediction(source="prior", confidence="unknown", samples=0))
        allowed, reason = scheduler.decision(host(), prediction(), [unknown], 2)
        self.assertFalse(allowed)
        self.assertIn("unfamiliar workload", reason)

    def test_incomplete_host_and_running_job_measurements_hold_admission(self):
        self.assertFalse(scheduler.decision(host(complete=False), prediction(), [], 2)[0])
        current = active(observed=measured(complete=False))
        allowed, reason = scheduler.decision(host(), prediction(), [current], 2)
        self.assertFalse(allowed)
        self.assertIn("complete measurements", reason)

    def test_operator_parallel_ceiling_takes_precedence_over_spare_resources(self):
        allowed, reason = scheduler.decision(host(), prediction(), [active()], 1)
        self.assertFalse(allowed)
        self.assertIn("parallel job limit", reason)


class TemporaryGate:
    def __init__(self, directory):
        self.state = Path(directory) / "state"
        self.slot = Path(directory) / "shared.slot"
        self.receipts = self.state / "receipts"
        self.control_deadline = None
        self.data = {"tasks": {}, "groups": {"default": {"parallel_tasks": 2, "status": "Running"}}}
        self.calls = []
        self.prepare()

    def prepare(self):
        for path in (self.state, self.state / "workloads", self.state / "resource-runs", self.receipts,
                     self.state / "jobs", self.state / "metadata"):
            path.mkdir(mode=0o700, parents=True, exist_ok=True)

    def snapshot(self):
        return copy.deepcopy(self.data)

    def receipt(self, task_id):
        try:
            return agent_heavy.read_private(self.receipts / f"{int(task_id)}.json")
        except FileNotFoundError:
            return None

    def payload_path(self, token):
        return self.state / "jobs" / f"{token}.json"

    def pueue(self, *args, **kwargs):
        self.calls.append((args, kwargs))
        if args[0] in ("stash", "enqueue", "start"):
            state = {"stash": "Stashed", "enqueue": "Queued", "start": "Running"}[args[0]]
            self.data["tasks"][str(args[1])]["status"] = {state: {}}


class MemoryStore:
    def __init__(self):
        self.demands = {}
        self.records = []
        self.decisions = []
        self.closed = False
        self.learning_revision = 0
        self.observations = []

    def revision(self):
        return self.learning_revision

    def observe(self, identity, description):
        self.observations.append((identity, description))

    def match(self, identity):
        return {"id": identity["family"], "label": "Fixture workload", "reason": "Automatic workload family"}

    def prediction(self, identity, _machine):
        return self.demands.get(identity["variant"], prediction()).copy()

    def record(self, token, workload, machine, metrics, **kwargs):
        self.records.append({"token": token, "workload": workload, "machine": machine,
                             "metrics": metrics, **kwargs})

    def record_decision(self, token, mode, prediction, allowed, reason, **kwargs):
        self.decisions.append({"token": token, "mode": mode, "prediction": prediction,
                               "allowed": allowed, "reason": reason, **kwargs})

    def close(self):
        self.closed = True


class FixtureSampler:
    def __init__(self):
        self.host_sample = host()
        self.group_sample = measured()
        self.failure = None

    def host(self):
        if self.failure:
            raise self.failure
        return self.host_sample.copy()

    def group(self, _pgid, _members):
        return self.group_sample.copy()

    def forget_group(self, _pgid):
        pass


class SchedulerFixture(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.gate = TemporaryGate(self.directory.name)
        self.store = MemoryStore()
        self.sampler = FixtureSampler()
        self.boot = patch.object(agent_heavy, "boot_identity", return_value="test-boot")
        self.boot.start()
        self.addCleanup(self.boot.stop)
        self.clock = patch.object(scheduler.time, "time", return_value=NOW)
        self.clock_mock = self.clock.start()
        self.addCleanup(self.clock.stop)
        self.marker_check = patch.object(agent_heavy, "validate_marker", side_effect=lambda gate, task_id: gate.receipt(task_id))
        self.marker_check.start()
        self.addCleanup(self.marker_check.stop)
        self.configuration = {"mode": "adaptive", "generation": "test-generation"}
        self.configure()
        self.controller = scheduler.Controller(self.gate, sampler=self.sampler, store=self.store)

    def configure(self, mode="adaptive", generation="test-generation"):
        self.configuration = {"mode": mode, "generation": generation}
        agent_heavy.write_private(self.gate.state / "scheduling.json", self.configuration)

    def workload(self, task_id):
        return {"family": "a" * 64, "variant": format(task_id, "064x"), "context": "b" * 64}

    def add_job(self, task_id, state="Stashed", *, priority=0, age=0, tracked=True, owned=False, result="Success"):
        token = format(task_id, "032x")
        details = {"result": result} if state == "Done" else {}
        self.gate.data["tasks"][str(task_id)] = {
            "command": "agent-heavy execute " + token, "status": {state: details}, "priority": priority,
            "created_at": datetime.fromtimestamp(NOW - age, UTC).isoformat(),
        }
        if tracked:
            agent_heavy.write_private(self.gate.state / "workloads" / f"{token}.json", self.workload(task_id))
        if owned:
            agent_heavy.write_private(self.gate.receipts / f"{task_id}.json", {
                "task_id": task_id, "token": token, "pgid": 10000 + task_id,
                "members": [{"pid": 20000 + task_id, "birth": "fixture birth"}],
                "executionStartedAt": NOW,
            })
        return token

    def publish(self, *, available=True, age=0, generation=None, boot="test-boot", admission_ready=True,
                sample=None, jobs=None):
        agent_heavy.write_private(self.gate.state / "resource-status.json", {
            "available": available, "sampledAt": NOW - age, "updatedAt": "fixture",
            "generation": generation or self.configuration["generation"], "boot": boot,
            "host": sample or host(), "jobs": jobs or {}, "admissionReady": admission_ready,
        })

    def admissions(self, grants, *, generation=None, boot="test-boot"):
        value = {"generation": generation or self.configuration["generation"], "boot": boot, "grants": grants}
        agent_heavy.write_private(self.gate.state / "resource-admissions.json", value)
        return value

    def grant(self, task_id, *, manual=False, expires=NOW + 5, demand=None):
        return {"task_id": task_id, "manual": manual, "expires": expires,
                "prediction": demand if demand is not None else prediction()}

    def starts(self):
        return [args[1] for args, _ in self.gate.calls if args[0] == "start"]

    def finish(self, task_id=1, controller=None):
        (self.gate.receipts / f"{task_id}.json").unlink(missing_ok=True)
        self.gate.data["tasks"][str(task_id)]["status"] = {"Done": {
            "result": "Success", "end": datetime.fromtimestamp(self.clock_mock.return_value, UTC).isoformat()}}
        (controller or self.controller).tick()


class PermitTests(SchedulerFixture):
    def setUp(self):
        super().setUp()
        self.token = self.add_job(1)
        self.marker = {"task_id": 1, "token": self.token, "resourceManaged": True}

    def test_automatic_grant_requires_current_heartbeat_and_ceiling(self):
        self.admissions({self.token: self.grant(1)})
        self.publish()
        self.assertTrue(scheduler.permit(self.gate, self.marker, 1))
        self.assertFalse(scheduler.permit(self.gate, self.marker, 2))
        self.publish(available=False)
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_blocking_queue_read_cannot_outlive_grant_or_host_freshness(self):
        for expires, elapsed in ((NOW + 1, 2), (NOW + 60, 12)):
            with self.subTest(expires=expires, elapsed=elapsed):
                self.clock_mock.return_value = NOW
                self.admissions({self.token: self.grant(1, expires=expires)})
                self.publish()

                def delayed_snapshot():
                    self.clock_mock.return_value = NOW + elapsed
                    return copy.deepcopy(self.gate.data)

                with patch.object(self.gate, "snapshot", side_effect=delayed_snapshot):
                    self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_paused_queue_and_unmanaged_running_owner_hold_automatic_start(self):
        self.admissions({self.token: self.grant(1)})
        self.publish()
        self.gate.data["groups"]["default"]["status"] = "Paused"
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))
        self.gate.data["groups"]["default"]["status"] = "Running"
        self.gate.data["tasks"]["10"] = {"command": "unmanaged", "status": {"Running": {}}}
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_grant_is_rechecked_against_latest_host_pressure_and_recovery_state(self):
        self.admissions({self.token: self.grant(1)})
        self.publish(sample=host(memoryPressure="critical"))
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))
        self.publish(admission_ready=False)
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_newly_started_job_must_have_measurements_before_another_grant_is_consumed(self):
        self.admissions({self.token: self.grant(1)})
        self.gate.slot.mkdir(mode=0o700)
        members = self.gate.slot / "agent-heavy-jobs"
        members.mkdir(mode=0o700)
        other = format(2, "032x")
        agent_heavy.write_private(members / "2.json", {"task_id": 2, "token": other})
        self.publish()
        self.assertFalse(scheduler.permit(self.gate, self.marker, 1))
        self.publish(jobs={other: active()})
        self.assertTrue(scheduler.permit(self.gate, self.marker, 1))

    def test_latest_operator_limit_is_used_at_grant_consumption(self):
        self.admissions({self.token: self.grant(1)})
        self.publish()
        self.gate.data["groups"]["default"]["parallel_tasks"] = 1
        self.assertFalse(scheduler.permit(self.gate, self.marker, 1))

    def test_expired_wrong_task_and_missing_grants_cannot_start(self):
        self.publish()
        for grant in (self.grant(1, expires=NOW - 1), self.grant(2)):
            self.admissions({self.token: grant})
            self.assertFalse(scheduler.permit(self.gate, self.marker, 0))
        self.admissions({})
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_stale_future_different_generation_and_different_boot_heartbeats_hold(self):
        self.admissions({self.token: self.grant(1)})
        for changes in ({"age": scheduler.FRESH_SECONDS + 1}, {"age": -1},
                        {"generation": "older-generation"}, {"boot": "old-boot"}):
            with self.subTest(changes=changes):
                self.publish(**changes)
                self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_different_generation_or_boot_ledger_cannot_reuse_old_grants(self):
        self.publish()
        for changes in ({"generation": "old-generation"}, {"boot": "old-boot"}):
            with self.subTest(changes=changes):
                self.admissions({self.token: self.grant(1)}, **changes)
                self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_explicit_manual_override_does_not_depend_on_controller_heartbeat(self):
        scheduler.manual_grant(self.gate, 1, self.token)
        self.assertTrue(scheduler.permit(self.gate, self.marker, 2))
        self.admissions({self.token: self.grant(1, manual=True, expires=NOW - 1)})
        self.assertFalse(scheduler.permit(self.gate, self.marker, 0))

    def test_fixed_policy_preserves_existing_helper_behavior(self):
        self.configure("fixed")
        self.assertTrue(scheduler.permit(self.gate, {"task_id": 1, "token": self.token}, 2))

    def test_previously_adaptive_helper_obeys_current_fixed_ceiling(self):
        self.configure("fixed")
        self.assertTrue(scheduler.permit(self.gate, self.marker, 1))
        self.assertFalse(scheduler.permit(self.gate, self.marker, 2))

    def test_corrupt_settings_and_admission_grants_do_not_authorize_work(self):
        for value in ({"mode": "unlimited", "generation": "x"}, {"mode": "adaptive", "generation": ""},
                      {"mode": "adaptive", "generation": "x", "extra": True}):
            agent_heavy.write_private(self.gate.state / "scheduling.json", value)
            with self.subTest(settings=value), self.assertRaises(scheduler.SchedulingError):
                scheduler.permit(self.gate, self.marker, 0)
        self.configure()
        for changes in ({"manual": "yes"}, {"expires": float("nan")}, {"expires": float("inf")},
                        {"prediction": []}, {"task_id": "1"}):
            self.admissions({self.token: {**self.grant(1), **changes}})
            with self.subTest(grant=changes), self.assertRaises(scheduler.SchedulingError):
                scheduler.permit(self.gate, self.marker, 0)

    def test_corrupt_automatic_prediction_cannot_bypass_resource_comparisons(self):
        self.publish()
        for changes in ({"memoryBytes": float("nan")}, {"memoryBytes": -GIB}, {"memoryBytes": True},
                        {"cpuCores": float("nan")}, {"cpuCores": -1}, {"samples": -1}, {"samples": True}):
            self.admissions({self.token: self.grant(1, demand=prediction(**changes))})
            with self.subTest(prediction=changes), self.assertRaises(scheduler.SchedulingError):
                scheduler.permit(self.gate, self.marker, 0)
        self.admissions({self.token: self.grant(1, demand={})})
        with self.assertRaises(scheduler.SchedulingError):
            scheduler.permit(self.gate, self.marker, 0)


class ControllerTests(SchedulerFixture):
    def test_changed_learning_during_collection_holds_old_predictions_and_pending_grants(self):
        token = self.add_job(1)
        manual = self.add_job(2)
        self.admissions({token: self.grant(1), manual: self.grant(2, manual=True)})
        original = self.store.prediction

        def changing_prediction(*args):
            result = original(*args)
            self.store.learning_revision = 1
            return result

        with patch.object(self.store, "prediction", side_effect=changing_prediction):
            result = self.controller.tick()
        self.assertFalse(result["available"])
        self.assertFalse(result["admissionReady"])
        self.assertEqual(result["jobs"], {})
        self.assertEqual(self.starts(), [])
        grants = scheduler.ledger(self.gate, self.configuration)["grants"]
        self.assertNotIn(token, grants)
        self.assertTrue(grants[manual]["manual"])

    def test_job_resources_link_to_safe_observed_profile(self):
        token = self.add_job(1)
        agent_heavy.write_private(self.gate.state / "metadata" / f"{token}.json", {
            "title": "Tests", "project": "example", "executable": "python", "directory": "private path",
        })
        result = self.controller.tick()
        self.assertEqual(self.store.observations[0][1], {"title": "Tests", "project": "example", "executable": "python"})
        self.assertEqual(result["jobs"][token]["learning"]["id"], self.workload(1)["family"])
        self.assertEqual(result["learningRevision"], 0)

    def test_job_finishing_inside_observation_gap_cannot_supply_complete_training(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        for seconds in (2, 4):
            self.clock_mock.return_value = NOW + seconds
            self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value = NOW + 123
        self.sampler.host_sample = host(sampledAt=NOW + 123, complete=False)
        self.finish()
        self.assertFalse(self.store.records[0]["complete"])
        self.assertEqual(self.store.records[0]["metrics"]["durationSeconds"], 123)
        self.assertEqual(self.store.records[0]["metrics"]["sampledAt"], NOW + 4)

    def test_timely_terminal_tail_retains_actual_duration_and_can_learn(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        for seconds in (2, 4):
            self.clock_mock.return_value = NOW + seconds
            self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value = NOW + 5
        self.finish()
        self.assertTrue(self.store.records[0]["complete"])
        self.assertEqual(self.store.records[0]["metrics"]["durationSeconds"], 5)

    def test_late_first_observation_preserves_duration_but_censors_unseen_prefix(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        marker["executionStartedAt"] = NOW - 600
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value += 2
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.finish()
        self.assertEqual(self.store.records[0]["metrics"]["durationSeconds"], 602)
        self.assertFalse(self.store.records[0]["complete"])

    def test_missing_execution_start_cannot_establish_a_cheap_complete_run(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        marker.pop("executionStartedAt")
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value += 2
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.finish()
        self.assertFalse(self.store.records[0]["complete"])

    def test_adaptive_grant_starts_one_job_and_publishes_decision(self):
        token = self.add_job(1)
        value = self.controller.tick()
        self.assertEqual(self.starts(), ["1"])
        self.assertTrue(value["jobs"][token]["decision"]["allowed"])
        self.assertTrue(scheduler.view(self.gate)["available"])
        grant = scheduler.ledger(self.gate, self.configuration)["grants"][token]
        self.assertFalse(grant["manual"])
        self.assertGreater(grant["expires"], NOW)

    def test_pending_start_reserves_capacity_across_ticks(self):
        first = self.add_job(1)
        second = self.add_job(2)
        self.gate.data["groups"]["default"]["parallel_tasks"] = 1
        self.controller.tick()
        self.controller.tick()
        self.assertEqual(self.starts(), ["1"])
        self.assertIn(first, scheduler.ledger(self.gate, self.configuration)["grants"])
        value = scheduler.view(self.gate)
        self.assertFalse(value["jobs"][second]["decision"]["allowed"])

    def test_expired_pending_grant_is_released_for_a_fresh_decision(self):
        token = self.add_job(1)
        self.admissions({token: self.grant(1, expires=NOW - 1)})
        self.controller.tick()
        self.assertEqual(self.starts(), ["1"])
        self.assertGreater(scheduler.ledger(self.gate, self.configuration)["grants"][token]["expires"], NOW)

    def test_missing_and_finished_tasks_release_pending_reservations(self):
        token = self.add_job(1, state="Done")
        missing = format(99, "032x")
        self.admissions({token: self.grant(1), missing: self.grant(99)})
        self.controller.tick()
        self.assertEqual(scheduler.ledger(self.gate, self.configuration)["grants"], {})

    def test_active_process_measurement_and_grant_are_not_counted_twice(self):
        running = self.add_job(1, state="Running", owned=True)
        self.admissions({running: self.grant(1)})
        self.add_job(2)
        self.controller.tick()
        self.assertEqual(self.starts(), ["2"])

    def test_unknown_old_running_job_prevents_unsafe_admission(self):
        self.add_job(1, state="Running", tracked=False)
        self.add_job(2)
        self.controller.tick()
        self.assertEqual(self.starts(), [])

    def test_shadow_and_fixed_modes_publish_predictions_without_starting_jobs(self):
        for mode in ("fixed", "shadow"):
            with self.subTest(mode=mode):
                self.configure(mode)
                token = self.add_job(1, state="Queued")
                value = self.controller.tick()
                self.assertEqual(self.starts(), [])
                self.assertTrue(value["jobs"][token]["decision"]["allowed"])

    def test_incomplete_host_and_active_intervals_do_not_grant_admission(self):
        self.sampler.host_sample["complete"] = False
        self.add_job(1)
        value = self.controller.tick()
        self.assertFalse(value["available"])
        self.assertEqual(self.starts(), [])
        self.sampler.host_sample["complete"] = True
        self.add_job(2, state="Running", owned=True)
        self.sampler.group_sample["complete"] = False
        self.controller.tick()
        self.assertEqual(self.starts(), [])

    def test_paused_queue_and_external_shared_owner_delay_admission(self):
        token = self.add_job(1)
        self.gate.data["groups"]["default"]["status"] = "Paused"
        value = self.controller.tick()
        self.assertIn("paused", value["jobs"][token]["decision"]["reason"])
        self.gate.data["groups"]["default"]["status"] = "Running"
        self.gate.slot.mkdir(mode=0o700)
        value = self.controller.tick()
        self.assertIn("external shared", value["jobs"][token]["decision"]["reason"])
        self.assertEqual(self.starts(), [])

    def test_old_host_sample_cannot_receive_a_fresh_admission_heartbeat(self):
        token = self.add_job(1)
        self.sampler.host_sample["sampledAt"] = NOW - scheduler.FRESH_SECONDS - 1
        value = self.controller.tick()
        self.assertFalse(value["available"])
        self.assertFalse(value["admissionReady"])
        self.assertIn("fresh host", value["jobs"][token]["decision"]["reason"])
        self.assertEqual(self.starts(), [])

    def test_unsafe_host_interval_revokes_pending_auto_grants_and_preserves_manual_override(self):
        automatic = self.add_job(1, state="Running")
        manual = self.add_job(2, state="Running")
        self.admissions({automatic: self.grant(1), manual: self.grant(2, manual=True)})
        self.sampler.host_sample["memoryPressure"] = "warning"
        self.controller.tick()
        self.assertEqual(set(scheduler.ledger(self.gate, self.configuration)["grants"]), {manual})
        self.assertEqual(self.starts(), [])

    def test_new_lease_acquired_after_sampling_is_reconciled_before_admission(self):
        token = self.add_job(1)
        self.gate.slot.mkdir(mode=0o700)
        members = self.gate.slot / "agent-heavy-jobs"
        members.mkdir(mode=0o700)
        other = format(2, "032x")
        agent_heavy.write_private(members / "2.json", {"task_id": 2, "token": other})
        with patch.object(agent_heavy, "shared_manifest", return_value={"lease_id": "fixture"}):
            value = self.controller.tick()
        self.assertFalse(value["jobs"][token]["decision"]["allowed"])
        self.assertEqual(self.starts(), [])

    def test_pressure_recovery_requires_a_stable_interval(self):
        token = self.add_job(1)
        with patch.object(scheduler.time, "monotonic", return_value=100):
            self.sampler.host_sample["memoryPressure"] = "warning"
            self.controller.tick()
        self.sampler.host_sample["memoryPressure"] = "normal"
        with patch.object(scheduler.time, "monotonic", return_value=101):
            value = self.controller.tick()
        self.assertIn("recovery interval", value["jobs"][token]["decision"]["reason"])
        self.assertEqual(self.starts(), [])
        with patch.object(scheduler.time, "monotonic", return_value=111):
            self.controller.tick()
        self.assertEqual(self.starts(), ["1"])

    def test_small_job_can_backfill_a_recent_same_priority_large_job(self):
        self.add_job(1, age=5)
        self.store.demands[self.workload(1)["variant"]] = prediction(memoryBytes=15 * GIB)
        self.add_job(2)
        self.controller.tick()
        self.assertEqual(self.starts(), ["2"])

    def test_high_priority_and_starved_jobs_prevent_backfill(self):
        for priority, age in ((1, 5), (0, scheduler.STARVATION_SECONDS + 1)):
            with self.subTest(priority=priority, age=age):
                self.gate.calls.clear()
                self.add_job(1, priority=priority, age=age)
                self.store.demands[self.workload(1)["variant"]] = prediction(memoryBytes=15 * GIB)
                token = self.add_job(2)
                value = self.controller.tick()
                self.assertEqual(self.starts(), [])
                self.assertFalse(value["jobs"][token]["decision"]["allowed"])

    def test_mode_transition_stashes_and_requeues_only_owned_queue_jobs(self):
        self.configure("fixed")
        self.publish()
        self.add_job(1, state="Queued")
        self.gate.data["tasks"]["99"] = {"command": "other task", "status": {"Queued": {}}}
        scheduler.set_mode(self.gate, "adaptive")
        self.assertEqual(agent_heavy.task_status(self.gate.data["tasks"]["1"])[0], "Stashed")
        self.assertEqual(agent_heavy.task_status(self.gate.data["tasks"]["99"])[0], "Queued")
        self.assertNotEqual(scheduler.settings(self.gate)["generation"], self.configuration["generation"])
        scheduler.set_mode(self.gate, "shadow")
        self.assertEqual(agent_heavy.task_status(self.gate.data["tasks"]["1"])[0], "Queued")

    def test_adaptive_transition_requires_fresh_controller_and_retains_manual_reservations(self):
        self.configure("fixed")
        with self.assertRaises(scheduler.SchedulingError):
            scheduler.set_mode(self.gate, "adaptive")
        token = self.add_job(1, state="Queued")
        self.publish()
        self.admissions({token: self.grant(1, manual=True)})
        scheduler.set_mode(self.gate, "adaptive")
        updated = scheduler.settings(self.gate)
        self.assertIn(token, scheduler.ledger(self.gate, updated)["grants"])

    def test_mode_change_drops_pending_auto_grants_but_preserves_active_and_manual_reservations(self):
        pending = self.add_job(1)
        manual = self.add_job(2)
        running = self.add_job(3, state="Running", owned=True)
        self.admissions({pending: self.grant(1), manual: self.grant(2, manual=True), running: self.grant(3)})
        scheduler.set_mode(self.gate, "shadow")
        current = scheduler.settings(self.gate)
        grants = scheduler.ledger(self.gate, current)["grants"]
        self.assertEqual(set(grants), {manual, running})

    def test_run_baseline_incompleteness_does_not_prevent_later_complete_learning(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.sampler.group_sample = measured(complete=False, baseline=True)
        first = self.controller.sample_run(1, token, marker, host(), prediction())
        self.assertFalse(first["observed"]["complete"])
        self.clock_mock.return_value += 2
        self.sampler.group_sample = measured(cpuSeconds=2.0)
        second = self.controller.sample_run(1, token, marker, host(), prediction())
        self.assertTrue(second["observed"]["complete"])
        self.clock_mock.return_value += 2
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.finish()
        self.assertTrue(self.store.records[0]["complete"])
        self.assertEqual(self.store.records[0]["metrics"]["durationSeconds"], 4)

    def test_repeated_missing_intervals_cannot_later_be_claimed_as_a_complete_run(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.sampler.group_sample = measured(complete=False, baseline=True)
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.sampler.group_sample = measured(complete=False, baseline=False)
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value += 2
        self.sampler.group_sample = measured(cpuSeconds=3.0)
        result = self.controller.sample_run(1, token, marker, host(), prediction())
        self.assertTrue(result["observed"]["complete"])
        saved = agent_heavy.read_private(self.gate.state / "resource-runs" / f"{token}.json")
        self.assertFalse(saved["complete"])
        self.finish()
        self.assertFalse(self.store.records[0]["complete"])

    def test_missing_first_measurement_is_not_exempted_as_an_intentional_baseline(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.sampler.group_sample = measured(complete=False, baseline=False)
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.clock_mock.return_value += 2
        self.sampler.group_sample = measured()
        self.assertTrue(self.controller.sample_run(1, token, marker, host(), prediction())["observed"]["complete"])
        saved = agent_heavy.read_private(self.gate.state / "resource-runs" / f"{token}.json")
        self.assertFalse(saved["complete"])
        self.finish()
        self.assertFalse(self.store.records[0]["complete"])

    def test_done_task_with_live_owned_workers_is_not_finalized_as_complete(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.gate.data["tasks"]["1"]["status"] = {"Done": {"result": "Success"}}
        self.sampler.group_sample = measured(memoryBytes=5 * GIB, cpuCores=3.0)
        self.controller.tick()
        self.assertFalse(any(record["complete"] for record in self.store.records))

    def test_repeated_terminal_ticks_preserve_original_observation_age(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        (self.gate.receipts / "1.json").unlink()
        self.gate.data["tasks"]["1"]["status"] = {"Done": {"result": "Success"}}
        self.clock_mock.return_value += 100
        self.controller.tick()
        self.clock_mock.return_value += 100
        self.controller.tick()
        self.assertEqual(len(self.store.records), 1)
        self.assertEqual(self.store.records[0]["metrics"]["sampledAt"], NOW)

    def test_pre_feature_payload_migration_checks_acknowledged_task_and_token(self):
        token = self.add_job(1, tracked=False)
        payload = {"token": token, "task_id": 1, "argv": ["fixture"], "cwd": ".", "env": {}}
        agent_heavy.write_private(self.gate.payload_path(token), payload)
        with patch.object(job_profiles, "identify_workload", return_value=self.workload(1)) as identify:
            self.assertEqual(scheduler.ensure_identity(self.gate, token, 1), self.workload(1))
            identify.assert_called_once()
        (self.gate.state / "workloads" / f"{token}.json").unlink()
        payload["task_id"] = 2
        agent_heavy.write_private(self.gate.payload_path(token), payload)
        with self.assertRaises(scheduler.SchedulingError):
            scheduler.ensure_identity(self.gate, token, 1)

    def test_restart_and_later_missing_measurements_keep_run_confidence_incomplete(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        restarted = scheduler.Controller(self.gate, sampler=self.sampler, store=self.store)
        self.assertTrue(restarted.sample_run(1, token, marker, host(), prediction())["observed"]["complete"])
        saved = agent_heavy.read_private(self.gate.state / "resource-runs" / f"{token}.json")
        self.assertFalse(saved["complete"])
        self.sampler.group_sample["complete"] = False
        self.assertFalse(restarted.sample_run(1, token, marker, host(), prediction())["observed"]["complete"])
        self.clock_mock.return_value += 2
        self.sampler.group_sample["complete"] = True
        self.assertTrue(restarted.sample_run(1, token, marker, host(), prediction())["observed"]["complete"])
        self.finish(controller=restarted)
        self.assertFalse(self.store.records[0]["complete"])

    def test_peak_evidence_increases_live_reservation_immediately(self):
        token = self.add_job(1, state="Running", owned=True)
        self.sampler.group_sample = measured(memoryBytes=5 * GIB, cpuCores=4.0)
        value = self.controller.sample_run(1, token, self.gate.receipt(1), host(), prediction())
        self.assertGreaterEqual(value["prediction"]["memoryBytes"], 6 * GIB)
        self.assertGreaterEqual(value["prediction"]["cpuCores"], 4.0)

    def test_service_sampling_failure_publishes_unavailability_and_never_starts_a_job(self):
        self.add_job(1)
        self.publish()
        self.sampler.failure = SamplingError("private provider error")
        with patch.object(job_profiles, "ProfileStore", return_value=self.store), \
                patch.object(scheduler.signal, "signal"), self.assertRaises(SamplingError):
            scheduler.serve(self.gate, sampler=self.sampler, once=True)
        value = scheduler.view(self.gate)
        self.assertFalse(value["available"])
        self.assertIn("admissions held", value["reason"])
        self.assertNotIn("private provider error", value["reason"])
        self.assertEqual(self.starts(), [])
        self.assertTrue(self.store.closed)

    def test_service_daemon_failure_invalidates_a_previously_fresh_heartbeat(self):
        self.add_job(1)
        self.publish()
        with patch.object(job_profiles, "ProfileStore", return_value=self.store), \
                patch.object(scheduler.signal, "signal"), \
                patch.object(self.gate, "pueue", side_effect=agent_heavy.GateError("private daemon failure")), \
                self.assertRaises(agent_heavy.GateError):
            scheduler.serve(self.gate, sampler=self.sampler, once=True)
        value = scheduler.view(self.gate)
        self.assertFalse(value["available"])
        self.assertNotIn("private daemon failure", value["reason"])
        self.assertTrue(self.store.closed)

    def test_service_failure_gap_permanently_censors_tracked_run_learning(self):
        token = self.add_job(1, state="Running", owned=True)
        marker = self.gate.receipt(1)
        self.controller.sample_run(1, token, marker, host(), prediction())
        self.sampler.failure = SamplingError("collection gap")
        with patch.object(scheduler, "Controller", return_value=self.controller), \
                patch.object(scheduler.signal, "signal"), self.assertRaises(SamplingError):
            scheduler.serve(self.gate, sampler=self.sampler, once=True)
        saved = agent_heavy.read_private(self.gate.state / "resource-runs" / f"{token}.json")
        self.assertFalse(saved["complete"])
        self.sampler.failure = None
        self.clock_mock.return_value += 2
        self.controller.tick()
        recovered = agent_heavy.read_private(self.gate.state / "resource-runs" / f"{token}.json")
        self.assertFalse(recovered["complete"])


if __name__ == "__main__":
    unittest.main()
