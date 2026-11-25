package tap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testMoved = "cccccccccccccccccccccccccccccccccccccccc"
const testPublished = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testBot = "homebrew-sync[bot]"
const testEmail = "12345+homebrew-sync[bot]@users.noreply.github.com"

var testSignature = strings.Repeat("A", 86) + "=="

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
	return asObject(normalized(map[string]any{"tag_name": data.Tag, "target_commitish": data.Commit, "draft": false, "assets": []any{map[string]any{"name": "agent-job-queue.json"}, map[string]any{"name": data.Asset}}}))
}

// deliveryFiles is the exact delivery the fixture expects for a build.
func deliveryFiles(t testing.TB, data manifest) files {
	t.Helper()
	cask, err := renderCask(data)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(data, "", "  ")
	archive := fmt.Sprintf("archive-%d", data.Build)
	return files{caskPath: []byte(cask), manifestPath: append(body, '\n'), appcastPath: []byte(renderAppcast(data, int64(len(archive)), testSignature) + "<!-- sparkle-signatures:\nedSignature: test\n-->\n")}
}

type fixture struct {
	t               *testing.T
	f               *publisher
	data            map[string]manifest
	source          map[string]map[string]any
	listed          []any
	head            string
	state           files            // main at head
	commits         map[string]files // content by commit
	mutations       []map[string]any
	failMutations   int  // fail this many mutations
	moveOnFailure   bool // a concurrent commit lands when a mutation fails
	unrelated       bool
	badArchive      bool
	brewFailure     bool
	commitResponse  func(map[string]any)
	downloads       int
	signCalls       [][]string
	signInputs      []string
	brewCalls       int
	output          bytes.Buffer
	publishedCommit string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	x := &fixture{t: t, data: map[string]manifest{}, source: map[string]map[string]any{}, head: testHead, state: files{}, commits: map[string]files{}, publishedCommit: testPublished}
	x.f = newPublisher(context.Background(), t.TempDir(), &x.output)
	x.f.signTool, x.f.signKey, x.f.botName, x.f.botEmail = "sign-update", "TEST-PRIVATE-KEY", testBot, testEmail
	for _, build := range []int{1, 2, 3} {
		data := fixtureManifest(build)
		x.data[data.Tag] = data
		x.source[data.Tag] = fixtureRelease(data)
	}
	x.listed = []any{x.source[fixtureManifest(2).Tag], x.source[fixtureManifest(1).Tag]}
	x.f.api = func(endpoint string) (any, error) {
		switch {
		case endpoint == "repos/"+repository+"/git/ref/heads/main":
			return map[string]any{"object": map[string]any{"sha": x.head}}, nil
		case endpoint == "repos/"+repository+"/releases?per_page=100&page=1":
			return normalized(x.listed), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/compare/"):
			older, _, _ := strings.Cut(strings.TrimPrefix(endpoint, "repos/"+repository+"/compare/"), "...")
			if x.unrelated {
				older = strings.Repeat("f", 40)
			}
			return map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": older}}, nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/releases/tags/"):
			release, ok := x.source[strings.TrimPrefix(endpoint, "repos/"+repository+"/releases/tags/")]
			if !ok {
				return nil, errors.New("Public release API lookup failed")
			}
			return normalized(release), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/commits/"):
			sha := strings.TrimPrefix(endpoint, "repos/"+repository+"/commits/")
			response := map[string]any{"sha": sha, "parents": []any{map[string]any{"sha": testHead}}, "commit": map[string]any{"author": map[string]any{"name": testBot, "email": testEmail}, "verification": map[string]any{"verified": true}, "message": "chore\n\nSigned-off-by: " + testBot + " <" + testEmail + ">"}}
			if x.commitResponse != nil {
				x.commitResponse(response)
			}
			return normalized(response), nil
		}
		return nil, fmt.Errorf("unexpected fixture API endpoint: %s", endpoint)
	}
	x.f.raw = func(endpoint string) ([]byte, error) {
		prefix := "repos/" + repository + "/contents/"
		path, ref, _ := strings.Cut(strings.TrimPrefix(endpoint, prefix), "?ref=")
		content := x.commits[ref]
		if ref == x.head {
			content = x.state
		}
		body, ok := content[path]
		if !ok {
			return nil, errNotFound
		}
		return body, nil
	}
	x.f.command = func(arguments []string, input []byte) ([]byte, error) {
		switch {
		case len(arguments) >= 4 && slices.Equal(arguments[:3], []string{"gh", "release", "download"}):
			data, ok := x.data[arguments[3]]
			if !ok {
				return nil, errors.New("unknown fixture release download")
			}
			name := arguments[slices.Index(arguments, "--pattern")+1]
			destination := arguments[slices.Index(arguments, "--dir")+1]
			x.downloads++
			body := []byte(fmt.Sprintf("archive-%d", data.Build))
			if name == "agent-job-queue.json" {
				body = jsonBytes(data)
			} else if x.badArchive {
				body = append(body, "corrupt"...)
			}
			return nil, os.WriteFile(filepath.Join(destination, name), body, 0600)
		case len(arguments) >= 2 && slices.Equal(arguments[:2], []string{"brew", "ruby"}):
			x.brewCalls++
			body, err := os.ReadFile(arguments[len(arguments)-1])
			if err != nil || !bytes.HasPrefix(body, []byte(`cask "agent-job-queue"`)) || x.brewFailure {
				return nil, errors.New("fixture Homebrew DSL failure")
			}
			return nil, nil
		case arguments[0] == "sign-update":
			x.signCalls = append(x.signCalls, arguments)
			x.signInputs = append(x.signInputs, string(input))
			if slices.Contains(arguments, "-p") {
				return []byte(testSignature + "\n"), nil
			}
			path := arguments[len(arguments)-1]
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			return nil, os.WriteFile(path, append(body, "<!-- sparkle-signatures:\nedSignature: test\n-->\n"...), 0600)
		}
		return nil, fmt.Errorf("unexpected fixture command %v", arguments)
	}
	x.f.graphql = func(query string, variables map[string]any) (map[string]any, error) {
		x.mutations = append(x.mutations, variables)
		if len(x.mutations) <= x.failMutations {
			if x.moveOnFailure {
				x.head = testMoved
			}
			return nil, errors.New("fixture expected-head CAS rejected")
		}
		input := asObject(variables["input"])
		published := files{}
		for _, addition := range input["fileChanges"].(map[string]any)["additions"].([]map[string]any) {
			body, err := base64.StdEncoding.DecodeString(addition["contents"].(string))
			if err != nil {
				return nil, err
			}
			published[addition["path"].(string)] = body
		}
		x.commits[x.publishedCommit] = published
		return map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": x.publishedCommit}}}, nil
	}
	return x
}

