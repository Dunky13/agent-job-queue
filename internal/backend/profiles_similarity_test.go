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

func similarityIdentity(name string) Object {
	return Object{"family": profileDigest([]any{"family", name}), "variant": profileDigest([]any{"variant", name}), "context": profileDigest("context")}
}

func similarityDescription(title, scope, kind string) Object {
	return Object{"title": title, "project": "fixture", "executable": "go", "learningScope": profileDigest(scope), "learningKind": kind}
}

func TestProfileSimilaritySharesConservativeLearningWithoutExactSuccesses(t *testing.T) {
	store := fixtureProfileStore(t)
	a, b := similarityIdentity("first target"), similarityIdentity("second target")
	for i, identity := range []Object{a, b} {
		if e := store.Observe(identity, similarityDescription(fmt.Sprintf("Issue %d regression tests", i), "same-package", "go:tests")); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 3; i++ {
		id := cloneResourceObject(a)
		id["variant"] = profileDigest(i)
		recordProfile(t, store, fmt.Sprint(i), id, GiB, 1, true, profileZero(), 0)
	}
	// Failed prefixes may raise the envelope, but never the success count.
	recordProfile(t, store, "censored peak", a, 2*GiB, 2, false, nil, 0)
	prediction := predictProfile(t, store, b)
	if prediction["source"] != "family" || prediction["confidence"] != "low" || metric(prediction["samples"]) != 0 || metric(prediction["familySamples"]) != 3 {
		t.Fatal("Borrowed observations became exact evidence", prediction)
	}
	if metric(prediction["memoryBytes"]) < 3*GiB || metric(prediction["cpuCores"]) < 3 {
		t.Fatal("Grouping lost a censored peak or the transfer margin", prediction)
	}
	match, e := store.Match(b)
	if e != nil || match["reason"] != "Automatic similar workloads" {
		t.Fatal("Automatic grouping is not explained", match, e)
	}
	detail, e := store.Detail(text(match["id"]), "fixture-machine")
	if e != nil || metric(obj(detail["profile"])["memberCount"]) != 2 || metric(obj(detail["prediction"])["samples"]) != 0 {
		t.Fatal("Group detail invented exact runs", detail, e)
	}
	collection := mustProfileCollection(t, store, []profileRequest{{b, similarityDescription("Issue 3 tests", "same-package", "go:tests")}})
	if !reflect.DeepEqual(prediction, collection.predictions[0]) {
		t.Fatal("Batched scheduler missed shared learning", prediction, collection.predictions[0])
	}
	other, e := store.Prediction(b, "other-machine")
	if e != nil || metric(other["familySamples"]) != 0 || other["source"] != "prior" {
		t.Fatal("Shared learning crossed machine boundaries", other, e)
	}
	// Shared history cannot admit an unmeasured worker/setup configuration.
	active := []Object{resourceActive(fixturePrediction(), GiB, 1, true)}
	host := fixtureResourceHost()
	host["availableMemoryBytes"] = 12 * GiB
	host["cpuBusyCores"] = 1.0
	if allowed, _ := ResourceDecision(host, prediction, active, 2); allowed {
		t.Fatal("An unmeasured exact configuration borrowed familiarity")
	}
	recordProfile(t, store, "first exact borrower", b, GiB, 1, true, profileZero(), 0)
	prediction = predictProfile(t, store, b)
	if allowed, _ := ResourceDecision(host, prediction, active, 2); allowed {
		t.Fatal("Shared group runs replaced the exact-run overlap guard")
	}
	for i := 0; i < 2; i++ {
		recordProfile(t, store, fmt.Sprintf("repeat borrower %d", i), b, GiB, 1, true, profileZero(), 0)
	}
	prediction = predictProfile(t, store, b)
	if allowed, reason := ResourceDecision(host, prediction, active, 2); !allowed {
		t.Fatal("Repeated exact measurements could not use their shared envelope", reason)
	}
}

func TestProfileSimilaritySeparatesScopeOperationsAndOperatorCorrections(t *testing.T) {
	store := fixtureProfileStore(t)
	identities := []Object{}
	for i, entry := range []struct{ scope, kind string }{
		{"package-one", "go:tests"}, {"package-one", "go:tests"},
		{"package-two", "go:tests"}, {"package-one", "go:build"}, {"package-one", "go:race-tests"},
	} {
		identity := similarityIdentity(fmt.Sprint(i))
		identities = append(identities, identity)
		if e := store.Observe(identity, similarityDescription("Tests", entry.scope, entry.kind)); e != nil {
			t.Fatal(e)
		}
	}
	catalog, e := store.Catalog(0, 100, "")
	if e != nil || metric(catalog["total"]) != 4 {
		t.Fatal("Different packages or operations grouped", catalog, e)
	}
	match, _ := store.Match(identities[0])
	revision, _ := store.Revision()
	if _, e = store.Split(text(match["id"]), text(identities[0]["family"]), revision); e != nil {
		t.Fatal(e)
	}
	for _, identity := range identities[:2] {
		if e = store.Observe(identity, similarityDescription("Renamed regression tests", "package-one", "go:tests")); e != nil {
			t.Fatal(e)
		}
	}
	first, _ := store.Match(identities[0])
	second, _ := store.Match(identities[1])
	if first["id"] == second["id"] {
		t.Fatal("Automatic learning undid an operator split")
	}
	revision, _ = store.Revision()
	if e = store.Edit(text(first["id"]), revision, "Preserve my minimum", 8*GiB, 4.0); e != nil {
		t.Fatal(e)
	}
	third := similarityIdentity("new target")
	if e = store.Observe(third, similarityDescription("New tests", "package-one", "go:tests")); e != nil {
		t.Fatal(e)
	}
	newMatch, _ := store.Match(third)
	if newMatch["id"] == first["id"] || metric(predictProfile(t, store, identities[0])["memoryBytes"]) < 8*GiB {
		t.Fatal("Automatic grouping changed an operator correction")
	}
}

func TestProfileSimilarityClassificationAndWrappers(t *testing.T) {
	for _, entry := range []struct {
		argv  []string
		title string
		want  string
	}{
		{[]string{"go", "test", "./internal/first"}, "Different display label", "go:tests"},
		{[]string{"go", "test", "-race", "./..."}, "Tests", "go:race-tests"},
		{[]string{"go", "test", "./..."}, "Tests", "go:full-tests"},
		{[]string{"go", "test", "./...", "-run=TestOne"}, "Tests", "go:tests"},
		{[]string{"go", "test", "./...", "-bench=BenchmarkLoad"}, "Tests", "go:benchmark"},
		{[]string{"cargo", "bench"}, "Anything", "cargo:benchmark"},
		{[]string{"env", "GOMAXPROCS=2", "go", "test", "./internal/second"}, "Anything", "go:tests"},
		{[]string{"fnm", "exec", "--using=26", "--", "pnpm", "test"}, "Anything", "javascript:tests"},
		{[]string{"bash", "/private/disposable-batch.sh"}, "Issue 19 browser regression tests", "shell:browser-tests"},
		{[]string{"python3.14", "-c", "opaque"}, "Issue 20 regression tests", "python:tests"},
		{[]string{"bash", "-c", "opaque"}, "Build and test all modules", ""},
		{[]string{"bash", "-c", "opaque"}, "Unclassified batch 123", ""},
	} {
		_, executable := SafeSummary(entry.argv)
		if got := profileCommandKind(entry.argv, entry.title, executable); got != entry.want {
			t.Errorf("%v / %s: got %q, want %q", entry.argv, entry.title, got, entry.want)
		}
	}
}

func TestProfileSimilarityRetroactivePlanPreservesRunsAndIsIdempotent(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	deletedDirectory := filepath.Join(t.TempDir(), "retired-checkout")
	for i := 0; i < 4; i++ {
		identity := similarityIdentity(fmt.Sprint(i))
		token := fmt.Sprintf("%032x", i+1)
		title := fmt.Sprintf("Issue %d Go regression tests", i)
		if e = store.Observe(identity, Object{"title": title, "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		recordProfile(t, store, token, identity, (i+1)*GiB, 1, i != 2, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": title, "project": "fixture", "executable": "go", "directory": deletedDirectory})
		// No workload sidecar or original command is needed for retained runs.
	}
	pinned := similarityIdentity("3")
	if e = store.Edit(text(pinned["family"]), 0, "My reserved profile", 8*GiB, 4.0); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(store.path)
	plan, e := LearningRegroup(g, false, nil)
	after, _ := os.ReadFile(store.path)
	if e != nil || !bytes.Equal(before, after) || metric(plan["profilesBefore"]) != 4 || metric(plan["profilesAfter"]) != 2 || boolean(plan["applied"]) {
		t.Fatal("Retroactive preview changed history or missed compatible families", plan, e)
	}
	var originalRuns, originalEnvelopes []Object
	if e = store.connection(false, func(c profileSQL) error {
		originalRuns, e = profileRows(c, "SELECT * FROM runs ORDER BY token")
		if e == nil {
			originalEnvelopes, e = profileRows(c, "SELECT * FROM envelopes ORDER BY family,variant")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	saved, e := LearningRegroup(g, true, Object{"revision": 1})
	if e != nil || !boolean(saved["applied"]) || metric(saved["revision"]) != 2 {
		t.Fatal("Retroactive grouping failed", saved, e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		runs, err := profileRows(c, "SELECT * FROM runs ORDER BY token")
		if err != nil || !reflect.DeepEqual(runs, originalRuns) {
			t.Fatal("Retroactive grouping rewrote measured runs", err)
		}
		envelopes, err := profileRows(c, "SELECT * FROM envelopes ORDER BY family,variant")
		if err != nil || !reflect.DeepEqual(envelopes, originalEnvelopes) {
			t.Fatal("Retroactive grouping rewrote measured peaks", err)
		}
		return err
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = LearningRegroup(g, true, Object{"revision": 1}); e == nil {
		t.Fatal("Stale retrospective correction succeeded")
	}
	again, e := LearningRegroup(g, true, Object{"revision": 2})
	if e != nil || metric(again["profilesBefore"]) != 2 || metric(again["profilesAfter"]) != 2 || metric(again["revision"]) != 2 {
		t.Fatal("Retrospective grouping is not idempotent", again, e)
	}
	if metric(predictProfile(t, store, pinned)["memoryBytes"]) < 8*GiB {
		t.Fatal("Retroactive grouping lost a pinned minimum")
	}
	// An old client may add more families after explicit regroup and before the
	// updated controller starts. Its first backfill must still incorporate them.
	newIdentity := similarityIdentity("arrived before runtime upgrade")
	newToken := strings.Repeat("f", 32)
	if e = store.Observe(newIdentity, Object{"title": "New regression tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	recordProfile(t, store, newToken, newIdentity, GiB, 1, true, profileZero(), 0)
	mustWrite(t, filepath.Join(g.Metadata, newToken+".json"), Object{"title": "New regression tests", "project": "fixture", "executable": "go", "directory": deletedDirectory})
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal(e)
	}
	catalog, e := store.Catalog(0, 100, "")
	if e != nil || metric(catalog["total"]) != 2 || metric(catalog["revision"]) != 3 {
		t.Fatal("Explicit regroup prematurely marked the controller backfill complete", catalog, e)
	}
	beforeSecondStart, _ := os.ReadFile(store.path)
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal(e)
	}
	afterSecondStart, _ := os.ReadFile(store.path)
	if !bytes.Equal(beforeSecondStart, afterSecondStart) {
		t.Fatal("Completed controller backfill wrote history on restart")
	}
}

func TestProfileSimilarityFailedMigrationRollsBackEveryMembership(t *testing.T) {
	store := fixtureProfileStore(t)
	descriptions := map[string]Object{}
	for i := 0; i < 3; i++ {
		identity := similarityIdentity(fmt.Sprint(i))
		if e := store.Observe(identity, Object{"title": "Tests", "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		descriptions[text(identity["family"])] = similarityDescription("Tests", "package", "go:tests")
	}
	if e := store.connection(true, func(c profileSQL) error {
		return profileExec(c, `CREATE TRIGGER reject_regroup BEFORE UPDATE OF profile ON learning_members BEGIN SELECT RAISE(ABORT,'fixture interrupted migration'); END`)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := store.regroup(descriptions, 0, true); e == nil {
		t.Fatal("Injected migration interruption did not fail")
	}
	catalog, e := store.Catalog(0, 100, "")
	if e != nil || metric(catalog["total"]) != 3 || metric(catalog["revision"]) != 0 {
		t.Fatal("Failed grouping committed partial membership or a revision", catalog, e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		rows, e := profileRows(c, "SELECT * FROM learning_similarity")
		if e == nil && len(rows) != 0 {
			t.Fatal("Failed grouping committed partial classification")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestWorkloadIdentitySourceRevisionTracksContextWithoutDiscardingLearning(t *testing.T) {
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	for _, path := range []string{bin, filepath.Join(directory, ".git")} {
		if e := os.Mkdir(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	git := "#!/bin/sh\ncase \"$4\" in\n--path-format=absolute) printf '%s\\n%s\\n' \"$2\" \"$2/.git\" ;;\nHEAD) /bin/cat \"$2/revision\" ;;\n*) exit 1 ;;\nesac\n"
	if e := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin)
	if e := os.WriteFile(filepath.Join(directory, "revision"), []byte("first"), 0600); e != nil {
		t.Fatal(e)
	}
	first := identityForTest(t, []string{"go", "test", "./..."}, directory, map[string]string{"PATH": "/nonexistent"})
	if e := os.WriteFile(filepath.Join(directory, "revision"), []byte("second"), 0600); e != nil {
		t.Fatal(e)
	}
	second := identityForTest(t, []string{"go", "test", "./..."}, directory, map[string]string{"PATH": "/nonexistent"})
	if first["family"] != second["family"] || first["variant"] != second["variant"] || first["context"] == second["context"] {
		t.Fatal("A source revision reset resource learning or lost freshness context")
	}
}

func TestProfileSimilarityCLIRejectsUnreviewedAndUnknownMutations(t *testing.T) {
	g := testGate(t)
	for _, entry := range []struct {
		args  []string
		input string
	}{
		{[]string{"regroup", "--apply", "--json"}, `{}`},
		{[]string{"regroup", "--apply", "--json"}, `{"revision":0,"extra":true}`},
		{[]string{"regroup", "--apply", "--json"}, `{"revision":true}`},
		{[]string{"regroup", "--apply", "--apply", "--json"}, `{"revision":0}`},
		{[]string{"regroup", "unknown", "--json"}, ""},
	} {
		var output, stderr bytes.Buffer
		if code := LearningCLI(g, entry.args, strings.NewReader(entry.input), &output, &stderr); code != 125 {
			t.Fatal("Unreviewed regroup mutation accepted", entry.args, output.String())
		}
	}
}

func TestProfileSimilarityAutomaticJoinRetiresPendingGrant(t *testing.T) {
	for _, entry := range []struct {
		name     string
		revision any
	}{{"published revision", 0}, {"missing revision after failure", nil}} {
		t.Run(entry.name, func(t *testing.T) {
			testSimilarityAutomaticJoinRetiresPendingGrant(t, entry.revision)
		})
	}
}

func testSimilarityAutomaticJoinRetiresPendingGrant(t *testing.T, priorRevision any) {
	t.Helper()
	g, snapshot := schedulerFixtureGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	a, b := similarityIdentity("large prior target"), similarityIdentity("new target")
	if e = store.Observe(a, similarityDescription("Earlier regression tests", "package", "go:tests")); e != nil {
		t.Fatal(e)
	}
	if e = store.Record("prior-large", a, "opaque-capacity", Object{"memoryBytes": 32 * GiB, "cpuCores": 4, "durationSeconds": 10}, true, profileZero()); e != nil {
		t.Fatal(e)
	}
	value := strings.Repeat("1", 32)
	mustWrite(t, filepath.Join(g.State, "workloads", value+".json"), b)
	mustWrite(t, filepath.Join(g.Metadata, value+".json"), similarityDescription("New regression tests", "package", "go:tests"))
	obj(snapshot["tasks"])["1"] = Object{"command": "agent-heavy execute " + value, "status": Object{"Stashed": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
	writeSchedulerSnapshot(t, g, snapshot)
	publishSchedulerFixture(t, g, fixtureResourceHost(), priorRevision)
	schedulerFixtureGrant(t, g, false, resourceNow()+5)
	sampler, backend, now, _ := resourceFixture()
	sampler.wallClock = resourceNow
	mustHost(t, sampler)
	*now += 2
	backend.raw["cpuTicks"] = []any{110, 100, 290, 100}
	controller, e := NewController(g, sampler, store)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(controller.Close)
	state, e := controller.Tick()
	if e != nil {
		t.Fatal(e)
	}
	prediction := obj(obj(obj(state["jobs"])[value])["prediction"])
	if metric(prediction["memoryBytes"]) < 32*GiB {
		t.Fatal("The automatic join did not raise the reservation", prediction)
	}
	settings, _ := SchedulingSettings(g)
	ledger, e := SchedulingLedger(g, settings)
	if e != nil || len(obj(ledger["grants"])) != 0 || metric(state["learningRevision"]) != 1 {
		t.Fatal("Automatic grouping republished an old pending reservation", ledger, state, e)
	}
}

func TestProfileSimilarityGroupEvidenceRejectsStaleFailedAndMalformedCounts(t *testing.T) {
	store := fixtureProfileStore(t)
	a, b := similarityIdentity("existing"), similarityIdentity("borrower")
	for _, identity := range []Object{a, b} {
		if e := store.Observe(identity, similarityDescription("Tests", "package", "go:tests")); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 3; i++ {
		recordProfile(t, store, fmt.Sprintf("old-%d", i), a, GiB, 1, true, profileZero(), resourceNow()-31*profileDay)
		recordProfile(t, store, fmt.Sprintf("partial-%d", i), a, GiB, 1, false, nil, 0)
	}
	prediction := predictProfile(t, store, b)
	if metric(prediction["familySamples"]) != 0 {
		t.Fatal("Incomplete or stale group runs became familiarity evidence", prediction)
	}
	for _, count := range []any{-1, true, 3.0, "3"} {
		prediction["familySamples"] = count
		if validPrediction(prediction) {
			t.Fatal("Malformed group evidence accepted", count)
		}
	}
}
