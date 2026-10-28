package backend

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProfileMemberInspectionFindsOlderEvidenceAndUngroupsWithoutLoss(t *testing.T) {
	store := fixtureProfileStore(t)
	older, newer := similarityIdentity("older member"), similarityIdentity("busy member")
	for _, identity := range []Object{older, newer} {
		if err := store.Observe(identity, similarityDescription("Tests", "same-package", "go:tests")); err != nil {
			t.Fatal(err)
		}
	}
	now := float64(time.Now().Unix())
	recordProfile(t, store, "older-success", older, 4*GiB, 3, true, profileZero(), now-100)
	recordProfile(t, store, "older-incomplete", older, 6*GiB, 4, false, nil, now-99)
	for i := 0; i < 35; i++ {
		recordProfile(t, store, fmt.Sprintf("busy-%d", i), newer, GiB, 1, true, profileZero(), now-float64(i))
	}
	match, err := store.Match(older)
	if err != nil {
		t.Fatal(err)
	}
	id := text(match["id"])
	before, err := store.Detail(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !boolean(obj(before["truncated"])["runs"]) {
		t.Fatal("Fixture must overflow the group run limit")
	}
	for _, row := range before["runs"].([]Object) {
		if row["family"] == older["family"] {
			t.Fatal("Fixture did not hide older member from group history")
		}
	}
	dbBefore, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := store.Inspect(id, "", ProfileDetailOptions{Family: text(older["family"]), MemberLimit: 256})
	if err != nil {
		t.Fatal(err)
	}
	members := inspected["members"].([]Object)
	runs := inspected["runs"].([]Object)
	if len(members) != 1 || len(runs) != 2 || inspected["inspectedFamily"] != older["family"] || !reflect.DeepEqual(inspected["profile"], before["profile"]) {
		t.Fatal("Member inspection did not isolate complete evidence", inspected)
	}
	measurements := obj(members[0]["measurements"])
	if metric(measurements["successCount"]) != 1 || metric(measurements["incompleteCount"]) != 1 || metric(measurements["peakMemoryBytes"]) != 6*GiB || metric(measurements["variantCount"]) != 1 {
		t.Fatal("Member summary borrowed sibling measurements", measurements)
	}
	for _, row := range append(inspected["variants"].([]Object), runs...) {
		if row["family"] != older["family"] {
			t.Fatal("Sibling evidence leaked into inspection", row)
		}
	}
	if _, err = store.Inspect(id, "", ProfileDetailOptions{Family: strings.Repeat("e", 64), MemberLimit: 256}); err == nil {
		t.Fatal("Accepted a family outside this group")
	}
	dbAfter, err := os.ReadFile(store.path)
	if err != nil || !bytes.Equal(dbBefore, dbAfter) {
		t.Fatal("Inspection wrote to the learning database", err)
	}
	revision, _ := store.Revision()
	separated, err := store.Split(id, text(older["family"]), revision)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Detail(separated, "")
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.Detail(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after["runs"], inspected["runs"]) || !reflect.DeepEqual(after["variants"], inspected["variants"]) || metric(obj(remaining["profile"])["successCount"]) != 35 {
		t.Fatal("Ungrouping lost or reassigned evidence", after, remaining)
	}
	for _, identity := range []Object{older, newer} {
		if err = store.Observe(identity, similarityDescription("Tests again", "same-package", "go:tests")); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := store.Match(older)
	b, _ := store.Match(newer)
	if a["id"] == b["id"] {
		t.Fatal("Automatic grouping undid mismatch correction")
	}
}

func TestProfileMemberPagesSearchAllMembersAndRetainOwnStats(t *testing.T) {
	group := &profileGroup{}
	for i := 0; i < 300; i++ {
		family := fmt.Sprintf("%064x", i)
		group.members = append(group.members, Object{"family": family, "title": fmt.Sprintf("Task %03d", i), "project": "fixture", "executable": "go"})
	}
	last := text(group.members[299]["family"])
	group.envelopes = []Object{{"family": last, "variant": strings.Repeat("a", 64), "memory": 7 * GiB, "cpu": 2, "sampled": 123.0}}
	page, info, err := profileMemberPage(group, ProfileDetailOptions{MemberOffset: 256, MemberLimit: 256})
	if err != nil || len(page) != 44 || metric(info["total"]) != 300 || metric(obj(page[43]["measurements"])["peakMemoryBytes"]) != 7*GiB || metric(obj(page[43]["measurements"])["successCount"]) != 0 {
		t.Fatal("Paged members lost retained peaks or invented runs", page, info, err)
	}
	page, info, err = profileMemberPage(group, ProfileDetailOptions{MemberQuery: "tASK 299", MemberLimit: 256})
	if err != nil || len(page) != 1 || page[0]["family"] != last || metric(info["total"]) != 1 {
		t.Fatal("Search only searched visible members", page, info, err)
	}
}

func TestProfileMemberCLIRejectsInvalidInspectionOptions(t *testing.T) {
	gate := testGate(t)
	for _, args := range [][]string{
		{"--family", "invalid"}, {"--family", strings.Repeat("a", 64), "--members-query", "query"}, {"--members-limit", "257"},
		{"--members-offset", "-1"}, {"--members-query", "bad\nquery"}, {"--members-query", "x", "--members-query", "y"}, {"--members-limit"},
	} {
		var output, stderr bytes.Buffer
		command := append([]string{"show", strings.Repeat("a", 64)}, args...)
		if code := LearningCLI(gate, command, strings.NewReader(""), &output, &stderr); code != 125 {
			t.Fatal("Invalid inspection accepted", command, output.String())
		}
	}
}
