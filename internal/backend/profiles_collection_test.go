package backend

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func collectionRequests(identities ...Object) []profileRequest {
	result := []profileRequest{}
	for _, identity := range identities {
		result = append(result, profileRequest{identity, Object{"title": "Fixture compile", "project": "fixture", "executable": "go"}})
	}
	return result
}
func mustProfileCollection(t *testing.T, s *ProfileStore, requests []profileRequest) *profileCollection {
	t.Helper()
	c, e := s.collectProfiles(requests, "fixture-machine")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.Close)
	return c
}
func TestProfileCollectionMatchesIndependentHistoryReads(t *testing.T) {
	store := fixtureProfileStore(t)
	a := fixtureIdentity()
	b := cloneResourceObject(a)
	b["family"] = strings.Repeat("d", 64)
	variant := cloneResourceObject(a)
	variant["variant"] = strings.Repeat("e", 64)
	for i, identity := range []Object{a, b, variant} {
		for j := 0; j < 6; j++ {
			recordProfile(t, store, fmt.Sprintf("%d-%d", i, j), identity, (i+1)*GiB, float64(i+1), true, profileZero(), 0)
		}
	}
	requests := collectionRequests(a, b, variant, a)
	first := mustProfileCollection(t, store, requests)
	first.Close()
	if e := store.connection(true, func(c profileSQL) error {
		machine := profileDigest([]any{"machine-v1", "fixture-machine"})
		if e := profileExec(c, "INSERT INTO floors VALUES (?,?,?,?)", machine, 7*GiB, 6.0, resourceNow()); e != nil {
			return e
		}
		if e := profileExec(c, "INSERT INTO floors VALUES ('*',?,?,?)", 6*GiB, 5.0, resourceNow()); e != nil {
			return e
		}
		if e := profileExec(c, "INSERT INTO floors VALUES ('other-machine',?,?,?)", 100*GiB, 100.0, resourceNow()); e != nil {
			return e
		}
		return profileExec(c, "INSERT INTO learning_retained VALUES (?,?,?,?,?,?,?)", machine, text(b["family"]), strings.Repeat("f", 64), 9*GiB, 8.0, 20.0, resourceNow())
	}); e != nil {
		t.Fatal(e)
	}
	if e := store.Edit(text(a["family"]), 0, "Operator floor", 5*GiB, 4.0); e != nil {
		t.Fatal(e)
	}
	if e := store.Merge(text(a["family"]), text(b["family"]), 1); e != nil {
		t.Fatal(e)
	}
	for _, reset := range []bool{false, true} {
		if reset {
			if e := store.Reset(text(b["family"]), 2); e != nil {
				t.Fatal(e)
			}
		}
		c := mustProfileCollection(t, store, requests)
		for i, request := range requests {
			prediction, e := store.Prediction(request.identity, "fixture-machine")
			if e != nil {
				t.Fatal(e)
			}
			match, e := store.Match(request.identity)
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(prediction, c.predictions[i]) || !reflect.DeepEqual(match, c.matches[i]) {
				t.Fatalf("Batch changed history semantics: prediction=%v/%v match=%v/%v", prediction, c.predictions[i], match, c.matches[i])
			}
		}
		c.predictions[0]["memoryBytes"] = 99 * GiB
		if metric(c.predictions[3]["memoryBytes"]) == 99*GiB {
			t.Fatal("Job maps alias each other")
		}
		c.Close()
	}
}
func TestProfileCollectionInvalidatesCorrectionsAndCompletions(t *testing.T) {
	for _, kind := range []string{"correction", "completion"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureProfileStore(t)
			identity := fixtureIdentity()
			requests := collectionRequests(identity)
			before := mustProfileCollection(t, store, requests)
			valid, release, e := before.Validate()
			if e != nil || !valid {
				t.Fatalf("Fresh collection rejected: %v %v", valid, e)
			}
			release()
			if kind == "correction" {
				e = store.Edit(text(identity["family"]), 0, "Bigger", 8*GiB, 4.0)
			} else {
				e = store.Record("completion", identity, "fixture-machine", Object{"memoryBytes": 8 * GiB, "durationSeconds": 10}, true, profileZero())
			}
			if e != nil {
				t.Fatal(e)
			}
			valid, release, e = before.Validate()
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			if valid {
				t.Fatal("Concurrent database commit authorized stale prediction")
			}
			release()
			after := mustProfileCollection(t, store, requests)
			if metric(after.predictions[0]["memoryBytes"]) < 8*GiB {
				t.Fatalf("New collection missed committed %s: %v", kind, after.predictions)
			}
			valid, release, e = after.Validate()
			if e != nil || !valid {
				t.Fatalf("Fresh replacement rejected: %v %v", valid, e)
			}
			release()
		})
	}
}
func TestProfileCollectionAdmissionReadLockSerializesWrites(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	c := mustProfileCollection(t, store, collectionRequests(identity))
	valid, release, e := c.Validate()
	if e != nil || !valid {
		t.Fatalf("Validation failed: %v %v", valid, e)
	}
	defer release()
	// A real second connection can obtain RESERVED and execute updates, but cannot
	// commit while admission owns SHARED. Its 250 ms busy timeout stays bounded.
	writer, e := store.openSession(true)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close()
	if _, e = writer.conn.ExecContext(profileContext, "PRAGMA busy_timeout=10;BEGIN IMMEDIATE;UPDATE learning_state SET revision=revision+1"); e != nil {
		t.Fatal(e)
	}
	if _, e = writer.conn.ExecContext(profileContext, "COMMIT"); e == nil {
		t.Fatal("Correction committed while stale grant could still be written")
	}
	release()
	if _, e = writer.conn.ExecContext(profileContext, "COMMIT"); e != nil {
		t.Fatal("Writer could not commit after admission release", e)
	}
	valid, release, e = c.Validate()
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if valid {
		t.Fatal("Serialized correction did not invalidate the next admission")
	}
}
func TestProfileUnchangedObservationsDoNotAcquireWriterLock(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	requests := collectionRequests(identity)
	c := mustProfileCollection(t, store, requests)
	c.Close()
	writer, e := store.openSession(true)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close()
	if _, e = writer.conn.ExecContext(profileContext, "BEGIN IMMEDIATE"); e != nil {
		t.Fatal(e)
	}
	if e = store.Observe(identity, requests[0].description); e != nil {
		t.Fatal("Unchanged observation requested another write lock", e)
	}
	c = mustProfileCollection(t, store, requests)
	c.Close()
}
func TestProfileCollectionRejectsReplacedFilesAndUnsafeSidecars(t *testing.T) {
	for _, kind := range []string{"replacement", "symlink", "permissions", "sidecar", "directory"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureProfileStore(t)
			c := mustProfileCollection(t, store, collectionRequests(fixtureIdentity()))
			var e error
			switch kind {
			case "replacement", "symlink":
				moved := store.path + ".old"
				if e = os.Rename(store.path, moved); e != nil {
					t.Fatal(e)
				}
				if kind == "symlink" {
					e = os.Symlink(moved, store.path)
				} else {
					var contents []byte
					contents, e = os.ReadFile(moved)
					if e == nil {
						e = os.WriteFile(store.path, contents, 0600)
					}
				}
			case "permissions":
				e = os.Chmod(store.path, 0644)
			case "sidecar":
				e = os.Symlink(store.path, store.path+"-journal")
			case "directory":
				e = os.Chmod(store.state, 0755)
			}
			if e != nil {
				t.Fatal(e)
			}
			if valid, release, e := c.Validate(); e == nil {
				release()
				t.Fatalf("Unsafe %s validated: %v", kind, valid)
			}
		})
	}
}
func TestProfileCollectionReadonlyAndLegacyNeverWrite(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	recordProfile(t, store, "legacy", identity, 3*GiB, 2, true, profileZero(), 0)
	for _, legacy := range []bool{false, true} {
		if legacy {
			if e := store.connection(true, func(c profileSQL) error {
				return profileExec(c, "DROP TABLE learning_state;DROP TABLE learning_members;DROP TABLE learning_profiles;DROP TABLE learning_retained;DROP TABLE learning_resets;DROP TABLE decisions")
			}); e != nil {
				t.Fatal(e)
			}
		}
		before, e := os.ReadFile(store.path)
		if e != nil {
			t.Fatal(e)
		}
		readonly, e := NewProfileStore(store.state, true)
		if e != nil {
			t.Fatal(e)
		}
		c := mustProfileCollection(t, readonly, collectionRequests(identity))
		prediction, e := readonly.Prediction(identity, "fixture-machine")
		if e != nil || !reflect.DeepEqual(prediction, c.predictions[0]) {
			t.Fatalf("Readonly history changed: %v %v", prediction, e)
		}
		if _, e = c.session.conn.ExecContext(profileContext, "DELETE FROM runs"); e == nil {
			t.Fatal("Readonly collection allowed writes")
		}
		c.Close()
		after, e := os.ReadFile(store.path)
		if e != nil || !bytes.Equal(before, after) {
			t.Fatal("Readonly collection mutated schema/history", e)
		}
	}
}

