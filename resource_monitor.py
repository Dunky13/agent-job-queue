"""Read-only macOS resource observations for admission and workload learning.

Process counters are sampled lower bounds, not kernel-enforced job accounting.
Workers which start and exit between polls and peaks between polls are invisible.
``complete`` means that this observation and its interval were available; it does
not claim that an entire job lifetime was observed. No command arguments,
environment variables, executable paths, or hardware serial numbers are read.
"""

import ctypes
import hashlib
import math
import os
import platform
import subprocess
import time

MAX_SAMPLE_INTERVAL = 10.0
MAX_CLOCK_DRIFT = 1.0


class SamplingError(Exception):
    """A required observation could not be established safely."""


class _RUsageV2(ctypes.Structure):
    _fields_ = [("uuid", ctypes.c_uint8 * 16)] + [
        (name, ctypes.c_uint64) for name in (
            "user_time", "system_time", "pkg_idle_wkups", "interrupt_wkups", "pageins",
            "wired_size", "resident_size", "phys_footprint", "proc_start_abstime", "proc_exit_abstime",
            "child_user_time", "child_system_time", "child_pkg_idle_wkups", "child_interrupt_wkups",
            "child_pageins", "child_elapsed_abstime", "diskio_bytesread", "diskio_byteswritten",
        )
    ]


class _VMStatistics64(ctypes.Structure):
    # The stable VM_STATISTICS64 rev2 prefix, through swapped_count. Newer
    # kernels may return more fields into the deliberately oversized buffer.
    _fields_ = [
        ("free_count", ctypes.c_uint32), ("active_count", ctypes.c_uint32),
        ("inactive_count", ctypes.c_uint32), ("wire_count", ctypes.c_uint32),
        *[(name, ctypes.c_uint64) for name in (
            "zero_fill_count", "reactivations", "pageins", "pageouts", "faults", "cow_faults",
            "lookups", "hits", "purges",
        )],
        ("purgeable_count", ctypes.c_uint32), ("speculative_count", ctypes.c_uint32),
        *[(name, ctypes.c_uint64) for name in ("decompressions", "compressions", "swapins", "swapouts")],
        *[(name, ctypes.c_uint32) for name in (
            "compressor_page_count", "throttled_count", "external_page_count", "internal_page_count",
        )],
        ("total_uncompressed_pages_in_compressor", ctypes.c_uint64), ("swapped_count", ctypes.c_uint64),
    ]


class _SwapUsage(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in ("total", "available", "used")] + [
        ("pagesize", ctypes.c_uint32), ("encrypted", ctypes.c_int32),
    ]


def _process_listing(pgid: int) -> list[dict]:
    try:
        result = subprocess.run(
            ["/bin/ps", "-axo", "pid=,pgid=,lstart="],
            env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL": "C"},
            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=3, check=True,
        )
    except (OSError, subprocess.SubprocessError):
        raise SamplingError("Process resource identities are unavailable") from None
    if len(result.stdout) > 8 * 1024 * 1024:
        raise SamplingError("Process resource identity listing is too large")
    members = []
    observer_seen = False
    seen = set()
    for line in result.stdout.splitlines():
        fields = line.split(None, 2)
        if len(fields) != 3 or not fields[0].isdigit() or not fields[1].isdigit() or not fields[2].strip():
            raise SamplingError("Process resource identity listing is invalid")
        pid, group = int(fields[0]), int(fields[1])
        if pid in seen:
            raise SamplingError("Process resource identity listing is inconsistent")
        seen.add(pid)
        observer_seen |= pid == os.getpid()
        if group == pgid:
            members.append({"pid": pid, "birth": " ".join(fields[2].split())})
    if not observer_seen:
        raise SamplingError("Process resource identity listing is incomplete")
    return members


