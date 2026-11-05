package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/wire"
)

func TestPreviewQueueAndProfileScenarios(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := q.run(ctx, []string{"start", "102"}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := q.run(ctx, []string{"status", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue, _ := object(snapshot["queue"])
	if queue["running"] != float64(2) {
		t.Fatal(snapshot)
	}
	id := strings.Repeat("a", 64)
	result, err := q.run(ctx, []string{"profiles", "edit", id, "--json"}, []byte(`{"revision":4,"label":"Renamed","memoryFloorBytes":1,"cpuFloorCores":1e-20}`))
	if err != nil || result["revision"] != 5 {
		t.Fatal(result, err)
	}
	if _, err := q.run(ctx, []string{"profiles", "reset", id, "--json"}, []byte(`{"revision":4,"confirm":true}`)); err == nil {
		t.Fatal("accepted stale fixture revision")
	}
	result, err = q.run(ctx, []string{"profiles", "split", id, "--json"}, []byte(`{"revision":5,"family":"`+strings.Repeat("c", 64)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := object(result["profile"])
	if profile["id"] != strings.Repeat("3", 64) {
		t.Fatal(result)
	}
	result, err = q.run(ctx, []string{"profiles", "merge", strings.Repeat("3", 64), "--json"}, []byte(`{"revision":6,"targetId":"`+strings.Repeat("b", 64)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	profile, _ = object(result["profile"])
	if profile["memberCount"] != float64(2) {
		t.Fatal(result)
	}
	if len(q.events) != 4 {
		t.Fatal(q.events)
	}
}

func TestPreviewServesRealAPIWithIsolatedFixtureControls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, "127.0.0.1", 0, false, writer); _ = writer.Close() }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("preview did not stop")
		}
	})
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(reader).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	response, err := client.Get(ready.URL + "/api/csrf")
	if err != nil {
		t.Fatal(err)
	}
	var csrf struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&csrf); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	post := func(path, body, token string) int {
		t.Helper()
		request, _ := http.NewRequest("POST", ready.URL+path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", ready.URL)
		request.Header.Set("Referer", ready.URL+"/")
		request.Header.Set("X-CSRFToken", token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if code := post("/__fixture/state", `{"offline":true}`, ""); code != 400 {
		t.Fatal("fixture route bypassed CSRF", code)
	}
	if code := post("/__fixture/state", `{"profileCount":101}`, csrf.Token); code != 200 {
		t.Fatal(code)
	}
	response, err = client.Get(ready.URL + "/api/profiles?offset=100&limit=100&query=")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Total    int              `json:"total"`
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if catalog.Total != 101 || len(catalog.Profiles) != 1 {
		t.Fatal(catalog)
	}
	if code := post("/__fixture/state", `{"offline":true}`, csrf.Token); code != 200 {
		t.Fatal(code)
	}
	response, err = client.Get(ready.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 502 {
		t.Fatal("offline scenario not preserved")
	}
	if code := post("/__fixture/reset", `{}`, csrf.Token); code != 200 {
		t.Fatal(code)
	}
}

func TestPreviewRejectsPublicAndWildcardBindings(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "8.8.8.8", "::1", "localhost", "invalid"} {
		if err := Run(context.Background(), bind, 0, false, io.Discard); err == nil {
			t.Fatal("accepted binding", bind)
		}
	}
}

func TestPreviewServesAndResetsAContractSnapshot(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../../native/tests/fixtures/queue.json")
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) int {
		response := httptest.NewRecorder()
		q.ServeHTTP(response, httptest.NewRequest("POST", "/__fixture/state", strings.NewReader(body)))
		return response.Code
	}
	if code := post(`{"snapshot":` + string(golden) + `}`); code != 200 {
		t.Fatal(code)
	}
	served, err := q.run(context.Background(), []string{"status", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(golden, &expected); err != nil {
		t.Fatal(err)
	}
	for _, raw := range expected["jobs"].([]any) {
		job, _ := object(raw)
		job["logPath"] = filepath.Join(q.root, fmt.Sprint(job["id"])+".log")
	}
	if !reflect.DeepEqual(served, expected) {
		t.Fatalf("served %v, want %v", served, expected)
	}
	for _, chosen := range []string{"/etc/passwd", "../../secret"} {
		if code := post(`{"snapshot":{"jobs":[{"id":7,"logPath":"` + chosen + `"}]}}`); code != 200 {
			t.Fatal(code)
		}
		if path := q.snapshot.Jobs[0].LogPath; path != filepath.Join(q.root, "7.log") {
			t.Fatalf("a fixture chose log path %q", path)
		}
	}
	for _, invalid := range []string{`{"snapshot":{"undeclared":true}}`, `{"scheduler":{"undeclared":true}}`, `{"resources":{"undeclared":true}}`} {
		if code := post(invalid); code != 400 {
			t.Fatalf("%s: %d", invalid, code)
		}
	}
	if err := q.reset(); err != nil || q.snapshot != nil {
		t.Fatal("reset kept the contract snapshot", err)
	}
}

func TestPreviewStateReplacesProfilesAndSchedulerRatherThanMerging(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	q.scheduler = &wire.Scheduler{Mode: "fixed", Reason: "must be removed"}
	request := httptest.NewRequest("POST", "/__fixture/state", strings.NewReader(`{"profiles":{},"scheduler":{"mode":"adaptive"}}`))
	response := httptest.NewRecorder()
	q.ServeHTTP(response, request)
	if response.Code != 200 || len(q.profiles) != 0 || q.scheduler.Mode != "adaptive" || q.scheduler.Reason != "" {
		t.Fatal("fixture state retained replaced entries", response.Code, q.profiles, q.scheduler)
	}
	result, err := q.run(context.Background(), []string{"profiles", "list", "--offset", "0", "--limit", "100", "--query", "", "--json"}, nil)
	if err != nil || result["total"] != 0 {
		t.Fatal("cleared profiles remained visible", result, err)
	}
}

func TestPreviewMemberInspectionAndSplitKeepEvidenceWithItsOwner(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	id, family := strings.Repeat("a", 64), strings.Repeat("d", 64)
	detail, err := q.run(context.Background(), []string{"profiles", "show", id, "--family", family, "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := members(detail["runs"])
	if err != nil || len(rows) != 0 || detail["inspectedFamily"] != family || len(q.events) != 0 {
		t.Fatal("Inspection leaked sibling measurements or mutated", detail, err)
	}
	result, err := q.run(context.Background(), []string{"profiles", "split", id, "--json"}, []byte(`{"revision":4,"family":"`+family+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := object(result["profile"])
	if profile["successCount"] != float64(0) || profile["peakMemoryBytes"] != float64(0) {
		t.Fatal("Empty member inherited sibling measurements", result)
	}
	remaining, _ := object(q.profiles[id]["profile"])
	if remaining["successCount"] != float64(5) {
		t.Fatal("Remaining group lost measurements", remaining)
	}
}

func TestPreviewLargeMemberFixtureUsesBoundedControlInput(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	q.ServeHTTP(w, httptest.NewRequest("POST", "/__fixture/state", strings.NewReader(`{"profileMemberCount":300}`)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	id := strings.Repeat("a", 64)
	detail, err := q.run(context.Background(), []string{"profiles", "show", id, "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := object(detail["memberPage"])
	all, err := members(detail["members"])
	if err != nil || len(all) != 256 || page["total"] != 300 {
		t.Fatal("Large fixture was not paged", detail, err)
	}
	detail, err = q.run(context.Background(), []string{"profiles", "show", id, "--members-query", "Member 299", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err = members(detail["members"])
	if err != nil || len(all) != 1 || all[0]["label"] != "Member 299" {
		t.Fatal("Large fixture search missed hidden member", detail, err)
	}
}

func TestPreviewCombineGroupsAndWorkloadsRetainsSiblings(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	destination, source, family := strings.Repeat("b", 64), strings.Repeat("a", 64), strings.Repeat("c", 64)
	result, err := q.run(context.Background(), []string{"profiles", "combine", destination, "--json"}, []byte(`{"revision":4,"profileIds":[],"families":["`+family+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := object(result["profile"])
	if profile["id"] != destination || profile["memberCount"] != float64(2) || profile["successCount"] != float64(10) || result["revision"] != 5 {
		t.Fatal("Fixture combine lost history or destination", result)
	}
	remaining, err := members(q.profiles[source]["members"])
	if err != nil || len(remaining) != 1 || remaining[0]["family"] != strings.Repeat("d", 64) {
		t.Fatal("Fixture moved an unselected sibling", remaining, err)
	}
	before, _ := json.Marshal(q.profiles)
	_, err = q.run(context.Background(), []string{"profiles", "combine", destination, "--json"}, []byte(`{"revision":5,"profileIds":["`+strings.Repeat("f", 64)+`"],"families":[]}`))
	after, _ := json.Marshal(q.profiles)
	if err == nil || !bytes.Equal(before, after) || q.profileRevision != 5 || len(q.events) != 1 {
		t.Fatal("Failed fixture combine committed changes", err)
	}
}

func TestPreviewHierarchyAndFiltersFollowTheBackendContract(t *testing.T) {
	q := &fakeQueue{root: t.TempDir()}
	if err := q.reset(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list := func(arguments ...string) map[string]any {
		t.Helper()
		result, err := q.run(ctx, append([]string{"profiles", "list"}, append(arguments, "--json")...), nil)
		if err != nil {
			t.Fatal(arguments, err)
		}
		return result
	}
	overview := list("--hierarchy", "--limit", "50")
	rows, err := members(overview["projects"])
	if err != nil || overview["total"] != 2 || len(rows) != 3 {
		t.Fatal(overview, err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, row["project"].(string))
	}
	if !reflect.DeepEqual(names, []string{"Sample app", "Sample service", "Other projects"}) {
		t.Fatal("projects are not busiest first with the folded row last", names)
	}
	other := rows[2]
	if other["collapsed"] != true || other["projectCount"] != float64(2) || other["groupCount"] != float64(2) || other["workloadCount"] != float64(2) {
		t.Fatal(other)
	}
	operations, _ := members(rows[0]["operations"])
	if len(operations) < 3 || operations[0]["operation"] != "tests" {
		t.Fatal("operations are not busiest first", operations)
	}
	groups := func(arguments ...string) []map[string]any {
		t.Helper()
		page, err := members(list(append([]string{"--offset", "0", "--limit", "100", "--query", ""}, arguments...)...)["profiles"])
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	// Every count in the overview describes exactly the groups its filter returns.
	for _, row := range rows {
		filter := []string{"--project", row["project"].(string)}
		if row["collapsed"] == true {
			filter = []string{"--other-projects"}
		}
		if got := len(groups(filter...)); float64(got) != row["groupCount"] {
			t.Fatal(row["project"], got, row["groupCount"])
		}
		inner, _ := members(row["operations"])
		for _, operation := range inner {
			if got := len(groups(append(filter, "--operation", operation["operation"].(string))...)); float64(got) != operation["groupCount"] {
				t.Fatal(row["project"], operation["operation"], got, operation["groupCount"])
			}
		}
	}
	for _, bad := range [][]string{{"--hierarchy", "--query", "x"}, {"--hierarchy", "--project", "Sample app"}, {"--other-projects", "--project", "Sample app"}, {"--operation", "nope"}} {
		if _, err := q.run(ctx, append(append([]string{"profiles", "list", "--limit", "5"}, bad...), "--json"), nil); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if found := groups("--query", "tooling scripts"); len(found) != 1 || strings.Contains(strings.ToLower(found[0]["label"].(string)), "tooling") {
		t.Fatal("search must also match the project name", found)
	}
	if len(groups("--project", "Missing project")) != 0 {
		t.Fatal("an unknown project must be an empty page, not an error")
	}
	if len(q.events) != 0 {
		t.Fatal("reads recorded events", q.events)
	}
}
