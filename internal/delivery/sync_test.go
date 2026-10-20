package delivery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
	"github.com/Dunky13/agent-job-queue/internal/tooling"
)

const fixtureHead = "1111111111111111111111111111111111111111"
const fixtureNextHead = "2222222222222222222222222222222222222222"
const fixtureRun = int64(987)

type githubCall struct {
	endpoint string
	options  RequestOptions
}
type fakeGitHub struct {
	t         *testing.T
	responses map[string]any
	calls     []githubCall
	hook      func(string) any
}

func (f *fakeGitHub) Request(_ context.Context, endpoint string, options RequestOptions) ([]byte, error) {
	f.t.Helper()
	f.calls = append(f.calls, githubCall{endpoint, options})
	var value any
	if f.hook != nil {
		value = f.hook(endpoint)
	}
	if value == nil {
		value = f.responses[endpoint]
	}
	if value == nil {
		return nil, fmt.Errorf("missing fixture response: %s", endpoint)
	}
	if failure, ok := value.(error); ok {
		return nil, failure
	}
	if body, ok := value.([]byte); ok {
		return append([]byte(nil), body...), nil
	}
	return json.Marshal(value)
}
func fixtureManifest(build int, commit string) backend.Manifest {
	return backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: "0.1.0", Commit: &commit, Build: &build}, Tag: fmt.Sprintf("build-%d-%.7s", build, commit), Asset: "Agent-Job-Queue-Installer-0.1.0-universal.zip", SHA256: strings.TrimPrefix(digest([]byte("fixture archive")), "sha256:")}
}
func jsonBody(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
func (f *fakeGitHub) release(item backend.Manifest) {
	body := jsonBody(item)
	id := *item.Build * 10
	f.responses["repos/"+backend.ReleaseRepository+"/releases/tags/"+item.Tag] = map[string]any{"draft": false, "tag_name": item.Tag, "target_commitish": *item.Commit, "published_at": "2026-10-19T12:00:00Z", "assets": []any{map[string]any{"name": "agent-job-queue.json", "id": id, "size": len(body), "state": "uploaded", "digest": digest(body)}, map[string]any{"name": item.Asset, "id": id + 1, "size": 15, "state": "uploaded", "digest": "sha256:" + item.SHA256}}}
	f.responses[fmt.Sprintf("repos/%s/releases/assets/%d", backend.ReleaseRepository, id)] = body
}
func (f *fakeGitHub) file(path, sha string, body []byte) {
	f.responses["repos/"+TapRepository+"/contents/"+path+"?ref="+sha] = map[string]any{"type": "file", "path": path, "encoding": "base64", "size": len(body), "content": base64.StdEncoding.EncodeToString(body)}
}
func verifiedFixtureCommit(sha string) map[string]any {
	return map[string]any{"sha": sha, "commit": map[string]any{"author": map[string]any{"name": "Fixture Bot", "email": "fixture@example.invalid"}, "verification": map[string]any{"verified": true}, "message": "chore(cask): publish\n\nSigned-off-by: Fixture Bot <fixture@example.invalid>"}}
}
func (f *fakeGitHub) tap(item backend.Manifest, sha string) {
	f.responses["repos/"+TapRepository+"/git/ref/heads/tap"] = map[string]any{"ref": "refs/heads/tap", "object": map[string]any{"sha": sha, "type": "commit"}}
	f.responses["repos/"+TapRepository+"/commits/"+sha] = verifiedFixtureCommit(sha)
	f.file("Casks/agent-job-queue.json", sha, jsonBody(item))
	cask, err := tooling.RenderCask(item)
	if err != nil {
		f.t.Fatal(err)
	}
	f.file("Casks/agent-job-queue.rb", sha, []byte(cask))
}
func setupSync(t *testing.T) (backend.Manifest, *fakeGitHub, map[string]string) {
	t.Helper()
	item := fixtureManifest(123, strings.Repeat("a", 40))
	f := &fakeGitHub{t: t, responses: map[string]any{}}
	f.release(item)
	f.tap(item, fixtureHead)
	f.responses["repos/"+TapRepository+"/actions/workflows/refresh.yml/dispatches"] = map[string]any{"workflow_run_id": fixtureRun, "html_url": runURL(fixtureRun), "run_url": fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d", TapRepository, fixtureRun)}
	f.responses[fmt.Sprintf("repos/%s/actions/runs/%d", TapRepository, fixtureRun)] = map[string]any{"id": fixtureRun, "repository": map[string]any{"full_name": TapRepository}, "head_branch": "tap", "event": "workflow_dispatch", "path": ".github/workflows/refresh.yml", "status": "completed", "conclusion": "success"}
	path := filepath.Join(t.TempDir(), "refresh.go")
	_ = os.WriteFile(path, []byte("trusted fixture seed\n"), 0600)
	files := map[string]string{"refresh.go": path}
	f.file("refresh.go", fixtureHead, []byte("trusted fixture seed\n"))
	return item, f, files
}
func performSync(t *testing.T, item backend.Manifest, f *fakeGitHub, files map[string]string) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return Synchronize(ctx, jsonBody(item), f, files, time.Millisecond, io.Discard)
}

func TestSynchronizeExactRunPinnedSignatureAndTokenRouting(t *testing.T) {
	item, f, files := setupSync(t)
	var output bytes.Buffer
	result, err := Synchronize(context.Background(), jsonBody(item), f, files, time.Millisecond, &output)
	if err != nil || result != (Result{"current", 123, fixtureHead}) {
		t.Fatal(result, err)
	}
	if !strings.Contains(output.String(), "Dispatched tap refresh for build 123") || !strings.Contains(output.String(), "Verified Homebrew current") {
		t.Fatal(output.String())
	}
	for _, call := range f.calls {
		isAction := strings.Contains(call.endpoint, "/actions/")
		if call.options.Actions != isAction {
			t.Fatal("permission token routing changed", call.endpoint)
		}
		if strings.Contains(call.endpoint, "/contents/") && !strings.HasSuffix(call.endpoint, "?ref="+fixtureHead) {
			t.Fatal("read mutable tap file")
		}
	}
}

func TestSynchronizeRejectsInvalidReleaseBeforeDispatch(t *testing.T) {
	for _, mutation := range []string{"draft", "target", "assets", "asset-id", "asset-size", "asset-state", "archive-digest", "manifest-digest", "manifest-bytes", "manifest-size", "tooling"} {
		t.Run(mutation, func(t *testing.T) {
			item, f, files := setupSync(t)
			release := object(f.responses["repos/"+backend.ReleaseRepository+"/releases/tags/"+item.Tag])
			assets := release["assets"].([]any)
			switch mutation {
			case "draft":
				release["draft"] = true
			case "target":
				release["target_commitish"] = "main"
			case "assets":
				release["assets"] = []any{assets[0], assets[0], assets[1]}
			case "asset-id":
				object(assets[0])["id"] = true
			case "asset-size":
				object(assets[0])["size"] = 0
			case "asset-state":
				object(assets[1])["state"] = "new"
			case "archive-digest":
				object(assets[1])["digest"] = "sha256:" + strings.Repeat("f", 64)
			case "manifest-digest":
				delete(object(assets[0]), "digest")
			case "manifest-bytes":
				f.responses[fmt.Sprintf("repos/%s/releases/assets/%d", backend.ReleaseRepository, *item.Build*10)] = append(jsonBody(item), ' ')
			case "manifest-size":
				object(assets[0])["size"] = maxMetadata + 1
			case "tooling":
				f.file("refresh.go", fixtureHead, []byte("drift"))
			}
			if _, err := performSync(t, item, f, files); err == nil {
				t.Fatal("unsafe delivery accepted")
			}
			for _, call := range f.calls {
				if call.options.Data != nil {
					t.Fatal("dispatched before release/tooling validation")
				}
			}
		})
	}
}

func TestSynchronizeRunAndTapBoundaries(t *testing.T) {
	for _, mutation := range []string{"run-id", "run-url", "run-repo", "run-event", "run-state", "run-failure", "signature", "dco", "cask", "pinned-ref", "content-size", "content-encoding"} {
		t.Run(mutation, func(t *testing.T) {
			item, f, files := setupSync(t)
			run := object(f.responses[fmt.Sprintf("repos/%s/actions/runs/%d", TapRepository, fixtureRun)])
			dispatchData := object(f.responses["repos/"+TapRepository+"/actions/workflows/refresh.yml/dispatches"])
			commit := object(object(f.responses["repos/"+TapRepository+"/commits/"+fixtureHead])["commit"])
			switch mutation {
			case "run-id":
				dispatchData["workflow_run_id"] = true
			case "run-url":
				dispatchData["run_url"] = "https://evil.test"
			case "run-repo":
				run["repository"] = map[string]any{"full_name": "other/repo"}
			case "run-event":
				run["event"] = "push"
			case "run-state":
				run["status"] = "unknown"
			case "run-failure":
				run["conclusion"] = "cancelled"
			case "signature":
				object(commit["verification"])["verified"] = false
			case "dco":
				commit["message"] = "Signed-off-by: Wrong <other@example.invalid>"
			case "cask":
				f.file("Casks/agent-job-queue.rb", fixtureHead, []byte("malicious Ruby never evaluated"))
			case "pinned-ref":
				object(f.responses["repos/"+TapRepository+"/git/ref/heads/tap"])["ref"] = "refs/heads/main"
			case "content-size":
				object(f.responses["repos/"+TapRepository+"/contents/Casks/agent-job-queue.json?ref="+fixtureHead])["size"] = maxMetadata + 1
			case "content-encoding":
				object(f.responses["repos/"+TapRepository+"/contents/Casks/agent-job-queue.json?ref="+fixtureHead])["content"] = "!!!"
			}
			if _, err := performSync(t, item, f, files); err == nil {
				t.Fatal("invalid workflow/tap accepted")
			}
		})
	}
}

func TestSynchronizeSupersessionRequiresAncestry(t *testing.T) {
	for _, sameCommit := range []bool{false, true} {
		t.Run(strconv.FormatBool(sameCommit), func(t *testing.T) {
			expected, f, files := setupSync(t)
			commit := strings.Repeat("b", 40)
			if sameCommit {
				commit = *expected.Commit
			}
			current := fixtureManifest(124, commit)
			f.release(current)
			f.tap(current, fixtureHead)
			endpoint := fmt.Sprintf("repos/%s/compare/%s...%s?per_page=1", backend.ReleaseRepository, *expected.Commit, *current.Commit)
			status, ahead := "ahead", 1
			if sameCommit {
				status = "identical"
				ahead = 0
			}
			f.responses[endpoint] = map[string]any{"base_commit": map[string]any{"sha": *expected.Commit}, "merge_base_commit": map[string]any{"sha": *expected.Commit}, "ahead_by": ahead, "behind_by": 0, "status": status}
			result, err := performSync(t, expected, f, files)
			if err != nil || result.Status != "superseded" || result.Build != 124 {
				t.Fatal(result, err)
			}
			object(f.responses[endpoint])["behind_by"] = 1
			if _, err := performSync(t, expected, f, files); err == nil {
				t.Fatal("accepted unrelated newer build")
			}
		})
	}
}

func TestSynchronizeRestartsReadbackWhenPinnedHeadMoves(t *testing.T) {
	item, f, files := setupSync(t)
	f.tap(item, fixtureNextHead)
	f.tap(item, fixtureHead)
	count := 0
	f.hook = func(endpoint string) any {
		if endpoint != "repos/"+TapRepository+"/git/ref/heads/tap" {
			return nil
		}
		count++
		sha := fixtureHead
		if count >= 4 {
			sha = fixtureNextHead
		}
		return map[string]any{"ref": "refs/heads/tap", "object": map[string]any{"sha": sha, "type": "commit"}}
	}
	result, err := performSync(t, item, f, files)
	if err != nil || result.Commit != fixtureNextHead {
		t.Fatal(result, err)
	}
}

func TestGitHubTransportBoundsSanitizationAndDeadlines(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "gh")
	record := filepath.Join(directory, "record")
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "fixture-inherited")
	t.Setenv("GH_DEBUG", "api")
	t.Setenv("SOURCE_GH_TOKEN", "fixture-source-env")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s' \"$GH_TOKEN\" \"$GITHUB_TOKEN\" \"$GH_DEBUG\" \"$SOURCE_GH_TOKEN\" > '" + record + "'\nprintf '%s' '{}'\n"
	_ = os.WriteFile(executable, []byte(script), 0700)
	client := Client{ActionsToken: "fixture-actions", SourceToken: "fixture-source"}
	endpoint := "repos/" + TapRepository + "/actions/runs/1"
	for _, actions := range []bool{false, true} {
		if _, err := client.Request(context.Background(), endpoint, RequestOptions{Actions: actions}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(record)
		expected := "fixture-source|||"
		if actions {
			expected = "fixture-actions|||"
		}
		if string(data) != expected {
			t.Fatal("unsafe credential environment")
		}
	}
	_ = os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s' 'fixture-private-response'\nexit 9\n"), 0700)
	_, err := client.Request(context.Background(), endpoint, RequestOptions{})
	if err == nil || strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "fixture-source") {
		t.Fatal("exposed API body/token", err)
	}
	_ = os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s' 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'\n"), 0700)
	if _, err := client.Request(context.Background(), endpoint, RequestOptions{Limit: 8}); err == nil {
		t.Fatal("accepted oversized body")
	}
	_ = os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 2\n"), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Request(ctx, endpoint, RequestOptions{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatal(err)
	}
	if _, err := (Client{}).Request(context.Background(), endpoint, RequestOptions{}); err == nil {
		t.Fatal("accepted missing token pair")
	}
}