class _DarwinBackend:
    def __init__(self, disk_path: str):
        if platform.system() != "Darwin":
            raise SamplingError("Resource monitoring requires macOS")
        try:
            self.system = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
            self.proc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
            self.system.sysctlbyname.argtypes = [
                ctypes.c_char_p, ctypes.c_void_p, ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t,
            ]
            self.system.sysctlbyname.restype = ctypes.c_int
            self.system.mach_host_self.argtypes = []
            self.system.mach_host_self.restype = ctypes.c_uint32
            self.system.host_statistics.argtypes = [
                ctypes.c_uint32, ctypes.c_int, ctypes.c_void_p, ctypes.POINTER(ctypes.c_uint32),
            ]
            self.system.host_statistics.restype = ctypes.c_int
            self.system.host_statistics64.argtypes = self.system.host_statistics.argtypes
            self.system.host_statistics64.restype = ctypes.c_int
            self.system.host_page_size.argtypes = [ctypes.c_uint32, ctypes.POINTER(ctypes.c_uint32)]
            self.system.host_page_size.restype = ctypes.c_int
            self.proc.proc_pid_rusage.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_void_p]
            self.proc.proc_pid_rusage.restype = ctypes.c_int
            self.port = self.system.mach_host_self()
            self.disk_path = disk_path
            self.total = self._integer("hw.memsize")
            self.cores = self._integer("hw.logicalcpu")
            page_size = ctypes.c_uint32()
            if self.system.host_page_size(self.port, ctypes.byref(page_size)) != 0:
                raise SamplingError("Host memory page size is unavailable")
            self.page_size = page_size.value
            if self.total <= 0 or self.cores <= 0 or self.page_size <= 0:
                raise SamplingError("Host resource capacity is invalid")
            capacity = b"\0".join([
                self._sysctl("hw.model"), self._sysctl("hw.machine"),
                str(self.total).encode("ascii"), str(self.cores).encode("ascii"),
            ])
            self.machine = hashlib.sha256(capacity).hexdigest()[:24]
        except (OSError, AttributeError, ValueError):
            raise SamplingError("macOS resource APIs are unavailable") from None

    def _sysctl(self, name: str) -> bytes:
        size = ctypes.c_size_t()
        if self.system.sysctlbyname(name.encode("ascii"), None, ctypes.byref(size), None, 0) != 0:
            raise SamplingError("A required host resource metric is unavailable")
        if not 0 < size.value <= 4096:
            raise SamplingError("A host resource metric has an invalid size")
        buffer = ctypes.create_string_buffer(size.value)
        if self.system.sysctlbyname(name.encode("ascii"), buffer, ctypes.byref(size), None, 0) != 0:
            raise SamplingError("A required host resource metric is unavailable")
        return buffer.raw[:size.value]

    def _integer(self, name: str) -> int:
        raw = self._sysctl(name)
        if len(raw) not in (4, 8):
            raise SamplingError("A host resource counter is invalid")
        return int.from_bytes(raw, byteorder="little", signed=False)

    def host_counters(self) -> dict:
        ticks = (ctypes.c_uint32 * 4)()
        count = ctypes.c_uint32(4)
        if self.system.host_statistics(self.port, 3, ticks, ctypes.byref(count)) != 0 or count.value != 4:
            raise SamplingError("Host CPU counters are unavailable")
        buffer = (ctypes.c_uint32 * 256)()
        count = ctypes.c_uint32(256)
        if self.system.host_statistics64(self.port, 4, buffer, ctypes.byref(count)) != 0:
            raise SamplingError("Host memory counters are unavailable")
        if count.value * 4 < ctypes.sizeof(_VMStatistics64):
            raise SamplingError("Host memory counters are incomplete")
        vm = _VMStatistics64.from_buffer_copy(buffer)
        raw_swap = self._sysctl("vm.swapusage")
        if len(raw_swap) != ctypes.sizeof(_SwapUsage):
            raise SamplingError("Host swap usage is invalid")
        pressure = self._integer("kern.memorystatus_vm_pressure_level")
        if pressure not in (1, 2, 4):
            raise SamplingError("Host memory pressure is unavailable")
        try:
            disk = os.statvfs(self.disk_path)
        except OSError:
            raise SamplingError("Available disk capacity is unavailable") from None
        return {
            "totalMemoryBytes": self.total,
            # This is an estimate of free and reclaimable inactive memory.
            # Compressed pages and speculative pages are not added again.
            "availableMemoryBytes": min(self.total, (vm.free_count + vm.inactive_count) * self.page_size),
            "cpuCores": self.cores, "cpuTicks": list(ticks),
            "memoryPressure": {1: "normal", 2: "warning", 4: "critical"}[pressure],
            "swapUsedBytes": _SwapUsage.from_buffer_copy(raw_swap).used,
            "swapInBytes": vm.swapins * self.page_size, "swapOutBytes": vm.swapouts * self.page_size,
            "diskFreeBytes": disk.f_bavail * disk.f_frsize, "machine": self.machine,
        }

    def members(self, pgid: int) -> list[dict]:
        return _process_listing(pgid)

    def process(self, pid: int, pgid: int) -> dict:
        usage = _RUsageV2()
        try:
            if os.getpgid(pid) != pgid or self.proc.proc_pid_rusage(pid, 2, ctypes.byref(usage)) != 0:
                raise SamplingError("A process resource counter is unavailable")
            if os.getpgid(pid) != pgid or usage.proc_start_abstime == 0 or usage.proc_exit_abstime != 0:
                raise SamplingError("A process resource identity changed")
        except OSError:
            raise SamplingError("A process resource identity is unavailable") from None
        return {
            "start": usage.proc_start_abstime, "memoryBytes": usage.phys_footprint,
            "cpuNanoseconds": usage.user_time + usage.system_time,
            "readBytes": usage.diskio_bytesread, "writeBytes": usage.diskio_byteswritten,
        }


