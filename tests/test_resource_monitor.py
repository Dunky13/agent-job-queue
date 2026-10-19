import ctypes
import os
import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import resource_monitor as monitor


def host_counters(**changes):
    return {
        "totalMemoryBytes": 16 * 1024**3, "availableMemoryBytes": 4 * 1024**3,
        "cpuCores": 8, "cpuTicks": [100, 100, 100, 100], "memoryPressure": "normal",
        "swapUsedBytes": 1024**3, "swapInBytes": 1000, "swapOutBytes": 2000,
        "diskFreeBytes": 10 * 1024**3, "machine": "opaque-capacity", **changes,
    }


def usage(start=1, memory=100, cpu=1_000_000_000, read=10, write=20):
    return {"start": start, "memoryBytes": memory, "cpuNanoseconds": cpu,
            "readBytes": read, "writeBytes": write}


class FakeBackend:
    def __init__(self):
        self.raw = host_counters()
        self.identities = [{"pid": 101, "birth": "stable birth"}]
        self.usages = {101: usage()}

    def host_counters(self):
        return self.raw.copy()

    def members(self, _pgid):
        return self.identities.copy()

    def process(self, pid, _pgid):
        value = self.usages[pid]
        if isinstance(value, Exception):
            raise value
        return value.copy()


class ResourceSamplerTests(unittest.TestCase):
    def setUp(self):
        self.backend = FakeBackend()
        self.now = 10.0
        self.sampler = monitor.ResourceSampler(backend=self.backend, clock=lambda: self.now, wall_clock=lambda: 123.5)

    def test_first_host_read_reserves_all_cpu_and_marks_interval_unavailable(self):
        sample = self.sampler.host()
        self.assertFalse(sample["complete"])
        self.assertEqual(sample["cpuBusyCores"], 8.0)
        self.assertEqual(sample["sampledAt"], 123.5)
        self.assertNotIn("cpuTicks", sample)
        self.assertNotIn("swapInBytes", sample)

    def test_host_interval_cpu_and_swap_rates(self):
        self.sampler.host()
        self.now += 2
        self.backend.raw.update(cpuTicks=[120, 110, 170, 100], swapInBytes=9192, swapOutBytes=6096)
        sample = self.sampler.host()
        self.assertTrue(sample["complete"])
        self.assertAlmostEqual(sample["cpuBusyCores"], 2.4)
        self.assertEqual(sample["swapInBytesPerSecond"], 4096.0)
        self.assertEqual(sample["swapOutBytesPerSecond"], 2048.0)

    def test_counter_reset_is_incomplete_and_next_interval_recovers(self):
        self.sampler.host()
        self.now += 1
        self.backend.raw.update(cpuTicks=[1, 1, 1, 1], swapInBytes=0)
        sample = self.sampler.host()
        self.assertFalse(sample["complete"])
        self.assertEqual(sample["cpuBusyCores"], 8)
        self.now += 1
        self.backend.raw.update(cpuTicks=[2, 2, 6, 1], swapInBytes=10)
        self.assertTrue(self.sampler.host()["complete"])

    def test_unsigned_cpu_tick_wrap_preserves_valid_interval(self):
        maximum = 2**32
        self.backend.raw["cpuTicks"] = [maximum - 10, 100, 100, 100]
        self.sampler.host()
        self.now += 1
        self.backend.raw["cpuTicks"] = [10, 110, 170, 100]
        sample = self.sampler.host()
        self.assertTrue(sample["complete"])
        self.assertAlmostEqual(sample["cpuBusyCores"], 2.4)

    def test_host_stalled_clock_and_unchanged_counters_are_incomplete(self):
        self.sampler.host()
        self.assertFalse(self.sampler.host()["complete"])
        self.now += 1
        self.assertFalse(self.sampler.host()["complete"])

    def test_machine_capacity_change_requires_new_baseline(self):
        self.sampler.host()
        self.now += 1
        self.backend.raw.update(machine="different-capacity", cpuTicks=[120, 110, 170, 100])
        self.assertFalse(self.sampler.host()["complete"])

    def test_new_sampler_cannot_reuse_old_host_interval(self):
        self.sampler.host()
        fresh = monitor.ResourceSampler(backend=self.backend)
        self.assertFalse(fresh.host()["complete"])

    def test_group_first_sample_and_aggregate_simultaneous_memory(self):
        self.backend.identities.append({"pid": 102, "birth": "worker birth"})
        self.backend.usages[102] = usage(start=2, memory=500, cpu=2_000_000_000)
        sample = self.sampler.group(400, self.backend.identities)
        self.assertFalse(sample["complete"])
        self.assertTrue(sample["baseline"])
        self.assertEqual(sample["memoryBytes"], 600)
        self.assertEqual(sample["cpuSeconds"], 3.0)
        self.assertEqual(sample["processCount"], 2)
        self.assertEqual(sample["readBytes"], 20)
        self.assertEqual(sample["writeBytes"], 40)

    def test_group_cpu_interval_and_memory_use_current_values(self):
        self.sampler.group(400)
        self.now += 2
        self.backend.usages[101] = usage(memory=60, cpu=4_000_000_000, read=30, write=50)
        sample = self.sampler.group(400)
        self.assertTrue(sample["complete"])
        self.assertFalse(sample["baseline"])
        self.assertEqual(sample["cpuSeconds"], 4)
        self.assertEqual(sample["cpuCores"], 1.5)
        self.assertEqual(sample["memoryBytes"], 60)
        self.assertEqual(sample["readBytes"], 30)

    def test_worker_exit_retains_observed_cpu_without_memory_or_cpu_spike(self):
        self.backend.identities.append({"pid": 102, "birth": "worker birth"})
        self.backend.usages[102] = usage(start=2, memory=500, cpu=3_000_000_000)
        self.sampler.group(400)
        self.now += 2
        self.backend.identities.pop()
        self.backend.usages[101] = usage(cpu=2_000_000_000)
        sample = self.sampler.group(400)
        self.assertTrue(sample["complete"])
        self.assertEqual(sample["cpuSeconds"], 5)
        self.assertEqual(sample["cpuCores"], 0.5)
        self.assertEqual(sample["memoryBytes"], 100)

    def test_new_worker_and_reused_pid_get_distinct_kernel_birth_counters(self):
        self.sampler.group(400)
        self.now += 1
        self.backend.usages[101] = usage(start=2, cpu=2_000_000_000)
        self.backend.identities[0]["birth"] = "new birth"
        sample = self.sampler.group(400)
        self.assertTrue(sample["complete"])
        self.assertEqual(sample["cpuSeconds"], 3)
        self.assertEqual(sample["cpuCores"], 2)

    def test_kernel_birth_distinguishes_pid_reuse_with_same_displayed_second(self):
        self.sampler.group(400)
        self.now += 1
        self.backend.usages[101] = usage(start=9, cpu=500_000_000)
        self.assertEqual(self.sampler.group(400)["cpuSeconds"], 1.5)

    def test_process_counter_reset_does_not_subtract_or_invent_work(self):
        self.sampler.group(400)
        self.now += 1
        self.backend.usages[101] = usage(cpu=1, read=1, write=1)
        sample = self.sampler.group(400)
        self.assertFalse(sample["complete"])
        self.assertEqual(sample["cpuSeconds"], 1)
        self.assertEqual(sample["cpuCores"], 0)
        self.now += 1
        self.backend.usages[101] = usage(cpu=1_000_000_001, read=3, write=5)
        sample = self.sampler.group(400)
        self.assertTrue(sample["complete"])
        self.assertEqual(sample["cpuSeconds"], 2)
        self.assertEqual(sample["readBytes"], 12)

    def test_missing_or_failed_process_metrics_are_explicitly_incomplete(self):
        self.sampler.group(400)
        for value in ({"memoryBytes": 0}, usage(memory=-1), usage(cpu=True), monitor.SamplingError("unavailable")):
            self.now += 1
            self.backend.usages[101] = value
            with self.subTest(value=type(value).__name__):
                self.assertFalse(self.sampler.group(400)["complete"])

    def test_missing_first_process_metrics_are_not_an_intentional_baseline(self):
        self.backend.usages[101] = monitor.SamplingError("unavailable")
        sample = self.sampler.group(400)
        self.assertFalse(sample["baseline"])
        self.assertFalse(sample["complete"])

    def test_first_process_census_race_is_not_an_intentional_baseline(self):
        before = self.backend.identities.copy()
        after = [*before, {"pid": 102, "birth": "new worker"}]
        with patch.object(self.backend, "members", side_effect=[before, after]):
            sample = self.sampler.group(400)
        self.assertFalse(sample["baseline"])
        self.assertFalse(sample["complete"])

    def test_ownership_mismatch_prevents_reading_any_process_resources(self):
        with patch.object(self.backend, "process") as process:
            for expected in ([{"pid": 101, "birth": "old birth"}], [{"pid": 102, "birth": "stable birth"}]):
                with self.subTest(expected=expected), self.assertRaises(monitor.SamplingError):
                    self.sampler.group(400, expected)
            process.assert_not_called()

    def test_known_surviving_worker_can_anchor_ownership(self):
        expected = [{"pid": 999, "birth": "exited leader"}, {"pid": 101, "birth": "stable   birth"}]
        self.assertEqual(self.sampler.group(400, expected)["processCount"], 1)

    def test_ownership_race_fails_closed(self):
        expected = self.backend.identities.copy()
        with patch.object(self.backend, "members", side_effect=[expected, [{"pid": 101, "birth": "reused"}]]):
            with self.assertRaises(monitor.SamplingError):
                self.sampler.group(400, expected)

    def test_worker_census_race_is_incomplete(self):
        before = self.backend.identities.copy()
        after = [*before, {"pid": 102, "birth": "new worker"}]
        self.sampler.group(400)
        self.now += 1
        with patch.object(self.backend, "members", side_effect=[before, after]):
            self.assertFalse(self.sampler.group(400, before)["complete"])

    def test_invalid_process_groups_and_ownership_records_are_rejected(self):
        for pgid in (None, True, 0, 1, -2, "400", 2**31):
            with self.subTest(pgid=pgid), self.assertRaises(monitor.SamplingError):
                self.sampler.group(pgid)
        for expected in ([], {}, [{"pid": True, "birth": "x"}], [{"pid": 101, "birth": ""}], [None]):
            with self.subTest(expected=expected), self.assertRaises(monitor.SamplingError):
                self.sampler.group(400, expected)

    def test_empty_group_is_incomplete_and_expected_ownership_fails(self):
        self.backend.identities.clear()
        sample = self.sampler.group(400)
        self.assertFalse(sample["complete"])
        with self.assertRaises(monitor.SamplingError):
            self.sampler.group(400, [{"pid": 101, "birth": "stable birth"}])

    def test_forget_group_prevents_pid_group_reuse_from_inheriting_totals(self):
        self.sampler.group(400)
        self.sampler.forget_group(400)
        self.backend.usages[101] = usage(cpu=500_000_000)
        sample = self.sampler.group(400)
        self.assertFalse(sample["complete"])
        self.assertEqual(sample["cpuSeconds"], 0.5)


