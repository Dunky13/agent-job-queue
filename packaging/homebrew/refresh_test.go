package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testPublishedHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fixtureManifest(build int) manifest {
	commit := strings.Repeat(strconv.Itoa(build), 40)
	body := []byte(fmt.Sprintf("archive-%d", build))
	sum := sha256.Sum256(body)
	return manifest{1, repository, "0.1.0", commit, build, fmt.Sprintf("build-%d-%.7s", build, commit), "Agent-Queue-0.1.0-universal.zip", hex.EncodeToString(sum[:])}
}
func jsonBytes(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
func normalized(value any) any {
	result, err := strictJSON(jsonBytes(value))
	if err != nil {
		panic(err)
	}
	return result
}
func fixtureRelease(data manifest) map[string]any {
	cask, err := renderCask(data)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256([]byte(cask))
	return asObject(normalized(map[string]any{"tag_name": data.Tag, "target_commitish": data.Commit, "draft": false, "assets": []any{map[string]any{"name": "agent-job-queue.json"}, map[string]any{"name": data.Asset}, map[string]any{"name": "agent-job-queue.rb", "size": len(cask), "digest": "sha256:" + hex.EncodeToString(sum[:])}}}))
}

type refreshFixture struct {
	t                                  *testing.T
	f                                  *refresher
	data                               map[string]manifest
	source                             map[string]map[string]any
	historical                         map[string][]byte
	listed                             []any
	downloads                          [][2]string
	brewCalls                          [][]string
	apiCalls                           []string
	publications                       []manifest
	remoteHead                         string
	badArchive, brewFailure, unrelated bool
	output                             bytes.Buffer
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	x := &refreshFixture{t: t, data: map[string]manifest{}, source: map[string]map[string]any{}, historical: map[string][]byte{}, remoteHead: testHead}
	root := t.TempDir()
	x.f = newRefresher(context.Background(), root, root, &x.output)
	for _, build := range []int{1, 2, 3} {
		data := fixtureManifest(build)
		x.data[data.Tag] = data
		x.source[data.Tag] = fixtureRelease(data)
		cask, _ := renderCask(data)
		x.historical[data.Tag] = []byte(cask)
	}
	x.listed = []any{x.source[fixtureManifest(2).Tag], x.source[fixtureManifest(1).Tag]}
	x.f.api = func(endpoint string) (any, error) {
		x.apiCalls = append(x.apiCalls, endpoint)
		switch {
		case endpoint == "repos/"+tapRepository+"/git/ref/heads/tap":
			return map[string]any{"object": map[string]any{"sha": x.remoteHead}}, nil
		case endpoint == "repos/"+repository+"/releases?per_page=100&page=1":
			return normalized(x.listed), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/compare/"):
			pair := strings.TrimPrefix(endpoint, "repos/"+repository+"/compare/")
			older, _, _ := strings.Cut(pair, "...")
			if x.unrelated {
				older = strings.Repeat("f", 40)
			}
			return map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": older}}, nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/releases/tags/"):
			tag := strings.TrimPrefix(endpoint, "repos/"+repository+"/releases/tags/")
			release, ok := x.source[tag]
			if !ok {
				return nil, errors.New("Public release API lookup failed")
			}
			return normalized(release), nil
		default:
			return nil, fmt.Errorf("unexpected fixture API endpoint: %s", endpoint)
		}
	}
	x.f.command = func(arguments []string, _ []byte) ([]byte, error) {
		if slices.Equal(arguments, []string{"git", "rev-parse", "HEAD"}) {
			return []byte(testHead + "\n"), nil
		}
		if len(arguments) >= 4 && slices.Equal(arguments[:3], []string{"gh", "release", "download"}) {
			tag := arguments[3]
			data, ok := x.data[tag]
			if !ok {
				return nil, errors.New("unknown fixture release download")
			}
			name := arguments[slices.Index(arguments, "--pattern")+1]
			destination := arguments[slices.Index(arguments, "--dir")+1]
			x.downloads = append(x.downloads, [2]string{tag, name})
			var body []byte
			switch name {
			case "agent-job-queue.json":
				body = jsonBytes(data)
			case "agent-job-queue.rb":
				body = x.historical[tag]
			default:
				body = []byte(fmt.Sprintf("archive-%d", data.Build))
				if x.badArchive {
					body = append(body, []byte("corrupt")...)
				}
			}
			return nil, os.WriteFile(filepath.Join(destination, name), body, 0600)
		}
		if len(arguments) >= 2 && slices.Equal(arguments[:2], []string{"brew", "ruby"}) {
			x.brewCalls = append(x.brewCalls, append([]string(nil), arguments...))
			body, err := os.ReadFile(arguments[len(arguments)-1])
			if err != nil {
				return nil, err
			}
			if !bytes.HasPrefix(body, []byte(`cask "agent-job-queue"`)) || strings.Contains(arguments[len(arguments)-1], "published-cask") {
				t.Fatal("release-controlled Ruby was executed")
			}
			if x.brewFailure {
				return nil, errors.New("fixture Homebrew DSL failure")
			}
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected fixture command %v", arguments)
	}
	x.f.publish = func(data manifest, cask, head string) error {
		expected, _ := renderCask(data)
		if cask != expected || head != testHead {
			t.Fatal("publication lost trusted renderer or expected head")
		}
		x.publications = append(x.publications, data)
		return nil
	}
	return x
}
func (x *refreshFixture) installCurrent(data manifest, body []byte) {
	x.t.Helper()
	path := filepath.Join(x.f.root, "Casks")
	if err := os.MkdirAll(path, 0700); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "agent-job-queue.json"), jsonBytes(data), 0600); err != nil {
		x.t.Fatal(err)
	}
	if body == nil {
		cask, _ := renderCask(data)
		body = []byte(cask)
	}
	if err := os.WriteFile(filepath.Join(path, "agent-job-queue.rb"), body, 0600); err != nil {
		x.t.Fatal(err)
	}
}
func (x *refreshFixture) setHistorical(tag string, body []byte) {
	x.historical[tag] = body
	sum := sha256.Sum256(body)
	assets := x.source[tag]["assets"].([]any)
	for _, asset := range assets {
		metadata := asObject(asset)
		if metadata["name"] == "agent-job-queue.rb" {
			metadata["size"] = json.Number(strconv.Itoa(len(body)))
			metadata["digest"] = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
}

func TestRefreshSelectsLatestNumericBuildAndValidatesBeforePublication(t *testing.T) {
	for _, scenario := range []string{"empty-publish", "dry-run", "exact-noop", "superseded-request", "same-commit-build"} {
		t.Run(scenario, func(t *testing.T) {
			x := newRefreshFixture(t)
			expectedTag, expectedCommit := "", ""
			publish := true
			switch scenario {
			case "dry-run":
				publish = false
			case "exact-noop":
				x.installCurrent(fixtureManifest(2), nil)
			case "superseded-request":
				expectedTag = fixtureManifest(1).Tag
				expectedCommit = fixtureManifest(1).Commit
			case "same-commit-build":
				data := fixtureManifest(2)
				data.Commit = fixtureManifest(1).Commit
				data.Tag = "build-2-" + data.Commit[:7]
				x.data[data.Tag] = data
				x.source[data.Tag] = fixtureRelease(data)
				x.listed = []any{x.source[data.Tag]}
				x.installCurrent(fixtureManifest(1), nil)
			}
			if err := x.f.refresh(publish, expectedTag, expectedCommit); err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "dry-run" || scenario == "exact-noop" {
				want = 0
			}
			if len(x.publications) != want || len(x.brewCalls) == 0 {
				t.Fatal(x.publications, x.brewCalls)
			}
			if want == 1 && x.publications[0].Build != 2 {
				t.Fatal("did not select highest numeric build")
			}
		})
	}
}

func TestRefreshExpectedIdentityBeforeIO(t *testing.T) {
	for _, pair := range [][2]string{{fixtureManifest(1).Tag, ""}, {"", fixtureManifest(1).Commit}, {"invalid", fixtureManifest(1).Commit}, {fixtureManifest(1).Tag, strings.Repeat("f", 40)}} {
		x := newRefreshFixture(t)
		if err := x.f.refresh(true, pair[0], pair[1]); err == nil {
			t.Fatal("accepted expected identity")
		}
		if len(x.apiCalls) != 0 || len(x.downloads) != 0 {
			t.Fatal("invalid expected identity reached IO")
		}
	}
}

func TestRefreshRejectsConflictingStaleAndUnsafeReleaseState(t *testing.T) {
	for _, scenario := range []string{"stale-head", "missing-expected", "expected-full-commit", "expected-newer", "current-downgrade", "current-metadata-drift", "current-cask-drift", "current-missing-release", "unpaired-manifest", "unpaired-cask", "archive-corrupt", "draft", "target", "duplicate-assets", "unrelated", "brew-failure"} {
		t.Run(scenario, func(t *testing.T) {
			x := newRefreshFixture(t)
			tag, commit := "", ""
			switch scenario {
			case "stale-head":
				x.remoteHead = strings.Repeat("f", 40)
			case "missing-expected":
				tag = fixtureManifest(3).Tag
				commit = fixtureManifest(3).Commit
				delete(x.source, tag)
			case "expected-full-commit":
				tag = fixtureManifest(1).Tag
				commit = fixtureManifest(1).Commit[:7] + strings.Repeat("f", 33)
			case "expected-newer":
				tag = fixtureManifest(3).Tag
				commit = fixtureManifest(3).Commit
			case "current-downgrade":
				x.installCurrent(fixtureManifest(3), nil)
			case "current-metadata-drift":
				current := fixtureManifest(2)
				current.SHA256 = strings.Repeat("f", 64)
				x.installCurrent(current, nil)
			case "current-cask-drift":
				x.installCurrent(fixtureManifest(2), []byte("unknown Ruby"))
			case "current-missing-release":
				x.installCurrent(fixtureManifest(1), nil)
				delete(x.source, fixtureManifest(1).Tag)
			case "unpaired-manifest":
				x.installCurrent(fixtureManifest(1), nil)
				_ = os.Remove(filepath.Join(x.f.root, "Casks/agent-job-queue.rb"))
			case "unpaired-cask":
				x.installCurrent(fixtureManifest(1), nil)
				_ = os.Remove(filepath.Join(x.f.root, "Casks/agent-job-queue.json"))
			case "archive-corrupt":
				x.installCurrent(fixtureManifest(2), nil)
				x.badArchive = true
			case "draft":
				x.source[fixtureManifest(2).Tag]["draft"] = true
				tag, commit = fixtureManifest(2).Tag, fixtureManifest(2).Commit
			case "target":
				x.source[fixtureManifest(2).Tag]["target_commitish"] = "main"
			case "duplicate-assets":
				assets := x.source[fixtureManifest(2).Tag]["assets"].([]any)
				x.source[fixtureManifest(2).Tag]["assets"] = append(assets, assets[0])
			case "unrelated":
				x.installCurrent(fixtureManifest(1), nil)
				x.unrelated = true
			case "brew-failure":
				x.brewFailure = true
			}
			if err := x.f.refresh(true, tag, commit); err == nil {
				t.Fatal("unsafe refresh accepted")
			}
			if len(x.publications) != 0 {
				t.Fatal("published invalid refresh")
			}
			if scenario == "stale-head" && len(x.downloads) != 0 {
				t.Fatal("stale checkout downloaded source")
			}
		})
	}
}

func TestRenderedCaskRetainsQuarantine(t *testing.T) {
	cask, err := renderCask(fixtureManifest(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"preflight", "/usr/bin/xattr", "com.apple.quarantine", "source/scripts/install-user.sh"} {
		if strings.Contains(cask, unsafe) {
			t.Fatalf("rendered cask contains quarantine bypass: %s", unsafe)
		}
	}
	for _, expected := range []string{"Agent-Queue-0.1.0-universal.zip\"", "\n  app \"Agent Queue.app\"\n", "\n  uninstall quit: \"io.went.agent-queue\"\n", "Open Agent Queue from Applications to use its menu bar.", "--unregister-services", "Preserve quarantine so macOS can assess the notarized release."} {
		if !strings.Contains(cask, expected) {
			t.Fatalf("rendered cask is missing %q", expected)
		}
	}
	for _, forbidden := range []string{"finish setup", "after setup completes", "installer script:", "uninstall script:", "binary \"", "target:", "--repair-and-continue", "postflight", "sudo: true", "system_command", "run \"", "Installer.app"} {
		if strings.Contains(cask, forbidden) {
			t.Fatalf("rendered cask contains setup execution or installer ownership: %s", forbidden)
		}
	}
}

func TestRefreshHistoricalRubyIsComparedAsDataAndNeverExecuted(t *testing.T) {
	for _, scenario := range []string{"upgrade", "same-build", "legacy-quarantine-upgrade", "legacy-quarantine-same-build", "no-optional-digest", "unknown-current", "digest-mismatch", "declared-size", "actual-size", "duplicate", "unsupported-digest", "archive-first"} {
		t.Run(scenario, func(t *testing.T) {
			x := newRefreshFixture(t)
			current := fixtureManifest(1)
			sameBuild := scenario == "same-build" || scenario == "legacy-quarantine-same-build"
			if sameBuild {
				current = fixtureManifest(2)
			}
			historical := []byte("# Historical template, comparison only\nraise 'must never execute'\n")
			if strings.HasPrefix(scenario, "legacy-quarantine-") {
				historical = []byte("cask \"agent-job-queue\" do\n  preflight do\n    system_command \"/usr/bin/xattr\", args: [\"-dr\", \"com.apple.quarantine\", staged_path]\n  end\nend\n")
			}
			x.setHistorical(current.Tag, historical)
			x.installCurrent(current, historical)
			assets := x.source[current.Tag]["assets"].([]any)
			historicalMetadata := asObject(assets[2])
			wantFailure := false
			switch scenario {
			case "no-optional-digest":
				delete(historicalMetadata, "digest")
			case "unknown-current":
				x.installCurrent(current, []byte("unrecognized current Ruby"))
				wantFailure = true
			case "digest-mismatch":
				historicalMetadata["digest"] = "sha256:" + strings.Repeat("f", 64)
				wantFailure = true
			case "declared-size":
				historicalMetadata["size"] = json.Number("16385")
				wantFailure = true
			case "actual-size":
				historicalMetadata["size"] = json.Number("1")
				wantFailure = true
			case "duplicate":
				x.source[current.Tag]["assets"] = append(assets, historicalMetadata)
				wantFailure = true
			case "unsupported-digest":
				historicalMetadata["digest"] = "sha512:unsupported"
				wantFailure = true
			case "archive-first":
				x.badArchive = true
				wantFailure = true
			}
			err := x.f.refresh(true, "", "")
			if (err != nil) != wantFailure {
				t.Fatal(err)
			}
			if wantFailure && len(x.publications) != 0 {
				t.Fatal("published invalid historical state")
			}
			if !wantFailure && !sameBuild && len(x.publications) != 1 {
				t.Fatal("template migration blocked upgrade")
			}
			if sameBuild && len(x.publications) != 0 {
				t.Fatal("rewrote same immutable build")
			}
			if sameBuild {
				installed, err := os.ReadFile(filepath.Join(x.f.root, "Casks", "agent-job-queue.rb"))
				if err != nil || !bytes.Equal(installed, historical) || !strings.Contains(x.output.String(), "template change awaits a newer app release") {
					t.Fatalf("historical same-build cask was not preserved: %v", err)
				}
			}
			if scenario == "archive-first" {
				for _, download := range x.downloads {
					if download[1] == "agent-job-queue.rb" {
						t.Fatal("historical cask read before archive verification")
					}
				}
			}
		})
	}
}

// Taps published before the drag-and-drop app hold installer-wrapper metadata.
// Only that predecessor state may use the old asset name; it is verified but
// never evaluated, and the first app release replaces it.
func TestRefreshMigratesLegacyInstallerTapState(t *testing.T) {
	legacyName := "Agent-Job-Queue-Installer-0.1.0-universal.zip"
	legacify := func(x *refreshFixture, build int, name string) manifest {
		data := fixtureManifest(build)
		data.Asset = name
		x.data[data.Tag] = data
		asObject(x.source[data.Tag]["assets"].([]any)[1])["name"] = name
		return data
	}
	for _, scenario := range []string{"upgrade", "same-build", "latest-release", "unknown-asset"} {
		t.Run(scenario, func(t *testing.T) {
			x := newRefreshFixture(t)
			legacyBuild, name := 1, legacyName
			switch scenario {
			case "same-build":
				legacyBuild = 2
			case "unknown-asset":
				name = "Agent-Job-Queue-0.1.0-universal.zip"
			}
			current := legacify(x, legacyBuild, name)
			historical := []byte("# Legacy installer cask, comparison only\nraise 'must never execute'\n")
			x.setHistorical(current.Tag, historical)
			x.installCurrent(current, historical)
			if scenario == "latest-release" {
				legacify(x, 2, legacyName)
			}
			err := x.f.refresh(true, "", "")
			if (err != nil) != (scenario != "upgrade") {
				t.Fatalf("%s: %v", scenario, err)
			}
			want := 0
			if scenario == "upgrade" {
				want = 1
			}
			if len(x.publications) != want || (want == 1 && x.publications[0].Asset != "Agent-Queue-0.1.0-universal.zip") {
				t.Fatalf("unexpected publications %v", x.publications)
			}
			if scenario == "upgrade" {
				for _, call := range x.brewCalls {
					if strings.Contains(call[len(call)-1], fixtureManifest(1).Tag) {
						t.Fatal("legacy installer cask was evaluated by Homebrew")
					}
				}
			}
		})
	}
}

func TestRefreshValidatesLegacyManagedAppCaskAsData(t *testing.T) {
	for _, scenario := range []string{"known-template", "drift", "bad-archive", "unrelated-release"} {
		t.Run(scenario, func(t *testing.T) {
			x := newRefreshFixture(t)
			current := fixtureManifest(1)
			current.Asset = "Agent-Job-Queue-Installer-0.1.0-universal.zip"
			if scenario == "bad-archive" {
				current.SHA256 = strings.Repeat("f", 64)
			}
			x.data[current.Tag] = current
			asObject(x.source[current.Tag]["assets"].([]any)[1])["name"] = current.Asset
			// The public release holds an older script-installer template. The
			// current tap was generated later by the trusted managed-app template.
			x.setHistorical(current.Tag, []byte("# Original public installer cask\nraise 'never execute'\n"))
			installed := []byte(legacyManagedAppFixture)
			if scenario == "drift" {
				installed = append(installed, []byte("\n# untrusted drift\n")...)
			}
			x.installCurrent(current, installed)
			x.unrelated = scenario == "unrelated-release"
			err := x.f.refresh(true, "", "")
			if (err != nil) != (scenario != "known-template") {
				t.Fatalf("%s: %v", scenario, err)
			}
			if scenario == "bad-archive" && !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("legacy archive was not rejected by checksum verification: %v", err)
			}
			want := 0
			if scenario == "known-template" {
				want = 1
			}
			if len(x.publications) != want {
				t.Fatalf("unexpected publications: %v", x.publications)
			}
			for _, call := range x.brewCalls {
				if strings.Contains(call[len(call)-1], current.Tag) {
					t.Fatal("legacy cask was executed instead of compared as data")
				}
			}
		})
	}
}

func TestRefreshReleasePageBoundAndSelection(t *testing.T) {
	t.Run("page two", func(t *testing.T) {
		f := newRefresher(context.Background(), t.TempDir(), ".", io.Discard)
		calls := 0
		f.api = func(string) (any, error) {
			calls++
			if calls == 1 {
				page := make([]any, 100)
				for index := range page {
					page[index] = map[string]any{"draft": true}
				}
				return page, nil
			}
			return []any{fixtureRelease(fixtureManifest(3))}, nil
		}
		releases, err := f.publishedReleases()
		if err != nil || calls != 2 {
			t.Fatal(err, calls)
		}
		latest, err := selectRelease(releases)
		if err != nil || latest["tag_name"] != fixtureManifest(3).Tag {
			t.Fatal(latest, err)
		}
	})
	t.Run("incomplete pages", func(t *testing.T) {
		f := newRefresher(context.Background(), ".", ".", io.Discard)
		f.api = func(string) (any, error) {
			page := make([]any, 100)
			for index := range page {
				page[index] = map[string]any{"draft": true}
			}
			return page, nil
		}
		if _, err := f.publishedReleases(); err == nil {
			t.Fatal("selected from truncated release history")
		}
	})
	t.Run("ambiguous latest", func(t *testing.T) {
		first := fixtureManifest(2)
		second := first
		second.Commit = strings.Repeat("f", 40)
		second.Tag = "build-2-fffffff"
		if _, err := selectRelease([]map[string]any{fixtureRelease(first), fixtureRelease(second)}); err == nil {
			t.Fatal("accepted ambiguous latest build")
		}
		if latest, err := selectRelease([]map[string]any{fixtureRelease(first), fixtureRelease(second), fixtureRelease(fixtureManifest(3))}); err != nil || latest["tag_name"] != fixtureManifest(3).Tag {
			t.Fatal("rejected ambiguity below latest", err)
		}
	})
	t.Run("malformed page", func(t *testing.T) {
		for _, value := range []any{map[string]any{}, []any{true}, nil} {
			f := newRefresher(context.Background(), ".", ".", io.Discard)
			f.api = func(string) (any, error) { return value, nil }
			if _, err := f.publishedReleases(); err == nil {
				t.Fatal("accepted malformed page")
			}
		}
	})
}

func TestRefreshStrictManifestAndSafeExpectedInputs(t *testing.T) {
	valid := fixtureManifest(1)
	body := jsonBytes(valid)
	for _, data := range [][]byte{[]byte(`[]`), []byte(`null`), []byte(`{"a":1,"a":2}`), append(body, []byte(" {}")...), []byte(strings.Replace(string(body), `"build":1`, `"build":true`, 1)), []byte(strings.Replace(string(body), repository, "other/project", 1)), []byte(strings.Replace(string(body), `"version":"0.1.0"`, `"version":"01.1.0"`, 1)), append(body, bytes.Repeat([]byte(" "), metadataLimit)...)} {
		if _, err := validateManifest(data); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
	if _, err := validateManifest(body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schemaVersion", "repository", "version", "commit", "build", "tag", "asset", "sha256"} {
		var value map[string]any
		_ = json.Unmarshal(body, &value)
		value[field] = nil
		if _, err := validateManifest(jsonBytes(value)); err == nil {
			t.Fatal("accepted null", field)
		}
	}
}

func TestRefreshPublicationSignedCASAndMatchingAuthor(t *testing.T) {
	for _, scenario := range []string{"success", "cas-rejected", "non-bot", "signature", "author", "dco", "commit-identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRefresher(context.Background(), ".", ".", io.Discard)
			var captured map[string]any
			mutations := 0
			f.graphql = func(query string, variables map[string]any) (map[string]any, error) {
				if strings.HasPrefix(query, "query") {
					login := botName
					if scenario == "non-bot" {
						login = "other"
					}
					return map[string]any{"viewer": map[string]any{"login": login}}, nil
				}
				mutations++
				captured = variables
				if scenario == "cas-rejected" {
					return nil, errors.New("fixture expected-head CAS rejected")
				}
				return map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": testPublishedHead}}}, nil
			}
			f.api = func(string) (any, error) {
				sha := testPublishedHead
				verified := true
				name := botName
				message := "Signed-off-by: " + botName + " <" + botEmail + ">"
				switch scenario {
				case "signature":
					verified = false
				case "author":
					name = "other"
				case "dco":
					message = "Signed-off-by: Wrong <wrong@example.invalid>"
				case "commit-identity":
					sha = testHead
				}
				return map[string]any{"sha": sha, "commit": map[string]any{"author": map[string]any{"name": name, "email": botEmail}, "verification": map[string]any{"verified": verified}, "message": message}}, nil
			}
			data := fixtureManifest(2)
			cask, _ := renderCask(data)
			err := f.publishCommit(data, cask, testHead)
			if (err == nil) != (scenario == "success") {
				t.Fatal(err)
			}
			if scenario == "non-bot" {
				if mutations != 0 {
					t.Fatal("non-bot reached mutation")
				}
				return
			}
			if mutations != 1 {
				t.Fatal("publication retried or skipped CAS")
			}
			input := asObject(captured["input"])
			if input["expectedHeadOid"] != testHead || asObject(input["branch"])["branchName"] != "tap" || asObject(input["message"])["body"] != "Signed-off-by: "+botName+" <"+botEmail+">" {
				t.Fatal(captured)
			}
			additions := asObject(input["fileChanges"])["additions"].([]map[string]any)
			body, decodeErr := base64.StdEncoding.DecodeString(asString(additions[0]["contents"]))
			if decodeErr != nil || string(body) != cask {
				t.Fatal("publication changed trusted cask")
			}
		})
	}
}

func TestRefreshPublicationEnvironmentGate(t *testing.T) {
	valid := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": tapRepository, "GITHUB_REF": "refs/heads/tap", "GITHUB_EVENT_NAME": "schedule"}
	if !publicationAllowed(valid) {
		t.Fatal("valid scheduled tap publication rejected")
	}
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF", "GITHUB_EVENT_NAME"} {
		environment := map[string]string{}
		for field, value := range valid {
			environment[field] = value
		}
		environment[key] = "invalid"
		if publicationAllowed(environment) {
			t.Fatal("invalid publication environment accepted", key)
		}
	}
	valid["GITHUB_EVENT_NAME"] = "workflow_dispatch"
	if !publicationAllowed(valid) {
		t.Fatal("valid manual tap publication rejected")
	}
	if reflect.DeepEqual(valid, map[string]string{}) {
		t.Fatal("fixture unexpectedly empty")
	}
}