// install puts a delivered build on main, as an earlier run would have.
func (x *fixture) install(data manifest) {
	x.state = deliveryFiles(x.t, data)
}

func TestRefreshSelectsLatestBuildAndPublishesOneSignedCommit(t *testing.T) {
	for _, scenario := range []string{"first-delivery", "dry-run", "exact-noop", "newer-than-main", "superseded-request", "same-build-template-change"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			tag, commit, publish, commitsWanted := "", "", true, 1
			switch scenario {
			case "dry-run":
				publish, commitsWanted = false, 0
			case "exact-noop":
				x.install(fixtureManifest(2))
				commitsWanted = 0
			case "newer-than-main":
				x.install(fixtureManifest(1))
			case "superseded-request":
				tag, commit = fixtureManifest(1).Tag, fixtureManifest(1).Commit
			case "same-build-template-change":
				x.install(fixtureManifest(2))
				x.state[caskPath] = []byte(strings.Replace(string(x.state[caskPath]), "  auto_updates true\n", "", 1))
			}
			x.head = testHead
			if err := x.f.refresh(publish, tag, commit); err != nil {
				t.Fatal(err)
			}
			if len(x.mutations) != commitsWanted || x.brewCalls == 0 {
				t.Fatalf("mutations %d brew %d", len(x.mutations), x.brewCalls)
			}
			if !publish && len(x.signCalls) != 0 {
				t.Fatal("dry run signed")
			}
			if commitsWanted == 1 {
				got := x.commits[testPublished]
				if !sameFiles(got, deliveryFiles(t, fixtureManifest(2))) {
					t.Fatal("did not deliver the highest build as cask, manifest and signed feed")
				}
				input := asObject(x.mutations[0]["input"])
				if input["expectedHeadOid"] != testHead || asObject(input["branch"])["branchName"] != "main" || asObject(input["branch"])["repositoryNameWithOwner"] != repository || asObject(input["message"])["body"] != "Signed-off-by: "+testBot+" <"+testEmail+">" {
					t.Fatal(input)
				}
			}
		})
	}
}