func TestDeliveryStrictJSONAndCanonicalPortableSeed(t *testing.T) {
	for _, body := range []string{`{"a":1,"a":2}`, `{"outer":{"a":1,"a":2}}`, `{} {}`, `[`, "{\"\xff\":1}", strings.Repeat("[", 65) + strings.Repeat("]", 65)} {
		if _, err := parseJSON([]byte(body)); err == nil {
			t.Fatal("accepted malformed/ambiguous JSON")
		}
	}
	item := fixtureManifest(123, strings.Repeat("a", 40))
	if _, err := manifestBytes(append(jsonBody(item), bytes.Repeat([]byte(" "), maxMetadata)...)); err == nil {
		t.Fatal("accepted oversized manifest")
	}
	root := filepath.Join("..", "..")
	paths := ToolingPaths(root)
	if len(paths) != 4 {
		t.Fatal(paths)
	}
	for name, path := range paths {
		if filepath.IsAbs(path) || strings.Contains(name, "..") {
			t.Fatal("nonportable tooling map")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	// The standalone tap cannot import the app module. Its reviewed renderer must
	// nevertheless remain byte-identical to the app renderer.
	file, err := parser.ParseFile(token.NewFileSet(), paths["refresh.go"], nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	template := ""
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if len(declaration.Names) == 1 && declaration.Names[0].Name == "caskTemplate" {
			literal, ok := declaration.Values[0].(*ast.BasicLit)
			if !ok {
				t.Fatal("cask template must be a literal")
			}
			template, err = strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
		}
		return true
	})
	expected, err := tooling.RenderCask(item)
	if err != nil {
		t.Fatal(err)
	}
	actual := fmt.Sprintf(template, item.Version, *item.Build, item.SHA256, backend.ReleaseRepository, item.Tag, item.Asset, backend.ReleaseRepository)
	if actual != expected {
		t.Fatal("standalone tap renderer differs from canonical app renderer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForRun(ctx, fixtureRun, &fakeGitHub{t: t, responses: map[string]any{fmt.Sprintf("repos/%s/actions/runs/%d", TapRepository, fixtureRun): errors.New("cancelled")}}, time.Millisecond); err == nil {
		t.Fatal("accepted interrupted synchronization")
	}
}
