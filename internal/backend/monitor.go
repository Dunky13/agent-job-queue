package backend

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const MaxSampleInterval = 10.0
const maxClockDrift = 1.0

type ResourceBackend interface {
	HostCounters() (Object, error)
	Members(int) ([]Object, error)
	Process(int, int) (Object, error)
}
type clockPair [2]float64
type resourceGroup struct {
	previous         *clockPair
	attempts         int
	processes        map[string]Object
	cpu, read, write uint64
}
type ResourceSampler struct {
	backend          ResourceBackend
	diskPath         string
	clock, wallClock func() float64
	hostPrevious     Object
	hostTime         *clockPair
	groups           map[int]*resourceGroup
}

var monitorEpoch = time.Now()

func NewResourceSampler(path string, backend ResourceBackend) *ResourceSampler {
	return &ResourceSampler{backend: backend, diskPath: path, clock: func() float64 { return time.Since(monitorEpoch).Seconds() }, wallClock: resourceNow, groups: map[int]*resourceGroup{}}
}
func resourceNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }
func (s *ResourceSampler) reader() (ResourceBackend, error) {
	if s.backend == nil {
		b, e := newPlatformResourceBackend(s.diskPath)
		if e != nil {
			return nil, e
		}
		s.backend = b
	}
	return s.backend, nil
}
func sampleInterval(a, b clockPair, collection bool) (float64, bool) {
	for _, v := range []float64{a[0], a[1], b[0], b[1]} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
	}
	elapsed, wall := b[0]-a[0], b[1]-a[1]
	return elapsed, elapsed >= 0 && elapsed <= MaxSampleInterval && wall >= 0 && wall <= MaxSampleInterval && (collection || (elapsed != 0 && wall != 0)) && math.Abs(elapsed-wall) <= maxClockDrift
}
func metricUint(v any) (uint64, bool) {
	switch x := v.(type) {
	case uint64:
		return x, true
	case uint32:
		return uint64(x), true
	case int:
		if x >= 0 {
			return uint64(x), true
		}
	case int64:
		if x >= 0 {
			return uint64(x), true
		}
	case float64:
		if x >= 0 && x < 18446744073709551616 && x == math.Trunc(x) {
			return uint64(x), true
		}
	}
	if n, ok := integer(v); ok && n >= 0 {
		return uint64(n), true
	}
	return 0, false
}
func metric(v any) float64 { n, _ := number(v); return n }
func counterDelta(current, previous uint64, bits int) (uint64, bool) {
	if current >= previous {
		return current - previous, true
	}
	if bits > 0 && bits < 64 {
		limit := uint64(1) << bits
		if float64(previous) >= float64(limit)*.9 && float64(current) < float64(limit)*.1 {
			return limit - previous + current, true
		}
	}
	return 0, false
}
func (s *ResourceSampler) Host() (Object, error) {
	reader, e := s.reader()
	if e != nil {
		return nil, e
	}
	started := clockPair{s.clock(), s.wallClock()}
	raw, e := reader.HostCounters()
	if e != nil {
		return nil, e
	}
	current := clockPair{s.clock(), s.wallClock()}
	_, valid := sampleInterval(started, current, true)
	previous, oldTime := s.hostPrevious, s.hostTime
	s.hostPrevious = nil
	s.hostTime = nil
	if valid {
		s.hostPrevious = raw
		s.hostTime = &current
	}
	complete := false
	busy := metric(raw["cpuCores"])
	swapIn, swapOut := 0.0, 0.0
	if oldTime != nil && valid && text(raw["machine"]) == text(previous["machine"]) {
		if elapsed, ok := sampleInterval(*oldTime, current, false); ok {
			nowTicks, oldTicks := list(raw["cpuTicks"]), list(previous["cpuTicks"])
			deltas := make([]uint64, 4)
			good := len(nowTicks) == 4 && len(oldTicks) == 4
			if good {
				for i := range deltas {
					a, aok := metricUint(nowTicks[i])
					b, bok := metricUint(oldTicks[i])
					delta, ok := counterDelta(a, b, 32)
					if !aok || !bok || !ok {
						good = false
					}
					deltas[i] = delta
				}
			}
			a, aok := metricUint(raw["swapInBytes"])
			b, bok := metricUint(previous["swapInBytes"])
			inDelta, inOK := counterDelta(a, b, 0)
			good = good && aok && bok && inOK
			a, aok = metricUint(raw["swapOutBytes"])
			b, bok = metricUint(previous["swapOutBytes"])
			outDelta, outOK := counterDelta(a, b, 0)
			good = good && aok && bok && outOK
			total := deltas[0] + deltas[1] + deltas[2] + deltas[3]
			if good && total > 0 {
				busy = metric(raw["cpuCores"]) * float64(total-deltas[2]) / float64(total)
				swapIn = float64(inDelta) / elapsed
				swapOut = float64(outDelta) / elapsed
				complete = true
			}
		}
	}
	result := Object{}
	for k, v := range raw {
		if k != "cpuTicks" && k != "swapInBytes" && k != "swapOutBytes" {
			result[k] = v
		}
	}
	result["sampledAt"] = current[1]
	result["cpuBusyCores"] = busy
	result["swapInBytesPerSecond"] = swapIn
	result["swapOutBytesPerSecond"] = swapOut
	result["complete"] = complete
	return result, nil
}
func (s *ResourceSampler) ForgetGroup(pgid int) { delete(s.groups, pgid) }
func memberKey(member Object) string {
	return fmt.Sprintf("%v\x00%s", member["pid"], strings.Join(strings.Fields(text(member["birth"])), " "))
}
func memberSet(members []Object) map[string]bool {
	result := map[string]bool{}
	for _, m := range members {
		result[memberKey(m)] = true
	}
	return result
}
func intersect(a, b map[string]bool) bool {
	for key := range a {
		if b[key] {
			return true
		}
	}
	return false
}
func (s *ResourceSampler) Group(pgid int, expected []Object) (Object, error) {
	if pgid <= 1 || pgid > math.MaxInt32 {
		return nil, fmt.Errorf("Invalid resource process group")
	}
	var ownership map[string]bool
	if expected != nil {
		if len(expected) == 0 {
			return nil, fmt.Errorf("Resource group ownership identities are required")
		}
		for _, m := range expected {
			pid, ok := schedulingInteger(m["pid"])
			birth, isString := m["birth"].(string)
			if !ok || pid <= 0 || !isString || strings.TrimSpace(birth) == "" {
				return nil, fmt.Errorf("Resource group ownership identities are invalid")
			}
		}
		ownership = memberSet(expected)
	}
	reader, e := s.reader()
	if e != nil {
		return nil, e
	}
	started := clockPair{s.clock(), s.wallClock()}
	members, e := reader.Members(pgid)
	if e != nil {
		return nil, e
	}
	identities := memberSet(members)
	if ownership != nil && !intersect(identities, ownership) {
		return nil, fmt.Errorf("Resource group ownership cannot be verified")
	}
	state := s.groups[pgid]
	if state == nil {
		state = &resourceGroup{processes: map[string]Object{}}
		s.groups[pgid] = state
	}
	baseline := state.attempts == 0
	state.attempts++
	complete := true
	type reading struct{ member, usage Object }
	sampled := []reading{}
	for _, member := range members {
		pid, _ := integer(member["pid"])
		usage, e := reader.Process(pid, pgid)
		if e != nil {
			complete = false
			continue
		}
		valid := true
		for _, key := range []string{"start", "memoryBytes", "cpuNanoseconds", "readBytes", "writeBytes"} {
			v, ok := metricUint(usage[key])
			valid = valid && ok && (key != "start" || v > 0)
		}
		if !valid {
			complete = false
			continue
		}
		sampled = append(sampled, reading{member, usage})
	}
	membersAfter, e := reader.Members(pgid)
	if e != nil {
		return nil, e
	}
	after := memberSet(membersAfter)
	if len(after) != len(identities) {
		complete = false
	}
	for key := range identities {
		if !after[key] {
			complete = false
		}
	}
	if ownership != nil && !intersect(after, ownership) {
		return nil, fmt.Errorf("Resource group ownership changed during sampling")
	}
	beforeCPU := state.cpu
	memory := uint64(0)
	count := 0
	for _, sample := range sampled {
		if !after[memberKey(sample.member)] {
			complete = false
			continue
		}
		identity := fmt.Sprintf("%s\x00%v", memberKey(sample.member), sample.usage["start"])
		old := state.processes[identity]
		if old == nil && len(state.processes) >= 32768 {
			return nil, fmt.Errorf("Resource process identity history reached its safe limit")
		}
		for _, counter := range []struct {
			key    string
			target *uint64
		}{{"cpuNanoseconds", &state.cpu}, {"readBytes", &state.read}, {"writeBytes", &state.write}} {
			value, _ := metricUint(sample.usage[counter.key])
			delta, ok := value, true
			if old != nil {
				prior, _ := metricUint(old[counter.key])
				delta, ok = counterDelta(value, prior, 0)
			}
			if !ok {
				complete = false
			} else {
				*counter.target += delta
			}
		}
		state.processes[identity] = sample.usage
		mem, _ := metricUint(sample.usage["memoryBytes"])
		memory += mem
		count++
	}
	current := clockPair{s.clock(), s.wallClock()}
	_, collectionValid := sampleInterval(started, current, true)
	elapsed, intervalValid := 0.0, false
	if state.previous != nil && collectionValid {
		elapsed, intervalValid = sampleInterval(*state.previous, current, false)
	}
	state.previous = nil
	if collectionValid {
		state.previous = &current
	}
	cores := 0.0
	if intervalValid {
		cores = float64(state.cpu-beforeCPU) / 1e9 / elapsed
	}
	return Object{"memoryBytes": memory, "cpuSeconds": float64(state.cpu) / 1e9, "cpuCores": cores, "processCount": count, "complete": complete && count == len(members) && count > 0 && intervalValid, "baseline": baseline && complete && count == len(members) && count > 0 && collectionValid, "readBytes": state.read, "writeBytes": state.write}, nil
}