func TestRefreshExpectedIdentityBeforeIO(t *testing.T) {
	for _, pair := range [][2]string{{fixtureManifest(1).Tag, ""}, {"", fixtureManifest(1).Commit}, {"invalid", fixtureManifest(1).Commit}, {fixtureManifest(1).Tag, strings.Repeat("f", 40)}} {
		x := newFixture(t)
		x.f.api = func(string) (any, error) { t.Fatal("invalid expected identity reached IO"); return nil, nil }
		if err := x.f.refresh(true, pair[0], pair[1]); err == nil {
			t.Fatal("accepted expected identity")
		}
	}
}

func TestRefreshRejectsConflictingStaleAndUnsafeState(t *testing.T) {
	for _, scenario := range []string{"missing-expected", "expected-full-commit", "expected-newer", "current-downgrade", "current-metadata-drift", "current-missing-release", "unpaired-manifest", "unpaired-cask", "archive-corrupt", "draft", "target", "duplicate-assets", "unrelated", "brew-failure", "invalid-current-manifest"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			tag, commit := "", ""
			switch scenario {
			case "missing-expected":
				tag, commit = fixtureManifest(3).Tag, fixtureManifest(3).Commit
				delete(x.source, tag)
			case "expected-full-commit":
				tag, commit = fixtureManifest(1).Tag, fixtureManifest(1).Commit[:7]+strings.Repeat("f", 33)
			case "expected-newer":
				tag, commit = fixtureManifest(3).Tag, fixtureManifest(3).Commit
			case "current-downgrade":
				x.install(fixtureManifest(3))
			case "current-metadata-drift":
				current := fixtureManifest(2)
				current.SHA256 = strings.Repeat("f", 64)
				x.install(current)
			case "current-missing-release":
				x.install(fixtureManifest(1))
				delete(x.source, fixtureManifest(1).Tag)
			case "unpaired-manifest":
				x.install(fixtureManifest(1))
				delete(x.state, caskPath)
			case "unpaired-cask":
				x.install(fixtureManifest(1))
				delete(x.state, manifestPath)
			case "invalid-current-manifest":
				x.install(fixtureManifest(1))
				x.state[manifestPath] = []byte("{}")
			case "archive-corrupt":
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
				x.install(fixtureManifest(1))
				x.unrelated = true
			case "brew-failure":
				x.brewFailure = true
			}
			if err := x.f.refresh(true, tag, commit); err == nil {
				t.Fatal("unsafe delivery accepted")
			}
			if len(x.mutations) != 0 || len(x.signCalls) > 2 && scenario == "brew-failure" {
				t.Fatal("published or signed an invalid delivery")
			}
		})
	}
}

func TestAppcastMatchesGoldenFile(t *testing.T) {
	golden, err := os.ReadFile("testdata/appcast.golden.xml")
	if err != nil {
		t.Fatal(err)
	}
	data := fixtureManifest(7)
	got := renderAppcast(data, 12345678, testSignature)
	if got != string(golden) {
		t.Fatalf("appcast differs from the golden file:\n%s", got)
	}
}