class DarwinBoundaryTests(unittest.TestCase):
    def backend(self):
        backend = monitor._DarwinBackend.__new__(monitor._DarwinBackend)
        backend.port, backend.total, backend.cores = 123, 16 * 1024**3, 8
        backend.page_size, backend.machine, backend.disk_path = 16384, "opaque", "."

        def cpu(_port, _flavor, output, count):
            for index, value in enumerate((100, 100, 100, 100)):
                output[index] = value
            count._obj.value = 4
            return 0

        def memory(_port, _flavor, output, count):
            vm = monitor._VMStatistics64.from_buffer(output)
            vm.free_count, vm.inactive_count, vm.speculative_count = 10, 20, 8
            vm.compressor_page_count, vm.swapins, vm.swapouts = 100, 3, 4
            count._obj.value = ctypes.sizeof(vm) // 4
            return 0

        backend.system = SimpleNamespace(host_statistics=cpu, host_statistics64=memory)
        return backend

    def test_native_host_page_and_disk_units_avoid_double_counting_speculation(self):
        backend = self.backend()
        swap = monitor._SwapUsage(total=1000, available=800, used=200, pagesize=16384, encrypted=1)
        disk = SimpleNamespace(f_bavail=5, f_frsize=4096)
        with patch.object(backend, "_sysctl", return_value=bytes(swap)), \
                patch.object(backend, "_integer", return_value=2), patch.object(monitor.os, "statvfs", return_value=disk):
            raw = backend.host_counters()
        self.assertEqual(raw["availableMemoryBytes"], 30 * 16384)
        self.assertEqual(raw["swapInBytes"], 3 * 16384)
        self.assertEqual(raw["swapOutBytes"], 4 * 16384)
        self.assertEqual(raw["swapUsedBytes"], 200)
        self.assertEqual(raw["diskFreeBytes"], 5 * 4096)
        self.assertEqual(raw["memoryPressure"], "warning")

    def test_missing_host_statistics_fail_closed(self):
        backend = self.backend()
        backend.system.host_statistics = lambda *_: 1
        with self.assertRaises(monitor.SamplingError):
            backend.host_counters()
        backend = self.backend()
        backend.system.host_statistics64 = lambda *_: 1
        with self.assertRaises(monitor.SamplingError):
            backend.host_counters()

    def test_unknown_pressure_and_missing_swap_data_are_not_zero_usage(self):
        backend = self.backend()
        with patch.object(backend, "_sysctl", return_value=b""), self.assertRaises(monitor.SamplingError):
            backend.host_counters()
        with patch.object(backend, "_sysctl", return_value=bytes(monitor._SwapUsage())), \
                patch.object(backend, "_integer", return_value=0), self.assertRaises(monitor.SamplingError):
            backend.host_counters()

    def test_native_process_units_are_nanoseconds_bytes_and_kernel_birth(self):
        backend = self.backend()

        def counters(_pid, flavor, pointer):
            self.assertEqual(flavor, 2)
            value = pointer._obj
            value.user_time, value.system_time = 3_000_000_000, 2_000_000_000
            value.phys_footprint, value.resident_size = 150, 999
            value.proc_start_abstime = 10
            value.diskio_bytesread, value.diskio_byteswritten = 123, 456
            return 0

        backend.proc = SimpleNamespace(proc_pid_rusage=counters)
        with patch.object(monitor.os, "getpgid", return_value=400):
            sample = backend.process(101, 400)
        self.assertEqual(sample, {"start": 10, "memoryBytes": 150, "cpuNanoseconds": 5_000_000_000,
                                  "readBytes": 123, "writeBytes": 456})

    def test_native_process_group_change_or_missing_rusage_is_incomplete(self):
        backend = self.backend()
        backend.proc = SimpleNamespace(proc_pid_rusage=lambda *_: 1)
        with patch.object(monitor.os, "getpgid", return_value=400), self.assertRaises(monitor.SamplingError):
            backend.process(101, 400)
        with patch.object(monitor.os, "getpgid", return_value=401), self.assertRaises(monitor.SamplingError):
            backend.process(101, 400)

    def test_sdk_struct_prefix_sizes_and_offsets_are_architecture_portable(self):
        self.assertEqual(ctypes.sizeof(monitor._RUsageV2), 160)
        self.assertEqual(monitor._RUsageV2.diskio_bytesread.offset, 144)
        self.assertEqual(ctypes.sizeof(monitor._VMStatistics64), 160)
        self.assertEqual(monitor._VMStatistics64.swapins.offset, 112)
        self.assertEqual(ctypes.sizeof(monitor._SwapUsage), 32)

    def test_other_platform_does_not_silently_report_zero_resources(self):
        with patch.object(monitor.platform, "system", return_value="Linux"):
            with self.assertRaises(monitor.SamplingError):
                monitor.ResourceSampler().host()

    def test_process_listing_has_no_arguments_environment_or_path_fields(self):
        result = subprocess.CompletedProcess([], 0, f"{os.getpid()} 999 Mon Oct 19 10:00:00 2026\n101 400 stable birth\n")
        with patch.object(monitor.subprocess, "run", return_value=result) as run:
            self.assertEqual(monitor._process_listing(400), [{"pid": 101, "birth": "stable birth"}])
        self.assertEqual(run.call_args.args[0], ["/bin/ps", "-axo", "pid=,pgid=,lstart="])
        self.assertEqual(run.call_args.kwargs["timeout"], 3)

    def test_process_listing_failure_errors_do_not_leak_private_output(self):
        error = subprocess.CalledProcessError(1, ["ps"], stderr="private fixture")
        with patch.object(monitor.subprocess, "run", side_effect=error):
            with self.assertRaises(monitor.SamplingError) as raised:
                monitor._process_listing(400)
        self.assertNotIn("private fixture", str(raised.exception))
        self.assertTrue(raised.exception.__suppress_context__)

    def test_process_listing_malformed_or_truncated_output_is_rejected(self):
        observer = f"{os.getpid()} 999 Mon Oct 19 10:00:00 2026\n"
        for output in ("", "101 400 birth\n", observer + "invalid\n", observer + "101 400 a\n101 400 b\n"):
            result = subprocess.CompletedProcess([], 0, output)
            with self.subTest(output=output), patch.object(monitor.subprocess, "run", return_value=result):
                with self.assertRaises(monitor.SamplingError):
                    monitor._process_listing(400)


if __name__ == "__main__":
    unittest.main()
