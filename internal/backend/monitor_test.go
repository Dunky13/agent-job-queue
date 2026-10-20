package backend

import (
	"fmt"
	"math"
	"testing"
)

type fakeResourceBackend struct {
	raw       Object
	members   []Object
	usages    map[int]Object
	fail      map[int]bool
	after     []Object
	calls     int
	onHost    func()
	onProcess func()
}

func fixtureHostCounters() Object {
	return Object{"totalMemoryBytes": 16 * GiB, "availableMemoryBytes": 8 * GiB, "cpuCores": 8, "cpuTicks": []any{100, 100, 100, 100}, "memoryPressure": "normal", "swapUsedBytes": GiB, "swapInBytes": 1000, "swapOutBytes": 2000, "diskFreeBytes": 10 * GiB, "machine": "opaque-capacity"}
}
func fixtureUsage(start, memory, cpu uint64) Object {
	return Object{"start": start, "memoryBytes": memory, "cpuNanoseconds": cpu, "readBytes": uint64(10), "writeBytes": uint64(20)}
}
func newFakeResourceBackend() *fakeResourceBackend {
	return &fakeResourceBackend{raw: fixtureHostCounters(), members: []Object{{"pid": 101, "birth": "stable birth"}}, usages: map[int]Object{101: fixtureUsage(1, 100, 1e9)}, fail: map[int]bool{}}
}
func cloneResourceObject(source Object) Object {
	result := Object{}
	for k, v := range source {
		result[k] = v
	}
	return result
}
func (b *fakeResourceBackend) HostCounters() (Object, error) {
	if b.onHost != nil {
		b.onHost()
	}
	return cloneResourceObject(b.raw), nil
}
func (b *fakeResourceBackend) Members(int) ([]Object, error) {
	b.calls++
	if b.after != nil && b.calls%2 == 0 {
		return b.after, nil
	}
	return b.members, nil
}
func (b *fakeResourceBackend) Process(pid, pgid int) (Object, error) {
	if b.onProcess != nil {
		b.onProcess()
	}
	if b.fail[pid] {
		return nil, fmt.Errorf("Counter unavailable")
	}
	return cloneResourceObject(b.usages[pid]), nil
}
func resourceFixture() (*ResourceSampler, *fakeResourceBackend, *float64, *float64) {
	b := newFakeResourceBackend()
	now, shift := 10.0, 0.0
	s := NewResourceSampler(".", b)
	s.clock = func() float64 { return now }
	s.wallClock = func() float64 { return 123.5 + now - 10 + shift }
	return s, b, &now, &shift
}
func mustHost(t *testing.T, s *ResourceSampler) Object {
	t.Helper()
	v, e := s.Host()
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func mustGroup(t *testing.T, s *ResourceSampler) Object {
	t.Helper()
	v, e := s.Group(400, nil)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestResourceHostIntervalsAndRollover(t *testing.T) {
	s, b, now, _ := resourceFixture()
	first := mustHost(t, s)
	if boolean(first["complete"]) || metric(first["cpuBusyCores"]) != 8 || metric(first["sampledAt"]) != 123.5 {
		t.Fatalf("Unsafe first interval: %v", first)
	}
	if _, exists := first["cpuTicks"]; exists {
		t.Fatal("Leaked raw interval counter")
	}
	*now += 2
	b.raw["cpuTicks"] = []any{120, 110, 170, 100}
	b.raw["swapInBytes"] = 9192
	b.raw["swapOutBytes"] = 6096
	next := mustHost(t, s)
	if !boolean(next["complete"]) || math.Abs(metric(next["cpuBusyCores"])-2.4) > .00001 || metric(next["swapInBytesPerSecond"]) != 4096 || metric(next["swapOutBytesPerSecond"]) != 2048 {
		t.Fatalf("Wrong interval: %v", next)
	}
	s, b, now, _ = resourceFixture()
	b.raw["cpuTicks"] = []any{uint64(1<<32) - 10, 100, 100, 100}
	mustHost(t, s)
	*now++
	b.raw["cpuTicks"] = []any{10, 110, 170, 100}
	v := mustHost(t, s)
	if !boolean(v["complete"]) || metric(v["cpuBusyCores"]) != 2.4 {
		t.Fatalf("CPU rollover lost: %v", v)
	}
}
func TestResourceHostGapsClockEditsAndRecovery(t *testing.T) {
	for _, gap := range [][2]float64{{600, 600}, {2, 602}, {2, -298}, {600, 2}, {0, 600}, {-1, 2}, {2, 0}, {2, 5}} {
		t.Run(fmt.Sprint(gap), func(t *testing.T) {
			s, b, now, shift := resourceFixture()
			mustHost(t, s)
			*now += gap[0]
			*shift += gap[1] - gap[0]
			b.raw["cpuTicks"] = []any{101, 100, 699, 100}
			v := mustHost(t, s)
			if boolean(v["complete"]) || metric(v["cpuBusyCores"]) != 8 {
				t.Fatalf("Gap treated as idle: %v", v)
			}
			*now += 2
			b.raw["cpuTicks"] = []any{103, 101, 700, 100}
			if !boolean(mustHost(t, s)["complete"]) {
				t.Fatal("Fresh interval did not recover")
			}
		})
	}
}
func TestResourceHostResetMachineAndCollectionGap(t *testing.T) {
	s, b, now, shift := resourceFixture()
	mustHost(t, s)
	*now++
	b.raw["cpuTicks"] = []any{1, 1, 1, 1}
	if boolean(mustHost(t, s)["complete"]) {
		t.Fatal("Counter reset accepted")
	}
	*now++
	b.raw["cpuTicks"] = []any{2, 2, 6, 1}
	if !boolean(mustHost(t, s)["complete"]) {
		t.Fatal("Reset failed recovery")
	}
	*now++
	b.raw["machine"] = "new machine"
	if boolean(mustHost(t, s)["complete"]) {
		t.Fatal("Machine change reused prior baseline")
	}
	b.onHost = func() { *shift += 600 }
	mustHost(t, s)
	b.onHost = nil
	*now += 2
	b.raw["cpuTicks"] = []any{3, 3, 7, 1}
	if boolean(mustHost(t, s)["complete"]) {
		t.Fatal("Collection gap baseline reused")
	}
	*now += 2
	b.raw["cpuTicks"] = []any{4, 4, 8, 1}
	if !boolean(mustHost(t, s)["complete"]) {
		t.Fatal("Fresh baseline failed recovery")
	}
}
func TestResourceGroupWorkersExitsPIDReuseAndResets(t *testing.T) {
	s, b, now, _ := resourceFixture()
	b.members = append(b.members, Object{"pid": 102, "birth": "worker birth"})
	b.usages[102] = fixtureUsage(2, 500, 3e9)
	first := mustGroup(t, s)
	if boolean(first["complete"]) || !boolean(first["baseline"]) || metric(first["memoryBytes"]) != 600 || metric(first["cpuSeconds"]) != 4 {
		t.Fatalf("Wrong baseline: %v", first)
	}
	*now += 2
	b.members = b.members[:1]
	b.usages[101] = fixtureUsage(1, 60, 2e9)
	next := mustGroup(t, s)
	if !boolean(next["complete"]) || metric(next["memoryBytes"]) != 60 || metric(next["cpuSeconds"]) != 5 || metric(next["cpuCores"]) != .5 {
		t.Fatalf("Exited worker lost/double counted: %v", next)
	}
	*now++
	b.usages[101] = fixtureUsage(9, 80, 5e8)
	next = mustGroup(t, s)
	if !boolean(next["complete"]) || metric(next["cpuSeconds"]) != 5.5 {
		t.Fatalf("PID reuse was not distinct: %v", next)
	}
	*now++
	b.usages[101] = fixtureUsage(9, 80, 1)
	next = mustGroup(t, s)
	if boolean(next["complete"]) || metric(next["cpuCores"]) != 0 || metric(next["cpuSeconds"]) != 5.5 {
		t.Fatalf("Counter reset invented work: %v", next)
	}
	s.ForgetGroup(400)
	if metric(mustGroup(t, s)["cpuSeconds"]) != 1e-9 {
		t.Fatal("Forgotten process group inherited totals")
	}
}
func TestResourceGroupGapsAndCollectionSuspend(t *testing.T) {
	for _, gap := range [][2]float64{{600, 600}, {2, 602}, {2, -298}, {600, 2}, {0, 600}, {-1, 2}, {2, 0}, {2, 5}} {
		t.Run(fmt.Sprint(gap), func(t *testing.T) {
			s, b, now, shift := resourceFixture()
			mustGroup(t, s)
			*now += gap[0]
			*shift += gap[1] - gap[0]
			b.usages[101] = fixtureUsage(1, 500, 3e9)
			v := mustGroup(t, s)
			if boolean(v["complete"]) || boolean(v["baseline"]) || metric(v["cpuCores"]) != 0 || metric(v["cpuSeconds"]) != 3 {
				t.Fatalf("Gap certified as complete: %v", v)
			}
			*now += 2
			b.usages[101] = fixtureUsage(1, 100, 5e9)
			if !boolean(mustGroup(t, s)["complete"]) {
				t.Fatal("Group did not recover")
			}
		})
	}
	s, b, now, shift := resourceFixture()
	b.onProcess = func() { *shift += 600 }
	if boolean(mustGroup(t, s)["baseline"]) {
		t.Fatal("Collection suspend accepted baseline exemption")
	}
	b.onProcess = nil
	*now += 2
	if boolean(mustGroup(t, s)["complete"]) {
		t.Fatal("Collection suspend reused prior baseline")
	}
	*now += 2
	b.usages[101] = fixtureUsage(1, 100, 2e9)
	if !boolean(mustGroup(t, s)["complete"]) {
		t.Fatal("Collection recovery did not resume")
	}
}
func TestResourceGroupOwnershipRacesAndInvalidMetrics(t *testing.T) {
	s, b, _, _ := resourceFixture()
	if _, e := s.Group(400, []Object{{"pid": 101, "birth": "old"}}); e == nil {
		t.Fatal("Ownership mismatch authorized resource read")
	}
	expected := []Object{{"pid": 999, "birth": "exited leader"}, {"pid": 101, "birth": "stable   birth"}}
	if _, e := s.Group(400, expected); e != nil {
		t.Fatal(e)
	}
	b.after = []Object{{"pid": 101, "birth": "reused"}}
	if _, e := s.Group(400, expected); e == nil {
		t.Fatal("Ownership changed mid-read")
	}
	b.after = nil
	b.fail[101] = true
	s.ForgetGroup(400)
	v := mustGroup(t, s)
	if boolean(v["baseline"]) || boolean(v["complete"]) {
		t.Fatal("Missing first reading gained baseline exemption")
	}
	delete(b.fail, 101)
	for _, usage := range []Object{{"memoryBytes": 0}, fixtureUsage(0, 1, 1), {"start": 1, "memoryBytes": -1, "cpuNanoseconds": 1, "readBytes": 1, "writeBytes": 1}, {"start": 1, "memoryBytes": 1, "cpuNanoseconds": true, "readBytes": 1, "writeBytes": 1}} {
		b.usages[101] = usage
		if boolean(mustGroup(t, s)["complete"]) {
			t.Fatalf("Invalid usage accepted: %v", usage)
		}
	}
	for _, pgid := range []int{-1, 0, 1, 1 << 31} {
		if _, e := s.Group(pgid, nil); e == nil {
			t.Fatalf("Invalid pgid %d accepted", pgid)
		}
	}
	if _, e := s.Group(400, []Object{}); e == nil {
		t.Fatal("Empty ownership accepted")
	}
}
func TestResourceBoundaries(t *testing.T) {
	for _, gap := range []float64{10, 10.001} {
		s, b, now, _ := resourceFixture()
		mustHost(t, s)
		mustGroup(t, s)
		*now += gap
		b.raw["cpuTicks"] = []any{110, 100, 110, 100}
		b.usages[101] = fixtureUsage(1, 100, 2e9)
		want := gap == 10
		if boolean(mustHost(t, s)["complete"]) != want || boolean(mustGroup(t, s)["complete"]) != want {
			t.Fatalf("Incorrect interval boundary %v", gap)
		}
	}
	if _, ok := sampleInterval(clockPair{math.NaN(), 0}, clockPair{1, 1}, false); ok {
		t.Fatal("NaN clock accepted")
	}
}

func TestResourceCensusRejectsTruncationDuplicatesAndMalformedRows(t *testing.T) {
	for _, output := range []string{"", "101 400 birth\n", "999 900 observer birth\ninvalid\n", "999 900 observer birth\n101 400 a\n101 400 b\n"} {
		if _, e := parseResourceProcessListing(output, 400, 999); e == nil {
			t.Fatalf("Unsafe resource census accepted: %q", output)
		}
	}
	members, e := parseResourceProcessListing("999 900 observer birth\n101 400 stable   birth\n", 400, 999)
	if e != nil || len(members) != 1 || members[0]["birth"] != "stable birth" {
		t.Fatalf("Valid census rejected: %v %v", members, e)
	}
}