func TestRenderedCaskUpdatesItselfAndRetainsQuarantine(t *testing.T) {
	cask, err := renderCask(fixtureManifest(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"\n  auto_updates true\n", "Agent-Queue-0.1.0-universal.zip\"", "\n  app \"Agent Queue.app\"\n", "Preserve quarantine so macOS can assess the notarized release."} {
		if !strings.Contains(cask, expected) {
			t.Fatalf("rendered cask is missing %q", expected)
		}
	}
	for _, unsafe := range []string{"preflight", "/usr/bin/xattr", "com.apple.quarantine", "postflight", "binary \""} {
		if strings.Contains(cask, unsafe) {
			t.Fatalf("rendered cask contains %s", unsafe)
		}
	}
}

// The key must reach the signer on stdin only, using the real command runner.
func TestSignerReceivesKeyOnStdinNeverArguments(t *testing.T) {
	directory := t.TempDir()
	tool := filepath.Join(directory, "sign_update")
	script := `#!/bin/bash
printf '%s\n' "$@" >> "$FAKE_ARGS"
cat >> "$FAKE_STDIN"
if [[ " $* " == *" -p "* ]]; then printf '%s\n' "$FAKE_SIGNATURE"; else printf '<!-- sparkle-signatures:\nedSignature: test\n-->\n' >> "${@: -1}"; fi
`
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	args, stdin := filepath.Join(directory, "args"), filepath.Join(directory, "stdin")
	t.Setenv("FAKE_ARGS", args)
	t.Setenv("FAKE_STDIN", stdin)
	t.Setenv("FAKE_SIGNATURE", testSignature)
	f := newPublisher(context.Background(), directory, io.Discard)
	f.signTool, f.signKey = tool, "TOP-SECRET-KEY-MATERIAL"
	archive := filepath.Join(directory, "archive.zip")
	if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	appcast, err := f.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(appcast, "<!-- sparkle-signatures:\nedSignature: test\n-->\n") || !strings.Contains(appcast, `length="7"`) || !strings.Contains(appcast, `sparkle:edSignature="`+testSignature+`"`) {
		t.Fatal(appcast)
	}
	argv, _ := os.ReadFile(args)
	input, _ := os.ReadFile(stdin)
	if bytes.Contains(argv, []byte("TOP-SECRET")) || !strings.Contains(string(argv), "--ed-key-file\n-\n") || strings.Contains(string(argv), "\n-s\n") {
		t.Fatalf("key reached argv or the key file was not stdin: %s", argv)
	}
	if string(input) != "TOP-SECRET-KEY-MATERIAL\nTOP-SECRET-KEY-MATERIAL\n" {
		t.Fatalf("key not delivered on stdin for both signatures: %q", input)
	}
}

func TestSigningRejectsMissingKeyAndUnsignedFeed(t *testing.T) {
	x := newFixture(t)
	x.f.signKey = ""
	if _, err := x.f.signedAppcast(fixtureManifest(2), "x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("missing key accepted", err)
	}
	for _, signature := range []string{"", "short", testSignature + "extra"} {
		y := newFixture(t)
		archive := filepath.Join(t.TempDir(), "a.zip")
		_ = os.WriteFile(archive, []byte("a"), 0600)
		y.f.command = func([]string, []byte) ([]byte, error) { return []byte(signature), nil }
		if _, err := y.f.signedAppcast(fixtureManifest(2), archive, t.TempDir()); err == nil {
			t.Fatalf("accepted signature %q", signature)
		}
	}
	// A signer that leaves the feed untouched must not publish an unsigned feed.
	y := newFixture(t)
	archive := filepath.Join(t.TempDir(), "a.zip")
	_ = os.WriteFile(archive, []byte("a"), 0600)
	y.f.command = func(arguments []string, _ []byte) ([]byte, error) {
		if slices.Contains(arguments, "-p") {
			return []byte(testSignature), nil
		}
		return nil, nil
	}
	if _, err := y.f.signedAppcast(fixtureManifest(2), archive, t.TempDir()); err == nil {
		t.Fatal("accepted an unsigned feed")
	}
}

func TestPublicationRetriesWhenMainMovesAndStopsOtherwise(t *testing.T) {
	t.Run("head moved", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations, x.moveOnFailure = 1, true
		x.commitResponse = func(response map[string]any) { response["parents"] = []any{map[string]any{"sha": testMoved}} }
		if err := x.f.refresh(true, "", ""); err != nil {
			t.Fatal(err)
		}
		if len(x.mutations) != 2 || asObject(x.mutations[0]["input"])["expectedHeadOid"] != testHead || asObject(x.mutations[1]["input"])["expectedHeadOid"] != testMoved {
			t.Fatalf("retry did not re-read main: %v", x.mutations)
		}
		if len(x.signCalls) != 2 {
			t.Fatal("retry signed again")
		}
	})
	t.Run("rejected with unchanged head", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations = 1
		err := x.f.refresh(true, "", "")
		if err == nil || len(x.mutations) != 1 || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
			t.Fatal("permission failure must fail visibly with a docs pointer", err, len(x.mutations))
		}
	})
	t.Run("retries are bounded", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations, x.moveOnFailure = 100, true
		calls := 0
		x.f.api = wrapHead(x, &calls)
		if err := x.f.refresh(true, "", ""); err == nil || len(x.mutations) != publishAttempts {
			t.Fatal("unbounded or silent retries", err, len(x.mutations))
		}
	})
}

