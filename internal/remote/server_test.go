package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/gorilla/csrf"
)

type apiCall struct {
	args  []string
	input []byte
}
type apiFixture struct {
	t       *testing.T
	handler http.Handler
	calls   []apiCall
	token   string
	cookie  *http.Cookie
	result  map[string]any
	failure error
}

func testSettings() Settings {
	return Settings{Username: username, Port: 8743, Hosts: []string{"queue.local"}, PasswordHash: "$2a$14$" + strings.Repeat("a", 53), SecretKey: strings.Repeat("fixture", 8)}
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := &apiFixture{t: t, result: map[string]any{"ok": true}}
	handler, err := NewHandler(testSettings(), func(_ context.Context, args []string, input []byte) (map[string]any, error) {
		f.calls = append(f.calls, apiCall{append([]string(nil), args...), append([]byte(nil), input...)})
		return f.result, f.failure
	}, APIOptions{TestMode: true})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	r := f.request(http.MethodGet, "/api/csrf", "", nil)
	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	f.token = data.Token
	if cookies := r.Result().Cookies(); len(cookies) == 1 {
		f.cookie = cookies[0]
	} else {
		t.Fatal("missing CSRF cookie")
	}
	return f
}

func (f *apiFixture) request(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "https://queue.local:8743"+path, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://queue.local:8743")
		r.Header.Set("Referer", "https://queue.local:8743/")
		r.Header.Set("X-CSRFToken", f.token)
	}
	if f.cookie != nil {
		r.AddCookie(f.cookie)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestAPICSRFAndOriginBoundary(t *testing.T) {
	f := newAPIFixture(t)
	if !f.cookie.Secure || !f.cookie.HttpOnly || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Path != "/" || f.cookie.MaxAge != 3600 {
		t.Fatalf("unsafe cookie attributes: %+v", f.cookie)
	}
	for _, headers := range []map[string]string{{"X-CSRFToken": ""}, {"X-CSRFToken": "invalid"}, {"Origin": "https://evil.test"}, {"Origin": ""}, {"Referer": "https://evil.test/"}, {"Referer": ""}, {"Referer": "http://queue.local:8743/"}} {
		w := f.request(http.MethodPost, "/api/limit", `{"limit":2}`, headers)
		if w.Code != 400 && w.Code != 403 {
			t.Fatalf("boundary allowed %v: %d %s", headers, w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"https://evil.test:8743/api/status", "http://queue.local:8743/api/status", "https://queue.local/api/status"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d", target, w.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("boundary rejection invoked backend")
	}
	other := newAPIFixture(t)
	f.token = other.token
	if w := f.request(http.MethodPost, "/api/limit", `{"limit":2}`, nil); w.Code != 400 {
		t.Fatal("accepted another session's token")
	}
	for _, attribute := range []string{"Cache-Control", "X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if f.request("GET", "/api/csrf", "", nil).Header().Get(attribute) == "" {
			t.Fatalf("missing %s", attribute)
		}
	}
}

func TestAPIStrictQueueMutations(t *testing.T) {
	f := newAPIFixture(t)
	for _, body := range []string{`{"limit":0}`, `{"limit":9}`, `{"limit":true}`, `{"limit":"2"}`, `{"limit":2.0}`, `{"limit":null}`, `{"limit":2,"argv":"private"}`, `{"limit":2,"limit":3}`, `[]`, `null`, `{"limit":2} {}`, `{"limit":2`, "{\"limit\":2,\"\xff\":1}", strings.Repeat("x", 4097)} {
		if w := f.request("POST", "/api/limit", body, nil); w.Code != 400 && w.Code != 413 {
			t.Fatalf("accepted limit %s: %d", body, w.Code)
		}
	}
	for _, mode := range []string{"auto", "true", "[]", "null"} {
		if w := f.request("POST", "/api/scheduling", `{"mode":`+mode+`}`, nil); w.Code != 400 {
			t.Fatalf("accepted mode %s", mode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid mutation invoked backend")
	}
	for _, mode := range []string{"fixed", "shadow", "adaptive"} {
		if w := f.request("POST", "/api/scheduling", `{"mode":"`+mode+`"}`, nil); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	for _, action := range []string{"next", "last", "start", "cancel"} {
		if w := f.request("POST", "/api/jobs/4/"+action, `{}`, nil); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if w := f.request("POST", "/api/limit", `{"limit":8}`, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, target := range []string{"/api/jobs/4/run", "/api/jobs/-1/cancel", "/api/jobs/9223372036854775808/start", "/api/jobs/+4/start"} {
		if w := f.request("POST", target, `{}`, nil); w.Code != 404 {
			t.Fatalf("accepted %s", target)
		}
	}
	if w := f.request("POST", "/api/jobs/4/start", `{"command":"forbidden"}`, nil); w.Code != 400 {
		t.Fatal("accepted action parameters")
	}
	if len(f.calls) != 8 {
		t.Fatalf("unexpected actions: %#v", f.calls)
	}
	f.failure = errors.New("Resource sampler unavailable")
	if w := f.request("POST", "/api/scheduling", `{"mode":"adaptive"}`, nil); w.Code != 502 || !strings.Contains(w.Body.String(), "Resource sampler unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAPIProfilesReadOnlyBounds(t *testing.T) {
	f := newAPIFixture(t)
	id := strings.Repeat("a", 64)
	f.result = map[string]any{"revision": 8, "profile": map[string]any{"id": id}}
	if w := f.request("GET", "/api/profiles?offset=100&limit=25&query=Browser%20tests", "", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("GET", "/api/profiles/"+id, "", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if !reflect.DeepEqual(f.calls[0].args, []string{"profiles", "list", "--offset", "100", "--limit", "25", "--query", "Browser tests", "--json"}) || !reflect.DeepEqual(f.calls[1].args, []string{"profiles", "show", id, "--json"}) {
		t.Fatalf("wrong CLI arguments: %#v", f.calls)
	}
	for _, query := range []string{"limit=101", "limit=0", "offset=-1", "offset=1.2", "offset=00", "query=" + strings.Repeat("x", 121), "limit=10&limit=20", "command=run", "query=%00", "query=%invalid", "offset=9999999999", "limit=01", "limit="} {
		if w := f.request("GET", "/api/profiles?"+query, "", nil); w.Code != 400 {
			t.Fatalf("accepted %s: %d", query, w.Code)
		}
	}
	for _, target := range []string{"/api/profiles/not-a-hash", "/api/profiles/" + id + "?command=run"} {
		if w := f.request("GET", target, "", nil); w.Code != 404 {
			t.Fatal("accepted invalid profile detail")
		}
	}
	if len(f.calls) != 2 {
		t.Fatal("invalid query invoked backend")
	}
}

func TestAPIProfilesMutationValidation(t *testing.T) {
	f := newAPIFixture(t)
	id := strings.Repeat("a", 64)
	base := "/api/profiles/" + id
	valid := `{"revision":7,"label":"Browser tests","memoryFloorBytes":2147483648,"cpuFloorCores":2.5}`
	for _, body := range []string{strings.Replace(valid, `"revision":7`, `"revision":true`, 1), strings.Replace(valid, `"revision":7`, `"revision":null`, 1), strings.Replace(valid, `"revision":7`, `"revision":9007199254740992`, 1), strings.Replace(valid, `2147483648`, `true`, 1), strings.Replace(valid, `2147483648`, `1.0`, 1), strings.Replace(valid, `2.5`, `1e9999`, 1), strings.Replace(valid, `2.5`, `0`, 1), strings.Replace(valid, `Browser tests`, ` x `, 1), strings.Replace(valid, `Browser tests`, ``, 1), strings.Replace(valid, `Browser tests`, `x\n`, 1), strings.Replace(valid, `Browser tests`, strings.Repeat("x", 121), 1), strings.TrimSuffix(valid, "}") + `,"argv":"secret"}`} {
		if w := f.request("POST", base+"/edit", body, nil); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"revision":7,"confirm":false}`, `{"revision":7,"confirm":1}`, `{"revision":7,"confirm":"true"}`} {
		if w := f.request("POST", base+"/reset", body, nil); w.Code != 400 {
			t.Fatal("accepted unconfirmed reset")
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid profile mutation invoked backend")
	}
	for _, mutation := range []struct{ action, body string }{{"edit", valid}, {"edit", `{"revision":7,"label":"Browser tests","memoryFloorBytes":null,"cpuFloorCores":null}`}, {"merge", `{"revision":7,"targetId":"` + strings.Repeat("b", 64) + `"}`}, {"split", `{"revision":7,"family":"` + strings.Repeat("c", 64) + `"}`}, {"reset", `{"revision":7,"confirm":true}`}} {
		if w := f.request("POST", base+"/"+mutation.action, mutation.body, nil); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		call := f.calls[len(f.calls)-1]
		if !reflect.DeepEqual(call.args, []string{"profiles", mutation.action, id, "--json"}) {
			t.Fatal(call.args)
		}
		var got, want any
		_ = json.Unmarshal(call.input, &got)
		_ = json.Unmarshal([]byte(mutation.body), &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("stdin changed payload")
		}
	}
	if w := f.request("POST", base+"/delete", `{}`, nil); w.Code != 404 {
		t.Fatal("accepted profile delete")
	}
}

func TestAPILogsBoundedAndFailClosed(t *testing.T) {
	f := newAPIFixture(t)
	directory := t.TempDir()
	logPath := filepath.Join(directory, "4.log")
	if err := os.WriteFile(logPath, []byte(strings.Repeat("x", 70000)), 0600); err != nil {
		t.Fatal(err)
	}
	f.result = map[string]any{"jobs": []any{map[string]any{"id": 4, "logPath": logPath}}}
	w := f.request("GET", "/api/jobs/4/log", "", nil)
	var output struct {
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil || w.Code != 200 || len(output.Output) != maxLog || !output.Truncated {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(f.calls[0].args, []string{"status", "--json"}) {
		t.Fatal("log GET mutated queue")
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/jobs/4/log", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"output":""`) {
		t.Fatal(w.Body.String())
	}
	target := filepath.Join(directory, "private")
	_ = os.WriteFile(target, []byte("private fixture"), 0600)
	_ = os.Symlink(target, logPath)
	if w := f.request("GET", "/api/jobs/4/log", "", nil); w.Code != 502 {
		t.Fatal("followed log symlink")
	}
	_ = os.Remove(logPath)
	if err := syscall.Mkfifo(logPath, 0600); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/jobs/4/log", "", nil); w.Code != 502 {
		t.Fatal("opened log FIFO")
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, logPath); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/jobs/4/log", "", nil); w.Code != 502 {
		t.Fatal("opened hardlinked log")
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("public fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/jobs/4/log", "", nil); w.Code != 502 {
		t.Fatal("opened public log")
	}
	if w := f.request("GET", "/api/jobs/5/log", "", nil); w.Code != 404 {
		t.Fatal("returned missing job log")
	}
}

func TestProductionQueueLogLocations(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local/state/agent-heavy")
	for _, relative := range []string{"logs", "pueue/task_logs", "jobs"} {
		if err := os.MkdirAll(filepath.Join(state, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{filepath.Join(state, "logs/4.log"), true},
		{filepath.Join(state, "pueue/task_logs/4.log"), true},
		{filepath.Join(state, "logs/5.log"), false},
		{filepath.Join(state, "jobs/4.log"), false},
		{filepath.Join(home, "4.log"), false},
		{filepath.Join(state, "missing/4.log"), false},
	} {
		if got := validQueueLogPath(home, test.path, 4); got != test.want {
			t.Errorf("log path %s: got %v, want %v", test.path, got, test.want)
		}
	}
}

func TestAPIStaticAndProductionSecurity(t *testing.T) {
	f := newAPIFixture(t)
	for _, item := range []struct{ path, mime string }{{"/static/app-icon.png", "image/png"}, {"/static/favicon.png", "image/png"}, {"/static/apple-touch-icon.png", "image/png"}, {"/", "text/html"}, {"/static/app.css", "text/css"}, {"/static/app.js", "javascript"}, {"/static/profiles.js", "javascript"}, {"/static/profile-selection.js", "javascript"}, {"/static/schema.mjs", "javascript"}, {"/static/vendor/zod.mjs", "javascript"}} {
		w := f.request("GET", item.path, "", nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), item.mime) {
			t.Fatal(item.path, w.Code, w.Header())
		}
	}
	for _, target := range []string{"/static/../control.go", "/static/embed.go", "/static/vendor/../index.html", "/static/schema.test.mjs"} {
		if w := f.request("GET", target, "", nil); w.Code != 404 {
			t.Fatal("exposed source", target)
		}
	}
	for _, options := range []APIOptions{{Scheme: "http"}, {InsecureCookie: true}, {TestMode: true, Scheme: "http", InsecureCookie: true}} {
		if _, err := NewHandler(testSettings(), nil, options); err == nil {
			t.Fatalf("accepted insecure production options %+v", options)
		}
	}
	if _, err := NewHandler(testSettings(), func(context.Context, []string, []byte) (map[string]any, error) { return nil, nil }, APIOptions{TestMode: true, Scheme: "http", InsecureCookie: true}); err != nil {
		t.Fatal(err)
	}
	if csrf.Token(httptest.NewRequest("GET", "https://queue.local", nil)) != "" {
		t.Fatal("unexpected token outside CSRF middleware")
	}
}

func TestControllerRunnerPrivateStdinAndBoundedFailures(t *testing.T) {
	directory := t.TempDir()
	fixture := filepath.Join(directory, "controller")
	record := filepath.Join(directory, "stdin")
	script := "#!/bin/sh\ncat > '" + record + "'\nprintf '%s' '{\"revision\":9007199254740993,\"profile\":{}}'\n"
	if err := os.WriteFile(fixture, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"revision":7,"confirm":true}`)
	result, err := executableRunner(fixture)(context.Background(), []string{"profiles", "reset", strings.Repeat("a", 64), "--json"}, payload)
	if err != nil || result["revision"] != json.Number("9007199254740993") {
		t.Fatal(result, err)
	}
	data, _ := os.ReadFile(record)
	if string(data) != string(payload) {
		t.Fatal("private stdin was not preserved")
	}
	for _, test := range []struct{ output, want string }{{`{"error":"Learning changed; refresh before saving"}`, "Learning changed"}, {`private process output`, "inspect the local"}, {`{"error":"private","extra":true}`, "inspect the local"}} {
		if err := os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s' '"+test.output+"'\nexit 125\n"), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := executableRunner(fixture)(context.Background(), []string{"profiles", "edit"}, payload)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s' '{\"jobs\":{}}'\n"), 0700)
	if _, err := executableRunner(fixture)(context.Background(), []string{"status", "--json"}, nil); err == nil {
		t.Fatal("accepted invalid status")
	}
	_ = os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s' '{\"jobs\":[]} {}'\n"), 0700)
	if _, err := executableRunner(fixture)(context.Background(), []string{"status", "--json"}, nil); err == nil {
		t.Fatal("accepted trailing status")
	}
	if _, err := readConfigurationInput(io.LimitReader(strings.NewReader(strings.Repeat("x", 4097)), 4097)); err == nil {
		t.Fatal("accepted oversized configure input")
	}
}

func TestAPIProfileMemberInspectionIsBoundedAndReadOnly(t *testing.T) {
	f := newAPIFixture(t)
	id := strings.Repeat("a", 64)
	family := strings.Repeat("b", 64)
	for _, query := range []string{"family=" + family, "membersOffset=256&membersLimit=50&membersQuery=Browser%20tests"} {
		if w := f.request("GET", "/api/profiles/"+id+"?"+query, "", nil); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	expected := [][]string{{"profiles", "show", id, "--family", family, "--json"}, {"profiles", "show", id, "--members-offset", "256", "--members-limit", "50", "--members-query", "Browser tests", "--json"}}
	for i, call := range f.calls {
		if !reflect.DeepEqual(call.args, expected[i]) || len(call.input) != 0 {
			t.Fatal("Inspection mutated or forwarded wrong flags", call)
		}
	}
	for _, query := range []string{"family=bad", "family=" + family + "&family=" + family, "family=" + family + "&membersQuery=tests", "membersLimit=257", "membersLimit=0", "membersOffset=-1", "membersOffset=00", "membersQuery=%00", "membersQuery=%C2%85", "membersQuery=" + strings.Repeat("x", 121)} {
		if w := f.request("GET", "/api/profiles/"+id+"?"+query, "", nil); w.Code != 400 {
			t.Fatal("Invalid member query accepted", query, w.Code)
		}
	}
	if len(f.calls) != 2 {
		t.Fatal("Invalid inspection invoked backend")
	}
}

func TestAPIProfileCombineBoundsAndPrivateInput(t *testing.T) {
	f := newAPIFixture(t)
	target := strings.Repeat("a", 64)
	source := strings.Repeat("b", 64)
	family := strings.Repeat("c", 64)
	body := `{"revision":7,"profileIds":["` + source + `"],"families":["` + family + `"]}`
	if w := f.request("POST", "/api/profiles/"+target+"/combine", body, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, []string{"profiles", "combine", target, "--json"}) {
		t.Fatal("Combine did not use the allowlisted destination action", f.calls)
	}
	var sent map[string]any
	if err := json.Unmarshal(f.calls[0].input, &sent); err != nil || sent["revision"] != float64(7) {
		t.Fatal("Missing private revision-bound input", sent, err)
	}
	for _, invalid := range []string{`{"revision":7,"profileIds":[],"families":[]}`, `{"revision":7,"profileIds":null,"families":["` + family + `"]}`, `{"revision":7,"profileIds":[true],"families":[]}`, `{"revision":7,"profileIds":["bad"],"families":[]}`, `{"revision":7,"profileIds":["` + source + `","` + source + `"],"families":[]}`, `{"revision":7,"profileIds":["` + source + `"],"families":[],"extra":true}`} {
		if w := f.request("POST", "/api/profiles/"+target+"/combine", invalid, nil); w.Code != 400 {
			t.Fatal("Malformed bulk merge accepted", invalid, w.Code)
		}
	}
	if len(f.calls) != 1 {
		t.Fatal("Invalid bulk merge invoked controller")
	}
}