// resourceProcessListing deliberately reads no arguments, environment or paths.
// The observer and duplicate checks reject a truncated or inconsistent census.
func resourceProcessListing(pgid int) ([]Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,pgid=,lstart=")
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	command.WaitDelay = time.Second
	output := &boundedOutput{limit: 8 * 1024 * 1024}
	command.Stdout = output
	command.Stderr = io.Discard
	if e := command.Run(); e != nil || output.exceeded {
		return nil, fmt.Errorf("Process resource identities are unavailable")
	}
	return parseResourceProcessListing(output.String(), pgid, os.Getpid())
}
func parseResourceProcessListing(output string, pgid, observer int) ([]Object, error) {
	members := []Object{}
	seen := map[int]bool{}
	observerSeen := false
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("Process resource identity listing is invalid")
		}
		pid, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil || pid <= 0 || group < 0 {
			return nil, fmt.Errorf("Process resource identity listing is invalid")
		}
		if seen[pid] {
			return nil, fmt.Errorf("Process resource identity listing is inconsistent")
		}
		seen[pid] = true
		observerSeen = observerSeen || pid == observer
		if group == pgid {
			members = append(members, Object{"pid": pid, "birth": strings.Join(fields[2:], " ")})
		}
	}
	if !observerSeen {
		return nil, fmt.Errorf("Process resource identity listing is incomplete")
	}
	return members, nil
}