// Captured legacy managed-app cask, rebased to the build-1 fixture identity.
const legacyManagedAppFixture = `cask "agent-job-queue" do
  version "0.1.0,1"
  sha256 "510e3dbd33aab02ab240110ad683235b3727b8b0137c839ad67ddbf8a9af7b4d"

  url "https://github.com/Dunky13/agent-job-queue/releases/download/build-1-1111111/Agent-Job-Queue-Installer-0.1.0-universal.zip"
  name "Agent Job Queue"
  desc "Shared heavy-command queue with a macOS menu-bar app"
  homepage "https://github.com/Dunky13/agent-job-queue"

  depends_on macos: :sequoia

  # Preserve quarantine so macOS can assess the notarized release.

  application = "#{Dir.home}/Applications/Agent Queue.app"
  app "Agent Job Queue Installer.app/Contents/Resources/payload/Agent Queue.app", target: application

  # Stable per-user wrappers survive staged cask cleanup and future upgrades.
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-heavy"
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-remote"

  # The app artifact owns app removal and upgrade rollback.
  uninstall quit: "io.went.agent-queue",
            script: {
              executable: "/bin/rm",
              args: ["-f", "--", "#{Dir.home}/Library/LaunchAgents/io.went.agent-queue.plist"],
              sudo: false,
            }

  caveats <<~EOS
    Installs Agent Queue for this account without Python, uv, or a Go toolchain.
    Open Agent Queue to finish setup. Command-line tools are ready after setup completes.
    Release installer and app are Developer ID signed and notarized.
    Quarantine is retained. Phone access is off until enabled.
    Uninstall preserves the independent queue, jobs, credentials and remote access.
    Disable phone access in settings before uninstall to stop its HTTPS service.
  EOS
end
`
