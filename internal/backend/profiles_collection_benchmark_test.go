package backend

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

var profileBenchmarkHook sync.Once
var profileBenchmarkConnections atomic.Uint64

// Run finite rounds with -run '^$' -bench BenchmarkProfileCollection -benchtime
// 100x -count 3. Connections are measured by the driver's actual open hook.
func BenchmarkProfileCollection(b *testing.B) {
	profileBenchmarkHook.Do(func() {
		sqlite.RegisterConnectionHook(func(sqlite.ExecQuerierContext, string) error { profileBenchmarkConnections.Add(1); return nil })
	})
	for _, fixture := range []struct {
		name     string
		count    int
		distinct bool
	}{{"same1", 1, false}, {"same30", 30, false}, {"distinct30", 30, true}} {
		for _, batched := range []bool{false, true} {
			mode := "individual"
			if batched {
				mode = "collection"
			}
			b.Run(fixture.name+"/"+mode, func(b *testing.B) {
				store, e := NewProfileStore(filepath.Join(b.TempDir(), "state"), false)
				if e != nil {
					b.Fatal(e)
				}
				defer store.Close()
				requests := []profileRequest{}
				seeded := map[string]bool{}
				for i := 0; i < fixture.count; i++ {
					identity := fixtureIdentity()
					if fixture.distinct {
						identity["family"] = fmt.Sprintf("%064x", i+1)
					}
					request := profileRequest{identity, Object{"title": "fixture compile", "project": "fixture", "executable": "go"}}
					requests = append(requests, request)
					family := text(identity["family"])
					if seeded[family] {
						continue
					}
					seeded[family] = true
					if e = store.Observe(identity, request.description); e != nil {
						b.Fatal(e)
					}
					for j := 0; j < 6; j++ {
						if e = store.Record(fmt.Sprintf("%d-%d", i, j), identity, "fixture-machine", Object{"memoryBytes": 128 * 1024 * 1024, "cpuCores": .5, "durationSeconds": 10}, true, profileZero()); e != nil {
							b.Fatal(e)
						}
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				opened := profileBenchmarkConnections.Load()
				for range b.N {
					if batched {
						c, e := store.collectProfiles(requests, "fixture-machine")
						if e != nil {
							b.Fatal(e)
						}
						valid, release, e := c.Validate()
						if e != nil || !valid {
							c.Close()
							b.Fatalf("Collection validation failed: %v %v", valid, e)
						}
						release()
						c.Close()
					} else {
						for _, request := range requests {
							if e = store.Observe(request.identity, request.description); e != nil {
								b.Fatal(e)
							}
							if _, e = store.Prediction(request.identity, "fixture-machine"); e != nil {
								b.Fatal(e)
							}
							if _, e = store.Match(request.identity); e != nil {
								b.Fatal(e)
							}
						}
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(profileBenchmarkConnections.Load()-opened)/float64(b.N), "connections/op")
			})
		}
	}
}

// This fixture includes metadata/identity reads, terminal selection, controller
// locks, durable status/ledger writes, and profile collection. Counter sampling
// and queue snapshots are simulated; no installed queue or process is touched.
func BenchmarkProfileControllerTick(b *testing.B) {
	profileBenchmarkHook.Do(func() {
		sqlite.RegisterConnectionHook(func(sqlite.ExecQuerierContext, string) error { profileBenchmarkConnections.Add(1); return nil })
	})
	for _, count := range []int{1, 30} {
		b.Run(fmt.Sprintf("jobs%d", count), func(b *testing.B) {
			root := b.TempDir()
			state := filepath.Join(root, "state")
			g := &Gate{State: state, Slot: filepath.Join(root, "slot"), Config: filepath.Join(root, "queue.json"), Jobs: filepath.Join(state, "jobs"), Receipts: filepath.Join(state, "receipts"), Metadata: filepath.Join(state, "metadata"), Cancelled: filepath.Join(state, "cancelled"), ControlEnv: map[string]string{"PATH": ToolPath}}
			if e := g.Prepare(); e != nil {
				b.Fatal(e)
			}
			snapshot := Object{"tasks": Object{}, "groups": Object{"default": Object{"parallel_tasks": 2, "status": "Running"}}}
			g.queueControl = func(operation string, _ Object) (Object, error) {
				if operation != "snapshot" {
					return nil, fmt.Errorf("Unexpected fixture operation: %s", operation)
				}
				return snapshot, nil
			}
			if e := WritePrivate(filepath.Join(state, "scheduling.json"), Object{"mode": "fixed", "generation": "benchmark-generation"}); e != nil {
				b.Fatal(e)
			}
			for i := 0; i < count; i++ {
				value := fmt.Sprintf("%032x", i+1)
				identity := fixtureIdentity()
				identity["family"] = fmt.Sprintf("%064x", i+1)
				if e := WritePrivate(filepath.Join(state, "workloads", value+".json"), identity); e != nil {
					b.Fatal(e)
				}
				if e := WritePrivate(filepath.Join(state, "metadata", value+".json"), Object{"title": "fixture compile", "project": "fixture", "executable": "go"}); e != nil {
					b.Fatal(e)
				}
				obj(snapshot["tasks"])[fmt.Sprint(i+1)] = Object{"command": "agent-heavy execute " + value, "status": Object{"Stashed": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
			}
			sampler, backend, now, _ := resourceFixture()
			sampler.wallClock = resourceNow
			if _, e := sampler.Host(); e != nil {
				b.Fatal(e)
			}
			controller, e := NewController(g, sampler, nil)
			if e != nil {
				b.Fatal(e)
			}
			defer controller.Close()
			advance := func() {
				*now += 2
				backend.raw["cpuTicks"] = []any{uint64(*now)*10 + 100, 100, uint64(*now)*90 + 100, 100}
			}
			advance()
			if _, e = controller.Tick(); e != nil {
				b.Fatal(e)
			}
			b.ReportAllocs()
			b.ResetTimer()
			opened := profileBenchmarkConnections.Load()
			for range b.N {
				advance()
				if _, e = controller.Tick(); e != nil {
					b.Fatal(e)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(profileBenchmarkConnections.Load()-opened)/float64(b.N), "connections/op")
		})
	}
}