// wrapHead lets main move on every failed attempt so each retry sees a new head.
func wrapHead(x *fixture, calls *int) func(string) (any, error) {
	inner := x.f.api
	return func(endpoint string) (any, error) {
		if endpoint == "repos/"+repository+"/git/ref/heads/main" {
			*calls++
			sha := strings.Repeat(strconv.Itoa(*calls%10), 40)
			x.head = sha
			return map[string]any{"object": map[string]any{"sha": sha}}, nil
		}
		return inner(endpoint)
	}
}

func TestPublicationReadbackVerifiesCommit(t *testing.T) {
	for _, scenario := range []string{"success", "signature", "author", "email", "dco", "identity", "parent", "content"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			x.commitResponse = func(response map[string]any) {
				commit := response["commit"].(map[string]any)
				switch scenario {
				case "signature":
					commit["verification"] = map[string]any{"verified": false}
				case "author":
					commit["author"] = map[string]any{"name": "other", "email": testEmail}
				case "email":
					commit["author"] = map[string]any{"name": testBot, "email": "other@example.invalid"}
				case "dco":
					commit["message"] = "Signed-off-by: Wrong <wrong@example.invalid>"
				case "identity":
					response["sha"] = testHead
				case "parent":
					response["parents"] = []any{map[string]any{"sha": testMoved}}
				}
			}
			err := x.f.refresh(true, "", "")
			if scenario == "content" {
				x.commits[testPublished][appcastPath] = []byte("tampered")
				err = x.f.verifyCommit(testPublished, testHead, deliveryFiles(t, fixtureManifest(2)))
			}
			if (err == nil) != (scenario == "success") {
				t.Fatal(scenario, err)
			}
			if len(x.mutations) != 1 {
				t.Fatal("a verification failure must not retry or recommit")
			}
		})
	}
}

