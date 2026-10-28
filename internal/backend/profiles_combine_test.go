package backend

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func combineFixture(t *testing.T) (*ProfileStore, []Object) {
	t.Helper()
	s := fixtureProfileStore(t)
	ids := []Object{}
	for i := 0; i < 6; i++ {
		id := similarityIdentity(fmt.Sprint("combine", i))
		ids = append(ids, id)
		if err := s.Observe(id, similarityDescription(fmt.Sprintf("Task %d", i), fmt.Sprintf("scope-%d", i), "go:tests")); err != nil {
			t.Fatal(err)
		}
		recordProfile(t, s, fmt.Sprintf("combine-run-%d", i), id, (i+1)*GiB, float64(i+1), true, profileZero(), 0)
	}
	for _, pair := range [][2]int{{1, 0}, {4, 3}} {
		rev, _ := s.Revision()
		if err := s.Merge(text(ids[pair[0]]["family"]), text(ids[pair[1]]["family"]), rev); err != nil {
			t.Fatal(err)
		}
	}
	return s, ids
}
func combineNumericHistory(t *testing.T, s *ProfileStore) Object {
	t.Helper()
	result := Object{}
	err := s.connection(false, func(c profileSQL) error {
		for _, table := range []string{"runs", "envelopes"} {
			rows, err := profileRows(c, "SELECT * FROM "+table+" ORDER BY family,variant,machine")
			if err != nil {
				return err
			}
			result[table] = rows
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestProfileCombineGroupsAndRunWorkloadsAtomicallyPreservesHistory(t *testing.T) {
	s, ids := combineFixture(t)
	family := func(i int) string { return text(ids[i]["family"]) }
	for _, floor := range []struct {
		i, memory int
		cpu       float64
		label     string
	}{{0, 4 * GiB, 1, "First group"}, {3, 8 * GiB, 4, "Partial group"}, {5, 2 * GiB, 2, "Destination"}} {
		rev, _ := s.Revision()
		if err := s.Edit(family(floor.i), rev, floor.label, floor.memory, floor.cpu); err != nil {
			t.Fatal(err)
		}
	}
	before := combineNumericHistory(t, s)
	revision, _ := s.Revision()
	// A run selection is its original family. Selecting that family and its group is coalesced.
	if err := s.Combine(family(5), revision, []string{family(0), family(2)}, []string{family(1), family(4)}); err != nil {
		t.Fatal(err)
	}
	next, err := s.Detail(family(5), "")
	if err != nil {
		t.Fatal(err)
	}
	profile := obj(next["profile"])
	if metric(profile["memberCount"]) != 5 || metric(profile["successCount"]) != 5 || metric(profile["memoryFloorBytes"]) != 8*GiB || metric(profile["cpuFloorCores"]) != 4 || profile["label"] != "Destination" {
		t.Fatal("Combined summary lost members, floors or destination naming", profile)
	}
	remaining, err := s.Detail(family(3), "")
	if err != nil || metric(obj(remaining["profile"])["memberCount"]) != 1 || metric(obj(remaining["profile"])["memoryFloorBytes"]) != 8*GiB {
		t.Fatal("Unselected sibling or source floor was lost", remaining, err)
	}
	current, _ := s.Revision()
	if current != revision+1 || !reflect.DeepEqual(before, combineNumericHistory(t, s)) {
		t.Fatal("Merge rewrote numeric history or changed revision more than once")
	}
	for _, i := range []int{0, 1, 2, 4, 5} {
		match, err := s.Match(ids[i])
		if err != nil || match["id"] != family(5) {
			t.Fatal("Matching future runs did not follow their family", i, match, err)
		}
	}
	if _, err = s.Detail(family(0), ""); err == nil {
		t.Fatal("Consumed group survived")
	}
	// A subsequent observation cannot undo the operator's selection.
	if err = s.Observe(ids[4], similarityDescription("Tests again", "scope-3", "go:tests")); err != nil {
		t.Fatal(err)
	}
	match, _ := s.Match(ids[4])
	if match["id"] != family(5) {
		t.Fatal("Automatic learning undid a manual combination")
	}
}
func TestProfileCombineRejectsStaleInvalidAndRollsBackEveryChange(t *testing.T) {
	s, ids := combineFixture(t)
	f := func(i int) string { return text(ids[i]["family"]) }
	rev, _ := s.Revision()
	for _, selection := range []struct {
		revision         int
		groups, families []string
	}{{rev - 1, []string{f(0)}, nil}, {rev, []string{strings.Repeat("f", 64)}, nil}, {rev, []string{f(5)}, nil}, {rev, nil, []string{f(5)}}, {rev, []string{f(0), f(0)}, nil}, {rev, nil, nil}} {
		before, _ := os.ReadFile(s.path)
		if err := s.Combine(f(5), selection.revision, selection.groups, selection.families); err == nil {
			t.Fatal("Invalid bulk merge accepted", selection)
		}
		after, _ := os.ReadFile(s.path)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("Rejected bulk merge changed state")
		}
	}
	selectedFamilies := []string{f(0), f(1), f(2), f(4)}
	sort.Strings(selectedFamilies)
	if err := s.connection(true, func(c profileSQL) error {
		return profileExec(c, fmt.Sprintf("CREATE TRIGGER reject_combine BEFORE UPDATE OF profile ON learning_members WHEN NEW.family='%s' BEGIN SELECT RAISE(ABORT,'fixture rejection'); END", selectedFamilies[len(selectedFamilies)-1]))
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.path)
	if err := s.Combine(f(5), rev, []string{f(0), f(2)}, []string{f(4)}); err == nil {
		t.Fatal("Fixture did not reject the transaction")
	}
	after, _ := os.ReadFile(s.path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Failed combined transaction committed partial membership, floors or pins")
	}
}
func TestProfileCombineRequestBoundsAndTypes(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, value := range []Object{{"revision": 0, "profileIds": nil, "families": []string{id}}, {"revision": 0, "profileIds": "wrong", "families": []string{id}}, {"revision": 0, "profileIds": []string{}, "families": []string{}}, {"revision": 0, "profileIds": []string{id, id}, "families": []string{}}, {"revision": 0, "profileIds": []any{true}, "families": []string{}}, {"revision": 0, "profileIds": []string{id}, "families": []string{}, "extra": true}} {
		if _, err := ValidateLearningRequest("combine", value); err == nil {
			t.Fatal("Malformed combine request accepted", value)
		}
	}
	groups := []string{}
	for i := 0; i < 33; i++ {
		groups = append(groups, fmt.Sprintf("%064x", i))
	}
	if _, err := ValidateLearningRequest("combine", Object{"revision": 0, "profileIds": groups, "families": []string{}}); err == nil {
		t.Fatal("Unbounded selection accepted")
	}
	groups = groups[:32]
	if _, err := ValidateLearningRequest("combine", Object{"revision": 0, "profileIds": groups, "families": []string{}}); err != nil {
		t.Fatal("Bounded selection rejected", err)
	}
}

func TestProfileCombinePinsAnAutomaticGroupRemainder(t *testing.T) {
	store := fixtureProfileStore(t)
	a, b, target, newSibling := similarityIdentity("automatic-a"), similarityIdentity("automatic-b"), similarityIdentity("destination"), similarityIdentity("new-sibling")
	for _, identity := range []Object{a, b} {
		if err := store.Observe(identity, similarityDescription("Tests", "same-package", "go:tests")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Observe(target, similarityDescription("Build", "different-package", "go:build")); err != nil {
		t.Fatal(err)
	}
	original, _ := store.Match(a)
	destination, _ := store.Match(target)
	revision, _ := store.Revision()
	if err := store.Combine(text(destination["id"]), revision, nil, []string{text(a["family"])}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(newSibling, similarityDescription("Another test", "same-package", "go:tests")); err != nil {
		t.Fatal(err)
	}
	remainder, err := store.Detail(text(original["id"]), "")
	if err != nil || metric(obj(remainder["profile"])["memberCount"]) != 1 {
		t.Fatal("Automatic learning added a new member to the corrected remainder", remainder, err)
	}
	sibling, _ := store.Match(newSibling)
	if sibling["id"] == original["id"] || sibling["id"] == destination["id"] {
		t.Fatal("Automatic grouping undid a manual mixed selection", sibling)
	}
}