func TestProfileCollectionControllerRejectsConcurrentChanges(t *testing.T) {
	for _, kind := range []string{"correction", "completion"} {
		t.Run(kind, func(t *testing.T) {
			g, snapshot := schedulerFixtureGate(t)
			value := strings.Repeat("1", 32)
			identity := fixtureIdentity()
			if e := WritePrivate(filepath.Join(g.State, "workloads", value+".json"), identity); e != nil {
				t.Fatal(e)
			}
			obj(snapshot["tasks"])["1"] = Object{"command": "agent-heavy execute " + value, "status": Object{"Stashed": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
			writeSchedulerSnapshot(t, g, snapshot)
			sampler, backend, _, _ := resourceFixture()
			sampler.wallClock = resourceNow
			epoch := time.Now()
			sampler.clock = func() float64 { return time.Since(epoch).Seconds() }
			mustHost(t, sampler)
			backend.raw["cpuTicks"] = []any{110, 100, 290, 100}
			controller, e := NewController(g, sampler, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer controller.Close()
			original := g.queueControl
			calls := 0
			g.queueControl = func(operation string, payload Object) (Object, error) {
				if operation == "snapshot" {
					calls++
					if calls == 2 {
						if kind == "correction" {
							e = controller.store.Edit(text(identity["family"]), 0, "Concurrent correction", 12*GiB, 4.0)
						} else {
							e = controller.store.Record("concurrent-completion", identity, "opaque-capacity", Object{"memoryBytes": 12 * GiB, "durationSeconds": 10}, true, profileZero())
						}
						if e != nil {
							return nil, e
						}
					}
				}
				return original(operation, payload)
			}
			state, e := controller.Tick()
			if e != nil {
				t.Fatal(e)
			}
			if boolean(state["available"]) || boolean(state["admissionReady"]) || len(obj(state["jobs"])) != 0 {
				t.Fatalf("Stale collection exposed admission: %v", state)
			}
			ledger, e := SchedulingLedger(g, Object{"mode": "adaptive", "generation": "fixture-generation"})
			if e != nil {
				t.Fatal(e)
			}
			if len(obj(ledger["grants"])) != 0 {
				t.Fatal("Stale snapshot persisted a grant")
			}
			if data, _ := os.ReadFile(g.Config + ".calls"); strings.Contains(string(data), "start") {
				t.Fatal("Stale snapshot started a job")
			}
		})
	}
}

func TestProfileCollectionLargeFamilyQueryHasNoHostParameterLimit(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	recordProfile(t, store, "large-query", identity, 3*GiB, 2, true, profileZero(), 0)
	session, e := store.openSession(false)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	// Exercise the actual batch queries beyond the pinned SQLite driver's 32766
	// host parameter limit, without constructing thousands of result maps.
	families := []any{text(identity["family"])}
	for i := 0; i < 32768; i++ {
		families = append(families, fmt.Sprintf("%064x", i+1))
	}
	c := &profileCollection{predictions: make([]Object, 1), matches: make([]Object, 1)}
	if e = session.transaction(false, func(db profileSQL) error {
		return c.read(db, collectionRequests(identity), families, profileDigest([]any{"machine-v1", "fixture-machine"}), true)
	}); e != nil {
		t.Fatal(e)
	}
	if metric(c.predictions[0]["memoryBytes"]) < 3*GiB {
		t.Fatal("Large query dropped required history")
	}
}

func TestProfileCollectionControllerIncludesSameTickCompletion(t *testing.T) {
	g, snapshot := schedulerFixtureGate(t)
	identity := fixtureIdentity()
	waiting, done := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for _, value := range []string{waiting, done} {
		if e := WritePrivate(filepath.Join(g.State, "workloads", value+".json"), identity); e != nil {
			t.Fatal(e)
		}
	}
	ended := time.Now().Add(-time.Second)
	obj(snapshot["tasks"])["1"] = Object{"command": "agent-heavy execute " + waiting, "status": Object{"Stashed": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
	obj(snapshot["tasks"])["2"] = Object{"command": "agent-heavy execute " + done, "status": Object{"Done": Object{"start": ended.Add(-10 * time.Second).UTC().Format(time.RFC3339Nano), "end": ended.UTC().Format(time.RFC3339Nano), "result": "Success"}}, "priority": 0}
	writeSchedulerSnapshot(t, g, snapshot)
	sampler, backend, _, _ := resourceFixture()
	sampler.wallClock = resourceNow
	epoch := time.Now()
	sampler.clock = func() float64 { return time.Since(epoch).Seconds() }
	mustHost(t, sampler)
	backend.raw["cpuTicks"] = []any{110, 100, 290, 100}
	controller, e := NewController(g, sampler, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer controller.Close()
	sampled := float64(ended.UnixNano()) / 1e9
	controller.runs[done] = Object{"started": sampled - 10, "sampledAt": sampled, "peakMemoryBytes": 32 * GiB, "peakCpuCores": 2.0, "cpuSeconds": 20.0, "durationSeconds": 10.0, "complete": true, "samples": 3, "boot": controller.boot, "machine": "opaque-capacity"}
	state, e := controller.Tick()
	if e != nil {
		t.Fatal(e)
	}
	resources := obj(obj(state["jobs"])[waiting])
	if metric(obj(resources["prediction"])["memoryBytes"]) < 32*GiB || boolean(obj(resources["decision"])["allowed"]) {
		t.Fatalf("Same-collection completion failed to raise candidate reservation: %v", resources)
	}
	if data, _ := os.ReadFile(g.Config + ".calls"); strings.Contains(string(data), "start") {
		t.Fatal("Started using pre-completion prediction")
	}
}

func TestProfileCollectionDescriptionWritePreservesDurability(t *testing.T) {
	store := fixtureProfileStore(t)
	c := mustProfileCollection(t, store, collectionRequests(fixtureIdentity()))
	var synchronous, busy int
	var journal string
	if e := c.session.conn.QueryRowContext(profileContext, "PRAGMA synchronous").Scan(&synchronous); e != nil {
		t.Fatal(e)
	}
	if e := c.session.conn.QueryRowContext(profileContext, "PRAGMA journal_mode").Scan(&journal); e != nil {
		t.Fatal(e)
	}
	if e := c.session.conn.QueryRowContext(profileContext, "PRAGMA busy_timeout").Scan(&busy); e != nil {
		t.Fatal(e)
	}
	if synchronous != 2 || journal != "delete" || busy != 250 {
		t.Fatalf("Durability or bounded lock policy changed: sync=%d journal=%s busy=%d", synchronous, journal, busy)
	}
}

func TestProfileCollectionDescriptionBatchesRetainAllFamilies(t *testing.T) {
	store := fixtureProfileStore(t)
	requests := []profileRequest{}
	for i := 0; i < 129; i++ {
		identity := fixtureIdentity()
		identity["family"] = fmt.Sprintf("%064x", i+1)
		requests = append(requests, profileRequest{identity, Object{"title": fmt.Sprintf("Workload %d", i)}})
	}
	c := mustProfileCollection(t, store, requests)
	for i, match := range c.matches {
		if match["label"] != fmt.Sprintf("Workload %d", i) {
			t.Fatalf("Description batch lost workload %d: %v", i, match)
		}
	}
	valid, release, e := c.Validate()
	if e != nil || !valid {
		t.Fatalf("Batched descriptions left invalid snapshot: %v %v", valid, e)
	}
	release()
	if e = store.Edit(text(requests[0].identity["family"]), 0, "Preserved correction", 8*GiB, 4.0); e != nil {
		t.Fatal(e)
	}
	for i := range requests {
		requests[i].description["title"] = fmt.Sprintf("Updated workload %d", i)
	}
	requests[128].description["title"] = "Changed last family"
	fresh := mustProfileCollection(t, store, requests)
	if fresh.revision != 1 || fresh.matches[0]["label"] != "Preserved correction" || metric(fresh.predictions[0]["memoryBytes"]) < 8*GiB {
		t.Fatal("Description batches overwrote an operator correction", fresh.matches[0], fresh.predictions[0])
	}
	if fresh.matches[128]["label"] != "Changed last family" {
		t.Fatal("Last-family update lost across batch boundary")
	}
	valid, release, e = c.Validate()
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if valid {
		t.Fatal("Prior collection survived description update")
	}
}