func TestIdenticalMainContentCreatesNoCommit(t *testing.T) {
	x := newFixture(t)
	x.install(fixtureManifest(2))
	if err := x.f.refresh(true, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(x.mutations) != 0 || !strings.Contains(x.output.String(), "No commit created") {
		t.Fatal("identical content must verify without committing")
	}
}

func TestRefreshReleasePageBoundAndSelection(t *testing.T) {
	drafts := func() any {
		page := make([]any, 100)
		for index := range page {
			page[index] = map[string]any{"draft": true}
		}
		return page
	}
	t.Run("page two", func(t *testing.T) {
		f := newPublisher(context.Background(), ".", io.Discard)
		calls := 0
		f.api = func(string) (any, error) {
			calls++
			if calls == 1 {
				return drafts(), nil
			}
			return []any{fixtureRelease(fixtureManifest(3))}, nil
		}
		releases, err := f.publishedReleases()
		if err != nil || calls != 2 {
			t.Fatal(err, calls)
		}
		if latest, err := selectRelease(releases); err != nil || latest["tag_name"] != fixtureManifest(3).Tag {
			t.Fatal(latest, err)
		}
	})
	t.Run("incomplete pages", func(t *testing.T) {
		f := newPublisher(context.Background(), ".", io.Discard)
		f.api = func(string) (any, error) { return drafts(), nil }
		if _, err := f.publishedReleases(); err == nil {
			t.Fatal("selected from truncated release history")
		}
	})
	t.Run("ambiguous latest", func(t *testing.T) {
		first := fixtureManifest(2)
		second := first
		second.Commit, second.Tag = strings.Repeat("f", 40), "build-2-fffffff"
		if _, err := selectRelease([]map[string]any{fixtureRelease(first), fixtureRelease(second)}); err == nil {
			t.Fatal("accepted ambiguous latest build")
		}
		if latest, err := selectRelease([]map[string]any{fixtureRelease(first), fixtureRelease(second), fixtureRelease(fixtureManifest(3))}); err != nil || latest["tag_name"] != fixtureManifest(3).Tag {
			t.Fatal("rejected ambiguity below latest", err)
		}
	})
	t.Run("malformed page", func(t *testing.T) {
		for _, value := range []any{map[string]any{}, []any{true}, nil} {
			f := newPublisher(context.Background(), ".", io.Discard)
			f.api = func(string) (any, error) { return value, nil }
			if _, err := f.publishedReleases(); err == nil {
				t.Fatal("accepted malformed page")
			}
		}
	})
}

func TestStrictManifest(t *testing.T) {
	body := jsonBytes(fixtureManifest(1))
	for _, data := range [][]byte{[]byte(`[]`), []byte(`null`), []byte(`{"a":1,"a":2}`), append(body, []byte(" {}")...), []byte(strings.Replace(string(body), `"build":1`, `"build":true`, 1)), []byte(strings.Replace(string(body), repository, "other/project", 1)), []byte(strings.Replace(string(body), `"version":"0.1.0"`, `"version":"01.1.0"`, 1)), append(body, bytes.Repeat([]byte(" "), metadataLimit)...)} {
		if _, err := parseManifest(data); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
	if _, err := parseManifest(body); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationEnvironmentGate(t *testing.T) {
	valid := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": repository, "GITHUB_REF": "refs/heads/main"}
	if !publicationAllowed(valid) {
		t.Fatal("valid main publication rejected")
	}
	for key := range valid {
		environment := map[string]string{}
		for field, value := range valid {
			environment[field] = value
		}
		environment[key] = "invalid"
		if publicationAllowed(environment) {
			t.Fatal("invalid publication environment accepted", key)
		}
	}
}

func TestRunRequiresPublicationConfiguration(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", repository)
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("SPARKLE_ED_PRIVATE_KEY", "")
	t.Setenv("TAP_BOT_NAME", testBot)
	t.Setenv("TAP_BOT_EMAIL", testEmail)
	if err := Run([]string{"--publish", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("missing Sparkle key accepted", err)
	}
	t.Setenv("SPARKLE_ED_PRIVATE_KEY", "key")
	t.Setenv("TAP_BOT_EMAIL", "someone@example.invalid")
	if err := Run([]string{"--publish", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("non-bot identity accepted", err)
	}
}

// Opt-in: SPARKLE_SIGN_UPDATE names the real pinned sign_update. A throwaway key
// is generated here and never leaves memory, stdin or the test.
func TestRealSparkleSignerAcceptedAndDeterministic(t *testing.T) {
	tool := os.Getenv("SPARKLE_SIGN_UPDATE")
	if tool == "" {
		t.Skip("set SPARKLE_SIGN_UPDATE to the pinned sign_update to run")
	}
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archive := filepath.Join(directory, "archive.zip")
	if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	f := newPublisher(context.Background(), directory, io.Discard)
	f.signTool, f.signKey = tool, base64.StdEncoding.EncodeToString(private.Seed())
	first, err := f.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil || first != second {
		t.Fatal("real signing must be deterministic so identical content never recommits", err)
	}
}