def _counter_delta(current: int, previous: int, bits: int | None = None) -> int | None:
    if current >= previous:
        return current - previous
    # Only accept an unmistakable unsigned rollover, not a reset or reboot.
    if bits is not None and previous >= (1 << bits) * 0.9 and current < (1 << bits) * 0.1:
        return (1 << bits) - previous + current
    return None


def _sample_interval(previous: tuple, current: tuple, *, collection: bool = False) -> float | None:
    """Reject gaps and suspend/clock changes instead of averaging across them.

    Darwin's monotonic clock can exclude sleep. Wall time bounds that gap, and
    comparison of the two clocks also catches a discontinuous wall-clock edit.
    """
    elapsed = current[0] - previous[0]
    wall_elapsed = current[1] - previous[1]
    if (not all(math.isfinite(value) for value in (*previous, *current))
            or not 0 <= elapsed <= MAX_SAMPLE_INTERVAL or not 0 <= wall_elapsed <= MAX_SAMPLE_INTERVAL
            or (not collection and (elapsed == 0 or wall_elapsed == 0))
            or abs(elapsed - wall_elapsed) > MAX_CLOCK_DRIFT):
        return None
    return elapsed


class ResourceSampler:
    """Stateful interval sampler. Discard or forget group state after each job.

    The first host/group reading has ``complete=False``. First/reset host CPU
    demand reserves all cores, because unavailable demand must not become zero.
    Group ``baseline=True`` identifies only a clean first census whose interval
    is not yet available. Missing or racing first observations have no baseline
    exemption. Totals retain observed exited workers, keyed by PID and kernel
    birth identity. A restart intentionally starts a fresh observation window.
    Intervals over ten seconds, suspend and clock discontinuities are incomplete
    observations, preserving the distinction between a recent timestamp and a
    continuously observed interval.
    """

    def __init__(self, disk_path: str = ".", *, backend=None, clock=None, wall_clock=None):
        self._backend = backend
        self._disk_path = disk_path
        self._clock = clock or time.monotonic
        self._wall_clock = wall_clock or time.time
        self._host_previous = None
        self._groups = {}

    def _reader(self):
        if self._backend is None:
            self._backend = _DarwinBackend(self._disk_path)
        return self._backend

    def host(self) -> dict:
        started = (self._clock(), self._wall_clock())
        raw = self._reader().host_counters()
        current = (self._clock(), self._wall_clock())
        collection_valid = _sample_interval(started, current, collection=True) is not None
        previous = self._host_previous
        self._host_previous = (*current, raw) if collection_valid else None
        complete = False
        busy = float(raw["cpuCores"])
        swap_in = swap_out = 0.0
        elapsed = _sample_interval(previous[:2], current) if previous is not None and collection_valid else None
        if elapsed is not None and raw["machine"] == previous[2]["machine"]:
            old = previous[2]
            deltas = [_counter_delta(current, before, 32) for current, before in zip(raw["cpuTicks"], old["cpuTicks"])]
            in_delta = _counter_delta(raw["swapInBytes"], old["swapInBytes"])
            out_delta = _counter_delta(raw["swapOutBytes"], old["swapOutBytes"])
            if len(deltas) == 4 and None not in deltas and in_delta is not None and out_delta is not None:
                total = sum(deltas)
                if total > 0:
                    busy = raw["cpuCores"] * (total - deltas[2]) / total
                    swap_in = in_delta / elapsed
                    swap_out = out_delta / elapsed
                    complete = True
        return {**{key: value for key, value in raw.items() if key not in ("cpuTicks", "swapInBytes", "swapOutBytes")},
                "sampledAt": current[1], "cpuBusyCores": float(busy),
                "swapInBytesPerSecond": swap_in, "swapOutBytesPerSecond": swap_out, "complete": complete}

    def forget_group(self, pgid: int):
        self._groups.pop(pgid, None)

    def group(self, pgid: int, expected_members: list[dict] | None = None) -> dict:
        if type(pgid) is not int or not 1 < pgid <= 2**31 - 1:
            raise SamplingError("Invalid resource process group")
        expected = None
        if expected_members is not None:
            if not isinstance(expected_members, list) or not expected_members:
                raise SamplingError("Resource group ownership identities are required")
            expected = set()
            for member in expected_members:
                if (not isinstance(member, dict) or type(member.get("pid")) is not int or member["pid"] <= 0
                        or not isinstance(member.get("birth"), str) or not member["birth"].strip()):
                    raise SamplingError("Resource group ownership identities are invalid")
                expected.add((member["pid"], " ".join(member["birth"].split())))
        reader = self._reader()
        started = (self._clock(), self._wall_clock())
        members = reader.members(pgid)
        identities = {(member["pid"], member["birth"]) for member in members}
        if expected is not None and not identities.intersection(expected):
            raise SamplingError("Resource group ownership cannot be verified")
        state = self._groups.setdefault(pgid, {
            "previous": None, "attempts": 0, "processes": {}, "cpu": 0, "read": 0, "write": 0,
        })
        baseline = state["attempts"] == 0
        state["attempts"] += 1
        complete = True
        memory = 0
        sampled = []
        for member in members:
            try:
                usage = reader.process(member["pid"], pgid)
            except SamplingError:
                complete = False
                continue
            required = ("start", "memoryBytes", "cpuNanoseconds", "readBytes", "writeBytes")
            if (any(type(usage.get(key)) is not int or usage[key] < 0 for key in required)
                    or usage["start"] == 0):
                complete = False
                continue
            sampled.append((member, usage))
        after = {(member["pid"], member["birth"]) for member in reader.members(pgid)}
        if after != identities:
            complete = False
        if expected is not None and not after.intersection(expected):
            raise SamplingError("Resource group ownership changed during sampling")
        before_cpu = state["cpu"]
        count = 0
        for member, usage in sampled:
            if (member["pid"], member["birth"]) not in after:
                complete = False
                continue
            identity = (member["pid"], member["birth"], usage["start"])
            if identity not in state["processes"] and len(state["processes"]) >= 32768:
                raise SamplingError("Resource process identity history reached its safe limit")
            old = state["processes"].get(identity)
            for target, key in (("cpu", "cpuNanoseconds"), ("read", "readBytes"), ("write", "writeBytes")):
                delta = usage[key] if old is None else _counter_delta(usage[key], old[key])
                if delta is None:
                    complete = False
                else:
                    state[target] += delta
            state["processes"][identity] = usage
            memory += usage["memoryBytes"]
            count += 1
        current = (self._clock(), self._wall_clock())
        collection_valid = _sample_interval(started, current, collection=True) is not None
        elapsed = (_sample_interval(state["previous"], current)
                   if state["previous"] is not None and collection_valid else None)
        state["previous"] = current if collection_valid else None
        return {"memoryBytes": memory, "cpuSeconds": state["cpu"] / 1_000_000_000,
                "cpuCores": (state["cpu"] - before_cpu) / 1_000_000_000 / elapsed if elapsed is not None else 0.0,
                "processCount": count, "complete": complete and count == len(members) and count > 0 and elapsed is not None,
                "baseline": baseline and complete and count == len(members) and count > 0 and collection_valid,
                "readBytes": state["read"], "writeBytes": state["write"]}
