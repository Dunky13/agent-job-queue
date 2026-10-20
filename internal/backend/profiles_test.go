package backend

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixtureIdentity() Object {
	return Object{"family": strings.Repeat("a", 64), "variant": strings.Repeat("b", 64), "context": strings.Repeat("c", 64)}
}
func fixtureProfileStore(t *testing.T) *ProfileStore {
	t.Helper()
	store, e := NewProfileStore(filepath.Join(t.TempDir(), "state"), false)
	if e != nil {
		t.Fatal(e)
	}
	return store
}
func recordProfile(t *testing.T, store *ProfileStore, token string, identity Object, memory int, cpu float64, complete bool, code *int, sampled float64) {
	t.Helper()
	metrics := Object{"memoryBytes": memory, "cpuCores": cpu, "cpuSeconds": cpu * 10, "durationSeconds": 10}
	if sampled != 0 {
		metrics["sampledAt"] = sampled
	}
	if e := store.Record(token, identity, "fixture-machine", metrics, complete, code); e != nil {
		t.Fatal(e)
	}
}
func predictProfile(t *testing.T, store *ProfileStore, identity Object) Object {
	t.Helper()
	result, e := store.Prediction(identity, "fixture-machine")
	if e != nil {
		t.Fatal(e)
	}
	return result
}
func profileZero() *int { n := 0; return &n }
func TestProfileDigestPythonCompatibility(t *testing.T) {
	for _, entry := range []struct {
		value any
		want  string
	}{{[]any{"machine-v1", "opaque-capacity"}, "60db8f56debe0f074fc40a0702c94b3125398d2a2509392ec998e6373d23d998"}, {[]any{"run-v1", strings.Repeat("1", 32)}, "f6e05d4dab7be35bdc743136d9467c283b3c1e9cbe0ad6a8ef69ede81db8d4e3"}, {Object{"é": "🙂", "a": "<repo>/x", "x": "\u2028"}, "fde7d6a7596ee79b4180610a58a3d1f58fd4fb5824ec6838755a7dea4ec56369"}} {
		if got := profileDigest(entry.value); got != entry.want {
			t.Fatalf("Python hash changed: %s != %s", got, entry.want)
		}
	}
}
func TestProfileConfidenceRequiresRepeatedCompleteExactSuccess(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	cold := predictProfile(t, store, identity)
	if cold["source"] != "prior" || cold["confidence"] != "unknown" || metric(cold["memoryBytes"]) < GiB {
		t.Fatalf("Unsafe cold estimate: %v", cold)
	}
	for i := 0; i < 12; i++ {
		recordProfile(t, store, fmt.Sprint(i), identity, 128*1024*1024, .5, true, profileZero(), 0)
		estimate := predictProfile(t, store, identity)
		if i < 4 && metric(estimate["memoryBytes"]) < GiB {
			t.Fatal("Prior dropped before five successes")
		}
		if i == 4 && (estimate["confidence"] != "medium" || metric(estimate["memoryBytes"]) >= GiB) {
			t.Fatalf("Five successes not learned: %v", estimate)
		}
	}
	if predictProfile(t, store, identity)["confidence"] != "high" {
		t.Fatal("Twelve exact successes not high confidence")
	}
	failed := 137
	recordProfile(t, store, "failed-large", identity, 6*GiB, 7, true, &failed, 0)
	estimate := predictProfile(t, store, identity)
	if estimate["confidence"] != "low" || metric(estimate["samples"]) != 12 || metric(estimate["memoryBytes"]) < 6*GiB || metric(estimate["cpuCores"]) < 7 {
		t.Fatalf("Failed peak erased: %v", estimate)
	}
}
func TestProfileBorrowedFamilyCensoredPeaksAndMachineIsolation(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	for i := 0; i < 12; i++ {
		recordProfile(t, store, fmt.Sprint(i), identity, 8*GiB, 4, true, profileZero(), 0)
	}
	changed := cloneResourceObject(identity)
	changed["variant"] = strings.Repeat("d", 64)
	borrowed := predictProfile(t, store, changed)
	if borrowed["source"] != "family" {
		t.Fatal("Changed variant did not borrow")
	}
	recordProfile(t, store, "censored", changed, 64*1024*1024, .1, false, nil, 0)
	for i := 0; i < 5; i++ {
		estimate := predictProfile(t, store, changed)
		if estimate["source"] != "family" || metric(estimate["memoryBytes"]) < metric(borrowed["memoryBytes"]) {
			t.Fatalf("Censored exact prefix replaced family evidence: %v", estimate)
		}
		recordProfile(t, store, fmt.Sprintf("new-%d", i), changed, 64*1024*1024, .1, true, profileZero(), 0)
	}
	exact := predictProfile(t, store, changed)
	if exact["source"] != "variant" || exact["confidence"] != "medium" || metric(exact["memoryBytes"]) >= metric(borrowed["memoryBytes"]) {
		t.Fatalf("Repeat exact successes did not separate: %v", exact)
	}
	other, e := store.Prediction(changed, "other-machine")
	if e != nil || other["source"] != "prior" {
		t.Fatal("History crossed machine capacity identity")
	}
	recordProfile(t, store, "larger-censored", changed, 24*GiB, 12, false, nil, 0)
	if metric(predictProfile(t, store, changed)["memoryBytes"]) < 24*GiB {
		t.Fatal("Censored peak lost")
	}
}
func TestProfileIdempotencePrivacyAndInvalidMetrics(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	recordProfile(t, store, "private-token", identity, 3*GiB, .5, false, nil, 0)
	recordProfile(t, store, "private-token", identity, 32*1024*1024, .5, true, profileZero(), 0)
	estimate := predictProfile(t, store, identity)
	if metric(estimate["samples"]) != 1 || metric(estimate["memoryBytes"]) < 3*GiB {
		t.Fatalf("Idempotent sample lost peak: %v", estimate)
	}
	if e := store.Record("private-token", identity, "different-machine", Object{}, false, nil); e == nil {
		t.Fatal("Token changed machine identity")
	}
	contents, e := os.ReadFile(store.path)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(contents, []byte("private-token")) || bytes.Contains(contents, []byte("fixture-machine")) {
		t.Fatal("History leaked raw token or machine")
	}
	for _, metrics := range []Object{{"cpuCores": math.NaN()}, {"memoryBytes": -1}, {"durationSeconds": true}, {"memoryBytes": float64(1 << 64)}, {"sampledAt": resourceNow() + 1000}, {"cpuSeconds": math.MaxFloat64, "durationSeconds": math.SmallestNonzeroFloat64}} {
		if e := store.Record("invalid", identity, "host", metrics, true, profileZero()); e == nil {
			t.Fatalf("Invalid metrics accepted: %v", metrics)
		}
	}
	readonly, e := NewProfileStore(store.state, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = readonly.Record("blocked", identity, "host", Object{}, false, nil); e == nil {
		t.Fatal("Readonly history allowed mutation")
	}
}
func TestProfileStaleSuccessesCannotBeRefreshedByFailedPrefixes(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	old := resourceNow() - 60*profileDay
	for i := 0; i < 12; i++ {
		recordProfile(t, store, fmt.Sprint(i), identity, 128*1024*1024, .5, true, profileZero(), old)
	}
	before := predictProfile(t, store, identity)
	for _, value := range []string{"new-partial", "0"} {
		recordProfile(t, store, value, identity, 64*1024*1024, .1, false, nil, 0)
		estimate := predictProfile(t, store, identity)
		if metric(estimate["memoryBytes"]) < GiB || estimate["confidence"] != "low" || metric(estimate["samples"]) != 12 {
			t.Fatalf("Failed prefix refreshed old successes: %v (was %v)", estimate, before)
		}
	}
}
func TestProfileStorageRejectsUnsafeFilesAndSidecars(t *testing.T) {
	source := fixtureProfileStore(t)
	for _, kind := range []string{"symlink", "hardlink", "permissions", "corrupt", "journal-symlink"} {
		t.Run(kind, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			if e := os.Mkdir(state, 0700); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(state, "resource-history.sqlite3")
			var e error
			switch kind {
			case "symlink":
				e = os.Symlink(source.path, path)
			case "hardlink":
				e = os.Link(source.path, path)
			case "permissions":
				e = os.WriteFile(path, []byte("fixture"), 0644)
			case "corrupt":
				e = os.WriteFile(path, []byte("corrupt history"), 0600)
			case "journal-symlink":
				e = os.Symlink(filepath.Join(state, "missing"), path+"-journal")
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = NewProfileStore(state, false); e == nil {
				t.Fatalf("Unsafe %s accepted", kind)
			}
		})
	}
	state := filepath.Join(t.TempDir(), "public")
	os.Mkdir(state, 0755)
	os.Chmod(state, 0755)
	if _, e := NewProfileStore(state, false); e == nil {
		t.Fatal("Public history directory accepted")
	}
}
func TestProfileReadonlyAbsentAndLegacySchemaNeverMigrate(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	store, e := NewProfileStore(state, true)
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := store.Catalog(0, 100, "")
	if e != nil || metric(catalog["total"]) != 0 {
		t.Fatal("Empty readonly catalog unavailable")
	}
	if _, e = os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("Readonly opened absent state with mutation")
	}
	store = fixtureProfileStore(t)
	identity := fixtureIdentity()
	recordProfile(t, store, "legacy", identity, 2*GiB, 2, true, profileZero(), 0)
	if e = store.connection(true, func(c profileSQL) error {
		for _, table := range []string{"learning_state", "learning_members", "learning_profiles", "learning_resets", "learning_retained", "decisions"} {
			if e := profileExec(c, "DROP TABLE "+table); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(store.path)
	readonly, e := NewProfileStore(store.state, true)
	if e != nil {
		t.Fatal(e)
	}
	detail, e := readonly.Detail(text(identity["family"]), "fixture-machine")
	if e != nil || metric(obj(detail["profile"])["successCount"]) != 1 {
		t.Fatalf("Legacy catalog unreadable: %v %v", detail, e)
	}
	if _, e = readonly.Prediction(identity, "fixture-machine"); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("Readonly client migrated legacy schema")
	}
	if _, e = NewProfileStore(store.state, false); e != nil {
		t.Fatal("Writer could not add schema", e)
	}
}
func TestProfileCorrectionsMergeSplitResetAndRevisionCAS(t *testing.T) {
	store := fixtureProfileStore(t)
	a := fixtureIdentity()
	b := cloneResourceObject(a)
	b["family"] = strings.Repeat("d", 64)
	for _, id := range []Object{a, b} {
		if e := store.Observe(id, Object{"title": "Fixture workload", "project": "project", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		recordProfile(t, store, "run-"+text(id["family"]), id, 3*GiB, 2, true, profileZero(), 0)
	}
	aID, bID := text(a["family"]), text(b["family"])
	if e := store.Edit(aID, 0, "Larger", 4*GiB, 4.0); e != nil {
		t.Fatal(e)
	}
	if e := store.Merge(aID, bID, 1); e != nil {
		t.Fatal(e)
	}
	detail, e := store.Detail(bID, "fixture-machine")
	if e != nil || metric(obj(detail["profile"])["memberCount"]) != 2 || metric(obj(detail["profile"])["memoryFloorBytes"]) != 4*GiB {
		t.Fatalf("Merge lost families/floors: %v %v", detail, e)
	}
	if metric(predictProfile(t, store, b)["samples"]) != 1 {
		t.Fatal("Merge fabricated exact successes")
	}
	newID, e := store.Split(bID, bID, 2)
	if e != nil {
		t.Fatal(e)
	}
	if newID == bID {
		t.Fatal("Splitting root reused grouped profile identity")
	}
	if metric(predictProfile(t, store, b)["memoryBytes"]) < 4*GiB {
		t.Fatal("Split lost operator reservation")
	}
	sampled := resourceNow() - 1
	if e = store.Reset(newID, 3); e != nil {
		t.Fatal(e)
	}
	recordProfile(t, store, "old-replayed-completion", b, 32*GiB, 8, true, profileZero(), sampled)
	estimate := predictProfile(t, store, b)
	if metric(estimate["samples"]) != 0 || estimate["source"] != "prior" || metric(estimate["memoryBytes"]) != 4*GiB {
		t.Fatalf("Reset replay resurrected history or dropped floor: %v", estimate)
	}
	if e = store.Edit(newID, 0, "Stale", nil, nil); e == nil {
		t.Fatal("Stale correction succeeded")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, label := range []string{"First", "Second"} {
		wg.Add(1)
		go func(label string) { defer wg.Done(); results <- store.Edit(newID, 4, label, nil, nil) }(label)
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("Concurrent CAS had %d winners", successes)
	}
}
func TestProfileDecisionCoalescingAndAdditiveSchema(t *testing.T) {
	store := fixtureProfileStore(t)
	prediction := predictProfile(t, store, fixtureIdentity())
	if e := store.RecordDecision("private-decision", "shadow", prediction, false, "Queue is paused", 100); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(store.path)
	if e := store.RecordDecision("private-decision", "shadow", prediction, false, "Queue is paused", 200); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("Unchanged decision mutated its timestamp/database")
	}
	if e := store.RecordDecision("private-decision", "adaptive", prediction, true, "Fits current headroom", 201); e != nil {
		t.Fatal(e)
	}
	if e := store.connection(false, func(c profileSQL) error {
		rows, e := profileRows(c, "SELECT * FROM decisions ORDER BY id")
		if e != nil {
			return e
		}
		if len(rows) != 2 || metric(rows[0]["sampled"]) != 100 {
			t.Fatalf("Unexpected coalesced decisions: %v", rows)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	for _, reason := range []string{" padded ", "control\x00", strings.Repeat("x", 257)} {
		if e := store.RecordDecision("job", "shadow", prediction, false, reason, 100); e == nil {
			t.Fatal("Uncontrolled audit reason persisted")
		}
	}
}
func TestProfileRetentionPreservesFamilyUpperEvidence(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	recordProfile(t, store, "upper", identity, 8*GiB, 4, true, profileZero(), 0)
	if e := store.connection(true, func(c profileSQL) error {
		for i := 0; i < maxProfileEnvelopes; i++ {
			variant := profileDigest([]any{"retention-fixture", i})
			if e := profileExec(c, "INSERT INTO envelopes VALUES (?,?,?,?,?,?,?)", profileDigest([]any{"machine-v1", "fixture-machine"}), identity["family"], variant, 128*1024*1024, .5, 10, resourceNow()+1); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	changed := cloneResourceObject(identity)
	changed["variant"] = strings.Repeat("e", 64)
	recordProfile(t, store, "trigger-pruning", changed, 128*1024*1024, .5, true, profileZero(), 0)
	if metric(predictProfile(t, store, identity)["memoryBytes"]) < 8*GiB {
		t.Fatal("Envelope pruning erased old peak")
	}
	if e := store.connection(false, func(c profileSQL) error {
		var envelopes, retained int
		c.QueryRowContext(profileContext, "SELECT COUNT(*) FROM envelopes").Scan(&envelopes)
		c.QueryRowContext(profileContext, "SELECT COUNT(*) FROM learning_retained").Scan(&retained)
		if envelopes > maxProfileEnvelopes || retained == 0 {
			t.Fatalf("History bounds not enforced: %d %d", envelopes, retained)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileUnicodeSearchAndLargePredictionsRemainSafe(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	if e := store.Observe(identity, Object{"title": "Straße tests"}); e != nil {
		t.Fatal(e)
	}
	catalog, e := store.Catalog(0, 100, "STRASSE")
	if e != nil || metric(catalog["total"]) != 1 {
		t.Fatalf("Unicode casefold compatibility changed: %v %v", catalog, e)
	}
	if e = store.connection(true, func(c profileSQL) error {
		return profileExec(c, "INSERT INTO envelopes VALUES (?,?,?,?,?,?,?)", profileDigest([]any{"machine-v1", "fixture-machine"}), identity["family"], identity["variant"], int64(1<<62), 1.0, 10.0, resourceNow())
	}); e != nil {
		t.Fatal(e)
	}
	estimate := predictProfile(t, store, identity)
	if metric(estimate["memoryBytes"]) <= 0 {
		t.Fatal("Large prediction wrapped negative")
	}
	if allowed, _ := ResourceDecision(fixtureResourceHost(), estimate, nil, 2); allowed {
		t.Fatal("Machine-exceeding prediction authorized work")
	}
	if profileRoundedCPU(1.2345) != 1.234 {
		t.Fatal("CPU rounding differs from Python round(value,3)")
	}
}

func TestProfileOuterRetentionFallsBackToMachineAndGlobalFloors(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	machine := profileDigest([]any{"machine-v1", "fixture-machine"})
	if e := store.Observe(identity, Object{"title": "Pinned reset group"}); e != nil {
		t.Fatal(e)
	}
	if e := store.Edit(text(identity["family"]), 0, "Pinned reset group", nil, nil); e != nil {
		t.Fatal(e)
	}
	if e := store.Reset(text(identity["family"]), 1); e != nil {
		t.Fatal(e)
	}
	if e := store.connection(true, func(c profileSQL) error {
		if e := profileExec(c, "INSERT INTO learning_retained VALUES (?,?,?,?,?,?,?)", machine, identity["family"], identity["variant"], 8*GiB, 6.0, 100.0, resourceNow()-100); e != nil {
			return e
		}
		for i := 0; i < maxRetainedEnvelopes; i++ {
			variant := profileDigest([]any{"outer-retention-fixture", i})
			if e := profileExec(c, "INSERT INTO learning_retained VALUES (?,?,?,?,?,?,?)", machine, strings.Repeat("d", 64), variant, 128*1024*1024, .5, 10.0, resourceNow()); e != nil {
				return e
			}
		}
		for i := 0; i < maxProfileMachines+1; i++ {
			if e := profileExec(c, "INSERT INTO floors VALUES (?,?,?,?)", profileDigest([]any{"floor-machine", i}), 2*GiB, 3.0, resourceNow()+float64(i)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	changed := cloneResourceObject(identity)
	changed["variant"] = strings.Repeat("e", 64)
	recordProfile(t, store, "trigger-outer-retention", changed, 128*1024*1024, .5, true, profileZero(), 0)
	estimate := predictProfile(t, store, identity)
	if metric(estimate["memoryBytes"]) < 8*GiB || metric(estimate["cpuCores"]) < 6 {
		t.Fatalf("Outer pruning lost upper evidence after reset: %v", estimate)
	}
	if e := store.connection(false, func(c profileSQL) error {
		var retained, machines int
		if e := c.QueryRowContext(profileContext, "SELECT COUNT(*) FROM learning_retained").Scan(&retained); e != nil {
			return e
		}
		if e := c.QueryRowContext(profileContext, "SELECT COUNT(*) FROM floors WHERE machine!='*'").Scan(&machines); e != nil {
			return e
		}
		global, e := profileRow(c, "SELECT * FROM floors WHERE machine='*'")
		if e != nil {
			return e
		}
		if retained > maxRetainedEnvelopes || machines > maxProfileMachines || global == nil {
			t.Fatalf("Retention limits or global fallback failed: retained=%d machines=%d global=%v", retained, machines, global)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileCatalogHugeOffsetDoesNotOverflow(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := fixtureIdentity()
	if e := store.Observe(identity, Object{}); e != nil {
		t.Fatal(e)
	}
	result, e := store.Catalog(math.MaxInt, 100, "")
	if e != nil || len(list(result["profiles"])) != 0 || metric(result["total"]) != 1 {
		t.Fatalf("Huge offset failed safely: %v %v", result, e)
	}
}
